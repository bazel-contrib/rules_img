package ext4

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"path"
	"sort"
)

// planned holds the layout decisions for a node.
type planned struct {
	ino   uint32
	nlink uint32 // directory entries referencing the node; subdirectories for a directory
	path  string // first path of the node

	// parent is the directory containing a directory node.
	parent *node
	// dirents are the sorted entries of a directory, and dirBlocks the way
	// they are split over its blocks.
	dirents   []dirent
	dirBlocks [][]dirent

	// run is the contiguous run of blocks holding the data of a regular
	// file, the entries of a directory or a long symlink target.
	runStart, runLen uint64
	// treeBlocks hold the interior and leaf nodes of the extent tree when
	// the extents do not fit in the inode.
	treeBlocks []uint64
	// xattrBlock holds the extended attributes that do not fit the inode.
	xattrInBlock bool
	xattrBlock   uint64

	// digest is the SHA-256 of the content of a regular file, if requested.
	digest []byte
}

type dirent struct {
	name  string
	ino   uint32
	ftype uint8
}

// layout is the complete plan of an image.
type layout struct {
	bs         uint64
	fdb        uint64 // first data block: 1 for 1 KiB blocks, else 0
	bpg        uint64 // blocks per group
	ipg        uint32 // inodes per group
	groups     int
	itb        uint64 // inode table blocks per group
	gdtBlocks  uint64
	flexLog    uint8
	totalBlock uint64
	usedBlocks uint64
	usedInodes uint32 // including the reserved inodes

	gdtStart, blockBitmapStart, inodeBitmapStart, inodeTableStart uint64
	metaStart, dataStart                                          uint64

	nodes []*node // in inode order; the first is the root
	byIno []*node // indexed by inode number; nil for reserved inodes
	uuid  [16]byte
	seed  [16]byte
}

func ceilDiv(a, b uint64) uint64 { return (a + b - 1) / b }

func (b *Builder) plan() (*layout, error) {
	l := &layout{bs: uint64(b.opts.BlockSize)}
	if l.bs == 1024 {
		l.fdb = 1
	}
	l.bpg = min(8*l.bs, 65528)

	if err := b.deduplicate(); err != nil {
		return nil, err
	}
	b.collect(l)
	if err := b.sizeNodes(l); err != nil {
		return nil, err
	}
	if err := b.geometry(l); err != nil {
		return nil, err
	}
	b.assign(l)
	b.deriveIdentity(l)
	return l, nil
}

// collect finds the nodes reachable from the root, numbers them and counts
// their links.
func (b *Builder) collect(l *layout) {
	seen := map[*node]bool{b.root: true}
	nodes := []*node{b.root}
	b.root.planned = planned{path: "", parent: b.root}

	var walk func(dir *node)
	walk = func(dir *node) {
		names := make([]string, 0, len(dir.children))
		for name := range dir.children {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			child := dir.children[name]
			if !seen[child] {
				seen[child] = true
				child.planned = planned{path: path.Join(dir.planned.path, name)}
				if child.kind == kindDir {
					child.planned.parent = dir
				}
				nodes = append(nodes, child)
				if child.kind == kindDir {
					walk(child)
				}
			}
			if child.kind == kindDir {
				dir.planned.nlink++
			} else {
				child.planned.nlink++
			}
		}
	}
	walk(b.root)

	// Inodes are handed out in the order the entries were added. The root
	// is inode 2; everything else follows the reserved inodes.
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].seq < nodes[j].seq })
	l.nodes = nodes

	ino := uint32(firstIno)
	l.byIno = make([]*node, 0, len(nodes)+firstIno)
	l.byIno = append(l.byIno, make([]*node, firstIno)...)
	for _, n := range nodes {
		if n == b.root {
			n.planned.ino = rootIno
			l.byIno[rootIno] = n
			continue
		}
		n.planned.ino = ino
		l.byIno = append(l.byIno, n)
		ino++
	}
	l.usedInodes = ino - 1
}

func ftypeOf(k kind) uint8 {
	switch k {
	case kindReg:
		return 1
	case kindDir:
		return 2
	case kindChar:
		return 3
	case kindBlock:
		return 4
	case kindFifo:
		return 5
	case kindSymlink:
		return 7
	}
	return 0
}

