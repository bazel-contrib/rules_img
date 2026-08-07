package erofs_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	erofs "github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs/internal/builder"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs/internal/disk"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs/internal/erofstest"
)

// TestCreateFSSpool exercises spool mode: CreateFS without a data file.
func TestCreateFSSpool(t *testing.T) {
	var buf testBuffer
	fsys := erofs.Create(&buf)

	// Create a regular file.
	f, err := fsys.Create("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("hello world\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// Create a directory and a file inside it.
	if err := fsys.Mkdir("/subdir", 0o755); err != nil {
		t.Fatal(err)
	}
	f2, err := fsys.Create("/subdir/nested.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f2.Write([]byte("nested\n")); err != nil {
		t.Fatal(err)
	}
	if err := f2.Close(); err != nil {
		t.Fatal(err)
	}

	// Create a symlink.
	if err := fsys.Symlink("hello.txt", "/link"); err != nil {
		t.Fatal(err)
	}

	// Create an empty file.
	f3, err := fsys.Create("/empty")
	if err != nil {
		t.Fatal(err)
	}
	if err := f3.Close(); err != nil {
		t.Fatal(err)
	}

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())

	// Read back the image.
	erofstest.FsckErofsBytes(t, buf.Bytes())
	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	erofstest.CheckFile(t, efs, "hello.txt", "hello world\n")
	erofstest.CheckFile(t, efs, "subdir/nested.txt", "nested\n")
	erofstest.CheckFile(t, efs, "empty", "")
	erofstest.CheckSymlink(t, efs, "link", "hello.txt")
	erofstest.CheckDirEntries(t, efs, ".", []string{"empty", "hello.txt", "link", "subdir"})
	erofstest.CheckDirEntries(t, efs, "subdir", []string{"nested.txt"})
}

