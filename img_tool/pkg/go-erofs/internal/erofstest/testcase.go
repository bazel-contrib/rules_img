package erofstest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs"
)

// TestCase is a reusable test case that produces tar content, converts it
// to an EROFS image, and verifies the result.
type TestCase interface {
	// Run creates an EROFS image from tar content using conv, opens it,
	// and verifies the result. If conv is nil, MkfsErofs() is used.
	Run(t testing.TB, conv Converter)
}

type testCase struct {
	tar    func() WriterToTar
	verify func(t testing.TB, fsys fs.FS)
}

// Converter creates an fs.FS from tar content. The converter owns the
// entire pipeline: it may write the tar to disk, convert through an
// intermediate format (e.g. ext4), use a Go-native builder, etc.
type Converter func(t testing.TB, wt WriterToTar) fs.FS

// MkfsErofs returns a Converter that pipes the tar stream to the
// mkfs.erofs binary. Extra CLI flags can be passed via opts.
func MkfsErofs(opts ...string) Converter {
	return func(t testing.TB, wt WriterToTar) fs.FS {
		t.Helper()
		tarStream := TarFromWriterTo(wt)
		defer func() { _ = tarStream.Close() }()

		path := filepath.Join(t.TempDir(), "test.erofs")
		if err := ConvertTarErofs(context.Background(), tarStream, path, "", opts); err != nil {
			t.Fatal(err)
		}
		return openEroFS(t, path)
	}
}

// MkfsErofsBlobDev returns a Converter that uses the mkfs.erofs binary
// with --blobdev, writing file data to a separate device file. Extra
// CLI flags can be passed via extraOpts.
func MkfsErofsBlobDev(chunkSize int, extraOpts ...string) Converter {
	return func(t testing.TB, wt WriterToTar) fs.FS {
		t.Helper()
		tarStream := TarFromWriterTo(wt)
		defer func() { _ = tarStream.Close() }()

		path := filepath.Join(t.TempDir(), "test.erofs")
		blobPath := path + ".blob"
		opts := append([]string{
			fmt.Sprintf("--blobdev=%s", blobPath),
			fmt.Sprintf("--chunksize=%d", chunkSize),
		}, extraOpts...)
		if err := ConvertTarErofs(context.Background(), tarStream, path, "", opts); err != nil {
			t.Fatal(err)
		}
		bf, err := os.Open(blobPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = bf.Close() })
		return openEroFS(t, path, erofs.WithExtraDevices(bf))
	}
}

// MkfsErofsMaxSize is like MkfsErofs but also asserts the image is at
// most maxBytes. Useful for verifying sparse/dedup optimizations.
func MkfsErofsMaxSize(maxBytes int64, opts ...string) Converter {
	return func(t testing.TB, wt WriterToTar) fs.FS {
		t.Helper()
		tarStream := TarFromWriterTo(wt)
		defer func() { _ = tarStream.Close() }()

		path := filepath.Join(t.TempDir(), "test.erofs")
		if err := ConvertTarErofs(context.Background(), tarStream, path, "", opts); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() > maxBytes {
			t.Errorf("image size %d exceeds limit %d", fi.Size(), maxBytes)
		}
		return openEroFS(t, path)
	}
}

