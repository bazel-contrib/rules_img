package ext4

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"time"
)

const (
	flagExtents = 0x80000

	bgInodeUninit = 0x1
	bgBlockUninit = 0x2
	bgInodeZeroed = 0x4

	featCompatExtAttr     = 0x8
	featCompatSparseSuper = 0x200
	featIncompatFiletype  = 0x2
	featIncompatExtents   = 0x40
	featIncompatFlexBG    = 0x200
	featROSparseSuper     = 0x1
	featROLargeFile       = 0x2
	featROHugeFile        = 0x8
	featRODirNlink        = 0x20
	featROExtraIsize      = 0x40
	featROMetadataCsum    = 0x400

	// maxLinks is the most hard links ext4 allows to a file.
	maxLinks = 65000

	copyBufferSize = 1 << 20
)

var le = binary.LittleEndian

// truncater is implemented by files that can be extended without writing.
type truncater interface {
	Truncate(size int64) error
}

// Build lays out the filesystem and writes it to w, which must read as zeros
// wherever nothing was written (see the package documentation). Build may be
// called once. Content passed by path is read now, so the files must still
// be there.
func (b *Builder) Build(w io.WriterAt) (*Info, error) {
	if b.built {
		return nil, errors.New("ext4: image already built")
	}
	b.built = true

	l, err := b.plan()
	if err != nil {
		return nil, err
	}
	e := &emitter{
		b:    b,
		l:    l,
		w:    w,
		bs:   int(l.bs),
		seed: crc32c(0xFFFFFFFF, l.uuid[:]),
		buf:  make([]byte, copyBufferSize),
		zero: make([]byte, max(int(l.bs), 4096)),
	}
	if err := e.writeFiles(); err != nil {
		return nil, err
	}
	if err := e.writeInodes(); err != nil {
		return nil, err
	}
	if err := e.writeBitmaps(); err != nil {
		return nil, err
	}
	e.writeDescriptors()
	e.writeSuperblock()
	if e.err != nil {
		return nil, e.err
	}
	return e.finish()
}

type emitter struct {
	b    *Builder
	l    *layout
	w    io.WriterAt
	bs   int
	seed uint32
	buf  []byte
	zero []byte

	// end is the end of the furthest byte written.
	end int64
	err error
}

// writeSparse writes p at off, leaving out the blocks that are all zero.
func (e *emitter) writeSparse(off int64, p []byte) {
	for i := 0; i < len(p) && e.err == nil; {
		end := min(i+e.bs, len(p))
		if bytes.Equal(p[i:end], e.zero[:end-i]) {
			i = end
			continue
		}
		j := end
		for j < len(p) {
			next := min(j+e.bs, len(p))
			if bytes.Equal(p[j:next], e.zero[:next-j]) {
				break
			}
			j = next
		}
		e.write(off+int64(i), p[i:j])
		i = j
	}
}

func (e *emitter) write(off int64, p []byte) {
	if e.err != nil || len(p) == 0 {
		return
	}
	if _, err := e.w.WriteAt(p, off); err != nil {
		e.err = fmt.Errorf("ext4: writing at offset %d: %w", off, err)
		return
	}
	e.end = max(e.end, off+int64(len(p)))
}

func (e *emitter) blockOffset(blk uint64) int64 { return int64(blk) * int64(e.bs) }

// writeFiles streams the content of every regular file to its run of blocks.
func (e *emitter) writeFiles() error {
	for _, n := range e.l.nodes {
		if n.kind != kindReg || n.size == 0 {
			continue
		}
		if err := e.writeFile(n); err != nil {
			return err
		}
	}
	return e.err
}

