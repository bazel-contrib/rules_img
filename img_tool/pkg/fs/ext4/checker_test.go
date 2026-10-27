package ext4

import (
	"encoding/binary"
	"io"
	"sort"
	"testing"
)

// This file contains an independent reader and checker for the images the
// package writes. It only relies on the on-disk format, not on the writer's
// bookkeeping, so it catches layout mistakes the way fsck would.

type vnode struct {
	ino     uint32
	mode    uint16
	uid     uint32
	gid     uint32
	size    uint64
	links   uint16
	blocks  uint32 // i_blocks, in 512 byte sectors
	flags   uint32
	data    []byte // regular file content
	target  string
	dev     uint32
	atime   int64
	mtime   int64
	nsec    uint32
	xattrs  map[string]string
	extents []vextent
	// contiguous reports whether the data is one run of blocks.
	contiguous bool
	children   map[string]uint32
	ftypes     map[string]uint8
}

type vextent struct {
	logical uint32
	length  uint32
	phys    uint64
}

type checker struct {
	t      *testing.T
	r      io.ReaderAt
	bs     int
	fdb    uint32
	bpg    uint32
	ipg    uint32
	groups int
	blocks uint64
	seed   uint32
	gdt    []byte
	sb     []byte

	// the reachable part of the filesystem
	nodes     map[string]*vnode
	byIno     map[uint32]*vnode
	inodeUsed map[uint32]bool
	blockUsed []bool
	refs      map[uint32]int
	subdirs   map[uint32]int
	dirCount  map[int]int // by group
}

func (c *checker) read(off int64, n int) []byte {
	c.t.Helper()
	b := make([]byte, n)
	if _, err := c.r.ReadAt(b, off); err != nil && err != io.EOF {
		c.t.Fatalf("reading %d bytes at %d: %v", n, off, err)
	}
	return b
}

func (c *checker) readBlock(blk uint64) []byte {
	c.t.Helper()
	return c.read(int64(blk)*int64(c.bs), c.bs)
}

func (c *checker) errorf(format string, args ...any) {
	c.t.Helper()
	c.t.Errorf(format, args...)
}

// checkImage verifies the whole image and returns the files by path.
func checkImage(t *testing.T, r io.ReaderAt, size int64) *checker {
	t.Helper()
	c := &checker{
		t: t, r: r,
		nodes:     map[string]*vnode{},
		byIno:     map[uint32]*vnode{},
		inodeUsed: map[uint32]bool{},
		refs:      map[uint32]int{},
		subdirs:   map[uint32]int{},
		dirCount:  map[int]int{},
	}
	c.parseSuperblock(size)
	c.parseDescriptors()
	c.walk(rootIno, "", rootIno)
	c.checkLinks()
	c.checkBitmaps()
	return c
}

func (c *checker) parseSuperblock(size int64) {
	sb := c.read(1024, 1024)
	c.sb = sb
	le := binary.LittleEndian
	if le.Uint16(sb[0x38:]) != 0xEF53 {
		c.t.Fatalf("bad magic %#x", le.Uint16(sb[0x38:]))
	}
	if want := crc32c(0xFFFFFFFF, sb[:0x3FC]); le.Uint32(sb[0x3FC:]) != want {
		c.errorf("superblock checksum %#x, want %#x", le.Uint32(sb[0x3FC:]), want)
	}
	c.bs = 1024 << le.Uint32(sb[0x18:])
	c.fdb = le.Uint32(sb[0x14:])
	if (c.bs == 1024) != (c.fdb == 1) {
		c.errorf("first data block %d for block size %d", c.fdb, c.bs)
	}
	c.blocks = uint64(le.Uint32(sb[0x04:]))
	c.bpg = le.Uint32(sb[0x20:])
	c.ipg = le.Uint32(sb[0x28:])
	c.groups = int((c.blocks - uint64(c.fdb) + uint64(c.bpg) - 1) / uint64(c.bpg))
	if int64(c.blocks)*int64(c.bs) != size {
		c.errorf("image size %d, filesystem says %d", size, int64(c.blocks)*int64(c.bs))
	}
	if got := le.Uint32(sb[0x00:]); got != c.ipg*uint32(c.groups) {
		c.errorf("inode count %d, want %d", got, c.ipg*uint32(c.groups))
	}
	if le.Uint32(sb[0x5C:]) != featCompatExtAttr|featCompatSparseSuper ||
		le.Uint32(sb[0x60:]) != featIncompatFiletype|featIncompatExtents|featIncompatFlexBG ||
		le.Uint32(sb[0x64:]) != featROSparseSuper|featROLargeFile|featROHugeFile|featRODirNlink|featROExtraIsize|featROMetadataCsum {
		c.errorf("unexpected feature flags %#x %#x %#x", le.Uint32(sb[0x5C:]), le.Uint32(sb[0x60:]), le.Uint32(sb[0x64:]))
	}
	if le.Uint32(sb[0x54:]) != firstIno || le.Uint16(sb[0x58:]) != inodeSize {
		c.errorf("bad first inode or inode size")
	}
	c.seed = crc32c(0xFFFFFFFF, sb[0x68:0x78])
	if c.bpg > 8*uint32(c.bs) || c.ipg > 8*uint32(c.bs) || c.ipg*inodeSize%uint32(c.bs) != 0 {
		c.errorf("bad group geometry bpg=%d ipg=%d", c.bpg, c.ipg)
	}
}

