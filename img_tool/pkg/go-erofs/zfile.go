package erofs

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs/internal/disk"
)

// CompressionAlgorithm selects how a regular file's data is compressed.
type CompressionAlgorithm uint8

const (
	// CompressionNone stores file data verbatim.
	CompressionNone CompressionAlgorithm = iota
	// CompressionLZ4 compresses file data with LZ4 into one-block physical
	// clusters, described by full (non-compacted) logical-cluster indexes.
	CompressionLZ4
	// CompressionZstd compresses file data with Zstandard the same way. The
	// image needs a kernel with Zstandard support in EROFS (Linux 6.10).
	CompressionZstd
	// CompressionDeflate compresses file data with DEFLATE the same way. The
	// image needs a kernel with DEFLATE support in EROFS (Linux 6.6).
	CompressionDeflate
)

// algorithmType is the value of the algorithm field of the compressed-inode map
// header (Z_EROFS_COMPRESSION_*), and the bit number in the superblock's bitmap
// of available algorithms.
func (a CompressionAlgorithm) algorithmType() uint8 {
	switch a {
	case CompressionZstd:
		return 3
	case CompressionDeflate:
		return 2
	}
	return 0
}

func (a CompressionAlgorithm) String() string {
	switch a {
	case CompressionNone:
		return "none"
	case CompressionLZ4:
		return "lz4"
	case CompressionZstd:
		return "zstd"
	case CompressionDeflate:
		return "deflate"
	default:
		return fmt.Sprintf("CompressionAlgorithm(%d)", uint8(a))
	}
}

// Logical cluster types of the compressed-index format (z_erofs_lcluster_index).
const (
	zTypePlain   = 0
	zTypeHead1   = 1
	zTypeNonHead = 2
)

const (
	// zMaxExtent bounds how much input one physical cluster may cover when the
	// clusters are a single block. Larger clusters allow proportionally more, see
	// maxExtent.
	zMaxExtent = 128 << 10
	// zMaxClusterBlocks bounds the size of a physical cluster in blocks. The
	// block count of a cluster is stored in 11 bits of the index.
	zMaxClusterBlocks = 1<<11 - 1
	// zMaxClusterBytes is the largest physical cluster EROFS allows.
	zMaxClusterBytes = 1 << 20
	// zCBlkCnt marks the delta[0] field of the first NONHEAD lcluster of an
	// extent as the block count of its physical cluster.
	zCBlkCnt = 1 << 11
	// zAdviseBigPcluster1 marks inodes whose HEAD1 clusters may span blocks.
	zAdviseBigPcluster1 = 0x2
	// zMaxDelta is the largest NONHEAD distance a lcluster index stores; the
	// next bit up is the CBLKCNT flag.
	zMaxDelta = 1<<11 - 1
	// zMapHeaderSize is the compressed-inode map header plus the reserved
	// bytes that precede the first full index.
	zMapHeaderSize = 16
	zIndexSize     = 8
)

// maxExtent is how much input one physical cluster of clusterBlocks blocks may
// cover.
func maxExtent(clusterBlocks, blockSize int) int {
	return min(max(zMaxExtent, 16*clusterBlocks*blockSize), zMaxClusterBytes)
}

// zIndex is one z_erofs_lcluster_index. block is relative to the file's first
// physical block until the file's position in the image is known.
type zIndex struct {
	advise     uint16
	clusterofs uint16
	// u is the block number of a HEAD or PLAIN lcluster, or delta[0] | delta[1]<<16
	// for a NONHEAD one.
	u uint32
	// end marks the PLAIN lcluster that records where a file's last extent
	// stops; it refers to no physical cluster, so u is not a block number.
	end bool
}

// EncodedFile describes a regular file that was encoded into physical clusters
// by [EncodeFile].
type EncodedFile struct {
	// Blocks is the number of physical blocks the encoded data occupies.
	Blocks int
	// AllRaw reports that no cluster is compressed: the encoded data is the
	// file itself followed by zero padding up to the block boundary.
	AllRaw bool

	idx        []zIndex
	clusterofs int // in-lcluster offset where the next extent starts
	blockSize  int
	alg        CompressionAlgorithm
	big        bool // clusters may span several blocks
}