func (e *emitter) writeFile(n *node) error {
	rc, err := n.src.open()
	if err != nil {
		return fmt.Errorf("ext4: %s: %w", n.planned.path, err)
	}
	defer rc.Close()

	var digest hash.Hash
	if e.b.opts.DigestFiles {
		digest = sha256.New()
	}
	off := e.blockOffset(n.planned.runStart)
	for remaining := n.size; remaining > 0; {
		chunk := int(min(int64(len(e.buf)), remaining))
		if _, err := io.ReadFull(rc, e.buf[:chunk]); err != nil {
			return fmt.Errorf("ext4: %s: reading content: %w (declared size %d)", n.planned.path, unexpected(err), n.size)
		}
		if digest != nil {
			digest.Write(e.buf[:chunk])
		}
		e.writeSparse(off, e.buf[:chunk])
		if e.err != nil {
			return e.err
		}
		off += int64(chunk)
		remaining -= int64(chunk)
	}
	if digest != nil {
		n.planned.digest = digest.Sum(nil)
	}
	return nil
}

// writeInodes writes the inode tables and, along the way, everything the
// inodes point to besides file data.
func (e *emitter) writeInodes() error {
	l := e.l
	for g := 0; g < l.groups; g++ {
		used := int(l.groupUsedInodes(g))
		if used == 0 {
			continue
		}
		table := make([]byte, used*inodeSize)
		for i := 0; i < used; i++ {
			n := l.byIno[g*int(l.ipg)+i+1]
			if n == nil {
				continue // reserved inode
			}
			rec, err := e.emitNode(n)
			if err != nil {
				return err
			}
			copy(table[i*inodeSize:], rec)
		}
		e.writeSparse(e.blockOffset(l.inodeTableStart+uint64(g)*l.itb), table)
		if e.err != nil {
			return e.err
		}
	}
	return nil
}

func typeBits(k kind) uint32 {
	switch k {
	case kindReg:
		return 0x8000
	case kindDir:
		return 0x4000
	case kindSymlink:
		return 0xA000
	case kindChar:
		return 0x2000
	case kindBlock:
		return 0x6000
	case kindFifo:
		return 0x1000
	}
	return 0
}

// encodeTime returns the 32 bit seconds and the extra field of an inode
// timestamp: two more bits of seconds and the nanoseconds.
func (e *emitter) encodeTime(t time.Time) (sec, extra uint32) {
	if t.IsZero() {
		t = e.b.opts.Time
	}
	s := t.Unix()
	const minSec, maxSec = -1 << 31, 3<<32 + 1<<31 - 1
	s = min(max(s, minSec), maxSec)
	lo := int32(s)
	epoch := uint32((s - int64(lo)) >> 32)
	return uint32(lo), epoch | uint32(t.Nanosecond())<<2
}