// openEroFS opens an EROFS image file and returns an fs.FS.
func openEroFS(t testing.TB, path string, opts ...erofs.OpenOpt) fs.FS {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	efs, err := erofs.Open(f, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return efs
}

func (tc *testCase) Run(t testing.TB, conv Converter) {
	t.Helper()
	if conv == nil {
		conv = MkfsErofs()
	}
	efs := conv(t, tc.tar())
	tc.verify(t, efs)
}

// Basic is the standard test case exercising files, directories, symlinks,
// empty files, large files, case-sensitive names, many-entry directories,
// xattrs (including long prefixes), and device files.
var Basic TestCase = &testCase{
	tar: func() WriterToTar {
		tc := TarContext{}.WithModTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

		lotsOfFilesC := make(chan WriterToTar)
		go func() {
			for i := range 5000 {
				lotsOfFilesC <- tc.File(fmt.Sprintf("/usr/lib/testdir/lotsoffiles/%d", i), []byte{}, 0600)
			}
			close(lotsOfFilesC)
		}()

		return TarAll(
			tc.Dir("/usr", 0755),
			tc.Dir("/usr/lib", 0755),
			tc.Dir("/usr/lib/testdir", 0755),
			tc.File("/in-root.txt", []byte("root file content\n"), 0600),
			tc.File("/usr/lib/testdir/emptyfile", []byte{}, 0600),
			tc.File("/usr/lib/testdir/13k-zeros.raw", bytes.Repeat([]byte{0}, 1024*13), 0600),
			tc.File("/usr/lib/testdir/16k-zeros.raw", bytes.Repeat([]byte{0}, 1024*16), 0600),
			tc.File("/usr/lib/testdir/5k-sequence.raw", bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7, 8}, 128*5), 0600),
			tc.File("/usr/lib/testdir/16k-sequence.raw", bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7, 8}, 128*16), 0600),
			tc.Dir("/usr/lib/testdir/emptydir", 0600),
			tc.Dir("/usr/lib/testdir/case", 0755),
			tc.File("/usr/lib/testdir/case/file.txt", []byte("lower case dir\n"), 0600),
			tc.Dir("/usr/lib/testdir/CASE", 0755),
			tc.File("/usr/lib/testdir/CASE/file.txt", []byte("upper case dir\n"), 0600),
			tc.File("/usr/lib/testdir/case.txt", []byte("lower case file\n"), 0600),
			tc.File("/usr/lib/testdir/CASE.txt", []byte("upper case file\n"), 0600),
			tc.Symlink("/in-root.txt", "/usr/lib/testdir/link"),
			tc.Symlink("../../../in-root.txt", "/usr/lib/testdir/link-to-root"),
			tc.Symlink("/in-root.txt", "/usr/lib/testdir/abs-link"),
			tc.Symlink("/usr/lib/testdir", "/links/dir-link"),
			tc.Symlink("/links/dir-link", "/links/dir-link2"),
			tc.Symlink("/usr/lib/testdir/abs-link", "/links/double-file-link"),
			tc.Symlink("/links/dir-link", "/links/file-via-dirs"),
			tc.WithXattrs(map[string]string{
				"user.custom":      "value1",
				"user.xdg.comment": "some random comment",
			}).Dir("/usr/lib/withxattr", 0600),
			tc.WithXattrs(map[string]string{
				"user.xdg.comment": "comment for f1",
				"user.common":      "same-value",
			}).File("/usr/lib/withxattr/f1", []byte{}, 0600),
			tc.WithXattrs(map[string]string{
				"user.xdg.comment": "comment for f2",
				"user.common":      "same-value",
			}).File("/usr/lib/withxattr/f2", []byte{}, 0600),
			tc.WithXattrs(map[string]string{
				"user.xdg.comment": "comment for f3",
				"user.common":      "same-value",
			}).File("/usr/lib/withxattr/f3", []byte{}, 0600),
			tc.WithXattrs(map[string]string{
				"user.xdg.comment": "comment for f4",
				"user.common":      "same-value",
			}).File("/usr/lib/withxattr/f4", []byte{}, 0600),
			tc.Dir("/dev", 0755),
			tc.Device("/dev/block0", fs.ModeDevice, 0, 1),
			tc.Device("/dev/block1", fs.ModeDevice, 0, 0),
			tc.Device("/dev/char0", fs.ModeCharDevice, 0, 2),
			tc.Device("/dev/char1", fs.ModeCharDevice, 0, 3),
			tc.Device("/dev/fifo0", fs.ModeNamedPipe, 0, 0),
			tc.Dir("/usr/lib/testdir/lotsoffiles", 0755),
			TarStream(lotsOfFilesC),
		)
	},
	verify: func(t testing.TB, fsys fs.FS) {
		t.Helper()

		CheckFile(t, fsys, "in-root.txt", "root file content\n")
		CheckFile(t, fsys, "usr/lib/testdir/emptyfile", "")
		CheckFileBytes(t, fsys, "usr/lib/testdir/13k-zeros.raw", bytes.Repeat([]byte{0}, 1024*13))
		CheckFileBytes(t, fsys, "usr/lib/testdir/16k-zeros.raw", bytes.Repeat([]byte{0}, 1024*16))
		CheckFileBytes(t, fsys, "usr/lib/testdir/5k-sequence.raw", bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7, 8}, 128*5))
		CheckFileBytes(t, fsys, "usr/lib/testdir/16k-sequence.raw", bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7, 8}, 128*16))
		CheckDirEntries(t, fsys, "usr/lib/testdir/emptydir", nil)
		CheckDirSize(t, fsys, "usr/lib/testdir/lotsoffiles", 5000)

		CheckFile(t, fsys, "usr/lib/testdir/case/file.txt", "lower case dir\n")
		CheckFile(t, fsys, "usr/lib/testdir/CASE/file.txt", "upper case dir\n")
		CheckFile(t, fsys, "usr/lib/testdir/case.txt", "lower case file\n")
		CheckFile(t, fsys, "usr/lib/testdir/CASE.txt", "upper case file\n")

		CheckSymlink(t, fsys, "usr/lib/testdir/link", "/in-root.txt")

		CheckNotExists(t, fsys, "not-exists.txt")
		CheckNotExists(t, fsys, "not-exists/somefile")
		CheckNotExists(t, fsys, "usr/lib/testdir/emptydir/somefile")

		// Opening a path through a non-directory component returns ErrNotDirectory.
		CheckOpenError(t, fsys, "in-root.txt/child", erofs.ErrNotDirectory)
		CheckOpenError(t, fsys, "usr/lib/testdir/emptyfile/child", erofs.ErrNotDirectory)

		CheckXattrs(t, fsys, "usr/lib/withxattr", map[string]string{
			"user.custom":      "value1",
			"user.xdg.comment": "some random comment",
		})
		CheckXattrs(t, fsys, "usr/lib/withxattr/f1", map[string]string{
			"user.xdg.comment": "comment for f1",
			"user.common":      "same-value",
		})
		CheckXattrs(t, fsys, "usr/lib/withxattr/f2", map[string]string{
			"user.xdg.comment": "comment for f2",
			"user.common":      "same-value",
		})
		CheckXattrs(t, fsys, "usr/lib/withxattr/f3", map[string]string{
			"user.xdg.comment": "comment for f3",
			"user.common":      "same-value",
		})
		CheckXattrs(t, fsys, "usr/lib/withxattr/f4", map[string]string{
			"user.xdg.comment": "comment for f4",
			"user.common":      "same-value",
		})

		CheckDevice(t, fsys, "dev/block0", fs.ModeDevice, 1)
		CheckDevice(t, fsys, "dev/block1", fs.ModeDevice, 0)
		CheckDevice(t, fsys, "dev/char0", fs.ModeDevice|fs.ModeCharDevice, 2)
		CheckDevice(t, fsys, "dev/char1", fs.ModeDevice|fs.ModeCharDevice, 3)
		CheckDevice(t, fsys, "dev/fifo0", fs.ModeNamedPipe, 0)

		// Symlink targets.
		CheckReadLink(t, fsys, "usr/lib/testdir/link-to-root", "../../../in-root.txt")
		CheckReadLink(t, fsys, "usr/lib/testdir/abs-link", "/in-root.txt")
		CheckLstat(t, fsys, "usr/lib/testdir/link-to-root", fs.ModeSymlink)
		CheckLstat(t, fsys, "in-root.txt", 0)
		CheckLstat(t, fsys, "usr/lib/testdir", fs.ModeDir)

		// Open/ReadFile/Stat follow symlinks.
		CheckFile(t, fsys, "usr/lib/testdir/link-to-root", "root file content\n")
		CheckFile(t, fsys, "usr/lib/testdir/abs-link", "root file content\n")
		CheckReadFile(t, fsys, "usr/lib/testdir/link-to-root", "root file content\n")

		// Traversal through symlinked directories.
		CheckFile(t, fsys, "links/dir-link/emptyfile", "")
		CheckFile(t, fsys, "links/dir-link2/emptyfile", "")
		CheckFile(t, fsys, "links/double-file-link", "root file content\n")
		CheckFile(t, fsys, "links/file-via-dirs/abs-link", "root file content\n")

		CheckReadFile(t, fsys, "in-root.txt", "root file content\n")
		CheckReadFileDir(t, fsys, "usr/lib/testdir")
		CheckReadDirSorted(t, fsys, "dev")

		// ReadDir on a file should return ErrNotDirectory.
		CheckReadDirFile(t, fsys, "in-root.txt")

		// ReadLink on a regular file should return ErrInvalid.
		CheckReadLinkFile(t, fsys, "in-root.txt")

		// Close without reading should not panic.
		CheckOpenClose(t, fsys, "in-root.txt")
		CheckOpenClose(t, fsys, "usr/lib/testdir")
	},
}