// recLen is the size of a directory entry with a name of n bytes.
func recLen(n int) int { return (8 + n + 3) &^ 3 }

// packDir splits the sorted entries of a directory over its blocks. The first
// block starts with "." and "..", every block ends with the checksum tail.
func packDir(ents []dirent, blockSize int) [][]dirent {
	capacity := blockSize - 12
	blocks := [][]dirent{nil}
	used := 24
	for _, e := range ents {
		sz := recLen(len(e.name))
		if used+sz > capacity {
			blocks = append(blocks, nil)
			used = 0
		}
		last := len(blocks) - 1
		blocks[last] = append(blocks[last], e)
		used += sz
	}
	return blocks
}

// treeBlockCount is the number of extent tree blocks needed for n extents.
func treeBlockCount(n int, blockSize uint64) uint64 {
	capacity := int((blockSize - 12) / 12)
	var total uint64
	for n > 4 {
		n = (n + capacity - 1) / capacity
		total += uint64(n)
	}
	return total
}

// sizeNodes determines how many blocks every node needs.
func (b *Builder) sizeNodes(l *layout) error {
	for _, n := range l.nodes {
		p := &n.planned
		switch n.kind {
		case kindDir:
			p.dirents = make([]dirent, 0, len(n.children))
			for name, c := range n.children {
				p.dirents = append(p.dirents, dirent{name: name, ino: c.planned.ino, ftype: ftypeOf(c.kind)})
			}
			sort.Slice(p.dirents, func(i, j int) bool { return p.dirents[i].name < p.dirents[j].name })
			p.dirBlocks = packDir(p.dirents, b.opts.BlockSize)
			p.runLen = uint64(len(p.dirBlocks))
		case kindReg:
			p.runLen = ceilDiv(uint64(n.size), l.bs)
		case kindSymlink:
			if len(n.target) >= maxFastSymlink {
				p.runLen = 1
			}
		}
		if p.runLen > 0 {
			p.treeBlocks = make([]uint64, treeBlockCount(int(ceilDiv(p.runLen, maxExtentLen)), l.bs))
		}

		if len(n.xattrs) > 0 && !xattrFitsInode(n.xattrs) {
			if !xattrFitsBlock(n.xattrs, b.opts.BlockSize) {
				return fmt.Errorf("ext4: %s: extended attributes do not fit a block", n.planned.path)
			}
			p.xattrInBlock = true
		}
	}
	return nil
}

// pools returns the number of blocks of metadata and of file data.
func (l *layout) pools() (meta, data uint64) {
	for _, n := range l.nodes {
		p := &n.planned
		meta += uint64(len(p.treeBlocks))
		if p.xattrInBlock {
			meta++
		}
		if n.kind == kindReg {
			data += p.runLen
		} else {
			meta += p.runLen
		}
	}
	return meta, data
}

