package erofs_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	erofs "github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs"
)

// TestChunkBasedXattrAlignment guards against the chunk-index map being written
// unaligned after the xattr area on a chunk-based (external-device) inode.
//
// The kernel and this package's reader locate the chunk-index map at
// ALIGN(inode_isize + xattr_isize, sizeof(chunk_index)). inode_isize is a
// multiple of 8, so a xattr area sized 4 (mod 8) pushes the map off alignment.
// The "user.t" = "abc" attribute makes xattr_isize == 20, so inode_isize +
// xattr_isize is 4 (mod 8) for both compact (32) and extended (64) inodes;
// before the fix the chunk index was written 4 bytes early and the file
// resolved to the wrong device block.
func TestChunkBasedXattrAlignment(t *testing.T) {
	dataPath := filepath.Join(t.TempDir(), "data.bin")
	df, err := os.Create(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = df.Close() }()

	var metaBuf testBuffer
	fsys := erofs.Create(&metaBuf, erofs.WithDataFile(df))

	f, err := fsys.Create("/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte{0xAB}, 4096) // exactly one chunk
	if _, err := f.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Setxattr("/file.bin", "user.t", "abc"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	dfRead, err := os.Open(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dfRead.Close() }()

	efs, err := erofs.Open(bytes.NewReader(metaBuf.Bytes()), erofs.WithExtraDevices(dfRead))
	if err != nil {
		t.Fatal("Open:", err)
	}

	got, err := fs.ReadFile(efs, "file.bin")
	if err != nil {
		t.Fatal("ReadFile:", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("file data mismatch: chunk-index map misaligned after xattr area (got %d bytes, first=0x%02x)", len(got), firstByte(got))
	}

	// The xattr itself should round-trip.
	fi, err := fs.Stat(efs, "file.bin")
	if err != nil {
		t.Fatal("Stat:", err)
	}
	xg, ok := fi.(interface {
		GetXattr(string) (string, bool)
	})
	if !ok {
		t.Fatal("FileInfo does not expose GetXattr")
	}
	if v, ok := xg.GetXattr("user.t"); !ok || v != "abc" {
		t.Fatalf("user.t = %q (ok=%v), want %q", v, ok, "abc")
	}
}

func firstByte(b []byte) byte {
	if len(b) == 0 {
		return 0
	}
	return b[0]
}