// extentEncoder compresses a prefix of src into at most budget bytes.
type extentEncoder interface {
	encode(src []byte, budget int, final bool) (block []byte, n int)
}

type encodeScratch struct {
	lz4     lz4Encoder
	zstd    *zstdEncoder
	deflate *deflateEncoder
	// zstdLog is the window log zstd was created with.
	zstdLog int
	buf     []byte
	blk     []byte
}

var scratchPool = sync.Pool{New: func() any {
	return &encodeScratch{}
}}

// EncodeFile compresses size bytes from r into physical clusters of one block
// each and writes them to w, block by block, padded to the block size. The
// result describes the file's logical-cluster index.
//
// The output is a pure function of the input, block size and algorithm. It does
// not depend on where the data ends up in an image, so it can be reproduced from
// the file alone.
func EncodeFile(w io.Writer, r io.Reader, size int64, blockSize int, alg CompressionAlgorithm) (*EncodedFile, error) {
	return EncodeFileClusters(w, r, size, blockSize, 1, alg)
}

// EncodeFileClusters is [EncodeFile] with physical clusters of up to
// clusterBlocks blocks. A cluster takes as many blocks as its compressed data
// needs, so more input fits behind one decompression, which compresses better.
// Files with clusters of more than one block need the superblock to say so (see
// [WithClusterBlocks]).
func EncodeFileClusters(w io.Writer, r io.Reader, size int64, blockSize, clusterBlocks int, alg CompressionAlgorithm) (*EncodedFile, error) {
	if clusterBlocks < 1 || clusterBlocks > zMaxClusterBlocks || clusterBlocks*blockSize > zMaxClusterBytes {
		return nil, fmt.Errorf("erofs: invalid cluster size of %d blocks of %d bytes", clusterBlocks, blockSize)
	}
	if alg != CompressionLZ4 && alg != CompressionZstd && alg != CompressionDeflate {
		return nil, fmt.Errorf("erofs: cannot encode with %v", alg)
	}
	if blockSize <= 0 || blockSize&(blockSize-1) != 0 {
		return nil, fmt.Errorf("erofs: invalid block size %d", blockSize)
	}
	z := &EncodedFile{AllRaw: true, blockSize: blockSize, alg: alg, big: clusterBlocks > 1}
	extentMax := maxExtent(clusterBlocks, blockSize)
	budget := clusterBlocks * blockSize
	scratch := scratchPool.Get().(*encodeScratch)
	defer scratchPool.Put(scratch)
	var enc extentEncoder = &scratch.lz4
	if alg == CompressionDeflate {
		if scratch.deflate == nil {
			var err error
			if scratch.deflate, err = newDeflateEncoder(); err != nil {
				return nil, err
			}
		}
		enc = scratch.deflate
	}
	if alg == CompressionZstd {
		if scratch.zstd == nil || scratch.zstdLog != zstdWindowLog(extentMax) {
			var err error
			scratch.zstdLog = zstdWindowLog(extentMax)
			if scratch.zstd, err = newZstdEncoder(scratch.zstdLog); err != nil {
				return nil, err
			}
		}
		enc = scratch.zstd
	}
	if len(scratch.buf) < 2*extentMax {
		scratch.buf = make([]byte, 2*extentMax)
	}
	buf := scratch.buf
	off, end := 0, 0 // buf[off:end] is the window starting at the current position
	eof := false
	if cap(scratch.blk) < budget {
		scratch.blk = make([]byte, budget)
	}
	blk := scratch.blk[:budget]

	for pos := int64(0); pos < size; {
		// Keep at least extentMax bytes of lookahead unless the data ends first.
		if !eof && end-off < extentMax {
			copy(buf, buf[off:end])
			end -= off
			off = 0
			want := min(int64(len(buf)-end), size-pos-int64(end))
			if want > 0 {
				n, err := io.ReadFull(r, buf[end:end+int(want)])
				end += n
				if err != nil {
					return nil, fmt.Errorf("erofs: reading file data at offset %d: %w", pos+int64(end), err)
				}
			}
			eof = pos+int64(end) >= size
		}
		avail := int64(end - off)
		if avail == 0 {
			return nil, fmt.Errorf("erofs: file data ended at offset %d, expected %d bytes", pos, size)
		}
		src := buf[off:end]
		if len(src) > extentMax {
			src = src[:extentMax]
		}
		final := eof && int64(len(src)) == avail
		c := int(pos % int64(blockSize))
		rest := size - pos
		length, plen := 0, 1
		var block []byte
		// Compress only when it can cover more than a raw block would: an
		// uncompressed extent takes the rest of its lcluster. A cluster of
		// several blocks must beat one raw block per block.
		if rest > int64(blockSize-c) {
			if out, n := enc.encode(src, budget, final); n > 0 {
				p := (len(out) + blockSize - 1) / blockSize
				// The block count of a cluster of several blocks is recorded in the
				// extent's first NONHEAD lcluster, so such an extent has to reach
				// the lcluster after the next one.
				if int64(n) > int64(blockSize-c)*int64(p) && (p == 1 || c+n >= 2*blockSize) {
					length, plen = n, p
					// Compressed data is right-aligned behind zero bytes, which
					// the reader skips; the superblock says so (LZ4_0PADDING).
					pad := plen*blockSize - len(out)
					clear(blk[:pad])
					copy(blk[pad:], out)
					block = blk[:plen*blockSize]
					z.AllRaw = false
				}
			}
		}
		raw := block == nil
		if raw {
			length = int(min(rest, int64(blockSize-c)))
			copy(blk, src[:length])
			clear(blk[length:blockSize])
			block = blk[:blockSize]
		}
		if _, err := w.Write(block); err != nil {
			return nil, err
		}
		z.addExtent(length, raw, uint32(z.Blocks), plen)
		z.Blocks += plen
		off += length
		pos += int64(length)
	}
	z.finish()
	return z, nil
}

