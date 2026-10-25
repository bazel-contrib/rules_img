package extentstream

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"sort"

	"github.com/klauspost/compress/zstd"
)

// Patch overwrites bytes of the content of a referenced blob or stream.
// Offsets are relative to the start of the content.
type Patch struct {
	Offset int64
	Data   []byte
}

// Writer builds an extent stream. Extents are added in the order of the
// content. The extent table and the (compressed) data section are kept in
// memory until Close writes the stream, so large amounts of data should be
// referenced rather than embedded.
type Writer struct {
	out     io.Writer
	extents []Extent
	size    int64

	data    bytes.Buffer // the compressed data section
	enc     *zstd.Encoder
	rawData int64

	format     ContentFormat
	contentSum [HashSize]byte
	hasContent bool
	closed     bool
}

// NewWriter returns a writer that writes the stream to out on Close.
func NewWriter(out io.Writer) *Writer {
	return &Writer{out: out}
}

// SetContentFormat records what the content of the stream is.
func (w *Writer) SetContentFormat(f ContentFormat) { w.format = f }

// SetContentDigest records the SHA-256 of the content of the stream.
func (w *Writer) SetContentDigest(digest []byte) error {
	if len(digest) != HashSize {
		return fmt.Errorf("content digest has %d bytes, want %d", len(digest), HashSize)
	}
	copy(w.contentSum[:], digest)
	w.hasContent = true
	return nil
}

// Offset is the size of the content added so far.
func (w *Writer) Offset() int64 { return w.size }

func (w *Writer) last() *Extent {
	if len(w.extents) == 0 {
		return nil
	}
	return &w.extents[len(w.extents)-1]
}

// AddData embeds the bytes in the stream.
func (w *Writer) AddData(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	if w.enc == nil {
		enc, err := zstd.NewWriter(&w.data, zstd.WithEncoderConcurrency(1))
		if err != nil {
			return err
		}
		w.enc = enc
	}
	if _, err := w.enc.Write(p); err != nil {
		return err
	}
	if last := w.last(); last != nil && last.Kind == KindData {
		last.Length += uint64(len(p))
	} else {
		w.extents = append(w.extents, Extent{Kind: KindData, Length: uint64(len(p)), SrcOffset: uint64(w.rawData)})
	}
	w.rawData += int64(len(p))
	w.size += int64(len(p))
	return nil
}

// AddHole adds n bytes of zeros.
func (w *Writer) AddHole(n int64) error {
	if n < 0 {
		return fmt.Errorf("negative hole of %d bytes", n)
	}
	if n == 0 {
		return nil
	}
	if last := w.last(); last != nil && last.Kind == KindHole {
		last.Length += uint64(n)
	} else {
		w.extents = append(w.extents, Extent{Kind: KindHole, Length: uint64(n)})
	}
	w.size += n
	return nil
}

// AddRef adds length bytes, starting at srcOffset, of the content of the blob
// with the digest. blobSize is the size of the blob: for KindCAS it bounds the
// range, for the stream kinds it is the size of the stream file.
func (w *Writer) AddRef(kind Kind, digest []byte, blobSize, srcOffset, length int64) error {
	if !kind.IsRef() {
		return fmt.Errorf("%s is not a reference kind", kind)
	}
	if len(digest) != HashSize {
		return fmt.Errorf("digest has %d bytes, want %d", len(digest), HashSize)
	}
	if blobSize < 0 || srcOffset < 0 || length < 0 {
		return fmt.Errorf("negative blob size, offset or length")
	}
	if length == 0 {
		return nil
	}
	if kind == KindCAS && srcOffset+length > blobSize {
		return fmt.Errorf("range %d+%d is beyond the blob of %d bytes", srcOffset, length, blobSize)
	}
	e := Extent{Kind: kind, Length: uint64(length), SrcOffset: uint64(srcOffset), BlobSize: uint64(blobSize)}
	copy(e.Digest[:], digest)
	if last := w.last(); last != nil && last.Kind == kind && last.Digest == e.Digest && last.BlobSize == e.BlobSize &&
		last.SrcOffset+last.Length == e.SrcOffset {
		last.Length += e.Length
	} else {
		w.extents = append(w.extents, e)
	}
	w.size += length
	return nil
}

// AddRefPatched adds the first contentSize bytes of the content of a blob or
// stream, with the patches written over them. The references around a patch
// keep pointing at the blob.
func (w *Writer) AddRefPatched(kind Kind, digest []byte, blobSize, contentSize int64, patches []Patch) error {
	sorted := append([]Patch(nil), patches...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Offset < sorted[j].Offset })
	cur := int64(0)
	for _, p := range sorted {
		end := p.Offset + int64(len(p.Data))
		if p.Offset < cur || end > contentSize {
			return fmt.Errorf("patch at %d+%d overlaps another patch or is beyond the %d bytes of content", p.Offset, len(p.Data), contentSize)
		}
		if err := w.AddRef(kind, digest, blobSize, cur, p.Offset-cur); err != nil {
			return err
		}
		if err := w.AddData(p.Data); err != nil {
			return err
		}
		cur = end
	}
	return w.AddRef(kind, digest, blobSize, cur, contentSize-cur)
}

// ApplyPatches returns a copy of data with the patches written over it.
func ApplyPatches(data []byte, patches []Patch) ([]byte, error) {
	out := append([]byte(nil), data...)
	for _, p := range patches {
		if p.Offset < 0 || p.Offset+int64(len(p.Data)) > int64(len(out)) {
			return nil, fmt.Errorf("patch at %d+%d is beyond the %d bytes of data", p.Offset, len(p.Data), len(data))
		}
		copy(out[p.Offset:], p.Data)
	}
	return out, nil
}

// Close writes the stream.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	h := Header{
		Version:          FormatVersion,
		ContentFormat:    w.format,
		Size:             uint64(w.size),
		ExtentCount:      uint64(len(w.extents)),
		HasContentDigest: w.hasContent,
		ContentDigest:    w.contentSum,
		tableOffset:      HeaderSize,
		tableSize:        uint64(len(w.extents)) * ExtentSize,
		dataRawSize:      uint64(w.rawData),
	}
	h.dataOffset = h.tableOffset + h.tableSize
	if w.enc != nil {
		if err := w.enc.Close(); err != nil {
			return err
		}
		h.dataCompression = dataCompressionZstd
	}
	h.dataSize = uint64(w.data.Len())

	table := make([]byte, h.tableSize)
	for i := range w.extents {
		w.extents[i].marshal(table[i*ExtentSize:])
	}
	for _, part := range [][]byte{h.marshal(), table, w.data.Bytes()} {
		if _, err := w.out.Write(part); err != nil {
			return err
		}
	}
	return nil
}

// Digest returns the SHA-256 of the file at r, which is how streams are
// referenced.
func Digest(r io.Reader) ([]byte, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return nil, 0, err
	}
	return h.Sum(nil), n, nil
}