const (
	longXattrPrefix = "user.long.prefix.vfvzyrvujoemkjztekxczhyyqpzncyav.xiksvigqpjttnvcvxgaxpnrghppufylkopprkdsfncibznsvmbicfknlkbnuntpuqmwffxkrnuhtpucxwllkxrfzmbvmdcluahylidncngjrxnlipwikplkxgfpiiiqtzsnigpcojpkxtzbzqcosttdxhtspbxltuezcakskakmskmaznvpwcqjakbyapaglwd."
	longXattrValue  = "value1-ppufylkopprkdsfncibznsvmbicfknlkbnuntpuqmwffxkrnuhtpucxwllkxrfzmbvmdcluahylidncngjrxnlipwikplkxgfpiiiqtzsnigpcojpkxtzbzqcosttdxhtspbxltuezcakskakmskmaznvpwcqjakbyapaglwdqfgvgkrgdwcegjpfmelrejllrjkpbwindlfynuzjgvcgygyayjvmtxgsbjkzrydoswbsknrrwjkwzxhasowuzdoxlhbxso"
)

// LongXattrs tests xattrs with very long prefix names and values. This
// requires mkfs.erofs --xattr-prefix support (>= 1.9) for chunk-based
// layouts where inline xattr space is limited. The tar content also
// includes very long PAX headers that mkfs.ext4's libarchive cannot parse,
// so this test case is not suitable for the ext4 converter.
var LongXattrs TestCase = &testCase{
	tar: func() WriterToTar {
		tc := TarContext{}.WithModTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
		return TarAll(
			tc.Dir("/usr", 0755),
			tc.Dir("/usr/lib", 0755),
			tc.Dir("/usr/lib/generated", 0755),
			tc.Dir("/usr/lib/generated/xattrs", 0755),
			tc.WithXattrs(map[string]string{
				longXattrPrefix + "long-value": longXattrValue,
				longXattrPrefix + "shortvalue": "y",
			}).File("/usr/lib/generated/xattrs/long-prefix-xattrs", []byte{}, 0600),
			tc.WithXattrs(map[string]string{
				"user.short.long-value": longXattrValue,
				"user.short.shortvalue": "y",
			}).File("/usr/lib/generated/xattrs/short-prefix-xattrs", []byte{}, 0600),
		)
	},
	verify: func(t testing.TB, fsys fs.FS) {
		t.Helper()
		CheckXattrs(t, fsys, "usr/lib/generated/xattrs/long-prefix-xattrs", map[string]string{
			longXattrPrefix + "long-value": longXattrValue,
			longXattrPrefix + "shortvalue": "y",
		})
		CheckXattrs(t, fsys, "usr/lib/generated/xattrs/short-prefix-xattrs", map[string]string{
			"user.short.long-value": longXattrValue,
			"user.short.shortvalue": "y",
		})
	},
}