// addExtent appends the lcluster indexes for an extent of length bytes that
// starts at the current in-lcluster offset and whose physical cluster is block.
// It follows the layout mkfs.erofs writes, which is what the kernel and
// fsck.erofs expect.
func (z *EncodedFile) addExtent(length int, raw bool, block uint32, plen int) {
	bs := z.blockSize
	c := z.clusterofs
	head := uint16(zTypeHead1)
	if raw {
		head = zTypePlain
	}
	d0, d1 := 0, (c+length)/bs
	if d1 == 0 {
		// The extent starts and ends inside one lcluster. Only the last extent
		// of a file can do that, and then only as a raw one.
		z.idx = append(z.idx, zIndex{advise: zTypePlain, clusterofs: uint16(c), u: block})
		z.clusterofs = 0
		return
	}
	start := c
	count := length
	for {
		var ix zIndex
		ix.clusterofs = uint16(start)
		if d0 == 1 && z.big {
			// The first NONHEAD lcluster of an extent holds the size of its
			// physical cluster, which is one block if there is no such lcluster.
			ix.advise = zTypeNonHead
			ix.u = uint32(plen) | zCBlkCnt | uint32(d1)<<16
		} else if d0 > 0 {
			ix.advise = zTypeNonHead
			ix.u = uint32(min(d0, zMaxDelta)) | uint32(d1)<<16
		} else {
			ix.advise = head
			ix.u = block
		}
		z.idx = append(z.idx, ix)
		count -= bs - c
		c = 0
		d0++
		d1--
		if c+count < bs {
			break
		}
	}
	z.clusterofs = c + count
}

// finish ends the last extent when the file does not end on an lcluster boundary:
// a PLAIN lcluster with no data of its own records where the extent stops.
func (z *EncodedFile) finish() {
	if z.clusterofs != 0 {
		z.idx = append(z.idx, zIndex{advise: zTypePlain, clusterofs: uint16(z.clusterofs), end: true})
		z.clusterofs = 0
	}
}

// metaSize returns the number of metadata bytes the file's compressed-index map
// takes after its inode core and xattr area, which is coreSize bytes long.
func (z *EncodedFile) metaSize(coreSize int) int {
	return zAlignPad(coreSize) + zMapHeaderSize + len(z.idx)*zIndexSize
}

// zAlignPad is the padding that aligns the map header to 8 bytes.
func zAlignPad(coreSize int) int { return (8 - coreSize%8) % 8 }

