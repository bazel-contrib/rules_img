package erofs_test

import (
	"bytes"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	erofs "github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs/internal/erofstest"
)

// buildAndVerify creates an EROFS image using the writer, validates it with
// fsck.erofs, reads it back with Open, and returns the opened fs.FS.
// Calls t.Fatal on any error.
func buildAndVerify(t *testing.T, build func(w *erofs.Writer)) fs.FS {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.erofs")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	w := erofs.Create(f)
	build(w)
	if err := w.Close(); err != nil {
		t.Fatal("Close:", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	erofstest.FsckErofs(t, path)
	f, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	efs, err := erofs.Open(f)
	if err != nil {
		t.Fatal("Open:", err)
	}
	return efs
}

// FuzzWriterFileContent fuzzes the content of a single file, verifying that
// the written content round-trips through fsck and Open.
func FuzzWriterFileContent(f *testing.F) {
	f.Add([]byte("hello world\n"))
	f.Add([]byte{})
	f.Add(bytes.Repeat([]byte("A"), 4096))     // exactly one block
	f.Add(bytes.Repeat([]byte("B"), 4097))     // one block + 1 byte (inline tail)
	f.Add(bytes.Repeat([]byte("C"), 8192))     // two blocks
	f.Add(bytes.Repeat([]byte("D"), 128*1024)) // large file
	f.Add([]byte{0x00, 0xff, 0x80, 0x7f})      // binary data

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 256*1024 {
			return // keep fuzz runs reasonable
		}

		efs := buildAndVerify(t, func(w *erofs.Writer) {
			file, err := w.Create("/test.bin")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write(data); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		})

		erofstest.CheckFileBytes(t, efs, "test.bin", data)
	})
}

// isValidName returns true if the name is valid for use as an EROFS entry
// name (single path component, no slashes, no empty, no dots-only, no nulls).
func isValidName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if len(name) > 255 {
		return false
	}
	for i := range len(name) {
		if name[i] == '/' || name[i] == 0 {
			return false
		}
	}
	return true
}