// SpecialModeBits checks that setuid, setgid and sticky bits survive the
// round trip into fs.FileInfo.Mode, and that the mode reported by Mode() is
// a well-formed fs.FileMode (no raw on-disk bits) for ordinary entries too.
var SpecialModeBits TestCase = &testCase{
	tar: func() WriterToTar {
		tc := TarContext{}.WithModTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
		return TarAll(
			tc.Dir("/bin", 0755),
			tc.File("/bin/su", []byte("setuid\n"), 0755|fs.ModeSetuid),
			tc.File("/bin/wall", []byte("setgid\n"), 0755|fs.ModeSetgid),
			tc.File("/bin/all-bits", []byte("all\n"), 0700|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky),
			tc.File("/bin/plain", []byte("plain\n"), 0644),
			tc.Symlink("/bin/su", "/bin/su-link"),
			tc.Dir("/tmp", 0777|fs.ModeSticky),
			tc.Dir("/var", 0775|fs.ModeSetgid),
			tc.Dir("/dev", 0755),
			tc.Device("/dev/null", fs.ModeCharDevice, 1, 3),
			tc.Device("/dev/loop0", fs.ModeDevice, 7, 0),
			tc.Device("/dev/initctl", fs.ModeNamedPipe, 0, 0),
		)
	},
	verify: func(t testing.TB, fsys fs.FS) {
		t.Helper()

		CheckMode(t, fsys, "bin/su", 0755|fs.ModeSetuid)
		CheckMode(t, fsys, "bin/wall", 0755|fs.ModeSetgid)
		CheckMode(t, fsys, "bin/all-bits", 0700|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)
		CheckMode(t, fsys, "bin/plain", 0644)
		CheckMode(t, fsys, "bin/su-link", 0777|fs.ModeSymlink)
		CheckMode(t, fsys, "bin", 0755|fs.ModeDir)
		CheckMode(t, fsys, "tmp", 0777|fs.ModeDir|fs.ModeSticky)
		CheckMode(t, fsys, "var", 0775|fs.ModeDir|fs.ModeSetgid)
		CheckMode(t, fsys, "dev/null", 0600|fs.ModeDevice|fs.ModeCharDevice)
		CheckMode(t, fsys, "dev/loop0", 0600|fs.ModeDevice)
		CheckMode(t, fsys, "dev/initctl", 0600|fs.ModeNamedPipe)

		// The natural "copy this out to disk" path: os.Chmod only applies
		// setuid/setgid/sticky when the fs.Mode* flags are set.
		fi, err := fs.Stat(fsys, "bin/su")
		if err != nil {
			t.Fatalf("stat bin/su: %v", err)
		}
		if fi.Mode()&fs.ModeSetuid == 0 {
			t.Errorf("bin/su: mode %v (%#o) has no fs.ModeSetuid", fi.Mode(), uint32(fi.Mode()))
		}
	},
}

