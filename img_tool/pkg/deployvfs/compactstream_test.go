package deployvfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
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
