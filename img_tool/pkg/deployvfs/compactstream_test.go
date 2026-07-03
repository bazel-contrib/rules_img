package deployvfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
)

type stubCASReader struct {
	blobs map[string][]byte // keyed by hex(digest.Hash)
}

func (s *stubCASReader) FindMissingBlobs(context.Context, []cas.Digest) ([]cas.Digest, error) {
	return nil, nil
}

func (s *stubCASReader) ReadBlob(_ context.Context, d cas.Digest) ([]byte, error) {
	b, ok := s.blobs[hex.EncodeToString(d.Hash)]
	if !ok {
		return nil, fmt.Errorf("blob not found")
	}
	return b, nil
}

func (s *stubCASReader) ReaderForBlob(_ context.Context, d cas.Digest) (io.ReadCloser, error) {
	b, ok := s.blobs[hex.EncodeToString(d.Hash)]
	if !ok {
		return nil, fmt.Errorf("blob not found")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func writeBlobFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readAllClose(t *testing.T, rc io.ReadCloser) []byte {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCasDirStoreInputDirHit(t *testing.T) {
	content := []byte("input dir blob content")
	d := sha256.Sum256(content)
	hexd := hex.EncodeToString(d[:])

	shaDir := filepath.Join(t.TempDir(), "sha256")
	writeBlobFile(t, filepath.Join(shaDir, hexd), content)

	s := &casDirStore{shaDir: shaDir}
	rc, err := s.ReaderForBlob(context.Background(), d[:], int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if got := readAllClose(t, rc); !bytes.Equal(got, content) {
		t.Fatalf("got %q, want %q", got, content)
	}
}

func TestCasDirStoreInputDirWinsOverDiskCache(t *testing.T) {
	content := []byte("the authoritative blob")
	d := sha256.Sum256(content)
	hexd := hex.EncodeToString(d[:])

	shaDir := filepath.Join(t.TempDir(), "sha256")
	writeBlobFile(t, filepath.Join(shaDir, hexd), content)

	diskCache := t.TempDir()
	// Different bytes (same length) in the disk cache, to prove the input dir wins.
	writeBlobFile(t, diskCacheBlobPath(diskCache, "sha256:"+hexd), bytes.Repeat([]byte("x"), len(content)))

	s := &casDirStore{shaDir: shaDir, diskCachePath: diskCache}
	rc, err := s.ReaderForBlob(context.Background(), d[:], int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if got := readAllClose(t, rc); !bytes.Equal(got, content) {
		t.Fatalf("input dir did not take priority: got %q", got)
	}
}

func TestCasDirStoreDiskCacheHit(t *testing.T) {
	content := []byte("disk cache blob content")
	d := sha256.Sum256(content)
	hexd := hex.EncodeToString(d[:])

	diskCache := t.TempDir()
	writeBlobFile(t, diskCacheBlobPath(diskCache, "sha256:"+hexd), content)

	s := &casDirStore{diskCachePath: diskCache}
	rc, err := s.ReaderForBlob(context.Background(), d[:], int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if got := readAllClose(t, rc); !bytes.Equal(got, content) {
		t.Fatalf("got %q, want %q", got, content)
	}
}

func TestCasDirStoreDiskCacheSizeMismatchFallsThrough(t *testing.T) {
	content := []byte("remote blob content")
	d := sha256.Sum256(content)
	hexd := hex.EncodeToString(d[:])

	diskCache := t.TempDir()
	// Disk cache file has the wrong size, so it must be skipped.
	writeBlobFile(t, diskCacheBlobPath(diskCache, "sha256:"+hexd), []byte("short"))

	remote := &stubCASReader{blobs: map[string][]byte{hexd: content}}
	s := &casDirStore{diskCachePath: diskCache, casReader: remote}
	rc, err := s.ReaderForBlob(context.Background(), d[:], int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if got := readAllClose(t, rc); !bytes.Equal(got, content) {
		t.Fatalf("expected remote blob after disk-cache size mismatch, got %q", got)
	}
}

func TestCasDirStoreNotFound(t *testing.T) {
	content := []byte("nonexistent blob")
	d := sha256.Sum256(content)
	s := &casDirStore{} // no sources configured
	if _, err := s.ReaderForBlob(context.Background(), d[:], int64(len(content))); err == nil {
		t.Fatal("expected a not-found error when no source resolves the blob")
	}
}

// buildOCILayoutCompactStream writes a compact stream to
// <layout>/blobs/sha256/<hex>.cstream that reconstructs to prefix + casBlob + suffix
// via a single CAS reference to casBlob. It returns the layer descriptor (whose digest
// is the sha256 of the reconstructed bytes) and the reconstructed bytes.
func buildOCILayoutCompactStream(t *testing.T, layout string, prefix, casBlob, suffix []byte) (api.Descriptor, []byte) {
	t.Helper()
	casDigest := sha256.Sum256(casBlob)

	var buf bytes.Buffer
	w := compactstream.NewWriter(&buf, compactstream.HashAlgoSHA256, 32, compactstream.StreamCompressionNone, compactstream.OriginalCompressionInfo{}, 0)
	if err := w.WriteStreamBytes(prefix); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteCASRef(casDigest[:], uint64(len(casBlob))); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteStreamBytes(suffix); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	reconstructed := append(append(append([]byte(nil), prefix...), casBlob...), suffix...)
	layerDigest := sha256.Sum256(reconstructed)
	layerHex := hex.EncodeToString(layerDigest[:])
	writeBlobFile(t, filepath.Join(layout, "blobs", "sha256", layerHex+".cstream"), buf.Bytes())

	return api.Descriptor{Digest: "sha256:" + layerHex, Size: int64(len(reconstructed))}, reconstructed
}

// shipCASBlob writes casBlob into the layout's content-addressed blobs directory at
// blobs/sha256/<hex-of-content>.
func shipCASBlob(t *testing.T, layout string, casBlob []byte) {
	t.Helper()
	d := sha256.Sum256(casBlob)
	writeBlobFile(t, filepath.Join(layout, "blobs", "sha256", hex.EncodeToString(d[:])), casBlob)
}

func mustOpen(t *testing.T, entry blobEntry) io.ReadCloser {
	t.Helper()
	rc, err := entry.Opener()
	if err != nil {
		t.Fatal(err)
	}
	return rc
}

// TestLayerFromOCILayoutCompactStreamResolvesFromLayoutBlobs verifies that a .cstream
// shipped inside an --oci-layout is discovered and that its CAS references resolve from
// the same layout's content-addressed blobs directory (blobs/sha256/<hex>).
func TestLayerFromOCILayoutCompactStreamResolvesFromLayoutBlobs(t *testing.T) {
	layout := t.TempDir()
	casBlob := []byte("referenced-input-file-content")
	desc, want := buildOCILayoutCompactStream(t, layout, []byte("HDR-"), casBlob, []byte("-END"))
	shipCASBlob(t, layout, casBlob)

	b := NewBuilder(api.DeployManifest{}).WithOCILayout(layout)
	entry, err := b.layerFromOCILayoutCompactStream(desc)
	if err != nil {
		t.Fatalf("layerFromOCILayoutCompactStream: %v", err)
	}
	if entry.Location != "compact_stream" {
		t.Errorf("Location = %q, want compact_stream", entry.Location)
	}
	if got := readAllClose(t, mustOpen(t, entry)); !bytes.Equal(got, want) {
		t.Fatalf("reconstructed = %q, want %q", got, want)
	}
	if n := b.stats.BlobsFromCompactStream.Load(); n != 1 {
		t.Errorf("BlobsFromCompactStream = %d, want 1", n)
	}
}

// TestLayerFromOCILayoutCompactStreamFallsBackToDiskCache verifies that when the layout
// does not ship a referenced CAS blob, reconstruction still resolves it via the Bazel
// disk cache (the casDirStore fallback).
func TestLayerFromOCILayoutCompactStreamFallsBackToDiskCache(t *testing.T) {
	layout := t.TempDir()
	casBlob := []byte("blob-only-in-disk-cache")
	desc, want := buildOCILayoutCompactStream(t, layout, []byte("A"), casBlob, []byte("Z"))

	// The referenced blob is absent from the layout; only the disk cache has it.
	diskCache := t.TempDir()
	casDigest := sha256.Sum256(casBlob)
	writeBlobFile(t, diskCacheBlobPath(diskCache, "sha256:"+hex.EncodeToString(casDigest[:])), casBlob)

	b := NewBuilder(api.DeployManifest{}).WithOCILayout(layout).WithDiskCache(diskCache)
	entry, err := b.layerFromOCILayoutCompactStream(desc)
	if err != nil {
		t.Fatalf("layerFromOCILayoutCompactStream: %v", err)
	}
	if got := readAllClose(t, mustOpen(t, entry)); !bytes.Equal(got, want) {
		t.Fatalf("reconstructed = %q, want %q", got, want)
	}
}

// TestLayerFromOCILayoutCompactStreamPrefersFirstMatch verifies the discovery scans
// layouts in order and picks the first one that ships the .cstream.
func TestLayerFromOCILayoutCompactStreamPrefersFirstMatch(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	casBlob := []byte("input")
	desc, want := buildOCILayoutCompactStream(t, first, []byte("<"), casBlob, []byte(">"))
	shipCASBlob(t, first, casBlob)

	// second is configured but empty; the stream must still be found in first.
	b := NewBuilder(api.DeployManifest{}).WithOCILayout(second).WithOCILayout(first)
	entry, err := b.layerFromOCILayoutCompactStream(desc)
	if err != nil {
		t.Fatalf("layerFromOCILayoutCompactStream: %v", err)
	}
	if got := readAllClose(t, mustOpen(t, entry)); !bytes.Equal(got, want) {
		t.Fatalf("reconstructed = %q, want %q", got, want)
	}
}

func TestLayerFromOCILayoutCompactStreamErrors(t *testing.T) {
	desc := api.Descriptor{Digest: "sha256:" + strings.Repeat("a", 64)}

	// No layouts configured -> unconfigured.
	var bse *BlobSourceError
	if _, err := NewBuilder(api.DeployManifest{}).layerFromOCILayoutCompactStream(desc); !errors.As(err, &bse) || bse.Kind != BlobSourceUnconfigured {
		t.Fatalf("expected BlobSourceUnconfigured, got %v", err)
	}

	// Layout configured but no matching .cstream -> blob missing.
	if _, err := NewBuilder(api.DeployManifest{}).WithOCILayout(t.TempDir()).layerFromOCILayoutCompactStream(desc); !errors.As(err, &bse) || bse.Kind != BlobSourceBlobMissing {
		t.Fatalf("expected BlobSourceBlobMissing, got %v", err)
	}
}

// TestBuilderContext pins the context-threading fix: an unset context defaults
// to Background, WithContext stores it, and Clone() carries it (the lazily
// reconstructing compact-stream opener captures Builder.context()).
func TestBuilderContext(t *testing.T) {
	b := NewBuilder(api.DeployManifest{})
	if b.context() != context.Background() {
		t.Error("unset Builder context should default to context.Background()")
	}

	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "value")
	b = b.WithContext(ctx)
	if b.context() != ctx {
		t.Error("WithContext did not store the context")
	}
	if b.Clone().context() != ctx {
		t.Error("Clone did not carry the context")
	}
}
