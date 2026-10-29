package compactstream

import (
	"crypto/sha256"
	"fmt"
	"hash"
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
	return WriteImageKnownDigest(w, image, size, ranges, minSize, nil)
}

// WriteImageKnownDigest is like [WriteImage] for an image whose SHA-256 digest
// the caller already knows. The digest is trusted. The bytes of ranges that
// become references are then not needed at all: if image is also an
// [io.Seeker] (like an [io.SectionReader]), they are skipped without reading.
func WriteImageKnownDigest(w *Writer, image io.Reader, size int64, ranges []Range, minSize int64, digest []byte) error {
	if w.hashAlgo != HashAlgoSHA256 || w.hashSize != sha256.Size {
		return fmt.Errorf("compact stream: WriteImage needs a SHA-256 writer")
	}
	if w.originalCompression.Compression != OriginalCompressionNone {
		return fmt.Errorf("compact stream: WriteImage needs a writer for an uncompressed original")
	}

	var whole hash.Hash
	seeker, _ := image.(io.Seeker)
	if digest == nil {
		seeker = nil
		whole = sha256.New()
		image = io.TeeReader(io.LimitReader(image, size), whole)
	} else if len(digest) != sha256.Size {
		return fmt.Errorf("compact stream: image digest has %d bytes, want %d", len(digest), sha256.Size)
	}
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
		if seeker != nil {
			if _, err := seeker.Seek(r.Size, io.SeekCurrent); err != nil {
				return fmt.Errorf("compact stream: skipping in the image at %d: %w", pos, err)
			}
			pos += r.Size
			continue
		}
		// Without a known digest, the referenced bytes still pass through the
		// whole-image digest.
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
	if whole != nil {
		digest = whole.Sum(nil)
	}
	return w.SetCompressedStreamInfo(digest, uint64(size))
}

func unexpectedEOF(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}
