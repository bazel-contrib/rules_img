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

// buildCompactStreamFileAt writes a compact stream to path that reconstructs to
// prefix + casBlob + suffix via a single CAS reference to casBlob. When
// embedDigest is true it records the reconstructed bytes' digest and size in the
// header (SetCompressedStreamInfo), which is what a bare `--layer=<path>` relies
// on to learn the layer digest. It returns the layer descriptor (digest = sha256
// of the reconstructed bytes; no re-compression) and the reconstructed bytes.
func buildCompactStreamFileAt(t *testing.T, path string, prefix, casBlob, suffix []byte, embedDigest bool) (api.Descriptor, []byte) {
	t.Helper()
	casDigest := sha256.Sum256(casBlob)
	reconstructed := append(append(append([]byte(nil), prefix...), casBlob...), suffix...)
	layerDigest := sha256.Sum256(reconstructed)

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
	if embedDigest {
		if err := w.SetCompressedStreamInfo(layerDigest[:], uint64(len(reconstructed))); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	writeBlobFile(t, path, buf.Bytes())

	return api.Descriptor{Digest: "sha256:" + hex.EncodeToString(layerDigest[:]), Size: int64(len(reconstructed))}, reconstructed
}

// shipDiskCacheBlob writes a content-addressed blob into a Bazel disk cache so
// that compact-stream reconstruction can resolve a CAS reference from it.
func shipDiskCacheBlob(t *testing.T, diskCache string, content []byte) {
	t.Helper()
	d := sha256.Sum256(content)
	writeBlobFile(t, diskCacheBlobPath(diskCache, "sha256:"+hex.EncodeToString(d[:])), content)
}

// TestWithLayerBareCompactStream verifies a bare `--layer=<path>.cstream` is
// detected, keyed by its embedded compressed digest, and reconstructs (resolving
// its CAS reference from the disk cache).
func TestWithLayerBareCompactStream(t *testing.T) {
	csPath := filepath.Join(t.TempDir(), "layer.cstream")
	casBlob := []byte("bare-input-content")
	desc, want := buildCompactStreamFileAt(t, csPath, []byte("A"), casBlob, []byte("Z"), true)

	diskCache := t.TempDir()
	shipDiskCacheBlob(t, diskCache, casBlob)

	b := NewBuilder(api.DeployManifest{}).WithDiskCache(diskCache).WithLayer(csPath)
	if b.layerSpecErr != nil {
		t.Fatalf("WithLayer: %v", b.layerSpecErr)
	}
	if got, ok := b.compactStreamLayers[desc.Digest]; !ok || got != csPath {
		t.Fatalf("compactStreamLayers[%s] = %q (ok=%v), want %q", desc.Digest, got, ok, csPath)
	}
	entry, err := b.layerFromExplicitCompactStream(desc)
	if err != nil {
		t.Fatalf("layerFromExplicitCompactStream: %v", err)
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

// TestWithLayerBareCompactStreamMissingEmbeddedDigest verifies that a bare path
// to a compact stream without an embedded compressed digest is rejected (there
// is no other way to learn which layer it reconstructs).
func TestWithLayerBareCompactStreamMissingEmbeddedDigest(t *testing.T) {
	csPath := filepath.Join(t.TempDir(), "layer.cstream")
	buildCompactStreamFileAt(t, csPath, []byte("A"), []byte("in"), []byte("Z"), false /* embedDigest */)

	_, err := NewBuilder(api.DeployManifest{}).WithLayer(csPath).Build()
	if err == nil || !strings.Contains(err.Error(), "does not embed a compressed digest") {
		t.Fatalf("expected embedded-digest error, got %v", err)
	}
}

// TestWithLayerCompactStreamDigestForm verifies the `digest=path` form for a
// compact stream: a matching digest reconstructs, and a mismatching digest is
// rejected against the stream's embedded compressed digest.
func TestWithLayerCompactStreamDigestForm(t *testing.T) {
	csPath := filepath.Join(t.TempDir(), "layer.cstream")
	casBlob := []byte("digest-form-input")
	desc, want := buildCompactStreamFileAt(t, csPath, []byte("A"), casBlob, []byte("Z"), true)

	diskCache := t.TempDir()
	shipDiskCacheBlob(t, diskCache, casBlob)

	b := NewBuilder(api.DeployManifest{}).WithDiskCache(diskCache).WithLayer(desc.Digest + "=" + csPath)
	if b.layerSpecErr != nil {
		t.Fatalf("WithLayer (matching digest): %v", b.layerSpecErr)
	}
	entry, err := b.layerFromExplicitCompactStream(desc)
	if err != nil {
		t.Fatalf("layerFromExplicitCompactStream: %v", err)
	}
	if got := readAllClose(t, mustOpen(t, entry)); !bytes.Equal(got, want) {
		t.Fatalf("reconstructed = %q, want %q", got, want)
	}

	wrong := "sha256:" + strings.Repeat("a", 64)
	_, err = NewBuilder(api.DeployManifest{}).WithLayer(wrong + "=" + csPath).Build()
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected digest-mismatch error, got %v", err)
	}
}

// TestLayerFromExplicitCompactStreamResolvesRefsFromRemoteCAS verifies that a
// `--layer` compact stream resolves its CAS references from the remote cache
// when no input directory or disk cache ships them.
func TestLayerFromExplicitCompactStreamResolvesRefsFromRemoteCAS(t *testing.T) {
	csPath := filepath.Join(t.TempDir(), "layer.cstream")
	casBlob := []byte("remote-input-content")
	desc, want := buildCompactStreamFileAt(t, csPath, []byte("<"), casBlob, []byte(">"), true)

	casDigest := sha256.Sum256(casBlob)
	remote := &stubCASReader{blobs: map[string][]byte{hex.EncodeToString(casDigest[:]): casBlob}}

	b := NewBuilder(api.DeployManifest{}).WithCASReader(remote).WithLayer(csPath)
	if b.layerSpecErr != nil {
		t.Fatalf("WithLayer: %v", b.layerSpecErr)
	}
	entry, err := b.layerFromExplicitCompactStream(desc)
	if err != nil {
		t.Fatalf("layerFromExplicitCompactStream: %v", err)
	}
	if got := readAllClose(t, mustOpen(t, entry)); !bytes.Equal(got, want) {
		t.Fatalf("reconstructed = %q, want %q", got, want)
	}
}

// TestWithLayerBareRawLayerHashesFile verifies a bare `--layer=<path>` to a raw
// (non-cstream) layer blob is hashed from disk to derive its digest and served
// verbatim via the explicit-layer source.
func TestWithLayerBareRawLayerHashesFile(t *testing.T) {
	blob := []byte("this is a raw compressed layer blob, not a compact stream")
	blobPath := filepath.Join(t.TempDir(), "layer.tar.gz")
	writeBlobFile(t, blobPath, blob)
	sum := sha256.Sum256(blob)
	desc := api.Descriptor{Digest: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(blob))}

	b := NewBuilder(api.DeployManifest{}).WithLayer(blobPath)
	if b.layerSpecErr != nil {
		t.Fatalf("WithLayer: %v", b.layerSpecErr)
	}
	entry, err := b.layerFromExplicit(desc)
	if err != nil {
		t.Fatalf("layerFromExplicit: %v", err)
	}
	if entry.Location != "file" {
		t.Errorf("Location = %q, want file", entry.Location)
	}
	if got := readAllClose(t, mustOpen(t, entry)); !bytes.Equal(got, blob) {
		t.Fatalf("blob = %q, want %q", got, blob)
	}
}

// TestWithLayerRawLayerDigestFormIsTrusted verifies the `digest=path` form for a
// raw layer registers under the supplied digest without hashing the file.
func TestWithLayerRawLayerDigestFormIsTrusted(t *testing.T) {
	blob := []byte("raw blob bytes")
	blobPath := filepath.Join(t.TempDir(), "layer.bin")
	writeBlobFile(t, blobPath, blob)

	// A digest that does NOT match the file's hash; digest=path is trusted as-is.
	desc := api.Descriptor{Digest: "sha256:" + strings.Repeat("b", 64), Size: int64(len(blob))}
	b := NewBuilder(api.DeployManifest{}).WithLayer(desc.Digest + "=" + blobPath)
	if b.layerSpecErr != nil {
		t.Fatalf("WithLayer: %v", b.layerSpecErr)
	}
	entry, err := b.layerFromExplicit(desc)
	if err != nil {
		t.Fatalf("layerFromExplicit: %v", err)
	}
	if got := readAllClose(t, mustOpen(t, entry)); !bytes.Equal(got, blob) {
		t.Fatalf("blob = %q, want %q", got, blob)
	}
}

// TestWithLayerOpenError verifies an unreadable --layer path surfaces at Build().
func TestWithLayerOpenError(t *testing.T) {
	if _, err := NewBuilder(api.DeployManifest{}).WithLayer(filepath.Join(t.TempDir(), "nope")).Build(); err == nil {
		t.Fatal("expected error for nonexistent --layer path")
	}
}

// TestLayerFromExplicitCompactStreamErrors covers the structured error kinds the
// source returns (it is aggregated by layerBlob, which requires *BlobSourceError).
func TestLayerFromExplicitCompactStreamErrors(t *testing.T) {
	desc := api.Descriptor{Digest: "sha256:" + strings.Repeat("a", 64)}
	var bse *BlobSourceError

	// Nothing configured -> unconfigured.
	if _, err := NewBuilder(api.DeployManifest{}).layerFromExplicitCompactStream(desc); !errors.As(err, &bse) || bse.Kind != BlobSourceUnconfigured {
		t.Fatalf("expected BlobSourceUnconfigured, got %v", err)
	}

	// Configured, but this digest is not registered -> blob missing.
	other := NewBuilder(api.DeployManifest{}).WithCompactStreamLayer("sha256:"+strings.Repeat("c", 64), "/some.cstream")
	if _, err := other.layerFromExplicitCompactStream(desc); !errors.As(err, &bse) || bse.Kind != BlobSourceBlobMissing {
		t.Fatalf("expected BlobSourceBlobMissing, got %v", err)
	}

	// Registered but the file is gone -> other.
	missing := NewBuilder(api.DeployManifest{}).WithCompactStreamLayer(desc.Digest, filepath.Join(t.TempDir(), "gone.cstream"))
	if _, err := missing.layerFromExplicitCompactStream(desc); !errors.As(err, &bse) || bse.Kind != BlobSourceOther {
		t.Fatalf("expected BlobSourceOther, got %v", err)
	}
}

// TestCloneCopiesCompactStreamLayers verifies Clone() deep-copies the
// compact-stream layer map so per-request worker clones do not leak into the base.
func TestCloneCopiesCompactStreamLayers(t *testing.T) {
	b := NewBuilder(api.DeployManifest{}).WithCompactStreamLayer("sha256:abc", "/p")
	clone := b.Clone()
	clone.WithCompactStreamLayer("sha256:def", "/q")
	if _, leaked := b.compactStreamLayers["sha256:def"]; leaked {
		t.Fatal("mutating clone leaked into the original compactStreamLayers")
	}
}