// geometry chooses the number of block groups and the size of the
// filesystem.
func (b *Builder) geometry(l *layout) error {
	meta, data := l.pools()
	bs := l.bs

	wantInodes := uint64(l.usedInodes) + uint64(b.opts.ExtraInodes)
	minBlocks := ceilDiv(uint64(b.opts.Size), bs)
	if b.opts.BytesPerInode > 0 && b.opts.Size > 0 {
		wantInodes = max(wantInodes, uint64(b.opts.Size/b.opts.BytesPerInode))
	}

	// ipgAlign makes the inode table of a group a whole number of blocks.
	ipgAlign := max(8, bs/inodeSize)
	// Without the 64bit feature the free inode count of a group is 16 bits.
	maxIPG := min(8*bs, 65535/ipgAlign*ipgAlign)

	groups := uint64(1)
	// inodeGroups is the number of groups the inode count alone requires. The
	// filesystem has to be large enough to have that many groups.
	inodeGroups := uint64(1)
	for {
		ipg := max(ceilDiv(ceilDiv(wantInodes, groups), ipgAlign)*ipgAlign, 2*ipgAlign)
		if ipg > maxIPG {
			groups = ceilDiv(wantInodes, maxIPG)
			inodeGroups = groups
			continue
		}
		itb := ipg * inodeSize / bs
		gdtBlocks := ceilDiv(groups*descSize, bs)
		overhead := l.fdb + 1 + gdtBlocks + 2*groups + groups*itb
		used := overhead + meta + data
		total := max(used, minBlocks)
		if inodeGroups > 1 {
			total = max(total, l.fdb+(inodeGroups-1)*l.bpg+minLastGroupBlocks)
		}

		need := ceilDiv(total-l.fdb, l.bpg)
		if need > groups {
			groups = need
			continue
		}
		// Do not leave a last group too small to be useful.
		if rem := (total - l.fdb) - (groups-1)*l.bpg; groups > 1 && rem < minLastGroupBlocks {
			total += minLastGroupBlocks - rem
		}
		if total >= math.MaxUint32 {
			return fmt.Errorf("ext4: image of %d blocks exceeds the 2^32 block limit", total)
		}
		if groups*ipg >= math.MaxUint32 {
			return fmt.Errorf("ext4: too many inodes")
		}

		l.groups = int(groups)
		l.ipg = uint32(ipg)
		l.itb = itb
		l.gdtBlocks = gdtBlocks
		l.totalBlock = total
		l.usedBlocks = used
		l.gdtStart = l.fdb + 1
		l.blockBitmapStart = l.gdtStart + gdtBlocks
		l.inodeBitmapStart = l.blockBitmapStart + groups
		l.inodeTableStart = l.inodeBitmapStart + groups
		l.metaStart = l.inodeTableStart + groups*itb
		l.dataStart = l.metaStart + meta
		for 1<<l.flexLog < groups && l.flexLog < 31 {
			l.flexLog++
		}
		l.flexLog = max(l.flexLog, 4)
		return nil
	}
}

// assign places every run of blocks.
func (b *Builder) assign(l *layout) {
	meta, data := l.metaStart, l.dataStart
	take := func(pool *uint64, n uint64) uint64 {
		start := *pool
		*pool += n
		return start
	}
	for _, n := range l.nodes {
		p := &n.planned
		if n.kind == kindReg {
			p.runStart = take(&data, p.runLen)
		} else {
			p.runStart = take(&meta, p.runLen)
		}
		for i := range p.treeBlocks {
			p.treeBlocks[i] = take(&meta, 1)
		}
		if p.xattrInBlock {
			p.xattrBlock = take(&meta, 1)
		}
	}
}

// deriveIdentity fills in the UUID and hash seed, from the options or from
// a digest of the metadata of the tree so equal trees give equal images.
func (b *Builder) deriveIdentity(l *layout) {
	h := sha256.New()
	var buf [8]byte
	put := func(v uint64) {
		binary.LittleEndian.PutUint64(buf[:], v)
		h.Write(buf[:])
	}
	for _, n := range l.nodes {
		put(uint64(n.planned.ino))
		put(uint64(n.kind))
		put(uint64(n.mode))
		put(uint64(n.uid)<<32 | uint64(n.gid))
		put(uint64(n.size))
		put(uint64(n.mtime.Unix()))
		h.Write([]byte(n.planned.path))
		h.Write([]byte{0})
		h.Write([]byte(n.target))
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	copy(l.seed[:], sum[16:])
	if b.opts.UUID != ([16]byte{}) {
		l.uuid = b.opts.UUID
		return
	}
	copy(l.uuid[:], sum[:16])
	l.uuid[6] = l.uuid[6]&0x0f | 0x50
	l.uuid[8] = l.uuid[8]&0x3f | 0x80
}

// groupBlocks is the number of blocks in group g.
func (l *layout) groupBlocks(g int) uint64 {
	if g == l.groups-1 {
		return l.totalBlock - l.fdb - uint64(g)*l.bpg
	}
	return l.bpg
}

// groupUsedBlocks is the number of blocks of group g that are in use. All
// used blocks form a prefix of the filesystem.
func (l *layout) groupUsedBlocks(g int) uint64 {
	start := l.fdb + uint64(g)*l.bpg
	if l.usedBlocks <= start {
		return 0
	}
	return min(l.usedBlocks-start, l.groupBlocks(g))
}

// groupUsedInodes is the number of inodes of group g that are in use. They
// also form a prefix.
func (l *layout) groupUsedInodes(g int) uint32 {
	start := uint64(g) * uint64(l.ipg)
	if uint64(l.usedInodes) <= start {
		return 0
	}
	return uint32(min(uint64(l.usedInodes)-start, uint64(l.ipg)))
}
