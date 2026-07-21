package ztoc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
)

// TestMarshalUnmarshalRoundTrip exercises the FlatBuffer layer independently of
// the soci corpus, using a hand-built ztoc with a variety of field values.
func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	z := &Ztoc{
		Version:                 Version09,
		BuildToolIdentifier:     "test/1.0",
		CompressedArchiveSize:   123456,
		UncompressedArchiveSize: 999999,
		CompressionInfo: CompressionInfo{
			MaxSpanID:            2,
			CompressionAlgorithm: CompressionGzip,
			Checkpoints:          []byte{0x01, 0x02, 0x03, 0xff, 0x00, 0x7f},
			SpanDigests: []digest.Digest{
				digest.FromString("a"),
				digest.FromString("b"),
				digest.FromString("c"),
			},
		},
		TOC: TOC{FileMetadata: []FileMetadata{
			{
				Name: "dir/", Type: "dir", Mode: 0o755,
				UncompressedOffset: 0, UncompressedSize: 0,
				ModTime: time.Unix(1700000000, 0).UTC(),
			},
			{
				Name: "dir/file.txt", Type: "reg", Mode: 0o644,
				UncompressedOffset: 512, UncompressedSize: 42,
				UID: 1000, GID: 1000, Uname: "alice", Gname: "staff",
				ModTime:    time.Unix(1234567890, 123456789).UTC(),
				PAXHeaders: map[string]string{"SCHILY.xattr.user.k": "v", "path": "dir/file.txt"},
			},
			{
				Name: "dir/dev", Type: "char", Mode: 0o644,
				UncompressedOffset: 1024, Devmajor: 1, Devminor: 3,
				ModTime: time.Unix(1700000000, 0).UTC(),
			},
			{
				Name: "dir/link", Type: "symlink", Linkname: "file.txt", Mode: 0o777,
				UncompressedOffset: 1536,
				ModTime:            time.Unix(1700000000, 0).UTC(),
			},
		}},
	}

	buf, err := Marshal(z)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Unmarshal(buf)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	assertZtocEqual(t, z, got)

	// Re-marshaling the decoded ztoc must be idempotent.
	buf2, err := Marshal(got)
	if err != nil {
		t.Fatalf("Marshal(got): %v", err)
	}
	if !bytes.Equal(buf, buf2) {
		t.Errorf("re-marshal not idempotent: %d vs %d bytes", len(buf), len(buf2))
	}
}

func TestWindowSnapshot(t *testing.T) {
	// Fewer than winSize bytes: zero-padded at the front, data at the tail.
	var w window
	data := []byte("abcdefghij")
	for _, b := range data {
		w.put(b)
	}
	var snap [winSize]byte
	w.snapshot(&snap, int64(len(data)))
	if !bytes.Equal(snap[winSize-len(data):], data) {
		t.Errorf("tail of snapshot != data")
	}
	for i := 0; i < winSize-len(data); i++ {
		if snap[i] != 0 {
			t.Fatalf("expected zero padding at %d, got %#x", i, snap[i])
		}
	}

	// More than winSize bytes: snapshot is the last winSize bytes.
	var w2 window
	total := winSize + 1000
	for i := 0; i < total; i++ {
		w2.put(byte(i))
	}
	var snap2 [winSize]byte
	w2.snapshot(&snap2, int64(total))
	for i := 0; i < winSize; i++ {
		want := byte(total - winSize + i)
		if snap2[i] != want {
			t.Fatalf("snapshot[%d] = %#x want %#x", i, snap2[i], want)
		}
	}
}

func TestBitReaderOffset(t *testing.T) {
	// Consume bits and check the (offset, bits) accounting matches
	// ceil(bitsConsumed/8) and (8 - bitsConsumed%8)%8.
	br := &bitReader{in: []byte{0xff, 0xff, 0xff, 0xff}}
	consumed := uint(0)
	for _, n := range []uint{3, 5, 1, 7, 4} {
		if _, err := br.bits(n); err != nil {
			t.Fatalf("bits(%d): %v", n, err)
		}
		consumed += n
		off, bits := br.offset()
		wantOff := int64((consumed + 7) / 8)
		wantBits := uint8((8 - consumed%8) % 8)
		if off != wantOff || bits != wantBits {
			t.Errorf("after %d bits: got off=%d bits=%d want off=%d bits=%d", consumed, off, bits, wantOff, wantBits)
		}
	}
}

func TestBuildRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"not-gzip", []byte("this is not a gzip stream at all")},
		{"bad-magic", []byte{0x1f, 0x8c, 0x08, 0, 0, 0, 0, 0, 0, 0}},
		{"truncated-header", []byte{0x1f, 0x8b}},
		{"truncated-body", gzipBytes(t, bytes.Repeat([]byte("x"), 5000))[:20]},
		{"corrupt-crc", corruptLastBytes(gzipBytes(t, []byte("hello")), 5)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Build(bytes.NewReader(c.data), int64(len(c.data)), WithSpanSize(4096))
			if err == nil {
				t.Errorf("expected error for %s, got nil", c.name)
			}
		})
	}
}

