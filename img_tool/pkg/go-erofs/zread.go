package erofs

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs/internal/disk"
)

// Reading of compressed files. Only what [WithCompression] writes is supported:
// the full (non-compacted) index, LZ4, and logical clusters one block long.

// zCacheSlots is how many decoded extents an image keeps. A sequential reader
// asks for an extent once per block of it; a few slots let several readers do so
// without throwing each other's extent away.
const zCacheSlots = 8

// zCache holds recently decoded extents.
type zCache struct {
	slots [zCacheSlots]zCacheEntry
	next  int
}

type zCacheEntry struct {
	nid   uint64
	start int64
	data  []byte
}

func (c *zCache) get(nid uint64, start int64) []byte {
	for i := range c.slots {
		if e := &c.slots[i]; e.data != nil && e.nid == nid && e.start == start {
			return e.data
		}
	}
	return nil
}

func (c *zCache) put(nid uint64, start int64, data []byte) {
	c.slots[c.next] = zCacheEntry{nid: nid, start: start, data: data}
	c.next = (c.next + 1) % zCacheSlots
}

// zIndexBase validates the compressed-inode map header and returns the offset of
// the first lcluster index.
func (img *image) zIndexBase(fi *inode) (int64, CompressionAlgorithm, error) {
	if fi.inodeLayout != disk.LayoutCompressedFull {
		return 0, 0, fmt.Errorf("compressed inode layout (%d) for %d: %w", fi.inodeLayout, fi.nid, ErrNotImplemented)
	}
	inodeAddr := img.metaStartPos() + int64(fi.nid)*disk.SizeInodeCompact
	hdrPos := (inodeAddr + fi.flatDataOffset() + 7) &^ 7
	var h [8]byte
	if _, err := img.meta.ReadAt(h[:], hdrPos); err != nil {
		return 0, 0, fmt.Errorf("reading compression map header of %d: %w", fi.nid, err)
	}
	advise := binary.LittleEndian.Uint16(h[4:6])
	var alg CompressionAlgorithm
	switch h[6] {
	case CompressionLZ4.algorithmType():
		alg = CompressionLZ4
	case CompressionZstd.algorithmType():
		alg = CompressionZstd
	case CompressionDeflate.algorithmType():
		alg = CompressionDeflate
	default:
		return 0, 0, fmt.Errorf("compression algorithms 0x%x of inode %d: %w", h[6], fi.nid, ErrNotImplemented)
	}
	if advise&^zAdviseBigPcluster1 != 0 || h[7] != 0 || binary.LittleEndian.Uint32(h[0:4]) != 0 {
		return 0, 0, fmt.Errorf("compression features of inode %d (advise 0x%x, cluster bits 0x%x): %w", fi.nid, advise, h[7], ErrNotImplemented)
	}
	return hdrPos + zMapHeaderSize, alg, nil
}

type zLcluster struct {
	typ        int
	clusterofs int
	u          uint32
}

// delta0 is the distance of a NONHEAD lcluster to the head of its extent. The
// first NONHEAD lcluster of an extent may hold the block count of the extent's
// cluster instead, and is then one lcluster from the head.
func (l zLcluster) delta0() int64 {
	if l.u&zCBlkCnt != 0 {
		return 1
	}
	return int64(l.u & 0xffff)
}

// clusterBlocks is the block count recorded in a first NONHEAD lcluster, or 0.
func (l zLcluster) clusterBlocks() int64 {
	if l.typ == zTypeNonHead && l.u&zCBlkCnt != 0 {
		return int64(l.u & 0xffff &^ zCBlkCnt)
	}
	return 0
}

func (img *image) zLoad(base, lcn int64) (zLcluster, error) {
	var b [zIndexSize]byte
	if _, err := img.meta.ReadAt(b[:], base+lcn*zIndexSize); err != nil {
		return zLcluster{}, fmt.Errorf("reading lcluster %d: %w", lcn, err)
	}
	l := zLcluster{
		typ:        int(binary.LittleEndian.Uint16(b[0:2]) & 3),
		clusterofs: int(binary.LittleEndian.Uint16(b[2:4])),
		u:          binary.LittleEndian.Uint32(b[4:8]),
	}
	if l.typ > zTypeNonHead {
		return zLcluster{}, fmt.Errorf("lcluster %d has type %d: %w", lcn, l.typ, ErrNotImplemented)
	}
	if l.typ != zTypeNonHead && l.clusterofs >= 1<<img.sb.BlkSizeBits {
		return zLcluster{}, fmt.Errorf("lcluster %d has offset %d: %w", lcn, l.clusterofs, ErrInvalid)
	}
	return l, nil
}

