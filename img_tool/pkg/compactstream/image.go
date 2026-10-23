package compactstream

import (
	"crypto/sha256"
	"fmt"
	"io"
)

// Range is a byte range of an image that holds the content of a file with the
// given digest.
type Range struct {
	Offset int64
	Size   int64
	Digest []byte
}

// WriteImage turns a plain (uncompressed) image of the given size into the
// contents of a compact stream.
//
// Every range of at least minSize bytes is written as a CAS reference; all
// other bytes of the image, including the gaps between ranges, are written
// inline. In particular, runs of zeros are kept inline for now. Ranges must be
// sorted by offset, must not overlap and must lie within the image; ranges of
// size zero are ignored.
//
// The SHA-256 digest and size of the whole image are recorded as the
// compressed-stream info, so reconstruction verifies it. That is only correct
// for a writer whose original compression is none, which is what WriteImage
// expects. The caller closes w. For large images, call
// [Writer.CompressIncrementally] first.
func WriteImage(w *Writer, image io.Reader, size int64, ranges []Range, minSize int64) error {
	if w.hashAlgo != HashAlgoSHA256 || w.hashSize != sha256.Size {
		return fmt.Errorf("compact stream: WriteImage needs a SHA-256 writer")
	}
	if w.originalCompression.Compression != OriginalCompressionNone {
		return fmt.Errorf("compact stream: WriteImage needs a writer for an uncompressed original")
	}

	whole := sha256.New()
	image = io.TeeReader(io.LimitReader(image, size), whole)
	buf := make([]byte, 1<<20)
	var pos int64

	copyInline := func(n int64) error {
		for n > 0 {
			chunk := buf[:min(int64(len(buf)), n)]
			if _, err := io.ReadFull(image, chunk); err != nil {
				return fmt.Errorf("compact stream: reading image at %d: %w", pos, unexpectedEOF(err))
			}
			if err := w.WriteStreamBytes(chunk); err != nil {
				return err
			}
			pos += int64(len(chunk))
			n -= int64(len(chunk))
		}
		return nil
	}

	for _, r := range ranges {
		if r.Size <= 0 {
			continue
		}
		if r.Offset < pos || r.Offset+r.Size > size {
			return fmt.Errorf("compact stream: range [%d,%d) is unsorted, overlapping or outside the image (position %d, size %d)", r.Offset, r.Offset+r.Size, pos, size)
		}
		if r.Size < minSize {
			continue // stays inline with the surrounding bytes
		}
		if err := copyInline(r.Offset - pos); err != nil {
			return err
		}
		if err := w.WriteCASRef(r.Digest, uint64(r.Size)); err != nil {
			return err
		}
		// The referenced bytes still pass through the whole-image digest.
		n, err := io.CopyBuffer(io.Discard, io.LimitReader(image, r.Size), buf)
		if err == nil && n != r.Size {
			err = io.ErrUnexpectedEOF
		}
		if err != nil {
			return fmt.Errorf("compact stream: reading image at %d: %w", pos, err)
		}
		pos += r.Size
	}
	if err := copyInline(size - pos); err != nil {
		return err
	}
	return w.SetCompressedStreamInfo(whole.Sum(nil), uint64(size))
}

func unexpectedEOF(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}