// TestCreateFSDataFile exercises data file mode (metadata-only).
func TestCreateFSDataFile(t *testing.T) {
	dataPath := filepath.Join(t.TempDir(), "data.bin")
	df, err := os.Create(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = df.Close() }()

	var metaBuf testBuffer
	fsys := erofs.Create(&metaBuf, erofs.WithDataFile(df))

	f, err := fsys.Create("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("data file mode\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	f2, err := fsys.Create("/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("ABCDEFGH"), 1024) // 8KB
	if _, err := f2.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := f2.Close(); err != nil {
		t.Fatal(err)
	}

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	// Re-open data file as ReaderAt for verification.
	dfRead, err := os.Open(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dfRead.Close() }()

	efs, err := erofs.Open(bytes.NewReader(metaBuf.Bytes()), erofs.WithExtraDevices(dfRead))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	erofstest.CheckFile(t, efs, "hello.txt", "data file mode\n")
	erofstest.CheckFileBytes(t, efs, "big.bin", data)
}

// TestCreateFSMetadata verifies Chmod, Chown, Setxattr, SetMtime.
func TestCreateFSMetadata(t *testing.T) {
	var buf testBuffer
	fsys := erofs.Create(&buf)

	f, err := fsys.Create("/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("content\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Chmod(0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.Chown(1000, 2000); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := fsys.Setxattr("/file.txt", "user.test", "value123"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Chtimes("/file.txt", time.Time{}, time.Unix(1700000000, 123456789)); err != nil {
		t.Fatal(err)
	}

	if err := fsys.Mkdir("/mydir", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Chmod("/mydir", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Chown("/mydir", 500, 600); err != nil {
		t.Fatal(err)
	}

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())
	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	// Check file metadata.
	st := erofstest.Stat(t, efs, "file.txt")
	if st.Mode.Perm() != 0o755 {
		t.Errorf("file perm: got %o, want 755", st.Mode.Perm())
	}
	if st.UID != 1000 || st.GID != 2000 {
		t.Errorf("file uid/gid: got %d/%d, want 1000/2000", st.UID, st.GID)
	}
	if st.Mtime != 1700000000 {
		t.Errorf("file mtime: got %d, want 1700000000", st.Mtime)
	}
	if st.MtimeNs != 123456789 {
		t.Errorf("file mtimeNs: got %d, want 123456789", st.MtimeNs)
	}
	erofstest.CheckXattrs(t, efs, "file.txt", map[string]string{"user.test": "value123"})

	// Check dir metadata.
	dst := erofstest.Stat(t, efs, "mydir")
	if dst.Mode.Perm() != 0o700 {
		t.Errorf("dir perm: got %o, want 700", dst.Mode.Perm())
	}
	if dst.UID != 500 || dst.GID != 600 {
		t.Errorf("dir uid/gid: got %d/%d, want 500/600", dst.UID, dst.GID)
	}
}

// TestCreateFSMknod verifies char and block device creation.
func TestCreateFSMknod(t *testing.T) {
	var buf testBuffer
	fsys := erofs.Create(&buf)

	if err := fsys.Mknod("/null", disk.StatTypeChrdev|0o666, 1<<8|3); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Mknod("/sda", disk.StatTypeBlkdev|0o660, 8<<8); err != nil { //nolint:staticcheck // minor=0
		t.Fatal(err)
	}

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())
	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	erofstest.CheckDevice(t, efs, "null", fs.ModeDevice|fs.ModeCharDevice, 1<<8|3)
	erofstest.CheckDevice(t, efs, "sda", fs.ModeDevice, 8<<8) //nolint:staticcheck // minor=0
}

// TestCreateFSLargeFile tests a file that spans many blocks and exercises
// the Chunk.Count uint16 split for files > 65535 blocks.
func TestCreateFSLargeFile(t *testing.T) {
	var buf testBuffer
	fsys := erofs.Create(&buf)

	// 128KB file — enough to span multiple blocks but not absurdly large.
	data := make([]byte, 128*1024)
	for i := range data {
		data[i] = byte(i % 251)
	}

	f, err := fsys.Create("/large.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())
	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	erofstest.CheckFileBytes(t, efs, "large.bin", data)
}

// TestCreateFSLargeFileDataFile tests a large file with data file mode,
// including chunk splitting for files > 65535 blocks.
func TestCreateFSLargeFileDataFile(t *testing.T) {
	dataPath := filepath.Join(t.TempDir(), "data.bin")
	df, err := os.Create(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = df.Close() }()

	var metaBuf testBuffer
	fsys := erofs.Create(&metaBuf, erofs.WithDataFile(df))

	// 128KB file.
	data := make([]byte, 128*1024)
	for i := range data {
		data[i] = byte(i % 251)
	}

	f, err := fsys.Create("/large.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
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
		t.Fatal("EroFS:", err)
	}

	erofstest.CheckFileBytes(t, efs, "large.bin", data)
}

// TestCreateFSErrors tests error cases.
func TestCreateFSErrors(t *testing.T) {
	t.Run("duplicate path", func(t *testing.T) {
		var buf testBuffer
		fsys := erofs.Create(&buf)
		f, err := fsys.Create("/dup.txt")
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		_, err = fsys.Create("/dup.txt")
		if err == nil {
			t.Fatal("expected error for duplicate path")
		}
	})

	t.Run("write after close", func(t *testing.T) {
		var buf testBuffer
		fsys := erofs.Create(&buf)
		f, err := fsys.Create("/file.txt")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("data")); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}

		_, err = f.Write([]byte("more"))
		if err == nil {
			t.Fatal("expected error writing to closed file")
		}
	})

	t.Run("file double close", func(t *testing.T) {
		var buf testBuffer
		fsys := erofs.Create(&buf)
		f, err := fsys.Create("/file.txt")
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		err = f.Close()
		if err == nil {
			t.Fatal("expected error on double close")
		}
	})

	t.Run("FS double close", func(t *testing.T) {
		var buf testBuffer
		fsys := erofs.Create(&buf)
		if err := fsys.Close(); err != nil {
			t.Fatal(err)
		}
		err := fsys.Close()
		if err == nil {
			t.Fatal("expected error on FS double close")
		}
	})

	t.Run("create after FS close", func(t *testing.T) {
		var buf testBuffer
		fsys := erofs.Create(&buf)
		if err := fsys.Close(); err != nil {
			t.Fatal(err)
		}
		_, err := fsys.Create("/file.txt")
		if err == nil {
			t.Fatal("expected error creating after FS close")
		}
	})
}

// TestCreateFSImplicitDirs verifies that parent directories are created
// implicitly when creating deeply nested files.
func TestCreateFSImplicitDirs(t *testing.T) {
	var buf testBuffer
	fsys := erofs.Create(&buf)

	f, err := fsys.Create("/a/b/c/deep.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("deep\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())
	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	erofstest.CheckFile(t, efs, "a/b/c/deep.txt", "deep\n")

	// Verify implicit directories exist.
	for _, dir := range []string{"a", "a/b", "a/b/c"} {
		fi, err := fs.Stat(efs, dir)
		if err != nil {
			t.Errorf("stat %s: %v", dir, err)
			continue
		}
		if !fi.IsDir() {
			t.Errorf("%s: not a directory", dir)
		}
	}
}

// TestCreateFSSpecialModeBits verifies that setuid, setgid and sticky bits
// written by the Writer are reported by the reader's fs.FileInfo.Mode, and
// that plain entries come back as well-formed fs.FileMode values with no raw
// on-disk bits left in them.
func TestCreateFSSpecialModeBits(t *testing.T) {
	var buf testBuffer
	fsys := erofs.Create(&buf)

	for _, f := range []struct {
		name string
		mode fs.FileMode
	}{
		{"/su", 0755 | fs.ModeSetuid},
		{"/wall", 0755 | fs.ModeSetgid},
		{"/all-bits", 0700 | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky},
		{"/plain", 0644},
	} {
		fh, err := fsys.Create(f.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fh.Write([]byte(f.name)); err != nil {
			t.Fatal(err)
		}
		if err := fh.Close(); err != nil {
			t.Fatal(err)
		}
		if err := fsys.Chmod(f.name, f.mode); err != nil {
			t.Fatal(err)
		}
	}

	if err := fsys.Mkdir("/tmp", 0777|fs.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Mkdir("/var", 0775|fs.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Symlink("/su", "/su-link"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Mknod("/null", disk.StatTypeChrdev|0666, 0x0103); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())
	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	erofstest.CheckMode(t, efs, "su", 0755|fs.ModeSetuid)
	erofstest.CheckMode(t, efs, "wall", 0755|fs.ModeSetgid)
	erofstest.CheckMode(t, efs, "all-bits", 0700|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)
	erofstest.CheckMode(t, efs, "plain", 0644)
	erofstest.CheckMode(t, efs, "tmp", 0777|fs.ModeDir|fs.ModeSticky)
	erofstest.CheckMode(t, efs, "var", 0775|fs.ModeDir|fs.ModeSetgid)
	erofstest.CheckMode(t, efs, "su-link", 0777|fs.ModeSymlink)
	erofstest.CheckMode(t, efs, "null", 0666|fs.ModeDevice|fs.ModeCharDevice)

	// Writer and reader must agree on the mode they report for an entry.
	wfi, err := fsys.Stat("/su")
	if err != nil {
		t.Fatal(err)
	}
	rfi, err := fs.Stat(efs, "su")
	if err != nil {
		t.Fatal(err)
	}
	if wfi.Mode() != rfi.Mode() {
		t.Errorf("su: writer mode %v (%#o), reader mode %v (%#o)",
			wfi.Mode(), uint32(wfi.Mode()), rfi.Mode(), uint32(rfi.Mode()))
	}
}

// TestCreateFSEmpty verifies that an empty FS produces a valid image.
func TestCreateFSEmpty(t *testing.T) {
	var buf testBuffer
	fsys := erofs.Create(&buf)
	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())
	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	entries, err := fs.ReadDir(efs, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty root dir, got %d entries", len(entries))
	}
}

// TestCreateFSBlockSize verifies WithBlockSize produces valid images
// with different block sizes and that invalid sizes are rejected.
func TestCreateFSBlockSize(t *testing.T) {
	for _, bs := range []int{512, 1024, 4096, 65536} {
		t.Run(fmt.Sprintf("%d", bs), func(t *testing.T) {
			var buf testBuffer
			fsys := erofs.Create(&buf, erofs.WithBlockSize(bs))

			f, err := fsys.Create("/file.txt")
			if err != nil {
				t.Fatal(err)
			}
			// Write enough data to span multiple blocks at the smallest size.
			data := bytes.Repeat([]byte("X"), bs+1)
			if _, err := f.Write(data); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}

			if err := fsys.Mkdir("/dir", 0o755); err != nil {
				t.Fatal(err)
			}

			if err := fsys.Close(); err != nil {
				t.Fatal("Close:", err)
			}

			// fsck.erofs mmaps the superblock and refuses block sizes
			// larger than the host's page size, so skip it in that case.
			// The library itself produces a valid image regardless; the
			// remaining checks below verify it independently.
			if bs <= os.Getpagesize() {
				erofstest.FsckErofsBytes(t, buf.Bytes())
			}

			// Verify BlkSizeBits in the superblock.
			var sb disk.SuperBlock
			r := bytes.NewReader(buf.Bytes()[disk.SuperBlockOffset:])
			if err := binary.Read(r, binary.LittleEndian, &sb); err != nil {
				t.Fatal("decode superblock:", err)
			}
			if got := 1 << sb.BlkSizeBits; got != bs {
				t.Errorf("superblock block size = %d, want %d", got, bs)
			}

			efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatal("Open:", err)
			}

			erofstest.CheckFileBytes(t, efs, "file.txt", data)
		})
	}

	// Invalid sizes must surface from the first Writer operation, not be
	// deferred until Close — otherwise a data file in WithDataFile mode
	// could be partially written before the error fires.
	for _, c := range []struct {
		name string
		size int
	}{
		{"not-power-of-two", 1000},
		{"too-small", 256},
		{"too-large", 1 << 17},
	} {
		t.Run("invalid/"+c.name, func(t *testing.T) {
			var buf testBuffer
			fsys := erofs.Create(&buf, erofs.WithBlockSize(c.size))
			if _, err := fsys.Create("/file.txt"); err == nil {
				t.Errorf("Create: expected error for invalid block size %d", c.size)
			}
			if err := fsys.Mkdir("/dir", 0o755); err == nil {
				t.Errorf("Mkdir: expected error for invalid block size %d", c.size)
			}
			if err := fsys.Close(); err == nil {
				t.Errorf("Close: expected error for invalid block size %d", c.size)
			}
		})
	}

	// Data-file mode exercises the padding/chunk-index paths in
	// File.closeDataFile, which use resolveBlockSize and so are sensitive
	// to a non-default block size.
	t.Run("dataFile/1024", func(t *testing.T) {
		const bs = 1024
		dataPath := filepath.Join(t.TempDir(), "data.bin")
		df, err := os.Create(dataPath)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = df.Close() }()

		var metaBuf testBuffer
		fsys := erofs.Create(&metaBuf, erofs.WithBlockSize(bs), erofs.WithDataFile(df))

		// Span multiple blocks so chunk indexes and padding both run.
		data := bytes.Repeat([]byte("Y"), bs*3+17)
		f, err := fsys.Create("/big.bin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}

		// Second file, smaller than a block, to verify padding around it.
		small := []byte("hi\n")
		f2, err := fsys.Create("/small.txt")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f2.Write(small); err != nil {
			t.Fatal(err)
		}
		if err := f2.Close(); err != nil {
			t.Fatal(err)
		}

		if err := fsys.Close(); err != nil {
			t.Fatal("Close:", err)
		}

		// Data file must be padded to a block boundary.
		fi, err := os.Stat(dataPath)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size()%int64(bs) != 0 {
			t.Errorf("data file size %d is not a multiple of block size %d", fi.Size(), bs)
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
		erofstest.CheckFileBytes(t, efs, "big.bin", data)
		erofstest.CheckFileBytes(t, efs, "small.txt", small)
	})
}

// TestCreateFSSetNlink verifies that SetNlink overrides computed nlink.
func TestCreateFSSetNlink(t *testing.T) {
	var buf testBuffer
	fsys := erofs.Create(&buf)

	f, err := fsys.Create("/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := fsys.SetNlink("/file.txt", 42); err != nil {
		t.Fatal(err)
	}

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())
	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	st := erofstest.Stat(t, efs, "file.txt")
	if st.Nlink != 42 {
		t.Errorf("nlink: got %d, want 42", st.Nlink)
	}
}

// TestCreateFSDirNlink verifies that directory nlink = 2 + child_dir_count.
func TestCreateFSDirNlink(t *testing.T) {
	var buf testBuffer
	fsys := erofs.Create(&buf)

	if err := fsys.Mkdir("/parent", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Mkdir("/parent/child1", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Mkdir("/parent/child2", 0o755); err != nil {
		t.Fatal(err)
	}

	f, err := fsys.Create("/parent/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())
	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	st := erofstest.Stat(t, efs, "parent")
	// nlink should be 2 (self + parent) + 2 (child dirs) = 4
	if st.Nlink != 4 {
		t.Errorf("parent nlink: got %d, want 4", st.Nlink)
	}
}

// TestCreateFSWithTempDir verifies that WithTempDir is respected.
func TestCreateFSWithTempDir(t *testing.T) {
	tmpDir := t.TempDir()
	var buf testBuffer
	fsys := erofs.Create(&buf, erofs.WithTempDir(tmpDir))

	f, err := fsys.Create("/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())
	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	erofstest.CheckFile(t, efs, "file.txt", "hello")
}

// TestCreateFSRootMetadata verifies that Mkdir("/") sets root permissions
// and path-based methods can set metadata on it.
func TestCreateFSRootMetadata(t *testing.T) {
	var buf testBuffer
	fsys := erofs.Create(&buf)

	if err := fsys.Mkdir("/", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Chown("/", 1000, 2000); err != nil {
		t.Fatal(err)
	}

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())
	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	st := erofstest.Stat(t, efs, ".")
	if st.Mode.Perm() != 0o700 {
		t.Errorf("root perm: got %o, want 700", st.Mode.Perm())
	}
	if st.UID != 1000 || st.GID != 2000 {
		t.Errorf("root uid/gid: got %d/%d, want 1000/2000", st.UID, st.GID)
	}
}

// TestCreateFSMultipleFiles tests creating many files to exercise the
// spool and verify ordering.
func TestCreateFSMultipleFiles(t *testing.T) {
	var buf testBuffer
	fsys := erofs.Create(&buf)

	for i := range 100 {
		f, err := fsys.Create(fmt.Sprintf("/file%03d.txt", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintf(f, "content %d\n", i); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())
	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	for i := range 100 {
		name := fmt.Sprintf("file%03d.txt", i)
		expected := fmt.Sprintf("content %d\n", i)
		erofstest.CheckFile(t, efs, name, expected)
	}

	// Verify directory has all 100 entries.
	entries, err := fs.ReadDir(efs, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 100 {
		t.Errorf("got %d entries, want 100", len(entries))
	}
}

// TestWriterOpen tests Open and Read for regular files in spool mode.
func TestWriterOpen(t *testing.T) {
	var buf testBuffer
	fsys := erofs.Create(&buf)

	f, err := fsys.Create("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("hello world\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// Open and read back.
	rf, err := fsys.Open("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rf.Close() }()

	got, err := io.ReadAll(rf)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello world\n" {
		t.Errorf("got %q, want %q", got, "hello world\n")
	}
}

// TestWriterOpenDataFile tests Open and Read for regular files in data file mode.
func TestWriterOpenDataFile(t *testing.T) {
	dataPath := filepath.Join(t.TempDir(), "data.bin")
	df, err := os.Create(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = df.Close() }()

	var metaBuf testBuffer
	fsys := erofs.Create(&metaBuf, erofs.WithDataFile(df))

	f, err := fsys.Create("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("data file content\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	rf, err := fsys.Open("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rf.Close() }()

	got, err := io.ReadAll(rf)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "data file content\n" {
		t.Errorf("got %q, want %q", got, "data file content\n")
	}
}

// TestWriterOpenEmpty tests Open on an empty file.
func TestWriterOpenEmpty(t *testing.T) {
	var buf testBuffer
	fsys := erofs.Create(&buf)

	f, err := fsys.Create("/empty")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	rf, err := fsys.Open("/empty")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rf.Close() }()

	got, err := io.ReadAll(rf)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %d bytes", len(got))
	}
}

// TestWriterStat tests Stat for various entry types.
func TestWriterStat(t *testing.T) {
	var buf testBuffer
	fsys := erofs.Create(&buf)

	f, err := fsys.Create("/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("content\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Chmod(0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := fsys.Chtimes("/file.txt", time.Time{}, time.Unix(1700000000, 123456789)); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Mkdir("/dir", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Symlink("file.txt", "/link"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Mknod("/null", disk.StatTypeChrdev|0o666, 1<<8|3); err != nil {
		t.Fatal(err)
	}

	// Stat regular file.
	fi, err := fsys.Stat("/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Name() != "file.txt" {
		t.Errorf("name: got %q, want %q", fi.Name(), "file.txt")
	}
	if fi.Size() != 8 {
		t.Errorf("size: got %d, want 8", fi.Size())
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("mode: got %o, want 755", fi.Mode().Perm())
	}
	if !fi.Mode().IsRegular() {
		t.Errorf("expected regular file mode")
	}
	if fi.ModTime() != time.Unix(1700000000, 123456789) {
		t.Errorf("modtime: got %v, want %v", fi.ModTime(), time.Unix(1700000000, 123456789))
	}

	// Stat directory.
	di, err := fsys.Stat("/dir")
	if err != nil {
		t.Fatal(err)
	}
	if !di.IsDir() {
		t.Error("expected directory")
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode: got %o, want 700", di.Mode().Perm())
	}

	// Stat symlink.
	li, err := fsys.Stat("/link")
	if err != nil {
		t.Fatal(err)
	}
	if li.Mode().Type() != fs.ModeSymlink {
		t.Errorf("expected symlink, got %v", li.Mode().Type())
	}

	// Stat device.
	ni, err := fsys.Stat("/null")
	if err != nil {
		t.Fatal(err)
	}
	if ni.Mode()&fs.ModeCharDevice == 0 {
		t.Errorf("expected char device, got %v", ni.Mode())
	}

	// Stat root.
	ri, err := fsys.Stat("/")
	if err != nil {
		t.Fatal(err)
	}
	if !ri.IsDir() {
		t.Error("root: expected directory")
	}
	if ri.Name() != "/" {
		t.Errorf("root name: got %q, want %q", ri.Name(), "/")
	}

	// Stat not found.
	_, err = fsys.Stat("/nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}

// TestWriterReadDir tests Open on directories and ReadDir.
func TestWriterReadDir(t *testing.T) {
	var buf testBuffer
	fsys := erofs.Create(&buf)

	if err := fsys.Mkdir("/subdir", 0o755); err != nil {
		t.Fatal(err)
	}

	f1, err := fsys.Create("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f1.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := f1.Close(); err != nil {
		t.Fatal(err)
	}

	f2, err := fsys.Create("/subdir/nested.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f2.Write([]byte("nested")); err != nil {
		t.Fatal(err)
	}
	if err := f2.Close(); err != nil {
		t.Fatal(err)
	}

	if err := fsys.Symlink("hello.txt", "/link"); err != nil {
		t.Fatal(err)
	}

	// ReadDir on root.
	d, err := fsys.Open("/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	rdf, ok := d.(fs.ReadDirFile)
	if !ok {
		t.Fatal("root Open did not return ReadDirFile")
	}

	entries, err := rdf.ReadDir(-1)
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"hello.txt", "link", "subdir"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("entry[%d]: got %q, want %q", i, names[i], want[i])
		}
	}

	// Verify subdir entry is a directory.
	for _, e := range entries {
		if e.Name() == "subdir" && !e.IsDir() {
			t.Error("subdir should be a directory")
		}
	}

	// ReadDir on subdir.
	sd, err := fsys.Open("/subdir")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sd.Close() }()

	sdEntries, err := sd.(fs.ReadDirFile).ReadDir(-1)
	if err != nil {
		t.Fatal(err)
	}
	if len(sdEntries) != 1 || sdEntries[0].Name() != "nested.txt" {
		t.Errorf("subdir entries: got %v, want [nested.txt]", sdEntries)
	}
}

// TestWriterOpenErrors tests error cases for Open.
func TestWriterOpenErrors(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		var buf testBuffer
		fsys := erofs.Create(&buf)
		_, err := fsys.Open("/nonexistent")
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("file not yet closed", func(t *testing.T) {
		var buf testBuffer
		fsys := erofs.Create(&buf)
		f, err := fsys.Create("/file.txt")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("data")); err != nil {
			t.Fatal(err)
		}

		_, err = fsys.Open("/file.txt")
		if err == nil {
			t.Fatal("expected error opening file still being written")
		}

		if err := f.Close(); err != nil {
			t.Fatal(err)
		}

		// Should succeed now.
		rf, err := fsys.Open("/file.txt")
		if err != nil {
			t.Fatal("expected success after file closed:", err)
		}
		_ = rf.Close()
	})

	t.Run("read from dir", func(t *testing.T) {
		var buf testBuffer
		fsys := erofs.Create(&buf)
		if err := fsys.Mkdir("/dir", 0o755); err != nil {
			t.Fatal(err)
		}

		d, err := fsys.Open("/dir")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = d.Close() }()

		_, err = d.Read(make([]byte, 10))
		if err == nil {
			t.Fatal("expected error reading from directory")
		}
	})
}

// TestMetadataOnlyNoFileData verifies that metadata-only images do not
// contain file data. The EROFS output should hold only inodes, dirents,
// xattrs, and chunk indexes — never the file content itself.
func TestMetadataOnlyNoFileData(t *testing.T) {
	// Use a recognizable, non-trivial pattern that won't appear by coincidence.
	marker := bytes.Repeat([]byte("EROFS_DATA_LEAK_CHECK!"), 200) // 4400 bytes

	t.Run("MetadataOnly flag", func(t *testing.T) {
		var meta testBuffer
		w := erofs.Create(&meta)

		srcFS := fstest.MapFS{
			"testfile.bin": &fstest.MapFile{Data: marker, Mode: 0o644},
		}
		if err := w.CopyFrom(srcFS, erofs.MetadataOnly()); err != nil {
			t.Fatal("CopyFrom:", err)
		}

		if err := w.Close(); err != nil {
			t.Fatal("Close:", err)
		}

		if bytes.Contains(meta.Bytes(), marker) {
			t.Error("metadata image contains file data — data leaked into metadata-only output")
		}
		t.Logf("metadata size: %d bytes, marker size: %d bytes", len(meta.Bytes()), len(marker))
	})

	t.Run("WithDataFile", func(t *testing.T) {
		dataPath := filepath.Join(t.TempDir(), "data.bin")
		df, err := os.Create(dataPath)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = df.Close() }()

		var meta testBuffer
		w := erofs.Create(&meta, erofs.WithDataFile(df))

		f, err := w.Create("/testfile.bin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(marker); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}

		if err := w.Close(); err != nil {
			t.Fatal("Close:", err)
		}

		if bytes.Contains(meta.Bytes(), marker) {
			t.Error("metadata image contains file data — data leaked into metadata-only output")
		}

		// Data file should contain the file data.
		dfRead, err := os.ReadFile(dataPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(dfRead, marker) {
			t.Error("data file does not contain file data")
		}
		t.Logf("metadata: %d bytes, data file: %d bytes", len(meta.Bytes()), len(dfRead))
	})

	t.Run("CopyFrom with pre-existing chunks", func(t *testing.T) {
		// Simulate a source that provides chunk mappings (like ext4).
		// The metadata image must not contain file data, and no data
		// should be written to a data file.

		dataPath := filepath.Join(t.TempDir(), "data.bin")
		df, err := os.Create(dataPath)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = df.Close() }()

		var meta testBuffer
		w := erofs.Create(&meta, erofs.WithDataFile(df))

		// Build a source FS where entries carry pre-existing chunks.
		srcFS := newChunkedFS(marker)

		if err := w.CopyFrom(srcFS, erofs.MetadataOnly()); err != nil {
			t.Fatal("CopyFrom:", err)
		}
		if err := w.Close(); err != nil {
			t.Fatal("Close:", err)
		}

		if bytes.Contains(meta.Bytes(), marker) {
			t.Error("metadata image contains file data — data leaked into metadata-only output")
		}

		// Data file should be empty — chunks reference the original device,
		// so no data should be copied.
		dfInfo, err := os.Stat(dataPath)
		if err != nil {
			t.Fatal(err)
		}
		if dfInfo.Size() != 0 {
			t.Errorf("data file should be empty for pre-existing chunks, got %d bytes", dfInfo.Size())
		}
		t.Logf("metadata: %d bytes, data file: %d bytes", len(meta.Bytes()), dfInfo.Size())
	})
}

// chunkedFS is a test fs.FS that provides entries with pre-existing chunk
// mappings and data readers, simulating an ext4-like source.
type chunkedFS struct {
	data []byte
}

func newChunkedFS(data []byte) *chunkedFS {
	return &chunkedFS{data: data}
}

func (cfs *chunkedFS) DeviceBlocks() uint64 { return 1024 }

func (cfs *chunkedFS) Open(name string) (fs.File, error) {
	if name == "." {
		return &chunkedDir{cfs: cfs}, nil
	}
	if name == "testfile.bin" {
		return &chunkedFile{data: cfs.data}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

type chunkedDir struct {
	cfs     *chunkedFS
	didRead bool
}

func (d *chunkedDir) Stat() (fs.FileInfo, error) {
	return &chunkedDirInfo{}, nil
}
func (d *chunkedDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: ".", Err: fmt.Errorf("is a directory")}
}
func (d *chunkedDir) Close() error { return nil }
func (d *chunkedDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.didRead {
		return nil, io.EOF
	}
	d.didRead = true
	return []fs.DirEntry{&chunkedDirEntry{data: d.cfs.data}}, nil
}

type chunkedDirInfo struct{}

func (i *chunkedDirInfo) Name() string       { return "." }
func (i *chunkedDirInfo) Size() int64        { return 0 }
func (i *chunkedDirInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o755 }
func (i *chunkedDirInfo) ModTime() time.Time { return time.Time{} }
func (i *chunkedDirInfo) IsDir() bool        { return true }
func (i *chunkedDirInfo) Sys() any           { return nil }

type chunkedDirEntry struct {
	data []byte
}

func (e *chunkedDirEntry) Name() string      { return "testfile.bin" }
func (e *chunkedDirEntry) IsDir() bool       { return false }
func (e *chunkedDirEntry) Type() fs.FileMode { return 0 }
func (e *chunkedDirEntry) Info() (fs.FileInfo, error) {
	return &chunkedFileInfo{size: int64(len(e.data))}, nil
}

type chunkedFileInfo struct {
	size int64
}

func (i *chunkedFileInfo) Name() string       { return "testfile.bin" }
func (i *chunkedFileInfo) Size() int64        { return i.size }
func (i *chunkedFileInfo) Mode() fs.FileMode  { return 0o644 }
func (i *chunkedFileInfo) ModTime() time.Time { return time.Time{} }
func (i *chunkedFileInfo) IsDir() bool        { return false }
func (i *chunkedFileInfo) Sys() any {
	nblocks := (i.size + 4095) / 4096
	return &builder.Entry{
		Nlink: 1,
		Data:  bytes.NewReader(make([]byte, i.size)),
		Chunks: []builder.Chunk{{
			PhysicalBlock: 100,
			Count:         uint16(nblocks),
			DeviceID:      1,
		}},
	}
}

type chunkedFile struct {
	data   []byte
	offset int
}

func (f *chunkedFile) Stat() (fs.FileInfo, error) {
	return &chunkedFileInfo{size: int64(len(f.data))}, nil
}
func (f *chunkedFile) Read(p []byte) (int, error) {
	if f.offset >= len(f.data) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.offset:])
	f.offset += n
	return n, nil
}
func (f *chunkedFile) Close() error { return nil }

// --- Merge tests ---

// TestMergeBasic verifies that two CopyFrom calls merge entries.
func TestMergeBasic(t *testing.T) {
	base := fstest.MapFS{
		"file1.txt":     {Data: []byte("base1"), Mode: 0o644},
		"dir/file2.txt": {Data: []byte("base2"), Mode: 0o644},
	}
	overlay := fstest.MapFS{
		"file3.txt":     {Data: []byte("overlay"), Mode: 0o644},
		"dir/file4.txt": {Data: []byte("new"), Mode: 0o644},
	}

	var buf testBuffer
	w := erofs.Create(&buf)

	if err := w.CopyFrom(base); err != nil {
		t.Fatal("CopyFrom base:", err)
	}
	if err := w.CopyFrom(overlay, erofs.Merge()); err != nil {
		t.Fatal("CopyFrom overlay:", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())
	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("Open:", err)
	}

	erofstest.CheckFile(t, efs, "file1.txt", "base1")
	erofstest.CheckFile(t, efs, "dir/file2.txt", "base2")
	erofstest.CheckFile(t, efs, "file3.txt", "overlay")
	erofstest.CheckFile(t, efs, "dir/file4.txt", "new")
}

// TestMergeWhiteout verifies that .wh.<name> files delete entries.
func TestMergeWhiteout(t *testing.T) {
	base := fstest.MapFS{
		"keep.txt":   {Data: []byte("keep"), Mode: 0o644},
		"remove.txt": {Data: []byte("gone"), Mode: 0o644},
		"dir/a.txt":  {Data: []byte("a"), Mode: 0o644},
	}
	overlay := fstest.MapFS{
		".wh.remove.txt": {Data: []byte{}, Mode: 0o644},
		"dir/.wh.a.txt":  {Data: []byte{}, Mode: 0o644},
		"dir/b.txt":      {Data: []byte("b"), Mode: 0o644},
	}

	var buf testBuffer
	w := erofs.Create(&buf)

	if err := w.CopyFrom(base); err != nil {
		t.Fatal("CopyFrom base:", err)
	}
	if err := w.CopyFrom(overlay, erofs.Merge()); err != nil {
		t.Fatal("CopyFrom overlay:", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())
	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("Open:", err)
	}

	erofstest.CheckFile(t, efs, "keep.txt", "keep")
	erofstest.CheckNotExists(t, efs, "remove.txt")
	erofstest.CheckNotExists(t, efs, "dir/a.txt")
	erofstest.CheckFile(t, efs, "dir/b.txt", "b")
}

// TestMergeOpaque verifies that .wh..wh..opq removes all prior children.
func TestMergeOpaque(t *testing.T) {
	base := fstest.MapFS{
		"dir/old1.txt":     {Data: []byte("old1"), Mode: 0o644},
		"dir/old2.txt":     {Data: []byte("old2"), Mode: 0o644},
		"dir/sub/deep.txt": {Data: []byte("deep"), Mode: 0o644},
	}
	overlay := fstest.MapFS{
		"dir/.wh..wh..opq": {Data: []byte{}, Mode: 0o644},
		"dir/new.txt":      {Data: []byte("new"), Mode: 0o644},
	}

	var buf testBuffer
	w := erofs.Create(&buf)

	if err := w.CopyFrom(base); err != nil {
		t.Fatal("CopyFrom base:", err)
	}
	if err := w.CopyFrom(overlay, erofs.Merge()); err != nil {
		t.Fatal("CopyFrom overlay:", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())
	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("Open:", err)
	}

	erofstest.CheckNotExists(t, efs, "dir/old1.txt")
	erofstest.CheckNotExists(t, efs, "dir/old2.txt")
	erofstest.CheckNotExists(t, efs, "dir/sub/deep.txt")
	erofstest.CheckFile(t, efs, "dir/new.txt", "new")
}

// TestMergeOverwrite verifies that overlay files replace base files.
func TestMergeOverwrite(t *testing.T) {
	base := fstest.MapFS{
		"file.txt": {Data: []byte("old"), Mode: 0o644},
	}
	overlay := fstest.MapFS{
		"file.txt": {Data: []byte("new"), Mode: 0o644},
	}

	var buf testBuffer
	w := erofs.Create(&buf)

	if err := w.CopyFrom(base); err != nil {
		t.Fatal("CopyFrom base:", err)
	}
	if err := w.CopyFrom(overlay, erofs.Merge()); err != nil {
		t.Fatal("CopyFrom overlay:", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())
	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("Open:", err)
	}

	erofstest.CheckFile(t, efs, "file.txt", "new")
}

// TestMergeWhiteoutDir verifies that a whiteout can remove an entire directory.
func TestMergeWhiteoutDir(t *testing.T) {
	base := fstest.MapFS{
		"dir/file.txt": {Data: []byte("content"), Mode: 0o644},
	}
	overlay := fstest.MapFS{
		".wh.dir": {Data: []byte{}, Mode: 0o644},
	}

	var buf testBuffer
	w := erofs.Create(&buf)

	if err := w.CopyFrom(base); err != nil {
		t.Fatal("CopyFrom base:", err)
	}
	if err := w.CopyFrom(overlay, erofs.Merge()); err != nil {
		t.Fatal("CopyFrom overlay:", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())
	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("Open:", err)
	}

	erofstest.CheckNotExists(t, efs, "dir")
	erofstest.CheckNotExists(t, efs, "dir/file.txt")
}

// TestCreateFSUIDGID tests a variety of UID/GID values including boundary
// values for compact (uint16) and extended (uint32) inodes.
func TestCreateFSUIDGID(t *testing.T) {
	type uidgid struct {
		uid, gid int
	}
	cases := []uidgid{
		{0, 0},
		{1, 1},
		{1000, 2000},
		{65534, 65534}, // nobody/nogroup on many systems
		{65535, 65535}, // max compact inode UID/GID
		{65536, 65536}, // first value requiring extended inode
		{100000, 100000},
		{0, 65536}, // mixed: compact-range UID, extended-range GID
		{65536, 0}, // mixed: extended-range UID, compact-range GID
		{0x7FFFFFFF, 0x7FFFFFFF},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("uid%d_gid%d", tc.uid, tc.gid), func(t *testing.T) {
			var buf testBuffer
			fsys := erofs.Create(&buf)

			// Test UID/GID on a regular file via File.Chown.
			f, err := fsys.Create("/file.txt")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write([]byte("hello")); err != nil {
				t.Fatal(err)
			}
			if err := f.Chown(tc.uid, tc.gid); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}

			// Test UID/GID on a directory via Writer.Chown.
			if err := fsys.Mkdir("/dir", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := fsys.Chown("/dir", tc.uid, tc.gid); err != nil {
				t.Fatal(err)
			}

			// Test UID/GID on a symlink.
			if err := fsys.Symlink("file.txt", "/link"); err != nil {
				t.Fatal(err)
			}
			if err := fsys.Chown("/link", tc.uid, tc.gid); err != nil {
				t.Fatal(err)
			}

			if err := fsys.Close(); err != nil {
				t.Fatal("Close:", err)
			}

			erofstest.FsckErofsBytes(t, buf.Bytes())
			efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatal("Open:", err)
			}

			wantUID := uint32(tc.uid)
			wantGID := uint32(tc.gid)

			// Verify file UID/GID.
			st := erofstest.Stat(t, efs, "file.txt")
			if st.UID != wantUID || st.GID != wantGID {
				t.Errorf("file uid/gid: got %d/%d, want %d/%d", st.UID, st.GID, wantUID, wantGID)
			}

			// Verify directory UID/GID.
			dst := erofstest.Stat(t, efs, "dir")
			if dst.UID != wantUID || dst.GID != wantGID {
				t.Errorf("dir uid/gid: got %d/%d, want %d/%d", dst.UID, dst.GID, wantUID, wantGID)
			}

			// Verify symlink UID/GID.
			lst := erofstest.Lstat(t, efs, "link")
			if lst.UID != wantUID || lst.GID != wantGID {
				t.Errorf("symlink uid/gid: got %d/%d, want %d/%d", lst.UID, lst.GID, wantUID, wantGID)
			}
		})
	}
}

// TestCopyFromStatSource verifies that CopyFrom preserves directory xattrs,
// directory ownership, and file ownership when the source FS returns *Stat
// from FileInfo.Sys() (the generic/EROFS reader path, not *builder.Entry).
//
// Before the fix, CopyFrom fell through to add(p, info) for directories
// and non-regular entries when be was extracted from *Stat, losing all
// metadata because entryFromSys does not handle *Stat.
func TestCopyFromStatSource(t *testing.T) {
	// Build a source EROFS image with a directory that has xattrs and
	// custom ownership, plus a file with custom ownership.
	var srcBuf erofstest.TestBuffer
	w := erofs.Create(&srcBuf)

	if err := w.Mkdir("/", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := w.Mkdir("/mydir", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := w.Setxattr("/mydir", "trusted.overlay.opaque", "y"); err != nil {
		t.Fatal(err)
	}
	if err := w.Setxattr("/mydir", "user.custom", "val"); err != nil {
		t.Fatal(err)
	}
	if err := w.Chown("/mydir", 1000, 1000); err != nil {
		t.Fatal(err)
	}
	f, err := w.Create("/mydir/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("content")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Chown("/mydir/file.txt", 2000, 2000); err != nil {
		t.Fatal(err)
	}

	// Add a character device with custom ownership.
	if err := w.Mknod("/mydir/null", disk.StatTypeChrdev|0o666, 1<<8|3); err != nil {
		t.Fatal(err)
	}
	if err := w.Chown("/mydir/null", 3000, 3000); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Open the source EROFS — Sys() returns *erofs.Stat, not *builder.Entry.
	srcFS, err := erofs.Open(bytes.NewReader(srcBuf.Bytes()))
	if err != nil {
		t.Fatal("open source:", err)
	}

	// CopyFrom into a new Writer (non-MetadataOnly → uses WalkDir path).
	var dstBuf erofstest.TestBuffer
	w2 := erofs.Create(&dstBuf)
	if err := w2.CopyFrom(srcFS); err != nil {
		t.Fatal("CopyFrom:", err)
	}
	if err := w2.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	// Open the destination and verify metadata survived the round-trip.
	dstFS, err := erofs.Open(bytes.NewReader(dstBuf.Bytes()))
	if err != nil {
		t.Fatal("open dest:", err)
	}

	// Directory xattrs must be preserved.
	erofstest.CheckXattrs(t, dstFS, "mydir", map[string]string{
		"trusted.overlay.opaque": "y",
		"user.custom":            "val",
	})

	// Directory ownership must be preserved.
	dirSt := erofstest.Stat(t, dstFS, "mydir")
	if dirSt.UID != 1000 {
		t.Errorf("mydir UID = %d, want 1000", dirSt.UID)
	}
	if dirSt.GID != 1000 {
		t.Errorf("mydir GID = %d, want 1000", dirSt.GID)
	}

	// File content and ownership must be preserved.
	erofstest.CheckFile(t, dstFS, "mydir/file.txt", "content")
	fileSt := erofstest.Stat(t, dstFS, "mydir/file.txt")
	if fileSt.UID != 2000 {
		t.Errorf("file.txt UID = %d, want 2000", fileSt.UID)
	}
	if fileSt.GID != 2000 {
		t.Errorf("file.txt GID = %d, want 2000", fileSt.GID)
	}

	// Device node metadata must be preserved.
	erofstest.CheckDevice(t, dstFS, "mydir/null", fs.ModeDevice|fs.ModeCharDevice, 1<<8|3)
	devSt := erofstest.Stat(t, dstFS, "mydir/null")
	if devSt.UID != 3000 {
		t.Errorf("null UID = %d, want 3000", devSt.UID)
	}
	if devSt.GID != 3000 {
		t.Errorf("null GID = %d, want 3000", devSt.GID)
	}
}

// --- DataRange-driven CopyFrom tests ---

// dataRangerFileInfo is a test fs.FileInfo whose DataRange() returns
// caller-supplied ranges, simulating a source (e.g. ext4 or OCI layer
// reader) that knows where its file bytes live without holding a data reader.
// content is the data returned by Read(); if nil, Read returns zeros.
type dataRangerFileInfo struct {
	name    string
	size    int64
	ranges  []erofs.DataRange
	content []byte // data served by Read(); nil means return zeros
}

func (fi *dataRangerFileInfo) Name() string                 { return fi.name }
func (fi *dataRangerFileInfo) Size() int64                  { return fi.size }
func (fi *dataRangerFileInfo) Mode() fs.FileMode            { return 0o644 }
func (fi *dataRangerFileInfo) ModTime() time.Time           { return time.Time{} }
func (fi *dataRangerFileInfo) IsDir() bool                  { return false }
func (fi *dataRangerFileInfo) Sys() any                     { return nil }
func (fi *dataRangerFileInfo) DataRange() []erofs.DataRange { return fi.ranges }

// dataRangerFS is a minimal fs.FS that exposes one regular file whose
// FileInfo implements DataRange().  It also implements DeviceBlocks() so
// CopyFrom records the backing-device size.
type dataRangerFS struct {
	file *dataRangerFileInfo
	// deviceBlocks is the declared size (in 4096-byte blocks) of the backing device.
	deviceBlocks uint64
}

func (f *dataRangerFS) DeviceBlocks() uint64 { return f.deviceBlocks }

func (f *dataRangerFS) Open(name string) (fs.File, error) {
	if name == "." {
		return &dataRangerDir{fs: f}, nil
	}
	if name == f.file.name {
		return &dataRangerFile{info: f.file}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

type dataRangerDir struct {
	fs      *dataRangerFS
	didRead bool
}

func (d *dataRangerDir) Stat() (fs.FileInfo, error) {
	return &dataRangerDirInfo{}, nil
}
func (d *dataRangerDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: ".", Err: fmt.Errorf("is a directory")}
}
func (d *dataRangerDir) Close() error { return nil }
func (d *dataRangerDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.didRead {
		return nil, io.EOF
	}
	d.didRead = true
	return []fs.DirEntry{&dataRangerEntry{info: d.fs.file}}, nil
}

type dataRangerDirInfo struct{}

func (i *dataRangerDirInfo) Name() string       { return "." }
func (i *dataRangerDirInfo) Size() int64        { return 0 }
func (i *dataRangerDirInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o755 }
func (i *dataRangerDirInfo) ModTime() time.Time { return time.Time{} }
func (i *dataRangerDirInfo) IsDir() bool        { return true }
func (i *dataRangerDirInfo) Sys() any           { return nil }

type dataRangerEntry struct{ info *dataRangerFileInfo }

func (e *dataRangerEntry) Name() string               { return e.info.name }
func (e *dataRangerEntry) IsDir() bool                { return false }
func (e *dataRangerEntry) Type() fs.FileMode          { return 0 }
func (e *dataRangerEntry) Info() (fs.FileInfo, error) { return e.info, nil }

type dataRangerFile struct {
	info   *dataRangerFileInfo
	offset int64
}

func (f *dataRangerFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *dataRangerFile) Read(p []byte) (int, error) {
	if f.offset >= f.info.size {
		return 0, io.EOF
	}
	var n int
	if f.info.content != nil {
		// Serve actual content so leak-detection tests can catch accidental reads.
		n = copy(p, f.info.content[f.offset:])
	} else {
		// No content supplied: return zeros up to size.
		end := f.info.size - f.offset
		if int64(len(p)) < end {
			end = int64(len(p))
		}
		for i := range p[:end] {
			p[i] = 0
		}
		n = int(end)
	}
	f.offset += int64(n)
	return n, nil
}
func (f *dataRangerFile) Close() error { return nil }

// TestCopyFromDataRange verifies that CopyFrom(MetadataOnly) uses the
// DataRange() accessor from a source FileInfo to synthesise chunk indexes
// in the output EROFS image, and that the resulting image is valid.
//
// It also verifies that chunksFromRanges rejects invalid inputs (negative
// offsets, non-positive sizes, unaligned offsets).
func TestCopyFromDataRange(t *testing.T) {
	const blockSize = 4096

	t.Run("single contiguous range", func(t *testing.T) {
		// One file, two blocks starting at physical block 10 on device 0.
		src := &dataRangerFS{
			deviceBlocks: 1024,
			file: &dataRangerFileInfo{
				name: "data.bin",
				size: blockSize * 2,
				ranges: []erofs.DataRange{
					{Device: 0, Offset: blockSize * 10, Size: blockSize * 2},
				},
			},
		}

		var buf testBuffer
		w := erofs.Create(&buf)
		if err := w.CopyFrom(src, erofs.MetadataOnly()); err != nil {
			t.Fatal("CopyFrom:", err)
		}
		if err := w.Close(); err != nil {
			t.Fatal("Close:", err)
		}

		erofstest.FsckErofsBytes(t, buf.Bytes())

		// Open the output image and verify DataRange() on the file.
		dstFS, err := erofs.Open(bytes.NewReader(buf.Bytes()),
			erofs.WithExtraDevices(bytes.NewReader(make([]byte, 1024*blockSize))))
		if err != nil {
			t.Fatal("Open:", err)
		}
		info := statDataRange(t, dstFS, "data.bin")
		if len(info) == 0 {
			t.Fatal("DataRange() returned nil for chunk-based file")
		}
		// Device 0 on the source becomes DeviceID=1 in EROFS, which is
		// what buildChunkDataRanges stores directly into DataRange.Device.
		got := info[0]
		if got.Device != 1 {
			t.Errorf("Device = %d, want 1", got.Device)
		}
		if got.Offset != blockSize*10 {
			t.Errorf("Offset = %d, want %d", got.Offset, blockSize*10)
		}
		if got.Size != blockSize*2 {
			t.Errorf("Size = %d, want %d", got.Size, blockSize*2)
		}
	})

	t.Run("multiple ranges on one device", func(t *testing.T) {
		// Non-contiguous ranges: block 5 (1 block) and block 20 (3 blocks).
		src := &dataRangerFS{
			deviceBlocks: 1024,
			file: &dataRangerFileInfo{
				name: "sparse.bin",
				size: blockSize * 4, // 4 logical blocks total
				ranges: []erofs.DataRange{
					{Device: 0, Offset: blockSize * 5, Size: blockSize * 1},
					{Device: 0, Offset: blockSize * 20, Size: blockSize * 3},
				},
			},
		}

		var buf testBuffer
		w := erofs.Create(&buf)
		if err := w.CopyFrom(src, erofs.MetadataOnly()); err != nil {
			t.Fatal("CopyFrom:", err)
		}
		if err := w.Close(); err != nil {
			t.Fatal("Close:", err)
		}

		erofstest.FsckErofsBytes(t, buf.Bytes())

		dstFS, err := erofs.Open(bytes.NewReader(buf.Bytes()),
			erofs.WithExtraDevices(bytes.NewReader(make([]byte, 1024*blockSize))))
		if err != nil {
			t.Fatal("Open:", err)
		}
		ranges := statDataRange(t, dstFS, "sparse.bin")
		if len(ranges) != 2 {
			t.Fatalf("DataRange() len = %d, want 2; ranges = %v", len(ranges), ranges)
		}
		if ranges[0].Device != 1 || ranges[0].Offset != blockSize*5 || ranges[0].Size != blockSize {
			t.Errorf("ranges[0] = %+v, want {Device:1 Offset:%d Size:%d}", ranges[0], blockSize*5, blockSize)
		}
		if ranges[1].Device != 1 || ranges[1].Offset != blockSize*20 || ranges[1].Size != blockSize*3 {
			t.Errorf("ranges[1] = %+v, want {Device:1 Offset:%d Size:%d}", ranges[1], blockSize*20, blockSize*3)
		}
	})

	t.Run("no data written for metadata-only", func(t *testing.T) {
		// CopyFrom(MetadataOnly) must not copy any file data into the image.
		// Read() returns the marker so that if CopyFrom accidentally opens
		// and reads the file the marker will appear in the output image.
		marker := bytes.Repeat([]byte("DATA_LEAK_CHECK"), 100)
		src := &dataRangerFS{
			deviceBlocks: 256,
			file: &dataRangerFileInfo{
				name:    "file.bin",
				size:    int64(len(marker)),
				content: marker,
				ranges:  []erofs.DataRange{{Device: 0, Offset: 0, Size: int64(len(marker))}},
			},
		}
		var buf testBuffer
		w := erofs.Create(&buf)
		if err := w.CopyFrom(src, erofs.MetadataOnly()); err != nil {
			t.Fatal("CopyFrom:", err)
		}
		if err := w.Close(); err != nil {
			t.Fatal("Close:", err)
		}
		if bytes.Contains(buf.Bytes(), marker) {
			t.Error("metadata-only image contains file data — leak detected")
		}
	})

	t.Run("sparse_hole_round_trip", func(t *testing.T) {
		// A full-coverage slice with a hole entry should round-trip correctly.
		// File layout: 1 data block at physical block 7, then 3-block hole.
		// After CopyFrom(MetadataOnly) the output image should be valid (fsck),
		// and reading the file back must yield:
		//   - DataRange with Device=1, Offset=blockSize*7, Size=blockSize  (data)
		//   - DataRange with Offset==-1, Size=blockSize*3                  (hole)
		const blockSize = 4096
		src := &dataRangerFS{
			deviceBlocks: 1024,
			file: &dataRangerFileInfo{
				name: "sparse.bin",
				size: blockSize * 4,
				ranges: []erofs.DataRange{
					{Device: 0, Offset: blockSize * 7, Size: blockSize},
					{Offset: -1, Size: blockSize * 3},
				},
			},
		}
		var buf testBuffer
		w := erofs.Create(&buf)
		if err := w.CopyFrom(src, erofs.MetadataOnly()); err != nil {
			t.Fatal("CopyFrom:", err)
		}
		if err := w.Close(); err != nil {
			t.Fatal("Close:", err)
		}

		erofstest.FsckErofsBytes(t, buf.Bytes())

		dstFS, err := erofs.Open(bytes.NewReader(buf.Bytes()),
			erofs.WithExtraDevices(bytes.NewReader(make([]byte, 1024*blockSize))))
		if err != nil {
			t.Fatal("Open:", err)
		}
		ranges := statDataRange(t, dstFS, "sparse.bin")

		// Verify total coverage equals file size.
		var total int64
		for _, r := range ranges {
			total += r.Size
		}
		if total != blockSize*4 {
			t.Fatalf("total DataRange size = %d, want %d; ranges = %+v", total, blockSize*4, ranges)
		}

		// Find the data range and verify it points to physical block 7 on device 1.
		var dataRanges, holeRanges []erofs.DataRange
		for _, r := range ranges {
			if r.Offset == -1 {
				holeRanges = append(holeRanges, r)
			} else {
				dataRanges = append(dataRanges, r)
			}
		}
		if len(dataRanges) != 1 {
			t.Fatalf("want 1 data range, got %d; ranges = %+v", len(dataRanges), ranges)
		}
		dr := dataRanges[0]
		if dr.Device != 1 || dr.Offset != blockSize*7 || dr.Size != blockSize {
			t.Errorf("data range = %+v, want {Device:1 Offset:%d Size:%d}", dr, blockSize*7, blockSize)
		}
		// Hole ranges should cover the remaining 3 blocks.
		var holeTotal int64
		for _, r := range holeRanges {
			holeTotal += r.Size
		}
		if holeTotal != blockSize*3 {
			t.Errorf("hole total = %d, want %d; holeRanges = %+v", holeTotal, blockSize*3, holeRanges)
		}
	})
}

// TestChunksFromRangesValidation verifies that chunksFromRanges rejects
// invalid DataRange inputs instead of silently producing corrupt chunk entries.
func TestChunksFromRangesValidation(t *testing.T) {
	// Use a non-EROFS source that exercises the chunksFromRanges code path.
	// CopyFrom(MetadataOnly) on a DataRange-implementing source calls it.
	tryRanges := func(t *testing.T, ranges []erofs.DataRange) error {
		t.Helper()
		src := &dataRangerFS{
			deviceBlocks: 1024,
			file: &dataRangerFileInfo{
				name:   "f.bin",
				size:   4096 * 4,
				ranges: ranges,
			},
		}
		var buf testBuffer
		w := erofs.Create(&buf)
		err := w.CopyFrom(src, erofs.MetadataOnly())
		if err == nil {
			_ = w.Close()
		}
		return err
	}

	t.Run("negative offset", func(t *testing.T) {
		err := tryRanges(t, []erofs.DataRange{{Device: 0, Offset: -4096, Size: 4096}})
		if err == nil {
			t.Fatal("expected error for negative Offset, got nil")
		}
	})

	t.Run("zero size", func(t *testing.T) {
		err := tryRanges(t, []erofs.DataRange{{Device: 0, Offset: 0, Size: 0}})
		if err == nil {
			t.Fatal("expected error for zero Size, got nil")
		}
	})

	t.Run("negative size", func(t *testing.T) {
		err := tryRanges(t, []erofs.DataRange{{Device: 0, Offset: 0, Size: -1}})
		if err == nil {
			t.Fatal("expected error for negative Size, got nil")
		}
	})

	t.Run("unaligned offset", func(t *testing.T) {
		err := tryRanges(t, []erofs.DataRange{{Device: 0, Offset: 100, Size: 4096}})
		if err == nil {
			t.Fatal("expected error for unaligned Offset, got nil")
		}
	})

	t.Run("device out of range", func(t *testing.T) {
		err := tryRanges(t, []erofs.DataRange{{Device: 1, Offset: 4096, Size: 4096}})
		if err == nil {
			t.Fatal("expected error for Device!=0, got nil")
		}
	})

	t.Run("device wrap overflow", func(t *testing.T) {
		err := tryRanges(t, []erofs.DataRange{{Device: 0xFFFF, Offset: 4096, Size: 4096}})
		if err == nil {
			t.Fatal("expected error for Device=0xFFFF (wraps to DeviceID=0), got nil")
		}
	})

	t.Run("total_size_mismatch_under", func(t *testing.T) {
		// Slice total (4096) < file size (16384).
		err := tryRanges(t, []erofs.DataRange{{Device: 0, Offset: 0, Size: 4096}})
		if err == nil {
			t.Fatal("expected error for under-coverage, got nil")
		}
	})

	t.Run("total_size_mismatch_over", func(t *testing.T) {
		// Slice total (4096*5) > file size (16384).
		err := tryRanges(t, []erofs.DataRange{{Device: 0, Offset: 0, Size: 4096 * 5}})
		if err == nil {
			t.Fatal("expected error for over-coverage, got nil")
		}
	})

	t.Run("hole_entry_accepted", func(t *testing.T) {
		// Full-coverage slice with a hole entry — holes are now supported.
		// 1 data block + 3-block hole = 4 blocks == file size (16384).
		err := tryRanges(t, []erofs.DataRange{
			{Device: 0, Offset: 0, Size: 4096},
			{Offset: -1, Size: 4096 * 3},
		})
		if err != nil {
			t.Fatalf("unexpected error for valid hole entry: %v", err)
		}
	})

	t.Run("non_final_size_unaligned", func(t *testing.T) {
		// Middle range has non-block-aligned Size (100 bytes).
		// total = 100 + (16384-100) = 16384 so coverage is satisfied.
		err := tryRanges(t, []erofs.DataRange{
			{Device: 0, Offset: 0, Size: 100},
			{Device: 0, Offset: 4096, Size: 4096*4 - 100},
		})
		if err == nil {
			t.Fatal("expected error for non-final unaligned Size, got nil")
		}
	})

	t.Run("final_size_partial_ok", func(t *testing.T) {
		// Final range has a partial last block (file size is 16384 = 4*4096 exactly,
		// but here we use a 3.5-block file to get a partial tail).
		// Override file size via a separate dataRangerFS with size 4096*3+512.
		partialSize := int64(4096*3 + 512)
		src := &dataRangerFS{
			deviceBlocks: 1024,
			file: &dataRangerFileInfo{
				name: "f.bin",
				size: partialSize,
				ranges: []erofs.DataRange{
					{Device: 0, Offset: 0, Size: 4096 * 3},
					{Device: 0, Offset: 4096 * 3, Size: 512},
				},
			},
		}
		var buf testBuffer
		w := erofs.Create(&buf)
		err := w.CopyFrom(src, erofs.MetadataOnly())
		if err == nil {
			_ = w.Close()
		}
		if err != nil {
			t.Fatalf("unexpected error for valid partial final range: %v", err)
		}
	})

	t.Run("valid range passes", func(t *testing.T) {
		err := tryRanges(t, []erofs.DataRange{{Device: 0, Offset: 4096, Size: 4096 * 4}})
		if err != nil {
			t.Fatalf("unexpected error for valid range: %v", err)
		}
	})
}

// statDataRange opens the named file in fsys via Stat() and returns its
// DataRange() if the FileInfo supports it.
func statDataRange(t *testing.T, fsys fs.FS, name string) []erofs.DataRange {
	t.Helper()
	f, err := fsys.Open(name)
	if err != nil {
		t.Fatalf("Open(%s): %v", name, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat(%s): %v", name, err)
	}
	type dataRanger interface {
		DataRange() []erofs.DataRange
	}
	dr, ok := info.(dataRanger)
	if !ok {
		t.Fatalf("FileInfo for %s does not implement DataRange()", name)
	}
	return dr.DataRange()
}