// FileSizes tests files at layout-significant size boundaries:
//   - 4096 bytes: exactly one block
//   - 4097 bytes: just over one block (exercises partial second block)
//   - 8000 bytes: spans 2 blocks with a partial last block
//   - 1MB: many blocks, exercises sustained sequential reads
var FileSizes TestCase = &testCase{
	tar: func() WriterToTar {
		tc := TarContext{}.WithModTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
		return TarAll(
			tc.File("/exact-block.bin", generateContent(4096), 0644),
			tc.File("/block-plus-one.bin", generateContent(4097), 0644),
			tc.File("/partial-block.bin", generateContent(8000), 0644),
			tc.File("/multi-block.bin", generateContent(1024*1024), 0644),
		)
	},
	verify: func(t testing.TB, fsys fs.FS) {
		t.Helper()
		verifyContent(t, fsys, "exact-block.bin", 4096)
		verifyContent(t, fsys, "block-plus-one.bin", 4097)
		verifyContent(t, fsys, "partial-block.bin", 8000)
		verifyContent(t, fsys, "multi-block.bin", 1024*1024)
	},
}

// LargeFile tests a file exceeding 65535 blocks (256MB at 4KB blocks),
// exercising chunk index overflow where multiple chunk entries are needed.
// This test case should be run with a single 4KB-chunk converter to avoid
// redundant work.
var LargeFile TestCase = &testCase{
	tar: func() WriterToTar {
		tc := TarContext{}.WithModTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
		// 256MB + 4KB to push past the 65535-block boundary.
		return TarAll(
			tc.File("/large.bin", generateContent(256*1024*1024+4096), 0644),
		)
	},
	verify: func(t testing.TB, fsys fs.FS) {
		t.Helper()
		verifyContent(t, fsys, "large.bin", 256*1024*1024+4096)
	},
}

// uidgidTestValues is the shared set of UID/GID pairs tested by
// UIDGIDValues. Defined once to avoid drift between tar creation and
// verification.
var uidgidTestValues = []struct{ uid, gid int }{
	{0, 0},
	{1, 1},
	{1000, 2000},
	{65534, 65534}, // nobody/nogroup
	{65535, 65535}, // max compact inode (uint16)
	{65536, 65536}, // first extended inode value
	{100000, 100000},
	{0, 65536}, // compact UID, extended GID
	{65536, 0}, // extended UID, compact GID
	{0x7FFFFFFF, 0x7FFFFFFF},
}

// UIDGIDValues tests a variety of UID/GID values including boundary values
// for compact (uint16) and extended (uint32) inodes. Each value is tested
// on a regular file, directory, and symlink.
var UIDGIDValues TestCase = &testCase{
	tar: func() WriterToTar {
		base := TarContext{}.WithModTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
		var entries []WriterToTar
		for _, id := range uidgidTestValues {
			tc := base.WithUIDGID(id.uid, id.gid)
			prefix := fmt.Sprintf("/id-%d-%d", id.uid, id.gid)
			entries = append(entries,
				tc.Dir(prefix, 0755),
				tc.File(prefix+"/file.txt", []byte("hello"), 0644),
				tc.Dir(prefix+"/dir", 0755),
				tc.Symlink("file.txt", prefix+"/link"),
			)
		}
		return TarAll(entries...)
	},
	verify: func(t testing.TB, fsys fs.FS) {
		t.Helper()
		for _, id := range uidgidTestValues {
			prefix := fmt.Sprintf("id-%d-%d", id.uid, id.gid)
			wantUID := uint32(id.uid)
			wantGID := uint32(id.gid)

			st := Stat(t, fsys, prefix+"/file.txt")
			if st.UID != wantUID || st.GID != wantGID {
				t.Errorf("%s/file.txt uid/gid: got %d/%d, want %d/%d", prefix, st.UID, st.GID, wantUID, wantGID)
			}

			dst := Stat(t, fsys, prefix+"/dir")
			if dst.UID != wantUID || dst.GID != wantGID {
				t.Errorf("%s/dir uid/gid: got %d/%d, want %d/%d", prefix, dst.UID, dst.GID, wantUID, wantGID)
			}

			lst := Lstat(t, fsys, prefix+"/link")
			if lst.UID != wantUID || lst.GID != wantGID {
				t.Errorf("%s/link uid/gid: got %d/%d, want %d/%d", prefix, lst.UID, lst.GID, wantUID, wantGID)
			}
		}
	},
}

