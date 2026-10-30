package casdirlayout

import (
	"os"
	"path/filepath"
	"testing"
)

const digest = "ab0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPathIsSharded(t *testing.T) {
	got := Path("/cas/sha256", digest)
	want := filepath.Join("/cas/sha256", "ab", digest)
	if got != want {
		t.Errorf("Path: got %q, want %q", got, want)
	}
}

func TestOpenAndStatFindShardedBlob(t *testing.T) {
	shaDir := t.TempDir()
	write(t, Path(shaDir, digest), "sharded")

	f, err := Open(shaDir, digest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	f.Close()
	if info, err := Stat(shaDir, digest); err != nil || info.Size() != int64(len("sharded")) {
		t.Errorf("Stat: got (%v, %v)", info, err)
	}
}

func TestOpenAndStatFallBackToFlatBlob(t *testing.T) {
	shaDir := t.TempDir()
	write(t, filepath.Join(shaDir, digest), "flat")

	f, err := Open(shaDir, digest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	f.Close()
	if info, err := Stat(shaDir, digest); err != nil || info.Size() != int64(len("flat")) {
		t.Errorf("Stat: got (%v, %v)", info, err)
	}
}

func TestMissingBlob(t *testing.T) {
	shaDir := t.TempDir()
	if _, err := Open(shaDir, digest); !os.IsNotExist(err) {
		t.Errorf("Open: got %v, want a not-exist error", err)
	}
	if _, err := Stat(shaDir, digest); !os.IsNotExist(err) {
		t.Errorf("Stat: got %v, want a not-exist error", err)
	}
}
