package tarcas

import (
	"bytes"
	"crypto/sha256"
	"io/fs"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compress"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree/merkle"
)

type readCountingFS struct {
	fstest.MapFS
	bytesRead atomic.Int64
	opens     atomic.Int64
	dirReads  atomic.Int64
}

func (f *readCountingFS) ReadDir(name string) ([]fs.DirEntry, error) {
	f.dirReads.Add(1)
	return f.MapFS.ReadDir(name)
}

func (f *readCountingFS) Open(name string) (fs.File, error) {
	f.opens.Add(1)
	file, err := f.MapFS.Open(name)
	if err != nil {
		return nil, err
	}
	return readCountingFile{File: file, count: &f.bytesRead}, nil
}

type readCountingFile struct {
	fs.File
	count *atomic.Int64
}

func (f readCountingFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	f.count.Add(int64(n))
	return n, err
}

func TestStoreTreeReusesFileHashes(t *testing.T) {
	files := fstest.MapFS{
		"a/file": {Data: []byte("payload")},
		"copy":   {Data: []byte("payload")},
		"other":  {Data: []byte("different")},
	}
	hash, err := merkle.NewTreeHasher(files, sha256.New).Build()
	if err != nil {
		t.Fatal(err)
	}
	write := func(knownTreeHash bool) []byte {
		t.Helper()
		f := &readCountingFS{MapFS: files}
		var buf bytes.Buffer
		appender, err := compress.TarAppenderFactory("sha256", "none", false, &buf)
		if err != nil {
			t.Fatal(err)
		}
		c := New[SHA256Helper](appender, CreateParentDirectories(true))
		if knownTreeHash {
			_, err = c.StoreTreeKnownHash(f, "tree", hash)
		} else {
			_, err = c.StoreTree(f, "tree")
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		if !knownTreeHash {
			if got := f.dirReads.Load(); got != 2 {
				t.Errorf("listed directories %d times, want 2", got)
			}
			if got := f.opens.Load(); got != 3 {
				t.Errorf("opened files %d times, want 3", got)
			}
			// Read all three files once; archive writing reuses the cached bytes.
			if got, want := f.bytesRead.Load(), int64(2*len("payload")+len("different")); got != want {
				t.Errorf("read %d bytes, want %d", got, want)
			}
		}
		return buf.Bytes()
	}
	if !bytes.Equal(write(false), write(true)) {
		t.Fatal("reusing file hashes changed the archive")
	}
}