// generateContent produces deterministic bytes: each byte is i % 251.
func generateContent(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}
	return data
}

// verifyContent checks a file matches the expected deterministic pattern.
func verifyContent(t testing.TB, fsys fs.FS, name string, size int) {
	t.Helper()
	f, err := fsys.Open(name)
	if err != nil {
		t.Errorf("open %s: %v", name, err)
		return
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		t.Errorf("stat %s: %v", name, err)
		return
	}
	if fi.Size() != int64(size) {
		t.Errorf("%s: size %d, want %d", name, fi.Size(), size)
		return
	}

	// Read in chunks and verify against expected pattern.
	buf := make([]byte, 64*1024)
	offset := 0
	for {
		n, err := f.Read(buf)
		for i := range n {
			if buf[i] != byte((offset+i)%251) {
				t.Errorf("%s: mismatch at offset %d: got %d, want %d", name, offset+i, buf[i], (offset+i)%251)
				return
			}
		}
		offset += n
		if err != nil {
			break
		}
	}
	if offset != size {
		t.Errorf("%s: read %d bytes, want %d", name, offset, size)
	}
}

// SparseFiles tests erofs sparse/hole chunk handling. Files contain large
// zero regions that mkfs.erofs --chunksize optimizes into null chunk entries.
// The reader must return zeros for these holes. The tar content uses regular
// (non-sparse) entries with zero-filled data — mkfs.erofs detects the zeros
// and creates sparse chunks automatically.
//
// Note: This test case requires mkfs.erofs with --chunksize to trigger
// sparse chunk creation. The Go-native builder does not yet produce sparse
// chunks, so only the mkfs.erofs converter produces correct results.
var SparseFiles TestCase = &testCase{
	tar: func() WriterToTar {
		tc := TarContext{}.WithModTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
		marker := []byte("hello sparse world!\n")
		return TarAll(
			// 10MB file with a marker at offset 5MB.
			tc.SparseFile("/sparse-10m.bin", 10*1024*1024, marker, 5*1024*1024, 0644),
			// 20MB file, entirely zeros.
			tc.SparseFile("/sparse-20m.bin", 20*1024*1024, nil, 0, 0644),
		)
	},
	verify: func(t testing.TB, fsys fs.FS) {
		t.Helper()
		marker := "hello sparse world!\n"

		verifySparse(t, fsys, "sparse-10m.bin", 10*1024*1024, 5*1024*1024, marker)
		verifySparse(t, fsys, "sparse-20m.bin", 20*1024*1024, -1, "")
	},
}

// verifySparse checks a sparse file's size, verifies zeros by sampling a
// 4KB block every 1MB, and optionally checks for a data marker at markerOff.
// Set markerOff to -1 to skip the marker check.
func verifySparse(t testing.TB, fsys fs.FS, name string, size int64, markerOff int64, marker string) {
	t.Helper()
	f, err := fsys.Open(name)
	if err != nil {
		t.Errorf("open %s: %v", name, err)
		return
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		t.Errorf("stat %s: %v", name, err)
		return
	}
	if fi.Size() != size {
		t.Errorf("%s: size %d, want %d", name, fi.Size(), size)
		return
	}

	// Sample a 4KB block every 1MB to verify zeros.
	const step = 1024 * 1024
	buf := make([]byte, 4096)
	offset := int64(0)
	for sampleOff := int64(0); sampleOff < size; sampleOff += step {
		// Skip to the sample offset.
		if sampleOff > offset {
			skipped, err := io.CopyN(io.Discard, f, sampleOff-offset)
			if err != nil {
				t.Errorf("%s: skip to offset %d: %v", name, sampleOff, err)
				return
			}
			offset += skipped
		}
		toRead := min(int64(len(buf)), size-offset)
		n, err := io.ReadFull(f, buf[:toRead])
		if err != nil && int64(n) != toRead {
			t.Errorf("%s: read at offset %d: got %d bytes, want %d: %v", name, offset, n, toRead, err)
			return
		}
		// If this overlaps with the marker, skip the zero check.
		if markerOff >= 0 && offset <= markerOff+int64(len(marker)) && offset+int64(n) > markerOff {
			offset += int64(n)
			continue
		}
		for i := range n {
			if buf[i] != 0 {
				t.Errorf("%s: non-zero byte at offset %d", name, offset+int64(i))
				return
			}
		}
		offset += int64(n)
	}

	// Check marker if specified.
	if markerOff >= 0 {
		f2, err := fsys.Open(name)
		if err != nil {
			t.Errorf("open %s for marker check: %v", name, err)
			return
		}
		defer func() { _ = f2.Close() }()
		if _, err := io.CopyN(io.Discard, f2, markerOff); err != nil {
			t.Errorf("%s: skip to marker at %d: %v", name, markerOff, err)
			return
		}
		mbuf := make([]byte, len(marker))
		if _, err := io.ReadFull(f2, mbuf); err != nil {
			t.Errorf("%s: read marker at %d: %v", name, markerOff, err)
			return
		}
		if string(mbuf) != marker {
			t.Errorf("%s at offset %d: got %q, want %q", name, markerOff, mbuf, marker)
		}
	}
}

