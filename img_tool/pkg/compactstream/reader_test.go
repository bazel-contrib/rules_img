package compactstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"testing"
)

// rawRef describes a CAS reference for hand-building a (possibly malformed)
// compact stream in tests.
type rawRef struct {
	offset uint64
	digest []byte
	size   uint64
}

// buildRawCompactStream serializes an uncompressed compact stream (no stream
// compression, no original compression, no compressed-stream info) with the
// given refs and byte stream. It bypasses Writer so tests can construct inputs a
// well-behaved Writer would never emit (unsorted/overlapping/oversized refs).
func buildRawCompactStream(refs []rawRef, stream []byte) []byte {
	const hs = sha256.Size
	entrySize := 16 + hs
	refTableSize := uint64(len(refs) * entrySize)

	var hdr [headerSize]byte
	copy(hdr[0:6], magic)
	hdr[7] = FormatVersion
	binary.BigEndian.PutUint16(hdr[8:10], HashAlgoSHA256)
	binary.BigEndian.PutUint16(hdr[10:12], hs)
	hdr[12] = StreamCompressionNone
	hdr[13] = OriginalCompressionNone
	binary.BigEndian.PutUint64(hdr[24:32], headerSize)
	binary.BigEndian.PutUint64(hdr[32:40], refTableSize)
	binary.BigEndian.PutUint64(hdr[40:48], headerSize+refTableSize)
	binary.BigEndian.PutUint64(hdr[48:56], uint64(len(stream)))

	var b bytes.Buffer
	b.Write(hdr[:])
	for _, r := range refs {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], r.offset)
		b.Write(n[:])
		d := make([]byte, hs)
		copy(d, r.digest)
		b.Write(d)
		binary.BigEndian.PutUint64(n[:], r.size)
		b.Write(n[:])
	}
	b.Write(stream)
	return b.Bytes()
}

type mapBlobStore map[string][]byte

func (m mapBlobStore) put(data []byte) []byte {
	d := sha256.Sum256(data)
	m[string(d[:])] = data
	return d[:]
}

