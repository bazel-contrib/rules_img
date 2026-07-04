package tarcas

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
)

// TestInspect builds an index with a known sequence of stream bytes and CAS
// references and checks that compactstream.Inspect recovers the header, the references
// (offset/digest/size), and the byte-stream and reconstructed sizes exactly.
func TestInspect(t *testing.T) {
	var buf bytes.Buffer
	iw := compactstream.NewWriter(&buf, compactstream.HashAlgoSHA256, 32, compactstream.StreamCompressionZstd,
		compactstream.OriginalCompressionInfo{
			Compression:      compactstream.OriginalCompressionGzip,
			CompressionLevel: -1,
			CompressorJobs:   1,
		}, 0)

	d1 := sha256.Sum256([]byte("blob one"))
	d2 := sha256.Sum256([]byte("blob two"))

	// Interleave the way the observer does: a tar header, a blob reference,
	// padding plus the next header, another reference, then a trailing segment.
	mustWriteStream(t, iw, 512)
	mustWriteRef(t, iw, d1[:], 1000)
	mustWriteStream(t, iw, 24)
	mustWriteRef(t, iw, d2[:], 2048)
	mustWriteStream(t, iw, 88)
	if err := iw.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := compactstream.Inspect(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("compactstream.Inspect: %v", err)
	}

	if info.Header.Version != compactstream.FormatVersion {
		t.Errorf("version = %d, want %d", info.Header.Version, compactstream.FormatVersion)
	}
	if info.Header.HashSize != 32 {
		t.Errorf("hash size = %d, want 32", info.Header.HashSize)
	}
	if info.Header.StreamCompression != compactstream.StreamCompressionZstd {
		t.Errorf("stream compression = %d, want zstd(%d)", info.Header.StreamCompression, compactstream.StreamCompressionZstd)
	}
	if info.Header.OriginalCompression.Compression != compactstream.OriginalCompressionGzip {
		t.Errorf("original compression = %d, want gzip(%d)", info.Header.OriginalCompression.Compression, compactstream.OriginalCompressionGzip)
	}

	if got := len(info.Refs); got != 2 {
		t.Fatalf("ref count = %d, want 2", got)
	}
	if info.Refs[0].Offset != 512 || info.Refs[0].Size != 1000 {
		t.Errorf("ref0 = {offset %d, size %d}, want {512, 1000}", info.Refs[0].Offset, info.Refs[0].Size)
	}
	if !bytes.Equal(info.Refs[0].Digest, d1[:]) {
		t.Errorf("ref0 digest = %x, want %x", info.Refs[0].Digest, d1[:])
	}
	// ref1 offset = 512 (header) + 1000 (blob) + 24 (gap) = 1536.
	if info.Refs[1].Offset != 1536 || info.Refs[1].Size != 2048 {
		t.Errorf("ref1 = {offset %d, size %d}, want {1536, 2048}", info.Refs[1].Offset, info.Refs[1].Size)
	}
	if !bytes.Equal(info.Refs[1].Digest, d2[:]) {
		t.Errorf("ref1 digest = %x, want %x", info.Refs[1].Digest, d2[:])
	}

	const streamBytes = uint64(512 + 24 + 88)
	if info.StreamUncompressedSize != streamBytes {
		t.Errorf("stream uncompressed size = %d, want %d", info.StreamUncompressedSize, streamBytes)
	}
	if got, want := info.ReferencedBytes(), uint64(1000+2048); got != want {
		t.Errorf("referenced bytes = %d, want %d", got, want)
	}
	if got, want := info.ReconstructedSize(), streamBytes+3048; got != want {
		t.Errorf("reconstructed size = %d, want %d", got, want)
	}
}

// TestInspectMatchesReconstruction builds an index through the observer (as
// the layer tool does) and checks that the size compactstream.Inspect computes equals the
// length of the actually-reconstructed stream when the original is uncompressed.
func TestInspectMatchesReconstruction(t *testing.T) {
	store := newMemBlobStore()

	bigContent := make([]byte, 5000)
	for i := range bigContent {
		bigContent[i] = byte(i % 251)
	}
	entries := []testEntry{
		{hdr: &tar.Header{Typeflag: tar.TypeDir, Name: "dir/", Mode: 0o755}},
		{hdr: &tar.Header{Typeflag: tar.TypeReg, Name: "dir/big.bin", Size: int64(len(bigContent)), Mode: 0o644}, content: bigContent},
		{hdr: &tar.Header{Typeflag: tar.TypeReg, Name: "empty", Size: 0, Mode: 0o644}},
		{hdr: &tar.Header{Typeflag: tar.TypeReg, Name: "small.txt", Size: 5, Mode: 0o644}, content: []byte("hello")},
	}

	var indexBuf bytes.Buffer
	iw := compactstream.NewWriter(&indexBuf, compactstream.HashAlgoSHA256, 32, compactstream.StreamCompressionZstd,
		compactstream.OriginalCompressionInfo{Compression: compactstream.OriginalCompressionNone, CompressionLevel: -1, CompressorJobs: 1}, 0)
	obs := newCompactStreamObserver[SHA256Helper](iw)
	for _, e := range entries {
		var digest []byte
		if e.hdr.Typeflag == tar.TypeReg && e.hdr.Size > 0 {
			digest = store.Store(e.content)
		}
		w, err := obs.BeginEntry(e.hdr, digest)
		if err != nil {
			t.Fatal(err)
		}
		if w != nil && e.content != nil {
			if _, err := w.Write(e.content); err != nil {
				t.Fatal(err)
			}
		}
		if err := obs.EndEntry(); err != nil {
			t.Fatal(err)
		}
	}
	if err := obs.Close(); err != nil {
		t.Fatal(err)
	}
	if err := iw.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := compactstream.Inspect(bytes.NewReader(indexBuf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	// Only the two regular files with content become CAS references.
	if len(info.Refs) != 2 {
		t.Fatalf("ref count = %d, want 2", len(info.Refs))
	}
	if got, want := info.ReferencedBytes(), uint64(len(bigContent)+5); got != want {
		t.Errorf("referenced bytes = %d, want %d", got, want)
	}

	var reconstructed bytes.Buffer
	if err := compactstream.Reconstruct(context.Background(), bytes.NewReader(indexBuf.Bytes()), store, &reconstructed); err != nil {
		t.Fatal(err)
	}
	// Original compression is None, so the reconstructed output is the raw tar
	// stream, whose length must equal the size compactstream.Inspect computed.
	if uint64(reconstructed.Len()) != info.ReconstructedSize() {
		t.Errorf("reconstructed length = %d, compactstream.Inspect computed %d", reconstructed.Len(), info.ReconstructedSize())
	}
}

func mustWriteStream(t *testing.T, iw *compactstream.Writer, n int) {
	t.Helper()
	if err := iw.WriteStreamBytes(make([]byte, n)); err != nil {
		t.Fatal(err)
	}
}

func mustWriteRef(t *testing.T, iw *compactstream.Writer, digest []byte, size uint64) {
	t.Helper()
	if err := iw.WriteCASRef(digest, size); err != nil {
		t.Fatal(err)
	}
}
