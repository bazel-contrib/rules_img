package compactstream

import (
	"crypto/sha256"
	"fmt"
	"io"
	"sort"
)

// This file has the helpers that compose a compact stream out of pieces: the
// pieces of a disk image, for example, are the partition table, runs of zeros,
// the file system images of its partitions (which may themselves be compact
// streams) and free space.

// zeroChunk is the unit zero runs are written in.
var zeroChunk = make([]byte, 1<<20)

// WriteZeroRun appends n zero bytes to the byte stream. The format has no
// primitive for zero runs: they are inline bytes, which a stream compressed
// with zstd (see CompressIncrementally) shrinks to almost nothing. The cost is
// the time to compress n zero bytes.
func (w *Writer) WriteZeroRun(n int64) error {
	if n < 0 {
		return fmt.Errorf("compact stream: negative zero run %d", n)
	}
	for n > 0 {
		chunk := min(n, int64(len(zeroChunk)))
		if err := w.WriteStreamBytes(zeroChunk[:chunk]); err != nil {
			return err
		}
		n -= chunk
	}
	return w.err
}

// Offset is the number of bytes of the reconstructed stream that were written
// so far.
func (w *Writer) Offset() int64 { return int64(w.currentOffset) }

// Patch overwrites bytes of a stream that is appended with AppendStream or
// AppendImage. Offset is relative to the start of the appended content.
type Patch struct {
	Offset int64
	Data   []byte
}

// patcher applies patches to the inline bytes that flow by.
type patcher struct {
	patches []Patch // sorted by offset, not overlapping
	applied []int   // bytes of each patch that were applied
}

func newPatcher(patches []Patch) (*patcher, error) {
	p := &patcher{patches: append([]Patch(nil), patches...)}
	sort.Slice(p.patches, func(i, j int) bool { return p.patches[i].Offset < p.patches[j].Offset })
	for i, patch := range p.patches {
		if patch.Offset < 0 {
			return nil, fmt.Errorf("compact stream: patch at negative offset %d", patch.Offset)
		}
		if i > 0 && p.patches[i-1].Offset+int64(len(p.patches[i-1].Data)) > patch.Offset {
			return nil, fmt.Errorf("compact stream: patches at %d and %d overlap", p.patches[i-1].Offset, patch.Offset)
		}
	}
	p.applied = make([]int, len(p.patches))
	return p, nil
}

// overlay writes the parts of the patches that fall into buf, which holds the
// bytes starting at offset start of the appended content.
func (p *patcher) overlay(buf []byte, start int64) {
	end := start + int64(len(buf))
	for i, patch := range p.patches {
		pend := patch.Offset + int64(len(patch.Data))
		if pend <= start || patch.Offset >= end {
			continue
		}
		from, to := max(patch.Offset, start), min(pend, end)
		copy(buf[from-start:to-start], patch.Data[from-patch.Offset:to-patch.Offset])
		p.applied[i] += int(to - from)
	}
}

// blockedBy returns an error if a patch falls into [start, end), which is not
// inline: the bytes are a reference to content that is not part of the stream.
func (p *patcher) blockedBy(start, end int64) error {
	for _, patch := range p.patches {
		if patch.Offset < end && patch.Offset+int64(len(patch.Data)) > start {
			return fmt.Errorf("compact stream: the patch at offset %d lies in bytes [%d,%d) that are a reference to a blob and cannot be patched", patch.Offset, start, end)
		}
	}
	return nil
}

// complete checks that every patch was applied in full, that is, it lies
// within the appended content.
func (p *patcher) complete() error {
	for i, patch := range p.patches {
		if p.applied[i] != len(patch.Data) {
			return fmt.Errorf("compact stream: the patch at offset %d (%d bytes) lies outside the appended content", patch.Offset, len(patch.Data))
		}
	}
	return nil
}

// copyInline copies n bytes of src to the writer as inline bytes, with the
// patches applied. base is the offset of the first byte in the appended
// content. It returns io.ErrUnexpectedEOF if src has fewer than n bytes.
func copyInline(w *Writer, src io.Reader, n, base int64, p *patcher, buf []byte) error {
	for done := int64(0); done < n; {
		chunk := buf[:min(int64(len(buf)), n-done)]
		if _, err := io.ReadFull(src, chunk); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
		p.overlay(chunk, base+done)
		if err := w.WriteStreamBytes(chunk); err != nil {
			return err
		}
		done += int64(len(chunk))
	}
	return nil
}