// FuzzWriterFileName fuzzes the filename used for a single file.
func FuzzWriterFileName(f *testing.F) {
	f.Add("file.txt")
	f.Add("a")
	f.Add("with spaces")
	f.Add(".hidden")
	f.Add("UPPER")
	f.Add("MiXeD.CaSe")
	f.Add("file-with-dashes_and_underscores")
	f.Add("\xff\xfe")

	f.Fuzz(func(t *testing.T, name string) {
		if !isValidName(name) {
			return
		}

		content := "content-" + name
		efs := buildAndVerify(t, func(w *erofs.Writer) {
			file, err := w.Create("/" + name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write([]byte(content)); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		})

		erofstest.CheckFile(t, efs, name, content)
		erofstest.CheckDirEntries(t, efs, ".", []string{name})
	})
}

// FuzzWriterMultipleFiles fuzzes the number and content of files in a flat directory.
func FuzzWriterMultipleFiles(f *testing.F) {
	f.Add(uint8(0), []byte("hello"))
	f.Add(uint8(1), []byte(""))
	f.Add(uint8(5), []byte("short"))
	f.Add(uint8(20), []byte("data"))
	f.Add(uint8(50), []byte("x"))

	f.Fuzz(func(t *testing.T, count uint8, baseContent []byte) {
		if len(baseContent) > 512 {
			baseContent = baseContent[:512]
		}
		n := int(count)
		if n > 50 {
			return
		}

		type entry struct {
			name    string
			content []byte
		}
		entries := make([]entry, n)
		for i := range n {
			suffix := fmt.Appendf(nil, "-%d", i)
			content := make([]byte, len(baseContent)+len(suffix))
			copy(content, baseContent)
			copy(content[len(baseContent):], suffix)
			entries[i] = entry{
				name:    fmt.Sprintf("file%03d.txt", i),
				content: content,
			}
		}

		efs := buildAndVerify(t, func(w *erofs.Writer) {
			for _, e := range entries {
				file, err := w.Create("/" + e.name)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.Write(e.content); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})

		for _, e := range entries {
			erofstest.CheckFileBytes(t, efs, e.name, e.content)
		}

		dirEntries, err := fs.ReadDir(efs, ".")
		if err != nil {
			t.Fatal(err)
		}
		if len(dirEntries) != n {
			t.Fatalf("got %d entries, want %d", len(dirEntries), n)
		}
	})
}

// FuzzWriterDirectoryTree fuzzes nested directory structures with files.
func FuzzWriterDirectoryTree(f *testing.F) {
	f.Add(uint8(1), uint8(1), []byte("data"))
	f.Add(uint8(3), uint8(2), []byte("content"))
	f.Add(uint8(5), uint8(5), []byte(""))
	f.Add(uint8(10), uint8(0), []byte("x"))

	f.Fuzz(func(t *testing.T, depth uint8, filesPerDir uint8, content []byte) {
		d := int(depth)
		fpd := int(filesPerDir)
		if d > 10 || fpd > 10 || len(content) > 1024 {
			return
		}

		type entry struct {
			path    string
			content string
		}
		var entries []entry
		var dirs []string

		dirPath := ""
		for level := range d {
			seg := fmt.Sprintf("d%d", level)
			if dirPath == "" {
				dirPath = seg
			} else {
				dirPath = dirPath + "/" + seg
			}
			dirs = append(dirs, dirPath)

			for fi := range fpd {
				fpath := fmt.Sprintf("%s/f%d.txt", dirPath, fi)
				fcontent := fmt.Sprintf("%s-%d-%d", content, level, fi)
				entries = append(entries, entry{path: fpath, content: fcontent})
			}
		}

		efs := buildAndVerify(t, func(w *erofs.Writer) {
			for _, dir := range dirs {
				if err := w.Mkdir("/"+dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, e := range entries {
				file, err := w.Create("/" + e.path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.Write([]byte(e.content)); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})

		for _, e := range entries {
			erofstest.CheckFile(t, efs, e.path, e.content)
		}

		for _, dir := range dirs {
			fi, err := fs.Stat(efs, dir)
			if err != nil {
				t.Fatalf("stat %s: %v", dir, err)
			}
			if !fi.IsDir() {
				t.Fatalf("%s: not a directory", dir)
			}
		}
	})
}

// FuzzWriterSymlink fuzzes symlink creation with various targets.
func FuzzWriterSymlink(f *testing.F) {
	f.Add("target.txt", "link")
	f.Add("../escape", "link")
	f.Add("/absolute/path", "mylink")
	f.Add("a/b/c", "deep-link")
	f.Add(".", "dot-link")
	f.Add("..", "dotdot-link")

	f.Fuzz(func(t *testing.T, target, linkName string) {
		if !isValidName(linkName) || target == "" || len(target) > 255 {
			return
		}
		// Target must not contain null bytes.
		for i := range len(target) {
			if target[i] == 0 {
				return
			}
		}

		efs := buildAndVerify(t, func(w *erofs.Writer) {
			if err := w.Symlink(target, "/"+linkName); err != nil {
				t.Fatal(err)
			}
		})

		erofstest.CheckSymlink(t, efs, linkName, target)
	})
}

// FuzzWriterMetadata fuzzes file metadata (permissions, uid, gid, mtime).
func FuzzWriterMetadata(f *testing.F) {
	f.Add(uint16(0o644), uint32(0), uint32(0), uint64(0), uint32(0))
	f.Add(uint16(0o755), uint32(1000), uint32(2000), uint64(1700000000), uint32(123456789))
	f.Add(uint16(0o777), uint32(math.MaxUint32), uint32(math.MaxUint32), uint64(math.MaxUint32), uint32(999999999))
	f.Add(uint16(0o000), uint32(65534), uint32(65534), uint64(1), uint32(0))

	f.Fuzz(func(t *testing.T, perm uint16, uid, gid uint32, mtimeSec uint64, mtimeNs uint32) {
		perm &= 0o777 // only rwx bits; Perm() returns bottom 9 bits
		if mtimeNs >= 1e9 {
			mtimeNs %= 1e9
		}
		// Cap to int-safe range so int(uid)/int(gid) don't overflow on
		// 32-bit platforms and time.Unix receives a non-negative value.
		if uid > math.MaxInt32 {
			uid = math.MaxInt32
		}
		if gid > math.MaxInt32 {
			gid = math.MaxInt32
		}
		if mtimeSec > math.MaxInt64 {
			mtimeSec = math.MaxInt64
		}

		efs := buildAndVerify(t, func(w *erofs.Writer) {
			file, err := w.Create("/file.txt")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write([]byte("content")); err != nil {
				t.Fatal(err)
			}
			if err := file.Chmod(fs.FileMode(perm)); err != nil {
				t.Fatal(err)
			}
			if err := file.Chown(int(uid), int(gid)); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := w.Chtimes("/file.txt", time.Time{}, time.Unix(int64(mtimeSec), int64(mtimeNs))); err != nil {
				t.Fatal(err)
			}
		})

		st := erofstest.Stat(t, efs, "file.txt")
		if st.Mode.Perm() != fs.FileMode(perm) {
			t.Errorf("perm: got %o, want %o", st.Mode.Perm(), perm)
		}
		if st.UID != uid {
			t.Errorf("uid: got %d, want %d", st.UID, uid)
		}
		if st.GID != gid {
			t.Errorf("gid: got %d, want %d", st.GID, gid)
		}
	})
}

// FuzzWriterXattr fuzzes extended attributes.
func FuzzWriterXattr(f *testing.F) {
	f.Add("user.key", "value")
	f.Add("user.test", "")
	f.Add("user.long", string(bytes.Repeat([]byte("v"), 256)))
	f.Add("security.selinux", "unconfined_u:object_r:default_t:s0")
	f.Add("trusted.overlay.opaque", "y")

	f.Fuzz(func(t *testing.T, key, value string) {
		if key == "" || len(key) > 255 || len(value) > 65535 {
			return
		}
		// Keys and values must not contain null bytes.
		for i := range len(key) {
			if key[i] == 0 {
				return
			}
		}
		for i := range len(value) {
			if value[i] == 0 {
				return
			}
		}

		efs := buildAndVerify(t, func(w *erofs.Writer) {
			file, err := w.Create("/file.txt")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write([]byte("content")); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := w.Setxattr("/file.txt", key, value); err != nil {
				t.Fatal(err)
			}
		})

		erofstest.CheckXattrs(t, efs, "file.txt", map[string]string{key: value})
	})
}

// FuzzWriterMixed builds an image with a mix of entry types determined by
// the fuzz input, then validates with fsck and reads back with Open.
func FuzzWriterMixed(f *testing.F) {
	// Seed: nFiles, nDirs, nSymlinks, fileSize, content byte
	f.Add(uint8(3), uint8(2), uint8(1), uint16(100), byte('A'))
	f.Add(uint8(0), uint8(0), uint8(0), uint16(0), byte(0))
	f.Add(uint8(10), uint8(5), uint8(3), uint16(4096), byte('Z'))
	f.Add(uint8(50), uint8(10), uint8(10), uint16(8192), byte(0xff))

	f.Fuzz(func(t *testing.T, nFiles, nDirs, nSymlinks uint8, fileSize uint16, fill byte) {
		nf := int(nFiles)
		nd := int(nDirs)
		ns := int(nSymlinks)
		if nf > 100 || nd > 50 || ns > 50 || fileSize > 8192 {
			return
		}
		fsize := int(fileSize)

		type fileEntry struct {
			name    string
			content []byte
		}
		var files []fileEntry
		var dirNames []string
		var symlinks []struct{ name, target string }

		efs := buildAndVerify(t, func(w *erofs.Writer) {
			// Create directories.
			for i := range nd {
				name := fmt.Sprintf("dir%03d", i)
				dirNames = append(dirNames, name)
				if err := w.Mkdir("/"+name, 0o755); err != nil {
					t.Fatal(err)
				}
			}

			// Create files (some in dirs if dirs exist).
			for i := range nf {
				var name string
				if nd > 0 && i%2 == 0 {
					name = fmt.Sprintf("%s/file%03d.dat", dirNames[i%nd], i)
				} else {
					name = fmt.Sprintf("file%03d.dat", i)
				}
				content := bytes.Repeat([]byte{fill ^ byte(i)}, fsize)
				files = append(files, fileEntry{name: name, content: content})

				file, err := w.Create("/" + name)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.Write(content); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}

			// Create symlinks.
			for i := range ns {
				linkName := fmt.Sprintf("link%03d", i)
				var target string
				if len(files) > 0 {
					target = files[i%len(files)].name
				} else {
					target = "nonexistent"
				}
				symlinks = append(symlinks, struct{ name, target string }{linkName, target})
				if err := w.Symlink(target, "/"+linkName); err != nil {
					t.Fatal(err)
				}
			}
		})

		// Verify all files read back correctly.
		for _, fe := range files {
			erofstest.CheckFileBytes(t, efs, fe.name, fe.content)
		}

		// Verify directories exist.
		for _, d := range dirNames {
			fi, err := fs.Stat(efs, d)
			if err != nil {
				t.Fatalf("stat dir %s: %v", d, err)
			}
			if !fi.IsDir() {
				t.Fatalf("%s: not a directory", d)
			}
		}

		// Verify symlinks.
		for _, sl := range symlinks {
			erofstest.CheckSymlink(t, efs, sl.name, sl.target)
		}

		// Walk entire tree — must not panic or error.
		count := 0
		if err := fs.WalkDir(efs, ".", func(_ string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			count++
			if count > 10000 {
				return fs.SkipAll
			}
			return nil
		}); err != nil {
			t.Fatalf("WalkDir: %v", err)
		}
	})
}

// FuzzWriterCopyFrom fuzzes CopyFrom with a MapFS source of varying structure.
func FuzzWriterCopyFrom(f *testing.F) {
	f.Add(uint8(3), uint8(1), uint16(100), byte('X'))
	f.Add(uint8(0), uint8(0), uint16(0), byte(0))
	f.Add(uint8(20), uint8(5), uint16(5000), byte(0xAB))

	f.Fuzz(func(t *testing.T, nFiles, nDirs uint8, fileSize uint16, fill byte) {
		nf := int(nFiles)
		nd := int(nDirs)
		if nf > 50 || nd > 20 || fileSize > 8192 {
			return
		}

		src := make(fstest.MapFS)
		type entry struct {
			name    string
			content []byte
		}
		var entries []entry

		for i := range nd {
			dirName := fmt.Sprintf("dir%03d", i)
			src[dirName] = &fstest.MapFile{Mode: fs.ModeDir | 0o755}
		}

		for i := range nf {
			var name string
			if nd > 0 && i%2 == 0 {
				name = fmt.Sprintf("dir%03d/f%03d.txt", i%nd, i)
			} else {
				name = fmt.Sprintf("f%03d.txt", i)
			}
			content := bytes.Repeat([]byte{fill ^ byte(i)}, int(fileSize))
			src[name] = &fstest.MapFile{Data: content, Mode: 0o644}
			entries = append(entries, entry{name: name, content: content})
		}

		efs := buildAndVerify(t, func(w *erofs.Writer) {
			if err := w.CopyFrom(src); err != nil {
				t.Fatal("CopyFrom:", err)
			}
		})

		for _, e := range entries {
			erofstest.CheckFileBytes(t, efs, e.name, e.content)
		}
	})
}

// FuzzWriterImplicitDirs fuzzes deeply nested paths to exercise implicit
// directory creation.
func FuzzWriterImplicitDirs(f *testing.F) {
	f.Add(uint8(1), "file.txt", []byte("data"))
	f.Add(uint8(5), "deep.bin", []byte{0xff})
	f.Add(uint8(10), "end", []byte(""))

	f.Fuzz(func(t *testing.T, depth uint8, baseName string, content []byte) {
		d := int(depth)
		if d > 15 || !isValidName(baseName) || len(content) > 1024 {
			return
		}

		// Build a deeply nested path.
		path := ""
		var dirs []string
		for i := range d {
			seg := fmt.Sprintf("d%d", i)
			if path == "" {
				path = seg
			} else {
				path = path + "/" + seg
			}
			dirs = append(dirs, path)
		}
		var filePath string
		if path == "" {
			filePath = baseName
		} else {
			filePath = path + "/" + baseName
		}

		efs := buildAndVerify(t, func(w *erofs.Writer) {
			file, err := w.Create("/" + filePath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write(content); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		})

		erofstest.CheckFileBytes(t, efs, filePath, content)

		for _, dir := range dirs {
			fi, err := fs.Stat(efs, dir)
			if err != nil {
				t.Fatalf("stat implicit dir %s: %v", dir, err)
			}
			if !fi.IsDir() {
				t.Fatalf("%s: not a directory", dir)
			}
		}
	})
}

// FuzzWriterEmpty verifies that creating images with only directories
// (no regular files) produces valid images.
func FuzzWriterEmpty(f *testing.F) {
	f.Add(uint8(0))
	f.Add(uint8(1))
	f.Add(uint8(5))
	f.Add(uint8(20))

	f.Fuzz(func(t *testing.T, nDirs uint8) {
		nd := int(nDirs)
		if nd > 50 {
			return
		}

		var dirNames []string
		efs := buildAndVerify(t, func(w *erofs.Writer) {
			for i := range nd {
				name := fmt.Sprintf("dir%03d", i)
				dirNames = append(dirNames, name)
				if err := w.Mkdir("/"+name, 0o755); err != nil {
					t.Fatal(err)
				}
			}
		})

		entries, err := fs.ReadDir(efs, ".")
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != nd {
			t.Fatalf("got %d entries, want %d", len(entries), nd)
		}
		for _, e := range entries {
			if !e.IsDir() {
				t.Fatalf("%s: expected directory", e.Name())
			}
		}
	})
}
