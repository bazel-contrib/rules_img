package extentstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"

	"github.com/klauspost/compress/zstd"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
)

// Index is the header and the extent table of a stream.
type Index struct {
	Header  Header
	Extents []Extent
}

// reader reads a stream sequentially: the header, the extent table and then
// the data section, which is left in data.
type reader struct {
	Index
	data io.Reader
}

func readStream(r io.Reader) (*reader, error) {
	hb := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, hb); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, errNotExtentStream
		}
		return nil, err
	}
	h, err := parseHeader(hb)
	if err != nil {
		return nil, err
	}
	table := make([]byte, h.tableSize)
	if _, err := io.ReadFull(r, table); err != nil {
		return nil, fmt.Errorf("reading the extent table: %w", err)
	}
	extents := make([]Extent, h.ExtentCount)
	for i := range extents {
		extents[i] = parseExtent(table[i*ExtentSize:])
	}
	if err := validate(&h, extents); err != nil {
		return nil, err
	}
	return &reader{Index: Index{Header: h, Extents: extents}, data: io.LimitReader(r, int64(h.dataSize))}, nil
}

// validate checks that the extents tile the content and are in bounds.
func validate(h *Header, extents []Extent) error {
	var pos, raw uint64
	for i, e := range extents {
		if e.Length == 0 || e.Length > math.MaxInt64 || pos+e.Length < pos {
			return fmt.Errorf("corrupt extent stream: extent %d has length %d", i, e.Length)
		}
		switch e.Kind {
		case KindData:
			if e.SrcOffset != raw {
				return fmt.Errorf("corrupt extent stream: data extent %d is not contiguous in the data section", i)
			}
			raw += e.Length
		case KindHole:
		case KindCAS:
			if e.SrcOffset > e.BlobSize || e.Length > e.BlobSize-e.SrcOffset {
				return fmt.Errorf("corrupt extent stream: extent %d is beyond its blob of %d bytes", i, e.BlobSize)
			}
		case KindCompactStream, KindExtentStream:
			if e.SrcOffset > math.MaxInt64 || e.SrcOffset+e.Length < e.SrcOffset {
				return fmt.Errorf("corrupt extent stream: extent %d has a bad range", i)
			}
		default:
			return fmt.Errorf("corrupt extent stream: extent %d has unknown kind %d", i, uint8(e.Kind))
		}
		pos += e.Length
	}
	if pos != h.Size {
		return fmt.Errorf("corrupt extent stream: the extents add up to %d bytes, the header says %d", pos, h.Size)
	}
	if raw != h.dataRawSize {
		return fmt.Errorf("corrupt extent stream: %d bytes of data extents, %d in the data section", raw, h.dataRawSize)
	}
	return nil
}

// Inspect reads the header and the extent table. It does not need any blobs.
func Inspect(r io.Reader) (*Index, error) {
	s, err := readStream(r)
	if err != nil {
		return nil, err
	}
	return &s.Index, nil
}

// HoleWriter is a writer that can skip holes instead of writing zeros. A file
// that is seeked over stays sparse.
type HoleWriter interface {
	io.Writer
	// WriteHole advances the output by n bytes of zeros.
	WriteHole(n int64) error
}

type zeroWriter struct{ io.Writer }

var zeros = make([]byte, 1<<20)

func (z zeroWriter) WriteHole(n int64) error {
	for n > 0 {
		c := min(n, int64(len(zeros)))
		if _, err := z.Write(zeros[:c]); err != nil {
			return err
		}
		n -= c
	}
	return nil
}

// Reconstruct writes the content of the stream to out. Blobs and the streams it
// references are read from the store. If out is a HoleWriter, holes are skipped.
func Reconstruct(ctx context.Context, index io.Reader, store compactstream.BlobStore, out io.Writer) error {
	s, err := readStream(index)
	if err != nil {
		return err
	}
	hw, ok := out.(HoleWriter)
	if !ok {
		hw = zeroWriter{out}
	}
	var sum hash.Hash
	if s.Header.HasContentDigest {
		sum = sha256.New()
		hw = &hashingWriter{HoleWriter: hw, h: sum}
	}
	if err := s.writeRange(ctx, store, hw, 0, int64(s.Header.Size), 0); err != nil {
		return err
	}
	if sum != nil && !bytes.Equal(sum.Sum(nil), s.Header.ContentDigest[:]) {
		return fmt.Errorf("the reconstructed content has the digest sha256:%x, the stream says sha256:%x", sum.Sum(nil), s.Header.ContentDigest[:])
	}
	return nil
}

type hashingWriter struct {
	HoleWriter
	h hash.Hash
}

func (w *hashingWriter) Write(p []byte) (int, error) {
	w.h.Write(p)
	return w.HoleWriter.Write(p)
}

func (w *hashingWriter) WriteHole(n int64) error {
	if err := (zeroWriter{w.h}).WriteHole(n); err != nil { // hash the zeros
		return err
	}
	return w.HoleWriter.WriteHole(n)
}

