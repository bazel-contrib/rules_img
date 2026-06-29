package tarcas

import (
	"archive/tar"
	"bytes"
	"context"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compress"
)

// estargz / seekable reconstruction was previously untested even though
// estargz + compact-stream is a reachable production configuration. These cases
// assert the seekable re-compression path reconstructs the directly-built layer
// bit-for-bit.

func TestReconstructEstargzGzip(t *testing.T) {
	settings := tarSettings{compression: "gzip", estargz: true, compressionLevel: 6, compressorJobs: 1}
	content := []byte("estargz gzip payload that is reasonably long to compress")
	entries := []testEntry{
		{hdr: &tar.Header{Typeflag: tar.TypeDir, Name: "dir/", Mode: 0o755}},
		{hdr: &tar.Header{Typeflag: tar.TypeReg, Name: "dir/file.txt", Size: int64(len(content)), Mode: 0o644}, content: content},
		{hdr: &tar.Header{Typeflag: tar.TypeSymlink, Name: "dir/link", Linkname: "file.txt"}},
	}

	direct := buildTarDirect(t, entries, settings)
	reconstructed := buildIndexAndReconstruct(t, entries, settings, compactstream.StreamCompressionZstd, 0)
	if !bytes.Equal(direct, reconstructed) {
		t.Fatalf("estargz gzip mismatch: direct=%d bytes, reconstructed=%d bytes", len(direct), len(reconstructed))
	}
}

func TestReconstructEstargzZstd(t *testing.T) {
	settings := tarSettings{compression: "zstd", estargz: true, compressionLevel: 3, compressorJobs: 1}
	content := []byte("estargz zstd payload that is reasonably long to compress")
	entries := []testEntry{
		{hdr: &tar.Header{Typeflag: tar.TypeReg, Name: "file.bin", Size: int64(len(content)), Mode: 0o644}, content: content},
	}

	direct := buildTarDirect(t, entries, settings)
	reconstructed := buildIndexAndReconstruct(t, entries, settings, compactstream.StreamCompressionZstd, 0)
	if !bytes.Equal(direct, reconstructed) {
		t.Fatalf("estargz zstd mismatch: direct=%d bytes, reconstructed=%d bytes", len(direct), len(reconstructed))
	}
}

func TestReconstructHardlink(t *testing.T) {
	settings := tarSettings{compression: "gzip", compressionLevel: 6, compressorJobs: 1}
	content := []byte("shared content that a hardlink points to")
	entries := []testEntry{
		{hdr: &tar.Header{Typeflag: tar.TypeReg, Name: "file.txt", Size: int64(len(content)), Mode: 0o644}, content: content},
		{hdr: &tar.Header{Typeflag: tar.TypeLink, Name: "hardlink.txt", Linkname: "file.txt", Size: 0}},
	}

	direct := buildTarDirect(t, entries, settings)
	reconstructed := buildIndexAndReconstruct(t, entries, settings, compactstream.StreamCompressionZstd, 0)
	if !bytes.Equal(direct, reconstructed) {
		t.Fatalf("hardlink mismatch: direct=%d bytes, reconstructed=%d bytes", len(direct), len(reconstructed))
	}
}

// TestReconstructThroughRealCAS drives the real tarcas.CAS write path (with the
// compact stream observer attached, parent-directory synthesis enabled, and
// CASFirst deferral of dirs/symlinks) and asserts that reconstructing from the
// emitted compact stream reproduces the exact tar the CAS produced. The other
// reconstruction tests drive the observer directly, so this is the only Go-level
// test that catches a divergence between CaptureTarHeaderBytes and the bytes the
// CAS writer actually emits.
func TestReconstructThroughRealCAS(t *testing.T) {
	content1 := []byte("first file contents, long enough to be a CAS ref")
	content2 := []byte("second file, distinct contents to dedup separately")

	var tarBuf bytes.Buffer
	appender, err := compress.TarAppenderFactory("sha256", "gzip", false, &tarBuf,
		compress.CompressionLevel(6), compress.CompressorJobs(1))
	if err != nil {
		t.Fatal(err)
	}

	var idxBuf bytes.Buffer
	iw := compactstream.NewWriter(&idxBuf, compactstream.HashAlgoSHA256, 32, compactstream.StreamCompressionZstd,
		compactstream.OriginalCompressionInfo{
			Compression:      compactstream.OriginalCompressionGzip,
			CompressionLevel: 6,
			CompressorJobs:   1,
		}, 0)

	c := New[SHA256Helper](appender,
		WithCompactStreamWriter{Writer: iw},
		CreateParentDirectories(true),
	)

	// Regular files in a nested directory: parent dirs "a/" and "a/b/" are
	// synthesized inline; the files become CAS refs.
	if err := c.WriteRegular(&tar.Header{Typeflag: tar.TypeReg, Name: "a/b/file1.txt", Size: int64(len(content1)), Mode: 0o644}, bytes.NewReader(content1)); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteRegular(&tar.Header{Typeflag: tar.TypeReg, Name: "a/b/file2.txt", Size: int64(len(content2)), Mode: 0o644}, bytes.NewReader(content2)); err != nil {
		t.Fatal(err)
	}
	// An explicit directory and a symlink: deferred to Close under CASFirst.
	if err := c.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "c/", Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "a/link", Linkname: "b/file1.txt"}); err != nil {
		t.Fatal(err)
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := appender.Finalize(); err != nil {
		t.Fatal(err)
	}
	if err := iw.Close(); err != nil {
		t.Fatal(err)
	}

	// The CAS references content by its sha256, so a store of the file contents
	// is sufficient to reconstruct.
	store := newMemBlobStore()
	store.Store(content1)
	store.Store(content2)

	var reconstructed bytes.Buffer
	if err := compactstream.Reconstruct(context.Background(), &idxBuf, store, &reconstructed); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tarBuf.Bytes(), reconstructed.Bytes()) {
		t.Fatalf("real CAS reconstruction mismatch: real=%d bytes, reconstructed=%d bytes", tarBuf.Len(), reconstructed.Len())
	}
}
