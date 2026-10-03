package treeartifact

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestReadDirFollowsSymlinks(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"inputs", "real", "real/directory"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "real/file"), []byte("contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"directory-link": filepath.Join(root, "real/directory"),
		"file-link":      "../real/file",
	} {
		if err := os.Symlink(target, filepath.Join(root, "inputs", name)); err != nil {
			t.Fatal(err)
		}
	}
	fsys := TreeArtifactFS(filepath.Join(root, "inputs"))
	entries, err := fsys.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	for i, name := range []string{"directory-link", "file-link"} {
		entry := entries[i]
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if entry.Name() != name || entry.IsDir() != (i == 0) || entry.Type()&fs.ModeSymlink != 0 || info.IsDir() != (i == 0) {
			t.Errorf("entry %q: name=%q type=%v info=%v", name, entry.Name(), entry.Type(), info.Mode())
		}
	}
	if contents, err := fs.ReadFile(fsys, "file-link"); err != nil || string(contents) != "contents" {
		t.Fatalf("reading file link: %q, %v", contents, err)
	}
	if err := os.Symlink("missing", filepath.Join(root, "inputs/broken")); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.ReadDir("."); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("broken link: got %v, want not-exist error", err)
	}
}