// AppendStream appends the reconstructed content of the compact stream read
// from index to w: its inline bytes are written inline, and its references to
// blobs are kept, now at their new offset. The blobs stay where they are, so
// the result references the same content-addressed store, and nothing is
// hashed or fetched. It returns the size of the appended content.
//
// The patches are applied to the inline bytes. A patch that falls onto a
// reference, or outside the content, is an error.
//
// The stream must be one that reconstructs to the plain bytes: not compressed
// (original compression none), with the same hash algorithm as w, and not a
// tar (the content of a tar layer is not a file system image).
func AppendStream(w *Writer, index io.Reader, patches []Patch) (int64, error) {
	if w.err != nil {
		return 0, w.err
	}
	header, err := ReadHeader(index)
	if err != nil {
		return 0, err
	}
	if header.HashAlgo != w.hashAlgo || header.HashSize != w.hashSize {
		return 0, fmt.Errorf("compact stream: cannot append a stream with hash algorithm %d (%d bytes) to one with %d (%d bytes)", header.HashAlgo, header.HashSize, w.hashAlgo, w.hashSize)
	}
	if header.OriginalCompression.Compression != OriginalCompressionNone {
		return 0, fmt.Errorf("compact stream: cannot append a stream of a compressed file (compression %d); only the plain bytes can be placed in another stream", header.OriginalCompression.Compression)
	}
	if header.ContentFormat == ContentFormatTar {
		return 0, fmt.Errorf("compact stream: cannot append the stream of a tar archive")
	}
	refs, err := readRefTable(index, header)
	if err != nil {
		return 0, err
	}
	stream, err := streamReader(index, header.StreamCompression)
	if err != nil {
		return 0, err
	}
	if closer, ok := stream.(io.Closer); ok {
		defer closer.Close()
	}
	p, err := newPatcher(patches)
	if err != nil {
		return 0, err
	}

	buf := make([]byte, 1<<20)
	var pos int64
	for _, ref := range refs {
		off, size := int64(ref.Offset), int64(ref.Size)
		if err := copyInline(w, stream, off-pos, pos, p, buf); err != nil {
			return 0, fmt.Errorf("compact stream: reading the inline bytes before the reference at %d: %w", off, err)
		}
		if err := p.blockedBy(off, off+size); err != nil {
			return 0, err
		}
		if err := w.WriteCASRef(ref.Digest, ref.Size); err != nil {
			return 0, err
		}
		pos = off + size
	}

	// The rest of the stream is inline.
	rest, err := io.CopyBuffer(&inlineWriter{w: w, p: p, base: pos}, stream, buf)
	if err != nil {
		return 0, fmt.Errorf("compact stream: reading the inline bytes after the last reference: %w", err)
	}
	pos += rest
	if pad := int64(header.OriginalCompression.EndPadding); pad > 0 {
		if err := w.WriteZeroRun(pad); err != nil {
			return 0, err
		}
		pos += pad
	}
	if err := p.complete(); err != nil {
		return 0, err
	}
	return pos, w.err
}

// inlineWriter writes the bytes that flow through it as inline bytes, with
// patches applied.
type inlineWriter struct {
	w    *Writer
	p    *patcher
	base int64
	done int64
}

func (iw *inlineWriter) Write(b []byte) (int, error) {
	chunk := append([]byte(nil), b...)
	iw.p.overlay(chunk, iw.base+iw.done)
	if err := iw.w.WriteStreamBytes(chunk); err != nil {
		return 0, err
	}
	iw.done += int64(len(b))
	return len(b), nil
}

// AppendImage appends size bytes read from r as inline bytes, with the patches
// applied. Use it for content that is small, or mostly zeros (zstd makes zeros
// cheap), or that has to be patched.
func AppendImage(w *Writer, r io.Reader, size int64, patches []Patch) error {
	if w.err != nil {
		return w.err
	}
	p, err := newPatcher(patches)
	if err != nil {
		return err
	}
	if err := copyInline(w, r, size, 0, p, make([]byte, 1<<20)); err != nil {
		return fmt.Errorf("compact stream: reading %d bytes of image: %w", size, err)
	}
	return p.complete()
}

// AppendImageRef appends size bytes read from r as one reference to a blob and
// returns the digest of the blob. The blob has to be in the content-addressed
// store when the stream is reconstructed.
func AppendImageRef(w *Writer, r io.Reader, size int64) ([]byte, error) {
	if w.err != nil {
		return nil, w.err
	}
	if w.hashAlgo != HashAlgoSHA256 {
		return nil, fmt.Errorf("compact stream: AppendImageRef needs a SHA-256 writer")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(r, size))
	if err == nil && n != size {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		return nil, fmt.Errorf("compact stream: reading %d bytes of image: %w", size, err)
	}
	digest := h.Sum(nil)
	return digest, w.WriteCASRef(digest, uint64(size))
}

// ReconstructedSize returns the size in bytes of what the compact stream in
// index reconstructs to. If the stream records the size of the compressed
// stream and it is not compressed, that is quick; otherwise the byte stream is
// decompressed to measure it, which is why index has to be seekable.
func ReconstructedSize(index io.ReadSeeker) (int64, error) {
	header, err := ReadHeader(index)
	if err != nil {
		return 0, err
	}
	if header.HasCompressedStreamInfo && header.OriginalCompression.Compression == OriginalCompressionNone && header.OriginalCompression.EndPadding == 0 {
		return int64(header.CompressedStreamSize), nil
	}
	if _, err := index.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	info, err := Inspect(index)
	if err != nil {
		return 0, err
	}
	return int64(info.ReconstructedSize()) + int64(info.Header.OriginalCompression.EndPadding), nil
}