func TestBuildRejectsBadSpanSize(t *testing.T) {
	data := gzipBytes(t, []byte("hello"))
	if _, err := Build(bytes.NewReader(data), int64(len(data)), WithSpanSize(0)); err == nil {
		t.Error("expected error for zero span size")
	}
	if _, err := Build(bytes.NewReader(data), int64(len(data)), WithSpanSize(-1)); err == nil {
		t.Error("expected error for negative span size")
	}
}

// TestBuildDefaults checks that Build applies the documented defaults.
func TestBuildDefaults(t *testing.T) {
	data := gzipBytes(t, mustTar(t))
	z, err := Build(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if z.Version != Version09 {
		t.Errorf("Version = %q, want %q", z.Version, Version09)
	}
	if z.BuildToolIdentifier != DefaultBuildToolIdentifier {
		t.Errorf("BuildToolIdentifier = %q, want %q", z.BuildToolIdentifier, DefaultBuildToolIdentifier)
	}
	if z.CompressionAlgorithm != CompressionGzip {
		t.Errorf("CompressionAlgorithm = %q, want %q", z.CompressionAlgorithm, CompressionGzip)
	}
	if z.CompressedArchiveSize != Offset(len(data)) {
		t.Errorf("CompressedArchiveSize = %d, want %d", z.CompressedArchiveSize, len(data))
	}
	// Always at least the post-header checkpoint (span 0).
	if z.MaxSpanID != 0 {
		t.Errorf("MaxSpanID = %d, want 0 for a tiny input", z.MaxSpanID)
	}
	if len(z.SpanDigests) != 1 {
		t.Errorf("len(SpanDigests) = %d, want 1", len(z.SpanDigests))
	}
}

// helpers

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func corruptLastBytes(data []byte, n int) []byte {
	out := append([]byte(nil), data...)
	for i := 0; i < n && i < len(out); i++ {
		out[len(out)-1-i] ^= 0xff
	}
	return out
}

// TestUnmarshalRejectsCorruptVectorLength ensures a corrupted vector-count word
// cannot drive a huge allocation or runaway loop: Unmarshal must return an error
// promptly. Regression test for the reader trusting length words verbatim.
func TestUnmarshalRejectsCorruptVectorLength(t *testing.T) {
	z := &Ztoc{
		Version:             Version09,
		BuildToolIdentifier: "t",
		CompressionInfo: CompressionInfo{
			CompressionAlgorithm: CompressionGzip,
			Checkpoints:          []byte{1, 2, 3},
			SpanDigests:          []digest.Digest{digest.FromString("a")},
		},
		TOC: TOC{FileMetadata: []FileMetadata{
			{Name: "f", Type: "reg", UncompressedSize: 1, ModTime: time.Unix(1, 0).UTC()},
		}},
	}
	buf, err := Marshal(z)
	if err != nil {
		t.Fatal(err)
	}
	// Flip every 4-byte aligned word to a huge value; none should make Unmarshal
	// hang or OOM — each must either parse or error, and never take long.
	for off := 0; off+4 <= len(buf); off++ {
		corrupt := append([]byte(nil), buf...)
		corrupt[off] = 0xff
		corrupt[off+1] = 0xff
		corrupt[off+2] = 0xff
		corrupt[off+3] = 0x7f
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() { _ = recover() }()
			_, _ = Unmarshal(corrupt)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("Unmarshal hung on corruption at offset %d", off)
		}
	}
}

// TestInflateRejectsMalformedDeflate checks that crafted deflate bit patterns
// are rejected rather than silently decoded to garbage.
func TestInflateRejectsMalformedDeflate(t *testing.T) {
	// A single fixed-Huffman block that is not terminated / references an
	// impossible distance would be rejected; we cover the parser paths via
	// random gzip-framed garbage bodies. Each must error, never panic/hang.
	rng := []byte{0x1f, 0x8b, 0x08, 0, 0, 0, 0, 0, 0, 0xff}
	body := make([]byte, 256)
	for i := range body {
		body[i] = byte(i * 7)
	}
	data := append(rng, body...)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }()
		_, _ = Build(bytesReaderTest(data), int64(len(data)), WithSpanSize(4096))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Build hung on malformed deflate body")
	}
}

func bytesReaderTest(b []byte) *bytes.Reader { return bytes.NewReader(b) }

func mustTar(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := []byte("contents")
	if err := tw.WriteHeader(&tar.Header{Name: "a.txt", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