// emitNode writes the blocks of a node and returns its inode.
func (e *emitter) emitNode(n *node) ([]byte, error) {
	p := &n.planned
	bs := uint64(e.bs)
	rec := make([]byte, inodeSize)

	le.PutUint16(rec[0x00:], uint16(n.mode&0o7777|typeBits(n.kind)))
	le.PutUint16(rec[0x02:], uint16(n.uid))
	le.PutUint16(rec[0x78:], uint16(n.uid>>16))
	le.PutUint16(rec[0x18:], uint16(n.gid))
	le.PutUint16(rec[0x7A:], uint16(n.gid>>16))

	var size uint64
	switch n.kind {
	case kindReg:
		size = uint64(n.size)
	case kindDir:
		size = p.runLen * bs
	case kindSymlink:
		size = uint64(len(n.target))
	}
	le.PutUint32(rec[0x04:], uint32(size))
	le.PutUint32(rec[0x6C:], uint32(size>>32))

	atime, atimeExtra := e.encodeTime(n.atime)
	ctime, ctimeExtra := e.encodeTime(n.ctime)
	mtime, mtimeExtra := e.encodeTime(n.mtime)
	le.PutUint32(rec[0x08:], atime)
	le.PutUint32(rec[0x0C:], ctime)
	le.PutUint32(rec[0x10:], mtime)
	le.PutUint32(rec[0x84:], ctimeExtra)
	le.PutUint32(rec[0x88:], mtimeExtra)
	le.PutUint32(rec[0x8C:], atimeExtra)
	le.PutUint32(rec[0x90:], mtime) // creation time
	le.PutUint32(rec[0x94:], mtimeExtra)

	links := p.nlink
	if n.kind == kindDir {
		links += 2
		if links > maxLinks {
			links = 1 // more than ext4 can count: "unknown", see dir_nlink
		}
	} else if links > maxLinks {
		return nil, fmt.Errorf("ext4: %s: %d hard links exceed the limit of %d", p.path, links, maxLinks)
	}
	le.PutUint16(rec[0x1A:], uint16(links))

	blocks := p.runLen + uint64(len(p.treeBlocks))
	if p.xattrInBlock {
		blocks++
	}
	sectors := blocks * (bs / 512)
	le.PutUint32(rec[0x1C:], uint32(sectors))
	le.PutUint16(rec[0x74:], uint16(sectors>>32))

	le.PutUint16(rec[0x80:], extraIsize)

	if err := e.fillBlockField(n, rec); err != nil {
		return nil, err
	}

	switch {
	case p.xattrInBlock:
		blk := encodeXattrBlock(n.xattrs, e.bs, p.xattrBlock, e.seed)
		e.writeSparse(e.blockOffset(p.xattrBlock), blk)
		le.PutUint32(rec[0x68:], uint32(p.xattrBlock))
		le.PutUint16(rec[0x76:], uint16(p.xattrBlock>>32))
	case len(n.xattrs) > 0:
		copy(rec[ibodyOffset:], encodeInodeXattrs(n.xattrs))
	}

	sum := crc32cLE32(e.seed, p.ino)
	sum = crc32cLE32(sum, 0) // generation
	sum = crc32c(sum, rec)
	le.PutUint16(rec[0x7C:], uint16(sum))
	le.PutUint16(rec[0x82:], uint16(sum>>16))
	return rec, e.err
}

// fillBlockField sets i_block and the flags, and writes the data blocks of
// directories and symlinks and the extent tree blocks.
func (e *emitter) fillBlockField(n *node, rec []byte) error {
	p := &n.planned
	iblock := rec[0x28 : 0x28+60]
	flags := uint32(0)

	switch n.kind {
	case kindSymlink:
		if p.runLen == 0 {
			copy(iblock, n.target)
		} else {
			data := make([]byte, e.bs)
			copy(data, n.target)
			e.writeSparse(e.blockOffset(p.runStart), data)
		}
	case kindChar, kindBlock:
		// Small device numbers use the old encoding in the first word,
		// larger ones the new encoding in the second.
		if n.major < 256 && n.minor < 256 {
			le.PutUint32(iblock[0:], n.major<<8|n.minor)
		} else {
			le.PutUint32(iblock[4:], n.minor&0xff|n.major<<8|(n.minor&^0xff)<<12)
		}
	case kindDir:
		e.writeSparse(e.blockOffset(p.runStart), e.dirData(n))
	}

	if p.runLen > 0 {
		flags |= flagExtents
		exts := runExtents(p.runStart, p.runLen)
		e.extentRoot(n, exts, iblock)
	} else if n.kind == kindReg {
		flags |= flagExtents
		e.extentRoot(n, nil, iblock)
	}
	le.PutUint32(rec[0x20:], flags)
	return e.err
}