func (c *checker) parseDescriptors() {
	gdtBlocks := (c.groups*descSize + c.bs - 1) / c.bs
	c.gdt = c.read(int64(c.fdb+1)*int64(c.bs), gdtBlocks*c.bs)
	le := binary.LittleEndian
	for g := 0; g < c.groups; g++ {
		d := c.gdt[g*descSize : (g+1)*descSize]
		cp := append([]byte(nil), d...)
		cp[0x1E], cp[0x1F] = 0, 0
		sum := crc32cLE32(c.seed, uint32(g))
		sum = crc32c(sum, cp)
		if uint16(sum) != le.Uint16(d[0x1E:]) {
			c.errorf("group %d: descriptor checksum %#x, want %#x", g, le.Uint16(d[0x1E:]), uint16(sum))
		}
	}
}

func (c *checker) group(g int) []byte { return c.gdt[g*descSize : (g+1)*descSize] }

func (c *checker) rawInode(ino uint32) []byte {
	le := binary.LittleEndian
	g := int((ino - 1) / c.ipg)
	idx := (ino - 1) % c.ipg
	table := uint64(le.Uint32(c.group(g)[8:]))
	rec := c.read(int64(table)*int64(c.bs)+int64(idx)*inodeSize, inodeSize)

	stored := uint32(le.Uint16(rec[0x7C:])) | uint32(le.Uint16(rec[0x82:]))<<16
	cp := append([]byte(nil), rec...)
	cp[0x7C], cp[0x7D], cp[0x82], cp[0x83] = 0, 0, 0, 0
	sum := crc32cLE32(c.seed, ino)
	sum = crc32cLE32(sum, le.Uint32(rec[0x64:]))
	sum = crc32c(sum, cp)
	if sum != stored {
		c.errorf("inode %d: checksum %#x, want %#x", ino, stored, sum)
	}
	return rec
}

// treeEntries walks an extent tree node, verifying checksums.
func (c *checker) treeEntries(ino uint32, node []byte, root bool, wantDepth int, blocks *[]uint64) []vextent {
	le := binary.LittleEndian
	if le.Uint16(node[0:]) != 0xF30A {
		c.t.Fatalf("inode %d: bad extent magic %#x", ino, le.Uint16(node[0:]))
	}
	entries, max, depth := int(le.Uint16(node[2:])), int(le.Uint16(node[4:])), int(le.Uint16(node[6:]))
	if wantDepth >= 0 && depth != wantDepth {
		c.errorf("inode %d: extent node depth %d, want %d", ino, depth, wantDepth)
	}
	if root && max != 4 || !root && max != (c.bs-12)/12 {
		c.errorf("inode %d: extent node max %d", ino, max)
	}
	if entries > max {
		c.t.Fatalf("inode %d: %d entries exceed max %d", ino, entries, max)
	}
	if !root {
		tail := 12 + 12*max
		sum := crc32cLE32(c.seed, ino)
		sum = crc32cLE32(sum, 0)
		sum = crc32c(sum, node[:tail])
		if le.Uint32(node[tail:]) != sum {
			c.errorf("inode %d: extent block checksum %#x, want %#x", ino, le.Uint32(node[tail:]), sum)
		}
	}
	var out []vextent
	for i := 0; i < entries; i++ {
		e := node[12+12*i:]
		if depth == 0 {
			length := uint32(le.Uint16(e[4:]))
			if length > maxExtentLen {
				c.errorf("inode %d: uninitialized extent", ino)
			}
			out = append(out, vextent{le.Uint32(e[0:]), length, uint64(le.Uint16(e[6:]))<<32 | uint64(le.Uint32(e[8:]))})
			continue
		}
		child := uint64(le.Uint16(e[8:]))<<32 | uint64(le.Uint32(e[4:]))
		*blocks = append(*blocks, child)
		out = append(out, c.treeEntries(ino, c.readBlock(child), false, depth-1, blocks)...)
	}
	return out
}