// appendMeta appends the map header and the indexes to dst, resolving block
// numbers against base, the image block of the file's first physical cluster.
func (z *EncodedFile) appendMeta(dst []byte, coreSize int, base uint32) []byte {
	dst = append(dst, make([]byte, zAlignPad(coreSize))...)
	var hdr [zMapHeaderSize]byte
	// h_reserved1/h_idata_size = 0, h_advise = 0, h_clusterbits = 0 (lclusters are
	// one block long); h_algorithmtype names the algorithm of the HEAD1 clusters.
	hdr[6] = z.alg.algorithmType()
	if z.big {
		binary.LittleEndian.PutUint16(hdr[4:], zAdviseBigPcluster1)
	}
	dst = append(dst, hdr[:]...)
	for _, ix := range z.idx {
		u := ix.u
		if ix.advise != zTypeNonHead && !ix.end {
			u += base
		}
		var b [zIndexSize]byte
		binary.LittleEndian.PutUint16(b[0:], ix.advise)
		binary.LittleEndian.PutUint16(b[2:], ix.clusterofs)
		binary.LittleEndian.PutUint32(b[4:], u)
		dst = append(dst, b[:]...)
	}
	return dst
}

// compressTree encodes the data of every eligible regular file in the tree.
func (fsys *Writer) compressTree(root *erofsEntry) error {
	if fsys.compression == CompressionNone {
		return nil
	}
	var walk func(e *erofsEntry) error
	walk = func(e *erofsEntry) error {
		switch e.mode & disk.StatTypeMask {
		case disk.StatTypeDir:
			for _, c := range e.children {
				if err := walk(c); err != nil {
					return err
				}
			}
		case disk.StatTypeReg:
			if fsys.mayCompress(e) {
				return fsys.compressEntry(e)
			}
		}
		return nil
	}
	return walk(root)
}

// mayCompress reports whether a regular file's data can be stored compressed.
func (fsys *Writer) mayCompress(e *erofsEntry) bool {
	return e.data != nil && e.size > uint64(fsys.blockSize) &&
		e.hardlinkTo == nil && e.shareSrc == nil && !e.isShareSrc &&
		len(e.chunks) == 0 && !e.metadataOnly
}

func (fsys *Writer) compressEntry(e *erofsEntry) error {
	if fsys.zspool == nil {
		tmp, err := os.CreateTemp(fsys.tempDir, "erofs-mkfs-z-*")
		if err != nil {
			return fmt.Errorf("mkfs: create compression spool: %w", err)
		}
		_ = os.Remove(tmp.Name()) // unlink immediately; fd keeps data accessible
		fsys.zspool = tmp
	}
	start := fsys.zoff
	bw := bufio.NewWriterSize(io.NewOffsetWriter(fsys.zspool, start), 256<<10)
	z, err := EncodeFileClusters(bw, e.data, int64(e.size), fsys.blockSize, fsys.clusterBlocks, fsys.compression)
	if c, ok := e.data.(io.Closer); ok {
		_ = c.Close()
	}
	if err != nil {
		return fmt.Errorf("mkfs: compress %s: %w", e.path, err)
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("mkfs: compress %s: %w", e.path, err)
	}
	if z.AllRaw {
		// Nothing compressed: the encoded blocks are the file, zero-padded, so
		// it is stored as an ordinary uncompressed file.
		e.data = io.NewSectionReader(fsys.zspool, start, int64(e.size))
	} else {
		e.z = z
		fsys.compressed = true
		e.data = io.NewSectionReader(fsys.zspool, start, int64(z.Blocks)*int64(fsys.blockSize))
	}
	fsys.zoff = start + int64(z.Blocks)*int64(fsys.blockSize)
	return nil
}

func (fsys *Writer) closeZSpool() {
	if fsys.zspool != nil {
		_ = fsys.zspool.Close()
	}
}

// compressionUsed is the algorithm the image's compressed files use, or
// CompressionNone when it has none.
func (fsys *Writer) compressionUsed() CompressionAlgorithm {
	if fsys.compressed {
		return fsys.compression
	}
	return CompressionNone
}