// XattrPrefixFlags returns mkfs.erofs flags to enable long xattr prefix
// compression for the prefixes used in LongXattrs. Returns nil if the
// installed mkfs.erofs doesn't support --xattr-prefix (< 1.9).
func XattrPrefixFlags() []string {
	tooOld, err := CheckMkfsVersion("1.9")
	if err != nil || tooOld {
		return nil
	}
	return []string{
		"--xattr-prefix=user.short",
		"--xattr-prefix=" + longXattrPrefix,
	}
}

// CheckFile verifies that the named file has the expected string content.
func CheckFile(t testing.TB, fsys fs.FS, name, expected string) {
	t.Helper()
	f, err := fsys.Open(name)
	if err != nil {
		t.Errorf("open %s: %v", name, err)
		return
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(f)
	if err != nil {
		t.Errorf("read %s: %v", name, err)
		return
	}

	if string(data) != expected {
		if len(data) > 64 {
			t.Errorf("%s: content mismatch (len %d vs %d)", name, len(data), len(expected))
		} else {
			t.Errorf("%s: got %q, want %q", name, string(data), expected)
		}
	}
}

// CheckFileBytes verifies that the named file has the expected byte content.
func CheckFileBytes(t testing.TB, fsys fs.FS, name string, expected []byte) {
	t.Helper()
	f, err := fsys.Open(name)
	if err != nil {
		t.Errorf("open %s: %v", name, err)
		return
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(f)
	if err != nil {
		t.Errorf("read %s: %v", name, err)
		return
	}

	if !bytes.Equal(data, expected) {
		t.Errorf("%s: content mismatch (len %d vs %d)", name, len(data), len(expected))
	}
}

// CheckDirEntries verifies that the named directory contains exactly the
// expected entries (sorted by name).
func CheckDirEntries(t testing.TB, fsys fs.FS, name string, expected []string) {
	t.Helper()
	entries, err := fs.ReadDir(fsys, name)
	if err != nil {
		t.Errorf("readdir %s: %v", name, err)
		return
	}

	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}

	if len(names) != len(expected) {
		t.Errorf("readdir %s: got %d entries %v, want %d entries %v", name, len(names), names, len(expected), expected)
		return
	}
	for i, n := range names {
		if n != expected[i] {
			t.Errorf("readdir %s[%d]: got %q, want %q", name, i, n, expected[i])
		}
	}
}

// CheckDirSize verifies that the named directory contains exactly n entries.
func CheckDirSize(t testing.TB, fsys fs.FS, name string, n int) {
	t.Helper()
	entries, err := fs.ReadDir(fsys, name)
	if err != nil {
		t.Errorf("readdir %s: %v", name, err)
		return
	}
	if len(entries) != n {
		t.Errorf("readdir %s: got %d entries, want %d", name, len(entries), n)
	}
}

// CheckSymlink verifies that the named symlink has the expected target
// using ReadLink.
func CheckSymlink(t testing.TB, fsys fs.FS, name, expectedTarget string) {
	t.Helper()
	CheckReadLink(t, fsys, name, expectedTarget)
}

// CheckNotExists verifies that the named path does not exist.
func CheckNotExists(t testing.TB, fsys fs.FS, name string) {
	t.Helper()
	f, err := fsys.Open(name)
	if err == nil {
		if err = f.Close(); err != nil {
			t.Errorf("close %s: %v", name, err)
		}
		t.Errorf("expected error opening %s, but succeeded", name)
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("open %s: got %v, want fs.ErrNotExist", name, err)
	}
}

// readLinkFS is the interface for ReadLink and Lstat, matching os.Root.
type readLinkFS interface {
	ReadLink(name string) (string, error)
	Lstat(name string) (fs.FileInfo, error)
}

// CheckReadLink verifies that ReadLink returns the expected target.
func CheckReadLink(t testing.TB, fsys fs.FS, name, target string) {
	t.Helper()
	rlfs, ok := fsys.(readLinkFS)
	if !ok {
		t.Errorf("FS does not implement ReadLink")
		return
	}
	got, err := rlfs.ReadLink(name)
	if err != nil {
		t.Errorf("ReadLink(%s): %v", name, err)
		return
	}
	if got != target {
		t.Errorf("ReadLink(%s) = %q, want %q", name, got, target)
	}
}

// CheckLstat verifies that Lstat returns the expected file type.
func CheckLstat(t testing.TB, fsys fs.FS, name string, wantType fs.FileMode) {
	t.Helper()
	rlfs, ok := fsys.(readLinkFS)
	if !ok {
		t.Errorf("FS does not implement Lstat")
		return
	}
	fi, err := rlfs.Lstat(name)
	if err != nil {
		t.Errorf("Lstat(%s): %v", name, err)
		return
	}
	gotType := fi.Mode() & fs.ModeType
	if gotType != wantType {
		t.Errorf("Lstat(%s) type = %v, want %v", name, gotType, wantType)
	}
}

// CheckReadFile verifies fs.ReadFile returns the expected content.
func CheckReadFile(t testing.TB, fsys fs.FS, name, expected string) {
	t.Helper()
	got, err := fs.ReadFile(fsys, name)
	if err != nil {
		t.Errorf("ReadFile(%s): %v", name, err)
		return
	}
	if string(got) != expected {
		t.Errorf("ReadFile(%s) = %q, want %q", name, got, expected)
	}
}

// CheckReadFileDir verifies that fs.ReadFile fails on a directory.
func CheckReadFileDir(t testing.TB, fsys fs.FS, name string) {
	t.Helper()
	_, err := fs.ReadFile(fsys, name)
	if err == nil {
		t.Errorf("ReadFile(%s) should fail on directory", name)
	} else if !errors.Is(err, erofs.ErrIsDirectory) {
		t.Errorf("ReadFile(%s): got %v, want erofs.ErrIsDirectory", name, err)
	}
}

// CheckReadDirSorted verifies that fs.ReadDir returns entries in sorted order.
func CheckReadDirSorted(t testing.TB, fsys fs.FS, name string) {
	t.Helper()
	entries, err := fs.ReadDir(fsys, name)
	if err != nil {
		t.Errorf("ReadDir(%s): %v", name, err)
		return
	}
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Name() >= entries[i].Name() {
			t.Errorf("ReadDir(%s) not sorted: %q >= %q at index %d", name, entries[i-1].Name(), entries[i].Name(), i)
			return
		}
	}
}