func (m mapBlobStore) ReaderForBlob(_ context.Context, digest []byte, _ int64) (io.ReadCloser, error) {
	data, ok := m[string(digest)]
	if !ok {
		return nil, fmt.Errorf("blob not found: %x", digest)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func validEmptyIndex(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	iw := NewWriter(&buf, HashAlgoSHA256, 32, StreamCompressionNone, OriginalCompressionInfo{}, 0)
	if err := iw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestReadHeaderRejectsMalformed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]byte)
	}{
		{"bad magic", func(b []byte) { b[0] = 'X' }},
		{"bad nul byte", func(b []byte) { b[6] = 1 }},
		{"unsupported version", func(b []byte) { b[7] = 0xFF }},
		{"unsupported hash algo", func(b []byte) { binary.BigEndian.PutUint16(b[8:10], 999) }},
		{"hash size mismatch", func(b []byte) { binary.BigEndian.PutUint16(b[10:12], 16) }},
		{"ref table not multiple of entry size", func(b []byte) { binary.BigEndian.PutUint64(b[32:40], 47) }},
		{"ref table exceeds maximum", func(b []byte) { binary.BigEndian.PutUint64(b[32:40], maxRefTableSize+48) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idx := validEmptyIndex(t)
			tc.mutate(idx)
			if _, err := ReadHeader(bytes.NewReader(idx)); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

func TestReadHeaderTruncated(t *testing.T) {
	idx := validEmptyIndex(t)
	if _, err := ReadHeader(bytes.NewReader(idx[:headerSize-1])); err == nil {
		t.Fatal("expected error for truncated header")
	}
}

func TestReconstructRejectsUnsortedRefs(t *testing.T) {
	store := mapBlobStore{}
	a := store.put([]byte("AAAA"))
	b := store.put([]byte("BBBB"))
	// Out-of-order: the second ref's offset precedes the end of the first.
	refs := []rawRef{
		{offset: 8, digest: a, size: 4},
		{offset: 0, digest: b, size: 4},
	}
	idx := buildRawCompactStream(refs, []byte("01234567"))
	err := Reconstruct(context.Background(), bytes.NewReader(idx), store, io.Discard)
	if err == nil {
		t.Fatal("expected error for unsorted refs, got nil (silent corruption)")
	}
	if !strings.Contains(err.Error(), "sorted") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestReconstructRejectsOverlappingRefs(t *testing.T) {
	store := mapBlobStore{}
	a := store.put([]byte("AAAA"))
	b := store.put([]byte("BBBB"))
	// Overlap: ref0 covers [0,4), ref1 starts at 2.
	refs := []rawRef{
		{offset: 0, digest: a, size: 4},
		{offset: 2, digest: b, size: 4},
	}
	idx := buildRawCompactStream(refs, nil)
	if err := Reconstruct(context.Background(), bytes.NewReader(idx), store, io.Discard); err == nil {
		t.Fatal("expected error for overlapping refs, got nil")
	}
}

func TestReconstructRejectsHugeRefSize(t *testing.T) {
	store := mapBlobStore{}
	d := store.put([]byte("x"))
	refs := []rawRef{{offset: 0, digest: d, size: 1 << 63}} // > math.MaxInt64
	idx := buildRawCompactStream(refs, nil)
	err := Reconstruct(context.Background(), bytes.NewReader(idx), store, io.Discard)
	if err == nil {
		t.Fatal("expected error for oversized ref, got nil")
	}
	if !strings.Contains(err.Error(), "size") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestReconstructTruncatedRefTable(t *testing.T) {
	store := mapBlobStore{}
	d := store.put([]byte("data"))
	idx := buildRawCompactStream([]rawRef{{offset: 0, digest: d, size: 4}}, nil)
	// Cut off in the middle of the ref table.
	if err := Reconstruct(context.Background(), bytes.NewReader(idx[:headerSize+10]), store, io.Discard); err == nil {
		t.Fatal("expected error for truncated ref table")
	}
}

func TestReconstructInterleavesGapsAndBlobs(t *testing.T) {
	store := mapBlobStore{}
	blob := store.put([]byte("BLOBDATA")) // 8 bytes, replaces [4,12)
	// Byte stream (CAS-referenced range removed) is "HDR-" + "-END".
	idx := buildRawCompactStream([]rawRef{{offset: 4, digest: blob, size: 8}}, []byte("HDR--END"))
	var out bytes.Buffer
	if err := Reconstruct(context.Background(), bytes.NewReader(idx), store, &out); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "HDR-BLOBDATA-END"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestReconstructEndPadding(t *testing.T) {
	var buf bytes.Buffer
	iw := NewWriter(&buf, HashAlgoSHA256, 32, StreamCompressionNone,
		OriginalCompressionInfo{Compression: OriginalCompressionNone, EndPadding: 7}, 0)
	stream := []byte("hello tar bytes")
	if err := iw.WriteStreamBytes(stream); err != nil {
		t.Fatal(err)
	}
	if err := iw.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := Reconstruct(context.Background(), &buf, mapBlobStore{}, &out); err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), stream...), make([]byte, 7)...)
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("end padding mismatch: got %d bytes, want %d bytes", out.Len(), len(want))
	}
}

// errAfterWriter fails writes once n bytes have been accepted, simulating a
// downstream consumer (e.g. a registry push) that aborts mid-stream.
type errAfterWriter struct {
	n       int
	written int
}

func (w *errAfterWriter) Write(p []byte) (int, error) {
	if w.written >= w.n {
		return 0, fmt.Errorf("downstream write failed")
	}
	if w.written+len(p) > w.n {
		allowed := w.n - w.written
		w.written = w.n
		return allowed, fmt.Errorf("downstream write failed")
	}
	w.written += len(p)
	return len(p), nil
}

// TestReconstructAppendErrorNoRace drives the gzip-original + zstd-stream path
// (which reconstructs through an io.Pipe with a goroutine reading the zstd
// stream decoder) and fails the downstream writer immediately. Reconstruct must
// return a clean error without panicking, and — when run under -race — without
// racing the deferred stream-decoder Close against the still-running reader
// goroutine. The payload is highly compressible (tiny on disk, large when
// decoded) so the decoder's workers stay busy while AppendTar fails early,
// which is the window the race occurs in; it is looped to make the detector
// reliably catch a regression.
func TestReconstructAppendErrorNoRace(t *testing.T) {
	large := bytes.Repeat([]byte("compressible-"), 1<<20) // ~13 MiB decoded
	for iter := 0; iter < 40; iter++ {
		var idx bytes.Buffer
		iw := NewWriter(&idx, HashAlgoSHA256, 32, StreamCompressionZstd,
			OriginalCompressionInfo{Compression: OriginalCompressionGzip, CompressionLevel: 6, CompressorJobs: 1}, 0)
		if err := iw.WriteStreamBytes(large); err != nil {
			t.Fatal(err)
		}
		if err := iw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := Reconstruct(context.Background(), &idx, mapBlobStore{}, &errAfterWriter{n: 1}); err == nil {
			t.Fatal("expected an error from the failing downstream writer")
		}
	}
}

func TestReconstructEndPaddingValidatedByDigest(t *testing.T) {
	stream := []byte("payload that needs trailing padding")
	const pad = 11

	build := func(digest []byte, size uint64) *bytes.Buffer {
		var buf bytes.Buffer
		iw := NewWriter(&buf, HashAlgoSHA256, 32, StreamCompressionNone,
			OriginalCompressionInfo{Compression: OriginalCompressionNone, EndPadding: pad}, 0)
		if err := iw.WriteStreamBytes(stream); err != nil {
			t.Fatal(err)
		}
		if digest != nil {
			if err := iw.SetCompressedStreamInfo(digest, size); err != nil {
				t.Fatal(err)
			}
		}
		if err := iw.Close(); err != nil {
			t.Fatal(err)
		}
		return &buf
	}

	// Digest/size must cover the stream bytes AND the trailing padding.
	full := append(append([]byte(nil), stream...), make([]byte, pad)...)
	good := sha256.Sum256(full)
	if err := Reconstruct(context.Background(), build(good[:], uint64(len(full))), mapBlobStore{}, io.Discard); err != nil {
		t.Fatalf("expected success when digest covers padding: %v", err)
	}

	// Digest over the stream WITHOUT the padding must be rejected.
	noPad := sha256.Sum256(stream)
	if err := Reconstruct(context.Background(), build(noPad[:], uint64(len(stream))), mapBlobStore{}, io.Discard); err == nil {
		t.Fatal("expected failure when recorded digest omits end padding")
	}
}
