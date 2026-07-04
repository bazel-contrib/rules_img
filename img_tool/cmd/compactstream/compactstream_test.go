package compactstreamcmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
)

func TestDirStoreResolvesByDigest(t *testing.T) {
	dir := t.TempDir()
	shaDir := filepath.Join(dir, "sha256")
	if err := os.MkdirAll(shaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("blob contents")
	sum := sha256.Sum256(content)
	if err := os.WriteFile(filepath.Join(shaDir, hex.EncodeToString(sum[:])), content, 0o644); err != nil {
		t.Fatal(err)
	}

	store := &dirStore{shaDir: shaDir}

	rc, err := store.ReaderForBlob(context.Background(), sum[:], int64(len(content)))
	if err != nil {
		t.Fatalf("ReaderForBlob: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Errorf("got %q, want %q", got, content)
	}

	absent := sha256.Sum256([]byte("absent"))
	if _, err := store.ReaderForBlob(context.Background(), absent[:], 6); err == nil {
		t.Error("expected error for a digest not present in the content-addressed directory")
	}
}

// buildTestIndex creates a small index with two CAS references separated by
// inline stream segments (mimicking tar headers around file content). When
// compressedDigest is non-nil, the optional compressed-stream digest and size
// are recorded in the header.
func buildTestIndex(t *testing.T, d1, d2 [32]byte, compressedDigest []byte, compressedSize uint64) []byte {
	t.Helper()
	var buf bytes.Buffer
	iw := compactstream.NewWriter(&buf, compactstream.HashAlgoSHA256, 32, compactstream.StreamCompressionZstd,
		compactstream.OriginalCompressionInfo{
			Compression:      compactstream.OriginalCompressionGzip,
			CompressionLevel: -1,
			CompressorJobs:   1,
		}, 0)
	for _, step := range []struct {
		stream int
		digest []byte
		size   uint64
	}{
		{stream: 512},
		{digest: d1[:], size: 4096},
		{stream: 512},
		{digest: d2[:], size: 100},
	} {
		if step.stream > 0 {
			if err := iw.WriteStreamBytes(make([]byte, step.stream)); err != nil {
				t.Fatal(err)
			}
		}
		if step.digest != nil {
			if err := iw.WriteCASRef(step.digest, step.size); err != nil {
				t.Fatal(err)
			}
		}
	}
	if compressedDigest != nil {
		if err := iw.SetCompressedStreamInfo(compressedDigest, compressedSize); err != nil {
			t.Fatal(err)
		}
	}
	if err := iw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestWriteListing(t *testing.T) {
	d1 := sha256.Sum256([]byte("one"))
	d2 := sha256.Sum256([]byte("two"))
	info, err := compactstream.Inspect(bytes.NewReader(buildTestIndex(t, d1, d2, nil, 0)))
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	writeListing(&out, "test.cstream", info, 600)
	got := out.String()

	want := []string{
		"compact stream: test.cstream",
		"Header",
		"Hash algorithm:",
		"sha256 (32-byte digests)",
		"Stream compression:",
		"Layer compression:",
		"gzip",
		"Contents",
		"512 bytes of stream data",
		"cas reference sha256:" + hex.EncodeToString(d1[:]) + " 4096 bytes",
		"cas reference sha256:" + hex.EncodeToString(d2[:]) + " 100 bytes",
		"Statistics",
		"CAS references:",
		"Reconstructed tar (uncompressed):",
		"unknown (not recorded in the compact stream)",
		"Efficiency",
		"Compact stream vs reconstructed tar (uncompressed):",
	}
	for _, sub := range want {
		if !strings.Contains(got, sub) {
			t.Errorf("listing missing %q\n--- full output ---\n%s", sub, got)
		}
	}

	// Referenced content (4096 + 100) must be reported, and the compressed-layer
	// efficiency line must be absent when the index records no compressed size.
	if info.ReferencedBytes() != 4196 {
		t.Errorf("referenced bytes = %d, want 4196", info.ReferencedBytes())
	}
	if strings.Contains(got, "Compact stream vs reconstructed layer (compressed):") {
		t.Errorf("did not expect a compressed-layer efficiency line without compressed-stream info:\n%s", got)
	}
}

func TestWriteListingWithCompressedSize(t *testing.T) {
	d1 := sha256.Sum256([]byte("one"))
	d2 := sha256.Sum256([]byte("two"))
	compressedDigest := sha256.Sum256([]byte("compressed layer"))
	info, err := compactstream.Inspect(bytes.NewReader(buildTestIndex(t, d1, d2, compressedDigest[:], 1234)))
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	writeListing(&out, "test.cstream", info, 600)
	got := out.String()

	for _, sub := range []string{
		"Compressed stream digest:",
		"sha256:" + hex.EncodeToString(compressedDigest[:]),
		"Reconstructed layer (gzip):",
		"1234 bytes",
		"Compact stream vs reconstructed layer (compressed):",
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("listing missing %q\n--- full output ---\n%s", sub, got)
		}
	}
}