var xattrPrefixByIndex = map[byte]string{1: "user.", 2: "system.posix_acl_access", 3: "system.posix_acl_default", 4: "trusted.", 6: "security.", 7: "system."}

func (c *checker) parseXattrs(area []byte, entries, base int, out map[string]string) {
	le := binary.LittleEndian
	for pos := entries; pos+4 <= len(area) && le.Uint32(area[pos:]) != 0; {
		nameLen := int(area[pos])
		idx := area[pos+1]
		offs := int(le.Uint16(area[pos+2:]))
		size := int(le.Uint32(area[pos+8:]))
		hash := le.Uint32(area[pos+12:])
		name := string(area[pos+16 : pos+16+nameLen])
		value := area[base+offs : base+offs+size]
		x := xattr{index: idx, name: name, value: value}
		if hash != xattrEntryHash(x) {
			c.errorf("xattr %s: hash %#x, want %#x", name, hash, xattrEntryHash(x))
		}
		out[xattrPrefixByIndex[idx]+name] = string(value)
		pos += 16 + pad4(nameLen)
	}
}

func (c *checker) walk(ino uint32, path string, parent uint32) {
	le := binary.LittleEndian
	rec := c.rawInode(ino)
	c.inodeUsed[ino] = true

	n := &vnode{
		ino:    ino,
		mode:   le.Uint16(rec[0:]),
		uid:    uint32(le.Uint16(rec[2:])) | uint32(le.Uint16(rec[0x78:]))<<16,
		gid:    uint32(le.Uint16(rec[0x18:])) | uint32(le.Uint16(rec[0x7A:]))<<16,
		size:   uint64(le.Uint32(rec[4:])) | uint64(le.Uint32(rec[0x6C:]))<<32,
		links:  le.Uint16(rec[0x1A:]),
		blocks: le.Uint32(rec[0x1C:]),
		flags:  le.Uint32(rec[0x20:]),
		xattrs: map[string]string{},
	}
	sectorsHigh := uint64(le.Uint16(rec[0x74:]))
	total := sectorsHigh<<32 | uint64(n.blocks)
	if le.Uint16(rec[0x80:]) != extraIsize {
		c.errorf("inode %d: extra isize %d", ino, le.Uint16(rec[0x80:]))
	}
	n.mtime = int64(int32(le.Uint32(rec[0x10:]))) + int64(le.Uint32(rec[0x88:])&3)<<32
	n.atime = int64(int32(le.Uint32(rec[0x08:]))) + int64(le.Uint32(rec[0x8C:])&3)<<32
	n.nsec = le.Uint32(rec[0x88:]) >> 2
	c.nodes[path] = n
	c.byIno[ino] = n

	// extended attributes
	if le.Uint32(rec[ibodyOffset:]) == xattrMagic {
		c.parseXattrs(rec[ibodyOffset+4:], 0, 0, n.xattrs)
	}
	var usedBlocks uint64
	if acl := uint64(le.Uint16(rec[0x76:]))<<32 | uint64(le.Uint32(rec[0x68:])); acl != 0 {
		blk := c.readBlock(acl)
		if le.Uint32(blk[0:]) != xattrMagic {
			c.errorf("inode %d: bad xattr block magic", ino)
		}
		cp := append([]byte(nil), blk...)
		cp[16], cp[17], cp[18], cp[19] = 0, 0, 0, 0
		sum := crc32c(crc32c(c.seed, binary.LittleEndian.AppendUint64(nil, acl)), cp)
		if le.Uint32(blk[16:]) != sum {
			c.errorf("inode %d: xattr block checksum %#x, want %#x", ino, le.Uint32(blk[16:]), sum)
		}
		c.parseXattrs(blk, xattrBlockHeader, 0, n.xattrs)
		c.markBlock(acl)
		usedBlocks++
	}

	kind := n.mode & 0xF000
	iblock := rec[0x28 : 0x28+60]
	if n.flags&flagExtents != 0 {
		var tree []uint64
		n.extents = c.treeEntries(ino, iblock, true, -1, &tree)
		for _, b := range tree {
			c.markBlock(b)
		}
		usedBlocks += uint64(len(tree))
		n.contiguous = true
		var next uint64
		for i, x := range n.extents {
			if i > 0 && x.phys != next {
				n.contiguous = false
			}
			if i > 0 && x.logical != n.extents[i-1].logical+n.extents[i-1].length {
				c.errorf("inode %d: extents are not adjacent in the file", ino)
			}
			next = x.phys + uint64(x.length)
			for b := uint64(0); b < uint64(x.length); b++ {
				c.markBlock(x.phys + b)
			}
			usedBlocks += uint64(x.length)
		}
	}
	if want := usedBlocks * uint64(c.bs/512); total != want {
		c.errorf("inode %d: i_blocks %d, want %d", ino, total, want)
	}

	readData := func() []byte {
		out := make([]byte, n.size)
		for _, x := range n.extents {
			for b := uint64(0); b < uint64(x.length); b++ {
				off := (uint64(x.logical) + b) * uint64(c.bs)
				if off >= n.size {
					break
				}
				blk := c.readBlock(x.phys + b)
				copy(out[off:], blk)
			}
		}
		return out
	}

	switch kind {
	case 0x8000:
		n.data = readData()
		want := (n.size + uint64(c.bs) - 1) / uint64(c.bs)
		var got uint64
		for _, x := range n.extents {
			got += uint64(x.length)
		}
		if got != want {
			c.errorf("%s: %d blocks for %d bytes", path, got, n.size)
		}
	case 0xA000:
		if n.flags&flagExtents == 0 {
			n.target = string(iblock[:n.size])
		} else {
			n.target = string(readData())
		}
	case 0x2000, 0x6000:
		if d := le.Uint32(iblock[0:]); d != 0 {
			n.dev = (d>>8)<<20 | d&0xff // old encoding: major<<8|minor
		} else {
			d := le.Uint32(iblock[4:])
			major, minor := d>>8&0xfff, d&0xff|(d>>12)&^0xff
			n.dev = major<<20 | minor
		}
	case 0x4000:
		c.readDir(ino, path, parent, n, readData())
	case 0x1000:
	default:
		c.errorf("inode %d: unexpected mode %#o", ino, n.mode)
	}
}