// dirData encodes the blocks of a directory.
func (e *emitter) dirData(n *node) []byte {
	p := &n.planned
	capacity := e.bs - 12
	out := make([]byte, int(p.runLen)*e.bs)

	for bi, ents := range p.dirBlocks {
		blk := out[bi*e.bs : (bi+1)*e.bs]
		pos := 0
		put := func(ino uint32, name string, ftype uint8, recLen int) {
			le.PutUint32(blk[pos:], ino)
			le.PutUint16(blk[pos+4:], uint16(recLen))
			blk[pos+6] = byte(len(name))
			blk[pos+7] = ftype
			copy(blk[pos+8:], name)
		}
		if bi == 0 {
			put(p.ino, ".", 2, 12)
			rl := 12
			if len(ents) == 0 {
				rl = capacity - 12
			}
			pos = 12
			put(p.parent.planned.ino, "..", 2, rl)
			pos = 24
		}
		for i, ent := range ents {
			rl := recLen(len(ent.name))
			if i == len(ents)-1 {
				rl = capacity - pos
			}
			put(ent.ino, ent.name, ent.ftype, rl)
			pos += recLen(len(ent.name))
		}

		// The checksum lives in a fake entry at the end of the block.
		le.PutUint16(blk[capacity+4:], 12)
		blk[capacity+7] = 0xDE
		sum := crc32cLE32(e.seed, p.ino)
		sum = crc32cLE32(sum, 0)
		le.PutUint32(blk[capacity+8:], crc32c(sum, blk[:capacity]))
	}
	return out
}

type extent struct {
	logical uint32
	length  uint16
	phys    uint64
}

// runExtents describes a contiguous run of blocks as the fewest extents.
func runExtents(start, length uint64) []extent {
	exts := make([]extent, 0, ceilDiv(length, maxExtentLen))
	for done := uint64(0); done < length; done += maxExtentLen {
		n := min(length-done, maxExtentLen)
		exts = append(exts, extent{logical: uint32(done), length: uint16(n), phys: start + done})
	}
	return exts
}

// extentRoot writes the extent tree of n: the root goes into the 60 bytes
// of i_block, any further levels into the tree blocks of the node.
func (e *emitter) extentRoot(n *node, exts []extent, iblock []byte) {
	capacity := (e.bs - 12) / 12
	p := &n.planned

	type entry struct {
		logical uint32
		length  uint16
		phys    uint64
	}
	level := make([]entry, len(exts))
	for i, x := range exts {
		level[i] = entry{x.logical, x.length, x.phys}
	}

	encode := func(dst []byte, x entry, depth int) {
		le.PutUint32(dst[0:], x.logical)
		if depth == 0 {
			le.PutUint16(dst[4:], x.length)
			le.PutUint16(dst[6:], uint16(x.phys>>32))
			le.PutUint32(dst[8:], uint32(x.phys))
			return
		}
		le.PutUint32(dst[4:], uint32(x.phys))
		le.PutUint16(dst[8:], uint16(x.phys>>32))
	}
	header := func(dst []byte, entries, max, depth int) {
		le.PutUint16(dst[0:], 0xF30A)
		le.PutUint16(dst[2:], uint16(entries))
		le.PutUint16(dst[4:], uint16(max))
		le.PutUint16(dst[6:], uint16(depth))
	}

	depth := 0
	next := 0
	for len(level) > 4 {
		var up []entry
		for i := 0; i < len(level); i += capacity {
			chunk := level[i:min(i+capacity, len(level))]
			blk := p.treeBlocks[next]
			next++

			data := make([]byte, e.bs)
			header(data, len(chunk), capacity, depth)
			for j, x := range chunk {
				encode(data[12+12*j:], x, depth)
			}
			tail := 12 + 12*capacity
			sum := crc32cLE32(e.seed, p.ino)
			sum = crc32cLE32(sum, 0)
			le.PutUint32(data[tail:], crc32c(sum, data[:tail]))
			e.writeSparse(e.blockOffset(blk), data)

			up = append(up, entry{logical: chunk[0].logical, phys: blk})
		}
		level = up
		depth++
	}
	header(iblock, len(level), 4, depth)
	for j, x := range level {
		encode(iblock[12+12*j:], x, depth)
	}
}