// zExtent decodes the extent containing file offset pos. It returns the extent's
// first file offset and its data.
func (img *image) zExtent(fi *inode, pos int64) (int64, []byte, error) {
	base, alg, err := img.zIndexBase(fi)
	if err != nil {
		return 0, nil, err
	}
	bits := img.sb.BlkSizeBits
	lastLcn := (fi.size - 1) >> bits
	lcn := pos >> bits
	endoff := int(pos & (1<<bits - 1))

	m, err := img.zLoad(base, lcn)
	if err != nil {
		return 0, nil, err
	}
	head, headLcn := m, lcn
	if m.typ == zTypeNonHead || endoff < m.clusterofs {
		dist := int64(1)
		if m.typ == zTypeNonHead {
			dist = m.delta0()
		}
		l := lcn
		for {
			if dist <= 0 || dist > l {
				return 0, nil, fmt.Errorf("bad lookback distance %d at lcluster %d of inode %d: %w", dist, l, fi.nid, ErrInvalid)
			}
			l -= dist
			head, err = img.zLoad(base, l)
			if err != nil {
				return 0, nil, err
			}
			if head.typ != zTypeNonHead {
				break
			}
			dist = head.delta0()
		}
		headLcn = l
	}
	start := headLcn<<bits | int64(head.clusterofs)

	// The extent ends where the next head starts.
	end := fi.size
	next := headLcn + 1
	plen := int64(1)
	if next <= lastLcn {
		n, err := img.zLoad(base, next)
		if err != nil {
			return 0, nil, err
		}
		if head.typ == zTypeHead1 {
			if cb := n.clusterBlocks(); cb > 0 {
				plen = cb
			}
		}
		if n.typ == zTypeNonHead {
			next += int64(n.u >> 16)
			if next <= lastLcn {
				if n, err = img.zLoad(base, next); err != nil {
					return 0, nil, err
				}
			}
		}
		if next <= lastLcn {
			if n.typ == zTypeNonHead {
				return 0, nil, fmt.Errorf("lcluster %d of inode %d is not an extent start: %w", next, fi.nid, ErrInvalid)
			}
			end = min(next<<bits|int64(n.clusterofs), fi.size)
		}
	}
	if end <= start || pos < start || pos >= end {
		return 0, nil, fmt.Errorf("extent [%d,%d) of inode %d does not contain offset %d: %w", start, end, fi.nid, pos, ErrInvalid)
	}

	img.zmu.Lock()
	if data := img.zcache.get(fi.nid, start); data != nil {
		img.zmu.Unlock()
		return start, data, nil
	}
	img.zmu.Unlock()

	blockSize := int64(1) << bits
	length := end - start
	cluster := make([]byte, plen*blockSize)
	if _, err := img.meta.ReadAt(cluster, int64(head.u)<<bits); err != nil {
		return 0, nil, fmt.Errorf("reading physical cluster %d of inode %d: %w", head.u, fi.nid, err)
	}
	data := make([]byte, length)
	switch head.typ {
	case zTypePlain:
		if length > int64(len(cluster)) {
			return 0, nil, fmt.Errorf("plain extent of %d bytes in a %d-byte cluster of inode %d: %w", length, len(cluster), fi.nid, ErrInvalid)
		}
		copy(data, cluster)
	default:
		skip := 0
		for skip < len(cluster) && cluster[skip] == 0 {
			skip++
		}
		var err error
		switch alg {
		case CompressionZstd:
			err = zstdDecode(data, cluster[skip:])
		case CompressionDeflate:
			err = deflateDecode(data, cluster[skip:])
		default:
			err = lz4Decode(data, cluster[skip:])
		}
		if err != nil {
			return 0, nil, fmt.Errorf("decompressing extent at %d of inode %d: %w", start, fi.nid, err)
		}
	}
	img.zmu.Lock()
	img.zcache.put(fi.nid, start, data)
	img.zmu.Unlock()
	return start, data, nil
}

// loadCompressedBlock returns the logical block of a compressed file that
// contains pos, decompressed.
func (img *image) loadCompressedBlock(fi *inode, pos int64) (*block, error) {
	blockSize := int64(1) << img.sb.BlkSizeBits
	blockStart := pos &^ (blockSize - 1)
	blockEnd := min(blockSize, fi.size-blockStart)
	b := img.getBlock()
	for p := blockStart; p < blockStart+blockEnd; {
		start, data, err := img.zExtent(fi, p)
		if err != nil {
			img.putBlock(b)
			return nil, err
		}
		p += int64(copy(b.buf[p-blockStart:blockEnd], data[p-start:]))
	}
	b.offset = int32(pos - blockStart)
	b.end = int32(blockEnd)
	return b, nil
}

// encodedRange locates the physical clusters of a compressed file. It reports
// false when they are not one contiguous run.
func (img *image) encodedRange(fi *inode) (DataRange, bool) {
	base, _, err := img.zIndexBase(fi)
	if err != nil {
		return DataRange{}, false
	}
	bits := img.sb.BlkSizeBits
	n := ((fi.size-1)>>bits + 1) * zIndexSize
	buf := make([]byte, n)
	if _, err := img.meta.ReadAt(buf, base); err != nil && err != io.EOF {
		return DataRange{}, false
	}
	var first, blocks int64
	entries := n / zIndexSize
	at := func(i int64) (typ int, u uint32) {
		off := i * zIndexSize
		return int(binary.LittleEndian.Uint16(buf[off:]) & 3), binary.LittleEndian.Uint32(buf[off+4:])
	}
	for i := int64(0); i < entries; i++ {
		typ, blk32 := at(i)
		blk := int64(blk32)
		if typ == zTypeNonHead || blk == 0 { // block 0 holds the superblock: a terminating marker
			continue
		}
		plen := int64(1)
		if typ == zTypeHead1 && i+1 < entries {
			nt, nu := at(i + 1)
			if nt == zTypeNonHead && nu&zCBlkCnt != 0 {
				plen = int64(nu & 0xffff &^ zCBlkCnt)
			}
		}
		if blocks == 0 {
			first = blk
		} else if blk != first+blocks {
			return DataRange{}, false
		}
		blocks += plen
	}
	if blocks == 0 || blocks != int64(fi.inodeData) {
		return DataRange{}, false
	}
	return DataRange{Device: 0, Offset: first << bits, Size: blocks << bits}, true
}
