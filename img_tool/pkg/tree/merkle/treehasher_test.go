package merkle

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

type countingFS struct {
	fstest.MapFS
	reads map[string]int
	mu    sync.Mutex
}

func (f *countingFS) ReadDir(name string) ([]fs.DirEntry, error) {
	f.mu.Lock()
	f.reads[name]++
	f.mu.Unlock()
	return f.MapFS.ReadDir(name)
}

func TestTreeHasherWalk(t *testing.T) {
	f := &countingFS{
		MapFS: fstest.MapFS{
			"a/file": {Data: []byte("nested")},
			"b":      {Data: []byte("root")},
		},
		reads: make(map[string]int),
	}
	fileNode := func(name string) FileNode {
		info, err := f.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(f.MapFS[name].Data)
		return DefaultFileNode(digest[:], info)
	}
	subdir := Directory{Files: []FileNode{fileNode("a/file")}}
	root := Directory{
		Files:       []FileNode{fileNode("b")},
		Directories: []DirectoryNode{{Name: "a", Hash: subdir.Hash(sha256.New())}},
	}
	hasher := NewTreeHasher(f, sha256.New)
	for range 2 {
		clear(f.reads)
		got, err := hasher.Build()
		if err != nil {
			t.Fatal(err)
		}
		if want := root.Hash(sha256.New()); !bytes.Equal(got, want) {
			t.Fatalf("hash = %x, want %x", got, want)
		}
		for _, dir := range []string{".", "a"} {
			if f.reads[dir] != 1 {
				t.Errorf("ReadDir(%q) called %d times, want 1", dir, f.reads[dir])
			}
		}
	}
}

func TestTreeHasherEmptyDirectories(t *testing.T) {
	if _, err := NewTreeHasher(fstest.MapFS{}, sha256.New).Build(); err != nil {
		t.Fatalf("empty root: %v", err)
	}
	f := fstest.MapFS{"empty": {Mode: fs.ModeDir}}
	if _, err := NewTreeHasher(f, sha256.New).Build(); err == nil || !strings.Contains(err.Error(), "empty directory empty") {
		t.Fatalf("empty child directory: %v", err)
	}
}

func BenchmarkTreeHasher(b *testing.B) {
	f := make(fstest.MapFS, 1000)
	for i := range 1000 {
		f[fmt.Sprintf("file%04d", i)] = &fstest.MapFile{Data: bytes.Repeat([]byte("payload"), 10)}
	}
	hasher := NewTreeHasher(f, sha256.New)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := hasher.Build(); err != nil {
			b.Fatal(err)
		}
	}
}