// bitmap returns a bitmap block with the first used bits set, and all bits
// from valid on, which do not describe anything.
func (e *emitter) bitmap(used, valid uint64) []byte {
	bm := make([]byte, e.bs)
	setRange := func(from, to uint64) {
		for i := from; i < to; {
			if i%8 == 0 && to-i >= 8 {
				bm[i/8] = 0xFF
				i += 8
				continue
			}
			bm[i/8] |= 1 << (i % 8)
			i++
		}
	}
	setRange(0, used)
	setRange(valid, uint64(e.bs)*8)
	return bm
}

func (e *emitter) groupFlags(g int) uint16 {
	flags := uint16(bgInodeZeroed)
	if e.l.groupUsedInodes(g) == 0 {
		flags |= bgInodeUninit
	}
	// The last group always has a bitmap: its padding cannot be derived.
	if e.l.groupUsedBlocks(g) == 0 && g != e.l.groups-1 {
		flags |= bgBlockUninit
	}
	return flags
}

func (e *emitter) blockBitmapFor(g int) []byte {
	l := e.l
	return e.bitmap(l.groupUsedBlocks(g), l.groupBlocks(g))
}

func (e *emitter) inodeBitmapFor(g int) []byte {
	return e.bitmap(uint64(e.l.groupUsedInodes(g)), uint64(e.l.ipg))
}

func (e *emitter) writeBitmaps() error {
	l := e.l
	for g := 0; g < l.groups; g++ {
		flags := e.groupFlags(g)
		if flags&bgBlockUninit == 0 {
			e.write(e.blockOffset(l.blockBitmapStart+uint64(g)), e.blockBitmapFor(g))
		}
		if flags&bgInodeUninit == 0 {
			e.write(e.blockOffset(l.inodeBitmapStart+uint64(g)), e.inodeBitmapFor(g))
		}
	}
	return e.err
}

func (e *emitter) writeDescriptors() {
	l := e.l
	gdt := make([]byte, l.gdtBlocks*l.bs)
	for g := 0; g < l.groups; g++ {
		d := gdt[g*descSize : (g+1)*descSize]
		flags := e.groupFlags(g)
		usedInodes := l.groupUsedInodes(g)

		le.PutUint32(d[0x00:], uint32(l.blockBitmapStart)+uint32(g))
		le.PutUint32(d[0x04:], uint32(l.inodeBitmapStart)+uint32(g))
		le.PutUint32(d[0x08:], uint32(l.inodeTableStart+uint64(g)*l.itb))
		le.PutUint16(d[0x0C:], uint16(l.groupBlocks(g)-l.groupUsedBlocks(g)))
		le.PutUint16(d[0x0E:], uint16(l.ipg-usedInodes))
		le.PutUint16(d[0x10:], uint16(e.groupDirs(g)))
		le.PutUint16(d[0x12:], flags)
		if flags&bgBlockUninit == 0 {
			le.PutUint16(d[0x18:], uint16(crc32c(e.seed, e.blockBitmapFor(g)[:l.bpg/8])))
		}
		if flags&bgInodeUninit == 0 {
			le.PutUint16(d[0x1A:], uint16(crc32c(e.seed, e.inodeBitmapFor(g)[:l.ipg/8])))
		}
		le.PutUint16(d[0x1C:], uint16(l.ipg-usedInodes))

		sum := crc32cLE32(e.seed, uint32(g))
		sum = crc32c(sum, d) // the checksum field is still zero
		le.PutUint16(d[0x1E:], uint16(sum))
	}
	e.writeSparse(e.blockOffset(l.gdtStart), gdt)
}

// groupDirs counts the directories among the inodes of group g.
func (e *emitter) groupDirs(g int) int {
	l := e.l
	count := 0
	for i := 0; i < int(l.groupUsedInodes(g)); i++ {
		if n := l.byIno[g*int(l.ipg)+i+1]; n != nil && n.kind == kindDir {
			count++
		}
	}
	return count
}

