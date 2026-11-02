package compactstream

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"

	erofs "github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs"
)

// Transform kinds.
const (
	// TransformErofsLZ4 is the encoding of a file as EROFS LZ4 physical
	// clusters (see erofs.EncodeFile): the bytes of the reference are the
	// compressed blocks of the file, each padded to a whole block.
	TransformErofsLZ4 uint8 = 1
	// TransformErofsZstd is the same with Zstandard-compressed clusters.
	TransformErofsZstd uint8 = 2
	// TransformErofsDeflate is the same with DEFLATE-compressed clusters.
	TransformErofsDeflate uint8 = 3
)

// Transform describes how the bytes of a reference are derived from the blob it
// names. A plain reference says "these bytes are the blob"; a transformed one
// says "these bytes are what the transform makes of the blob", so the stream can
// hold a deterministic function of a file, such as its compressed form inside a
// filesystem image, without anyone storing the result.
//
// Reconstruction runs the transform again, so it has to be reproducible:
// OutputDigest records the digest of the bytes the transform produced when the
// stream was written, and reconstruction fails if it does not produce the same
// bytes. That turns a compressor that changed between versions into an error
// instead of a corrupt layer.
type Transform struct {
	Kind uint8
	// BlockBits is log2 of the block size the encoding uses.
	BlockBits uint8
	// ClusterBlocks is the most blocks one compressed cluster of the encoding
	// takes (EncodeFileClusters), or 0 for the default of one.
	ClusterBlocks uint16
	// SourceSize is the length of the blob the transform reads.
	SourceSize uint64
	// OutputDigest is the digest of the transform's output, which is
	// [CASReference.Size] bytes long.
	OutputDigest []byte
}

// TransformKindFor returns the transform kind that encodes files with alg.
func TransformKindFor(alg erofs.CompressionAlgorithm) (uint8, bool) {
	switch alg {
	case erofs.CompressionLZ4:
		return TransformErofsLZ4, true
	case erofs.CompressionZstd:
		return TransformErofsZstd, true
	case erofs.CompressionDeflate:
		return TransformErofsDeflate, true
	}
	return 0, false
}

// algorithm returns the compression algorithm of a transform kind.
func (t Transform) algorithm() (erofs.CompressionAlgorithm, bool) {
	switch t.Kind {
	case TransformErofsLZ4:
		return erofs.CompressionLZ4, true
	case TransformErofsZstd:
		return erofs.CompressionZstd, true
	case TransformErofsDeflate:
		return erofs.CompressionDeflate, true
	}
	return erofs.CompressionNone, false
}

func (t Transform) validate() error {
	switch t.Kind {
	case TransformErofsLZ4, TransformErofsZstd, TransformErofsDeflate:
		if t.BlockBits < 9 || t.BlockBits > 16 {
			return fmt.Errorf("compact stream: transform block size 2^%d is out of range", t.BlockBits)
		}
	default:
		return fmt.Errorf("compact stream: unknown transform kind %d", t.Kind)
	}
	if t.ClusterBlocks > 2047 {
		return fmt.Errorf("compact stream: transform clusters of %d blocks are out of range", t.ClusterBlocks)
	}
	if t.SourceSize == 0 {
		return fmt.Errorf("compact stream: transform has an empty source")
	}
	return nil
}

// transformRecordSize is the size of the record that follows each reference
// table entry in a stream with transformed references: kind, block bits and six
// reserved bytes, the source size and the output digest.
func transformRecordSize(hashSize int) int { return 16 + hashSize }

// record serializes t, or the all-zero record of a plain reference when t is nil.
func (t *Transform) record(hashSize int) []byte {
	rec := make([]byte, transformRecordSize(hashSize))
	if t == nil {
		return rec
	}
	rec[0] = t.Kind
	rec[1] = t.BlockBits
	binary.BigEndian.PutUint16(rec[2:4], t.ClusterBlocks)
	binary.BigEndian.PutUint64(rec[8:16], t.SourceSize)
	copy(rec[16:], t.OutputDigest)
	return rec
}

func parseTransformRecord(rec []byte, hashSize int) (*Transform, error) {
	if rec[0] == 0 {
		for _, b := range rec {
			if b != 0 {
				return nil, fmt.Errorf("plain reference has a non-empty transform record")
			}
		}
		return nil, nil
	}
	for _, b := range rec[4:8] {
		if b != 0 {
			return nil, fmt.Errorf("reserved transform bytes are not zero")
		}
	}
	t := &Transform{
		Kind:          rec[0],
		BlockBits:     rec[1],
		ClusterBlocks: binary.BigEndian.Uint16(rec[2:4]),
		SourceSize:    binary.BigEndian.Uint64(rec[8:16]),
		OutputDigest:  append([]byte(nil), rec[16:16+hashSize]...),
	}
	if err := t.validate(); err != nil {
		return nil, err
	}
	if t.SourceSize > 1<<62 {
		return nil, fmt.Errorf("transform source size %d is too large", t.SourceSize)
	}
	return t, nil
}

// encodeTransform writes the bytes t derives from the blob read from src to dst
// and returns their digest.
func encodeTransform(dst io.Writer, src io.Reader, t Transform) (n int64, digest []byte, err error) {
	h := sha256.New()
	out := &countingWriter{w: io.MultiWriter(dst, h)}
	if alg, ok := t.algorithm(); ok {
		_, err = erofs.EncodeFileClusters(out, src, int64(t.SourceSize), 1<<t.BlockBits, max(int(t.ClusterBlocks), 1), alg)
	} else {
		err = fmt.Errorf("unknown transform kind %d", t.Kind)
	}
	return out.n, h.Sum(nil), err
}

// applyTransform writes the bytes a transformed reference stands for to dst and
// checks them against the digest and size the stream recorded.
func applyTransform(dst io.Writer, src io.Reader, ref CASReference) error {
	n, digest, err := encodeTransform(dst, src, *ref.Transform)
	if err != nil {
		return err
	}
	if uint64(n) != ref.Size || !bytes.Equal(digest, ref.Transform.OutputDigest) {
		return fmt.Errorf("the transform produced %d bytes with digest %s, but the stream recorded %d bytes with digest %s (the encoder may have changed since the stream was written)",
			n, hex.EncodeToString(digest), ref.Size, hex.EncodeToString(ref.Transform.OutputDigest))
	}
	return nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