func (c *checker) markBlock(b uint64) {
	if c.blockUsed == nil {
		c.blockUsed = make([]bool, c.blocks)
	}
	if b >= c.blocks {
		c.t.Fatalf("block %d out of range", b)
	}
	if c.blockUsed[b] {
		c.errorf("block %d is used twice", b)
	}
	c.blockUsed[b] = true
}

func (c *checker) readDir(ino uint32, path string, parent uint32, n *vnode, data []byte) {
	le := binary.LittleEndian
	if len(data)%c.bs != 0 {
		c.errorf("directory %q size %d is not a multiple of the block size", path, len(data))
	}
	n.children = map[string]uint32{}
	n.ftypes = map[string]uint8{}
	for off := 0; off < len(data); off += c.bs {
		blk := data[off : off+c.bs]
		tail := blk[c.bs-12:]
		sum := crc32cLE32(c.seed, ino)
		sum = crc32cLE32(sum, 0)
		sum = crc32c(sum, blk[:c.bs-12])
		if le.Uint32(tail[0:]) != 0 || le.Uint16(tail[4:]) != 12 || tail[6] != 0 || tail[7] != 0xDE || le.Uint32(tail[8:]) != sum {
			c.errorf("directory %q block %d: bad checksum tail", path, off/c.bs)
		}
		pos := 0
		for pos < c.bs-12 {
			e := blk[pos:]
			entIno := le.Uint32(e[0:])
			recLen := int(le.Uint16(e[4:]))
			nameLen := int(e[6])
			if recLen < recLenOf(nameLen) || recLen%4 != 0 || pos+recLen > c.bs-12 {
				c.t.Fatalf("directory %q: bad entry at %d: rec_len %d name_len %d", path, pos, recLen, nameLen)
			}
			name := string(e[8 : 8+nameLen])
			if entIno != 0 {
				if off == 0 && pos == 0 {
					if name != "." || entIno != ino {
						c.errorf("directory %q: first entry is %q -> %d", path, name, entIno)
					}
				} else if off == 0 && pos == 12 {
					if name != ".." || entIno != parent {
						c.errorf("directory %q: second entry is %q -> %d, parent is %d", path, name, entIno, parent)
					}
				} else {
					if _, dup := n.children[name]; dup {
						c.errorf("directory %q: duplicate entry %q", path, name)
					}
					n.children[name] = entIno
					n.ftypes[name] = e[7]
				}
			}
			pos += recLen
		}
		if pos != c.bs-12 {
			c.errorf("directory %q block %d does not end at the tail", path, off/c.bs)
		}
	}

	g := int((ino - 1) / c.ipg)
	c.dirCount[g]++
	names := make([]string, 0, len(n.children))
	for name := range n.children {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		child := n.children[name]
		c.refs[child]++
		childPath := name
		if path != "" {
			childPath = path + "/" + name
		}
		if _, seen := c.inodeUsed[child]; seen {
			c.nodes[childPath] = c.byIno[child] // hard link, already visited
			continue
		}
		c.walk(child, childPath, ino)
		if c.nodes[childPath].mode&0xF000 == 0x4000 {
			c.subdirs[ino]++
		}
		if want := ftypeOfMode(c.nodes[childPath].mode); n.ftypes[name] != want {
			c.errorf("%s: dirent type %d, want %d", childPath, n.ftypes[name], want)
		}
	}
}