func (e *emitter) writeSuperblock() {
	l := e.l
	sb := make([]byte, 1024)
	mkfsTime := uint32(max(e.b.opts.Time.Unix(), 0))

	le.PutUint32(sb[0x00:], l.ipg*uint32(l.groups))
	le.PutUint32(sb[0x04:], uint32(l.totalBlock))
	le.PutUint32(sb[0x0C:], uint32(l.totalBlock-l.usedBlocks))
	le.PutUint32(sb[0x10:], l.ipg*uint32(l.groups)-l.usedInodes)
	le.PutUint32(sb[0x14:], uint32(l.fdb))
	logBlock := uint32(0)
	for 1024<<logBlock < e.bs {
		logBlock++
	}
	le.PutUint32(sb[0x18:], logBlock)
	le.PutUint32(sb[0x1C:], logBlock) // cluster size, without bigalloc
	le.PutUint32(sb[0x20:], uint32(l.bpg))
	le.PutUint32(sb[0x24:], uint32(l.bpg))
	le.PutUint32(sb[0x28:], l.ipg)
	le.PutUint32(sb[0x30:], mkfsTime) // last write
	le.PutUint16(sb[0x36:], 0xFFFF)   // no mount count limit
	le.PutUint16(sb[0x38:], 0xEF53)
	le.PutUint16(sb[0x3A:], 1) // clean
	le.PutUint16(sb[0x3C:], 1) // continue on errors
	le.PutUint32(sb[0x40:], mkfsTime)
	le.PutUint32(sb[0x4C:], 1) // dynamic revision
	le.PutUint32(sb[0x54:], firstIno)
	le.PutUint16(sb[0x58:], inodeSize)
	le.PutUint32(sb[0x5C:], featCompatExtAttr|featCompatSparseSuper)
	le.PutUint32(sb[0x60:], featIncompatFiletype|featIncompatExtents|featIncompatFlexBG)
	le.PutUint32(sb[0x64:], featROSparseSuper|featROLargeFile|featROHugeFile|featRODirNlink|featROExtraIsize|featROMetadataCsum)
	copy(sb[0x68:], l.uuid[:])
	copy(sb[0x78:], e.b.opts.Label)
	copy(sb[0xEC:], l.seed[:])
	sb[0xFC] = 1                  // half MD4 directory hash
	le.PutUint32(sb[0x100:], 0xC) // user xattrs and posix ACLs
	le.PutUint32(sb[0x108:], mkfsTime)
	le.PutUint16(sb[0x15C:], extraIsize)
	le.PutUint16(sb[0x15E:], extraIsize)
	le.PutUint32(sb[0x160:], 0x2) // unsigned directory hash
	sb[0x174] = l.flexLog
	sb[0x175] = 1 // crc32c
	// sparse_super2 without backups: both backup groups stay 0.
	le.PutUint32(sb[0x3FC:], crc32c(0xFFFFFFFF, sb[:0x3FC]))
	e.write(1024, sb)
}

// finish makes sure the target has the size of the filesystem.
func (e *emitter) finish() (*Info, error) {
	l := e.l
	size := int64(l.totalBlock) * int64(e.bs)
	if t, ok := e.w.(truncater); ok {
		if err := t.Truncate(size); err != nil {
			return nil, fmt.Errorf("ext4: sizing the image: %w", err)
		}
	} else if e.end < size {
		e.write(size-1, []byte{0})
	}
	if e.err != nil {
		return nil, e.err
	}

	info := &Info{
		Size:       size,
		BlockSize:  e.bs,
		Blocks:     l.totalBlock,
		UsedBlocks: l.usedBlocks,
		Inodes:     l.ipg * uint32(l.groups),
		UsedInodes: l.usedInodes,
		Groups:     l.groups,
		UUID:       l.uuid,
	}
	for _, n := range l.nodes {
		if n.kind == kindReg && n.size > 0 {
			info.Files = append(info.Files, FileLayout{
				Path:   n.planned.path,
				Offset: e.blockOffset(n.planned.runStart),
				Size:   n.size,
				Digest: n.planned.digest,
			})
		}
	}
	return info, nil
}
