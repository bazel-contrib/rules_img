package casdir

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func hexOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// newWriter returns a writer storing into <dir>/out/sha256, mirroring what
// CASDirProcess sets up.
func newWriter(t *testing.T, dir string, useSymlinks bool) *casWriter {
	t.Helper()
	shaDir := filepath.Join(dir, "out", "sha256")
	if err := os.MkdirAll(shaDir, 0o755); err != nil {
		t.Fatalf("creating store: %v", err)
	}
	return &casWriter{shaDir: shaDir, useSymlinks: useSymlinks}
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating dir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

func TestCASDirCopies(t *testing.T) {
	dir := t.TempDir()
	w := newWriter(t, dir, false)
	src := writeFile(t, filepath.Join(dir, "src", "file"), "content")

	if err := w.addPath(src); err != nil {
		t.Fatalf("addPath: %v", err)
	}

	dest := filepath.Join(w.shaDir, hexOf("content"))
	info, err := os.Lstat(dest)
	if err != nil {
		t.Fatalf("stat %s: %v", dest, err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("entry mode: got %v, want a regular file", info.Mode())
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading %s: %v", dest, err)
	}
	if string(got) != "content" {
		t.Errorf("stored content: got %q, want %q", got, "content")
	}
}

func TestCASDirSymlinksAreRelative(t *testing.T) {
	dir := t.TempDir()
	w := newWriter(t, dir, true)
	src := writeFile(t, filepath.Join(dir, "src", "file"), "content")

	if err := w.addPath(src); err != nil {
		t.Fatalf("addPath: %v", err)
	}

	dest := filepath.Join(w.shaDir, hexOf("content"))
	target, err := os.Readlink(dest)
	if err != nil {
		t.Fatalf("reading symlink %s: %v", dest, err)
	}
	if filepath.IsAbs(target) {
		t.Errorf("symlink target must be relative, got %q", target)
	}
	// The relative target must resolve to the original source file.
	if resolved := filepath.Join(filepath.Dir(dest), target); resolved != src {
		t.Errorf("resolved symlink: got %q, want %q", resolved, src)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading through symlink %s: %v", dest, err)
	}
	if string(got) != "content" {
		t.Errorf("content through symlink: got %q, want %q", got, "content")
	}
}

// Walking a directory must link every regular file it contains, and identical
// content must be stored (linked) only once.
func TestCASDirSymlinkDirectoryAndDedup(t *testing.T) {
	dir := t.TempDir()
	w := newWriter(t, dir, true)
	srcDir := filepath.Join(dir, "src")
	writeFile(t, filepath.Join(srcDir, "first"), "shared")
	writeFile(t, filepath.Join(srcDir, "nested", "second"), "shared")
	writeFile(t, filepath.Join(srcDir, "nested", "third"), "other")

	if err := w.addPath(srcDir); err != nil {
		t.Fatalf("addPath: %v", err)
	}

	entries, err := os.ReadDir(w.shaDir)
	if err != nil {
		t.Fatalf("reading store: %v", err)
	}
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("store entries: got %v, want the 2 distinct contents", names)
	}
	for _, content := range []string{"shared", "other"} {
		got, err := os.ReadFile(filepath.Join(w.shaDir, hexOf(content)))
		if err != nil {
			t.Fatalf("reading entry for %q: %v", content, err)
		}
		if string(got) != content {
			t.Errorf("entry for %q: got %q", content, got)
		}
	}
}
