package ztoc

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/opencontainers/go-digest"
)

// This file builds the "zinfo" (compression index) portion of a ztoc for
// gzip-compressed layers. The algorithm mirrors soci-snapshotter's C port of
// zlib's zran.c (ztoc/compression/gzip_zinfo.c): while decompressing, it records
// a checkpoint at selected deflate block boundaries so that decompression can
// later be resumed from any checkpoint. The serialized layout is the soci
// "version 2" zinfo blob, byte-compatible with what soci itself produces.

// checkpoint captures the decompressor state at one deflate block boundary. It
// corresponds to soci's C struct gzip_checkpoint.
type checkpoint struct {
	in     int64         // compressed offset of the first full byte of this span
	out    int64         // uncompressed offset at this boundary
	bits   uint8         // unused bits (0..7) in the byte at in-1, or 0
	window [winSize]byte // 32 KiB of uncompressed data preceding out
}

const (
	// blobHeaderSize is the size of the zinfo blob header: 4-byte checkpoint
	// count + 8-byte span size.
	blobHeaderSize = 4 + 8
	// packedCheckpointSize is the serialized size of one checkpoint: 8-byte in
	// + 8-byte out + 1-byte bits + 32 KiB window.
	packedCheckpointSize = 8 + 8 + 1 + winSize
)

// zinfo is the result of the zran pass over a gzip stream.
type zinfo struct {
	checkpoints []checkpoint
	spanSize    int64
}

// buildGzipZinfo decompresses data, recording a checkpoint at each deflate block
// boundary whose uncompressed distance from the previous checkpoint exceeds
// spanSize (plus a mandatory checkpoint at the very first boundary, which serves
// as the post-header entry point).
func buildGzipZinfo(data []byte, spanSize int64) (*zinfo, error) {
	z := &zinfo{spanSize: spanSize}
	inf := &inflater{}
	inf.br.in = data

	last := int64(0)
	inf.onBoundary = func() {
		out := inf.total
		if out != 0 && out-last <= spanSize {
			return
		}
		in, bits := inf.br.offset()
		cp := checkpoint{in: in, out: out, bits: bits}
		inf.win.snapshot(&cp.window, out)
		z.checkpoints = append(z.checkpoints, cp)
		last = out
	}

	if err := inf.run(); err != nil {
		return nil, err
	}
	if len(z.checkpoints) == 0 {
		return nil, fmt.Errorf("ztoc: no checkpoints produced (empty or invalid gzip stream)")
	}
	return z, nil
}

// maxSpanID returns the ID of the last span.
func (z *zinfo) maxSpanID() SpanID {
	return SpanID(len(z.checkpoints) - 1)
}

// marshalCheckpoints serializes the checkpoints into the soci v2 zinfo blob.
// All integers are little-endian, matching the C implementation regardless of
// host byte order.
func (z *zinfo) marshalCheckpoints() []byte {
	buf := make([]byte, blobHeaderSize+len(z.checkpoints)*packedCheckpointSize)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(len(z.checkpoints)))
	binary.LittleEndian.PutUint64(buf[4:12], uint64(z.spanSize))
	off := blobHeaderSize
	for i := range z.checkpoints {
		cp := &z.checkpoints[i]
		binary.LittleEndian.PutUint64(buf[off:off+8], uint64(cp.in))
		binary.LittleEndian.PutUint64(buf[off+8:off+16], uint64(cp.out))
		buf[off+16] = cp.bits
		copy(buf[off+17:off+17+winSize], cp.window[:])
		off += packedCheckpointSize
	}
	return buf
}

// spanDigests computes the sha256 digest of each span's compressed bytes.
//
// Span i covers the compressed bytes [start_i, end_i) where:
//
//	start_i = checkpoints[i].in - (checkpoints[i].bits != 0 ? 1 : 0)
//	end_i   = (i == last) ? compressedSize : checkpoints[i+1].in
//
// This matches soci's getPerSpanDigests: the shared boundary byte is included
// in both adjacent spans when bits != 0, and the final span extends to the end
// of the compressed blob (covering the gzip trailer).
func (z *zinfo) spanDigests(data []byte, compressedSize int64) ([]digest.Digest, error) {
	digests := make([]digest.Digest, 0, len(z.checkpoints))
	last := len(z.checkpoints) - 1
	for i := range z.checkpoints {
		start := z.checkpoints[i].in
		if z.checkpoints[i].bits != 0 {
			start--
		}
		var end int64
		if i == last {
			end = compressedSize
		} else {
			end = z.checkpoints[i+1].in
		}
		if start < 0 || end > int64(len(data)) || start > end {
			return nil, fmt.Errorf("ztoc: invalid span %d range [%d,%d) for data of size %d", i, start, end, len(data))
		}
		sum := sha256.Sum256(data[start:end])
		digests = append(digests, digest.NewDigestFromBytes(digest.SHA256, sum[:]))
	}
	return digests, nil
}