func recLenOf(n int) int { return (8 + n + 3) &^ 3 }

func ftypeOfMode(mode uint16) uint8 {
	switch mode & 0xF000 {
	case 0x8000:
		return 1
	case 0x4000:
		return 2
	case 0x2000:
		return 3
	case 0x6000:
		return 4
	case 0x1000:
		return 5
	case 0xA000:
		return 7
	}
	return 0
}

func (c *checker) checkLinks() {
	for path, n := range c.nodes {
		want := c.refs[n.ino]
		if n.mode&0xF000 == 0x4000 {
			want = 2 + c.subdirs[n.ino]
		}
		if int(n.links) != want && !(want > maxLinks && n.links == 1) {
			c.errorf("%q: link count %d, want %d", path, n.links, want)
		}
	}
}

func (c *checker) checkBitmaps() {
	le := binary.LittleEndian
	if c.blockUsed == nil {
		c.blockUsed = make([]bool, c.blocks)
	}
	// Add the metadata the descriptors describe.
	mark := func(from, count uint64) {
		for b := from; b < from+count; b++ {
			c.markBlock(b)
		}
	}
	mark(0, uint64(c.fdb)+1)
	mark(uint64(c.fdb)+1, uint64((c.groups*descSize+c.bs-1)/c.bs))
	itb := uint64(c.ipg) * inodeSize / uint64(c.bs)
	for g := 0; g < c.groups; g++ {
		d := c.group(g)
		mark(uint64(le.Uint32(d[0:])), 1)
		mark(uint64(le.Uint32(d[4:])), 1)
		mark(uint64(le.Uint32(d[8:])), itb)
	}

	var totalFreeBlocks, totalFreeInodes uint64
	for g := 0; g < c.groups; g++ {
		d := c.group(g)
		flags := le.Uint16(d[0x12:])
		start := uint64(c.fdb) + uint64(g)*uint64(c.bpg)
		gBlocks := min(uint64(c.bpg), c.blocks-start)

		var free uint64
		for b := uint64(0); b < gBlocks; b++ {
			if !c.blockUsed[start+b] {
				free++
			}
		}
		totalFreeBlocks += free
		if uint64(le.Uint16(d[0x0C:])) != free {
			c.errorf("group %d: free blocks %d, want %d", g, le.Uint16(d[0x0C:]), free)
		}

		if flags&bgBlockUninit != 0 {
			if free != gBlocks || g == c.groups-1 {
				c.errorf("group %d: block-uninit group is not empty or is the last", g)
			}
		} else {
			bm := c.readBlock(uint64(le.Uint32(d[0:])))
			for b := uint64(0); b < uint64(c.bs)*8; b++ {
				got := bm[b/8]>>(b%8)&1 == 1
				want := b >= gBlocks || c.blockUsed[start+b]
				if got != want {
					c.errorf("group %d: block bitmap bit %d is %v, want %v", g, b, got, want)
					break
				}
			}
			if sum := crc32c(c.seed, bm[:c.bpg/8]); uint16(sum) != le.Uint16(d[0x18:]) {
				c.errorf("group %d: block bitmap checksum %#x, want %#x", g, le.Uint16(d[0x18:]), uint16(sum))
			}
		}

		// inodes: the reserved ones, then whatever was reached
		var usedInodes, last uint32
		isUsed := func(i uint32) bool {
			ino := uint32(g)*c.ipg + i + 1
			return ino < firstIno || c.inodeUsed[ino]
		}
		for i := uint32(0); i < c.ipg; i++ {
			if isUsed(i) {
				usedInodes++
				last = i + 1
			}
		}
		totalFreeInodes += uint64(c.ipg - usedInodes)
		if uint32(le.Uint16(d[0x0E:])) != c.ipg-usedInodes {
			c.errorf("group %d: free inodes %d, want %d", g, le.Uint16(d[0x0E:]), c.ipg-usedInodes)
		}
		if uint32(le.Uint16(d[0x1C:])) > c.ipg-last {
			c.errorf("group %d: itable_unused %d, but only %d at the end are unused", g, le.Uint16(d[0x1C:]), c.ipg-last)
		}
		if int(le.Uint16(d[0x10:])) != c.dirCount[g] {
			c.errorf("group %d: %d directories, want %d", g, le.Uint16(d[0x10:]), c.dirCount[g])
		}
		if flags&bgInodeZeroed == 0 {
			c.errorf("group %d: inode table not marked zeroed", g)
		}
		if flags&bgInodeUninit != 0 {
			if usedInodes != 0 {
				c.errorf("group %d: inode-uninit group has used inodes", g)
			}
		} else {
			bm := c.readBlock(uint64(le.Uint32(d[4:])))
			for b := uint32(0); b < uint32(c.bs)*8; b++ {
				got := bm[b/8]>>(b%8)&1 == 1
				want := b >= c.ipg || isUsed(b)
				if got != want {
					c.errorf("group %d: inode bitmap bit %d is %v, want %v", g, b, got, want)
					break
				}
			}
			if sum := crc32c(c.seed, bm[:c.ipg/8]); uint16(sum) != le.Uint16(d[0x1A:]) {
				c.errorf("group %d: inode bitmap checksum %#x, want %#x", g, le.Uint16(d[0x1A:]), uint16(sum))
			}
		}
	}
	if got := uint64(le.Uint32(c.sb[0x0C:])); got != totalFreeBlocks {
		c.errorf("superblock free blocks %d, want %d", got, totalFreeBlocks)
	}
	if got := uint64(le.Uint32(c.sb[0x10:])); got != totalFreeInodes {
		c.errorf("superblock free inodes %d, want %d", got, totalFreeInodes)
	}
}

// memFile is an in-memory WriterAt/ReaderAt that reads zeros for ranges that
// were never written.
type memFile struct {
	b      []byte
	writes int
	bytes  int
}

func (m *memFile) grow(n int64) {
	if int64(len(m.b)) < n {
		m.b = append(m.b, make([]byte, n-int64(len(m.b)))...)
	}
}

func (m *memFile) WriteAt(p []byte, off int64) (int, error) {
	m.grow(off + int64(len(p)))
	copy(m.b[off:], p)
	m.writes++
	m.bytes += len(p)
	return len(p), nil
}

func (m *memFile) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(m.b)) {
		return 0, io.EOF
	}
	n := copy(p, m.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *memFile) Truncate(size int64) error {
	m.grow(size)
	m.b = m.b[:size]
	return nil
}

// noTruncate hides Truncate to exercise the fallback for plain WriterAts.
type noTruncate struct{ m *memFile }

func (n noTruncate) WriteAt(p []byte, off int64) (int, error) { return n.m.WriteAt(p, off) }

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	x := uint32(seed)*2654435761 + 1
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}