// CheckOpenError verifies that Open returns an error matching target.
func CheckOpenError(t testing.TB, fsys fs.FS, name string, target error) {
	t.Helper()
	f, err := fsys.Open(name)
	if err == nil {
		_ = f.Close()
		t.Errorf("Open(%s): expected error %v, got nil", name, target)
	} else if !errors.Is(err, target) {
		t.Errorf("Open(%s): got %v, want %v", name, err, target)
	}
}

// CheckReadDirFile verifies that ReadDir on a non-directory returns ErrNotDirectory.
func CheckReadDirFile(t testing.TB, fsys fs.FS, name string) {
	t.Helper()
	type readDirFS interface {
		ReadDir(name string) ([]fs.DirEntry, error)
	}
	rdfs, ok := fsys.(readDirFS)
	if !ok {
		t.Errorf("FS does not implement ReadDir")
		return
	}
	_, err := rdfs.ReadDir(name)
	if err == nil {
		t.Errorf("ReadDir(%s) should fail on non-directory", name)
	} else if !errors.Is(err, erofs.ErrNotDirectory) {
		t.Errorf("ReadDir(%s): got %v, want erofs.ErrNotDirectory", name, err)
	}
}

// CheckReadLinkFile verifies that ReadLink on a non-symlink returns fs.ErrInvalid.
func CheckReadLinkFile(t testing.TB, fsys fs.FS, name string) {
	t.Helper()
	rlfs, ok := fsys.(readLinkFS)
	if !ok {
		t.Errorf("FS does not implement ReadLink")
		return
	}
	_, err := rlfs.ReadLink(name)
	if err == nil {
		t.Errorf("ReadLink(%s) should fail on non-symlink", name)
	} else if !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("ReadLink(%s): got %v, want fs.ErrInvalid", name, err)
	}
}

// CheckOpenClose verifies that opening and immediately closing a file
// without reading does not panic.
func CheckOpenClose(t testing.TB, fsys fs.FS, name string) {
	t.Helper()
	f, err := fsys.Open(name)
	if err != nil {
		t.Errorf("Open(%s): %v", name, err)
		return
	}
	if err := f.Close(); err != nil {
		t.Errorf("Close(%s): %v", name, err)
	}
}