// writeRange writes the content bytes [off, off+n) of the stream.
func (s *reader) writeRange(ctx context.Context, store compactstream.BlobStore, out HoleWriter, off, n int64, depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("streams are nested more than %d deep", maxDepth)
	}
	if off < 0 || n < 0 || off+n > int64(s.Header.Size) {
		return fmt.Errorf("range %d+%d is beyond the %d bytes of the stream", off, n, s.Header.Size)
	}
	var data io.Reader = s.data
	if s.Header.dataCompression == dataCompressionZstd {
		dec, err := zstd.NewReader(s.data, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return err
		}
		defer dec.Close()
		data = dec
	}
	discard := func(n int64) error {
		if _, err := io.CopyN(io.Discard, data, n); err != nil {
			return fmt.Errorf("reading the data section: %w", unexpected(err))
		}
		return nil
	}
	end := off + n
	var pos int64
	for _, e := range s.Extents {
		extStart, extEnd := pos, pos+int64(e.Length)
		pos = extEnd
		if extStart >= end {
			break
		}
		a, b := max(extStart, off), min(extEnd, end)
		if a >= b {
			if e.Kind == KindData { // the data section is read sequentially
				if err := discard(int64(e.Length)); err != nil {
					return err
				}
			}
			continue
		}
		skip, length := a-extStart, b-a
		switch e.Kind {
		case KindData:
			if err := discard(skip); err != nil {
				return err
			}
			if _, err := io.CopyN(out, data, length); err != nil {
				return fmt.Errorf("reading the data section: %w", unexpected(err))
			}
			// A later extent in the range never needs the rest of this one.
		case KindHole:
			if err := out.WriteHole(length); err != nil {
				return err
			}
		case KindCAS:
			if err := copyBlob(ctx, store, out, e, int64(e.SrcOffset)+skip, length); err != nil {
				return err
			}
		case KindCompactStream:
			if err := copyCompactStream(ctx, store, out, e, int64(e.SrcOffset)+skip, length); err != nil {
				return err
			}
		case KindExtentStream:
			raw, err := fetchStream(ctx, store, e)
			if err != nil {
				return err
			}
			child, err := readStream(bytes.NewReader(raw))
			if err != nil {
				return fmt.Errorf("stream sha256:%x: %w", e.Digest, err)
			}
			if err := child.writeRange(ctx, store, out, int64(e.SrcOffset)+skip, length, depth+1); err != nil {
				return fmt.Errorf("stream sha256:%x: %w", e.Digest, err)
			}
		}
	}
	return nil
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// copyBlob copies length bytes of the blob, from offset.
func copyBlob(ctx context.Context, store compactstream.BlobStore, out io.Writer, e Extent, offset, length int64) error {
	rc, err := store.ReaderForBlob(ctx, e.Digest[:], int64(e.BlobSize))
	if err != nil {
		return fmt.Errorf("blob sha256:%x: %w", e.Digest, err)
	}
	defer rc.Close()
	if err := skipBytes(rc, offset); err != nil {
		return fmt.Errorf("blob sha256:%x: %w", e.Digest, unexpected(err))
	}
	if _, err := io.CopyN(out, rc, length); err != nil {
		return fmt.Errorf("blob sha256:%x: %w", e.Digest, unexpected(err))
	}
	return nil
}

func skipBytes(r io.Reader, n int64) error {
	if n == 0 {
		return nil
	}
	if s, ok := r.(io.Seeker); ok {
		_, err := s.Seek(n, io.SeekStart)
		return err
	}
	_, err := io.CopyN(io.Discard, r, n)
	return err
}

// fetchStream reads the file of a referenced stream and checks its digest.
func fetchStream(ctx context.Context, store compactstream.BlobStore, e Extent) ([]byte, error) {
	if e.BlobSize > maxChildSize {
		return nil, fmt.Errorf("stream sha256:%x is %d bytes, more than the limit of %d", e.Digest, e.BlobSize, int64(maxChildSize))
	}
	rc, err := store.ReaderForBlob(ctx, e.Digest[:], int64(e.BlobSize))
	if err != nil {
		return nil, fmt.Errorf("stream sha256:%x: %w", e.Digest, err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, int64(e.BlobSize)+1))
	if err != nil {
		return nil, fmt.Errorf("stream sha256:%x: %w", e.Digest, err)
	}
	if sum := sha256.Sum256(raw); sum != e.Digest || uint64(len(raw)) != e.BlobSize {
		return nil, fmt.Errorf("stream sha256:%x has the digest sha256:%x and %d bytes, expected %d bytes", e.Digest, sum, len(raw), e.BlobSize)
	}
	return raw, nil
}

func copyCompactStream(ctx context.Context, store compactstream.BlobStore, out io.Writer, e Extent, offset, length int64) error {
	raw, err := fetchStream(ctx, store, e)
	if err != nil {
		return err
	}
	rr, err := compactstream.NewReconstructingReader(ctx, bytes.NewReader(raw), store)
	if err != nil {
		return fmt.Errorf("compact stream sha256:%x: %w", e.Digest, err)
	}
	defer rr.Close()
	if err := skipBytes(rr, offset); err != nil {
		return fmt.Errorf("compact stream sha256:%x: %w", e.Digest, unexpected(err))
	}
	if _, err := io.CopyN(out, rr, length); err != nil {
		return fmt.Errorf("compact stream sha256:%x: %w", e.Digest, unexpected(err))
	}
	return nil
}
