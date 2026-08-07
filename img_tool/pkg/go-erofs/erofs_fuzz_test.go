package erofs_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	erofs "github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs/internal/erofstest"
)

// fuzzImage holds a pre-built erofs image and the list of valid paths it contains.
type fuzzImage struct {
	fsys  fs.FS
	files []string // valid file paths (no leading slash)
	dirs  []string // valid directory paths (no leading slash)
}

var (
	fuzzFlat     fuzzImage
	fuzzFlatOnce sync.Once

	fuzzNested     fuzzImage
	fuzzNestedOnce sync.Once

	fuzzDeep     fuzzImage
	fuzzDeepOnce sync.Once

	fuzzWide     fuzzImage
	fuzzWideOnce sync.Once
)

func initFuzzFlat(t testing.TB) fuzzImage {
	fuzzFlatOnce.Do(func() {
		tc := erofstest.TarContext{}.WithModTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

		// Flat: all files in root directory, no subdirectories.
		var entries []erofstest.WriterToTar
		var files []string
		for i := range 50 {
			name := fmt.Sprintf("file%03d.txt", i)
			content := bytes.Repeat([]byte{byte(i)}, (i+1)*100)
			entries = append(entries, tc.File("/"+name, content, 0644))
			files = append(files, name)
		}
		entries = append(entries, tc.File("/empty", []byte{}, 0644))
		files = append(files, "empty")

		fuzzFlat = buildFuzzImage(t, entries, files, []string{"."})
	})
	skipIfFuzzImageUnavailable(t, fuzzFlat)
	return fuzzFlat
}

func initFuzzNested(t testing.TB) fuzzImage {
	fuzzNestedOnce.Do(func() {
		tc := erofstest.TarContext{}.WithModTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

		// Nested: 2-level directory structure with files at each level.
		var entries []erofstest.WriterToTar
		var files, dirs []string

		entries = append(entries, tc.File("/root.txt", []byte("root"), 0644))
		files = append(files, "root.txt")

		for i := range 10 {
			dirName := fmt.Sprintf("dir%d", i)
			entries = append(entries, tc.Dir("/"+dirName, 0755))
			dirs = append(dirs, dirName)

			for j := range 5 {
				name := fmt.Sprintf("%s/f%d.txt", dirName, j)
				entries = append(entries, tc.File("/"+name, fmt.Appendf(nil, "content-%d-%d", i, j), 0644))
				files = append(files, name)
			}
		}

		fuzzNested = buildFuzzImage(t, entries, files, append([]string{"."}, dirs...))
	})
	skipIfFuzzImageUnavailable(t, fuzzNested)
	return fuzzNested
}

func initFuzzDeep(t testing.TB) fuzzImage {
	fuzzDeepOnce.Do(func() {
		tc := erofstest.TarContext{}.WithModTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

		// Deep: 10-level nested directory chain with files at each level.
		var entries []erofstest.WriterToTar
		var files, dirs []string

		path := ""
		for depth := range 10 {
			seg := fmt.Sprintf("d%d", depth)
			if path == "" {
				path = seg
			} else {
				path = path + "/" + seg
			}
			entries = append(entries, tc.Dir("/"+path, 0755))
			dirs = append(dirs, path)

			for i := range 3 {
				fpath := fmt.Sprintf("%s/file%d.dat", path, i)
				content := bytes.Repeat([]byte{byte(depth*3 + i)}, 512)
				entries = append(entries, tc.File("/"+fpath, content, 0644))
				files = append(files, fpath)
			}
		}

		// Add a symlink at a mid-level pointing to a deeper file.
		entries = append(entries, tc.Symlink("d4/d5/file0.dat", "/d0/d1/d2/d3/link"))
		files = append(files, "d0/d1/d2/d3/link")

		fuzzDeep = buildFuzzImage(t, entries, files, append([]string{"."}, dirs...))
	})
	skipIfFuzzImageUnavailable(t, fuzzDeep)
	return fuzzDeep
}

func initFuzzWide(t testing.TB) fuzzImage {
	fuzzWideOnce.Do(func() {
		tc := erofstest.TarContext{}.WithModTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

		// Wide: many directories at the same level (stress directory lookup).
		var entries []erofstest.WriterToTar
		var files, dirs []string

		for i := range 200 {
			dirName := fmt.Sprintf("pkg%03d", i)
			entries = append(entries, tc.Dir("/"+dirName, 0755))
			dirs = append(dirs, dirName)

			fname := fmt.Sprintf("%s/main.go", dirName)
			entries = append(entries, tc.File("/"+fname, fmt.Appendf(nil, "package pkg%03d\n", i), 0644))
			files = append(files, fname)
		}

		fuzzWide = buildFuzzImage(t, entries, files, append([]string{"."}, dirs...))
	})
	skipIfFuzzImageUnavailable(t, fuzzWide)
	return fuzzWide
}

// skipIfFuzzImageUnavailable handles the edge case where a previous fuzz
// target already triggered the shared sync.Once and was skipped (e.g.
// mkfs.erofs is missing). Once.Do marks itself done even when its function
// calls t.Skipf via runtime.Goexit, leaving the shared fuzzImage zero. A
// later target's seeds would then dereference a nil fs.FS and panic, so any
// remaining caller skips here instead.
func skipIfFuzzImageUnavailable(t testing.TB, img fuzzImage) {
	t.Helper()
	if img.fsys == nil {
		t.Skipf("fuzz image unavailable (mkfs.erofs missing or earlier init skipped)")
	}
}

func buildFuzzImage(t testing.TB, entries []erofstest.WriterToTar, files, dirs []string) fuzzImage {
	t.Helper()

	if _, err := erofstest.CheckMkfsVersion("1.0"); err != nil {
		t.Skipf("skipping: %v", err)
	}

	wt := erofstest.TarAll(entries...)
	tarStream := erofstest.TarFromWriterTo(wt)
	defer func() { _ = tarStream.Close() }()

	// Use os.MkdirTemp instead of t.TempDir() because the opened file
	// handle must outlive the test that calls buildFuzzImage (the fuzzImage
	// is shared across tests via sync.Once). On Windows, t.TempDir()'s
	// automatic cleanup fails because the file is still open.
	dir, err := os.MkdirTemp("", "fuzz-erofs-*")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "fuzz.erofs")
	if err := erofstest.ConvertTarErofs(context.Background(), tarStream, path, "", nil); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	fsys, err := erofs.Open(f)
	if err != nil {
		t.Fatal(err)
	}

	return fuzzImage{fsys: fsys, files: files, dirs: dirs}
}

// pathSeeds returns interesting path strings to seed fuzzing beyond the
// valid paths in the image.
func pathSeeds() []string {
	return []string{
		"nonexistent",
		"../escape",
		"",
		".",
		"/",
		"a/b/c/d/e/f/g",
		"dir0/nonexistent/deep",
		strings.Repeat("a", 256),
		strings.Repeat("a/", 128),
		"dir0/../dir0/f0.txt",
		"dir0/./f0.txt",
		".hidden",
		"file with spaces",
		"file\x00null",
		"file\nnewline",
		"file\ttab",
		"\xff\xfe",
		"UPPER/lower/MiXeD",
		"....",
		"dir0//f0.txt",
		"a/b/../../a/b",
	}
}

// fuzzOpen exercises Open with arbitrary path strings.
// Valid paths must succeed; arbitrary paths must not panic.
func fuzzOpen(f *testing.F, img fuzzImage) {
	for _, p := range img.files {
		f.Add(p)
	}
	for _, d := range img.dirs {
		f.Add(d)
	}
	for _, s := range pathSeeds() {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, path string) {
		file, err := img.fsys.Open(path)
		if err != nil {
			return
		}
		defer func() { _ = file.Close() }()

		// If we opened it, Stat must not panic.
		fi, err := file.Stat()
		if err != nil {
			return
		}

		if fi.IsDir() {
			// ReadDir must not panic.
			rdf, ok := file.(fs.ReadDirFile)
			if !ok {
				return
			}
			_, _ = rdf.ReadDir(-1)
		} else {
			// Read must not panic.
			buf := make([]byte, 4096)
			for {
				_, err := file.Read(buf)
				if err != nil {
					break
				}
			}
		}
	})
}

// fuzzReadFile exercises ReadFile with arbitrary paths.
func fuzzReadFile(f *testing.F, img fuzzImage) {
	for _, p := range img.files {
		f.Add(p)
	}
	for _, s := range pathSeeds() {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, path string) {
		data, err := fs.ReadFile(img.fsys, path)
		if err != nil {
			return
		}
		// If successful, data must not be nil (may be empty).
		if data == nil {
			t.Fatal("ReadFile returned nil data without error")
		}
	})
}

// fuzzStat exercises Stat with arbitrary paths.
func fuzzStat(f *testing.F, img fuzzImage) {
	for _, p := range img.files {
		f.Add(p)
	}
	for _, d := range img.dirs {
		f.Add(d)
	}
	for _, s := range pathSeeds() {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, path string) {
		fi, err := fs.Stat(img.fsys, path)
		if err != nil {
			return
		}
		// Basic consistency: size non-negative.
		// Note: fi.Name() == "" is valid for "." (root directory).
		if fi.Size() < 0 {
			t.Fatal("Stat returned negative size")
		}
		// Mode must be consistent with IsDir.
		if fi.IsDir() != fi.Mode().IsDir() {
			t.Fatal("IsDir inconsistent with Mode().IsDir()")
		}
	})
}

// fuzzReadDir exercises ReadDir with arbitrary paths.
func fuzzReadDir(f *testing.F, img fuzzImage) {
	for _, d := range img.dirs {
		f.Add(d)
	}
	for _, s := range pathSeeds() {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, path string) {
		entries, err := fs.ReadDir(img.fsys, path)
		if err != nil {
			return
		}
		// Entries must be sorted per fs.ReadDir contract.
		for i := 1; i < len(entries); i++ {
			if entries[i-1].Name() >= entries[i].Name() {
				t.Fatalf("ReadDir entries not sorted: %q >= %q", entries[i-1].Name(), entries[i].Name())
			}
		}
		// Each entry must have a valid name and consistent type.
		for _, e := range entries {
			if e.Name() == "" {
				t.Fatal("ReadDir entry with empty name")
			}
			info, err := e.Info()
			if err != nil {
				t.Fatalf("entry %q Info failed: %v", e.Name(), err)
			}
			if info.Name() != e.Name() {
				t.Fatalf("entry name %q != info name %q", e.Name(), info.Name())
			}
			if e.IsDir() != info.IsDir() {
				t.Fatalf("entry %q IsDir mismatch", e.Name())
			}
		}
	})
}

// fuzzWalk exercises fs.WalkDir from an arbitrary starting path.
func fuzzWalk(f *testing.F, img fuzzImage) {
	for _, d := range img.dirs {
		f.Add(d)
	}
	for _, s := range pathSeeds() {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, root string) {
		count := 0
		_ = fs.WalkDir(img.fsys, root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			count++
			if count > 10000 {
				return fs.SkipAll
			}
			// Info must not panic and must be consistent.
			info, infoErr := d.Info()
			if infoErr != nil {
				return nil
			}
			if info.Name() != d.Name() {
				t.Errorf("walk %q: entry name %q != info name %q", path, d.Name(), info.Name())
			}
			return nil
		})
	})
}

// --- Flat structure (depth=0): all files in root ---

func FuzzFlatOpen(f *testing.F) {
	fuzzOpen(f, initFuzzFlat(f))
}

func FuzzFlatReadFile(f *testing.F) {
	fuzzReadFile(f, initFuzzFlat(f))
}

func FuzzFlatStat(f *testing.F) {
	fuzzStat(f, initFuzzFlat(f))
}

func FuzzFlatReadDir(f *testing.F) {
	fuzzReadDir(f, initFuzzFlat(f))
}

// --- Nested structure (depth=2): directories with files at each level ---

func FuzzNestedOpen(f *testing.F) {
	fuzzOpen(f, initFuzzNested(f))
}

func FuzzNestedReadFile(f *testing.F) {
	fuzzReadFile(f, initFuzzNested(f))
}

func FuzzNestedStat(f *testing.F) {
	fuzzStat(f, initFuzzNested(f))
}

func FuzzNestedReadDir(f *testing.F) {
	fuzzReadDir(f, initFuzzNested(f))
}

func FuzzNestedWalk(f *testing.F) {
	fuzzWalk(f, initFuzzNested(f))
}

// --- Deep structure (depth=10): long directory chain ---

func FuzzDeepOpen(f *testing.F) {
	fuzzOpen(f, initFuzzDeep(f))
}

func FuzzDeepReadFile(f *testing.F) {
	fuzzReadFile(f, initFuzzDeep(f))
}

func FuzzDeepStat(f *testing.F) {
	fuzzStat(f, initFuzzDeep(f))
}

func FuzzDeepReadDir(f *testing.F) {
	fuzzReadDir(f, initFuzzDeep(f))
}

func FuzzDeepWalk(f *testing.F) {
	fuzzWalk(f, initFuzzDeep(f))
}

// --- Wide structure (depth=1): many directories at same level ---

func FuzzWideOpen(f *testing.F) {
	fuzzOpen(f, initFuzzWide(f))
}

func FuzzWideReadFile(f *testing.F) {
	fuzzReadFile(f, initFuzzWide(f))
}

func FuzzWideStat(f *testing.F) {
	fuzzStat(f, initFuzzWide(f))
}

func FuzzWideReadDir(f *testing.F) {
	fuzzReadDir(f, initFuzzWide(f))
}

func FuzzWideWalk(f *testing.F) {
	fuzzWalk(f, initFuzzWide(f))
}

// fuzzPartialReadDir exercises partial ReadDir calls (ReadDir(n)) with
// arbitrary positive n values across multiple calls on the same directory handle.
// This enforces the fs.ReadDirFile contract: ReadDir(n) must eventually
// return io.EOF when the directory is exhausted.
func fuzzPartialReadDir(f *testing.F, img fuzzImage) {
	for _, d := range img.dirs {
		f.Add(d, 1)
		f.Add(d, 2)
		f.Add(d, 3)
		f.Add(d, 5)
		f.Add(d, 10)
		f.Add(d, 100)
		f.Add(d, 1000)
	}

	f.Fuzz(func(t *testing.T, path string, n int) {
		if n < 1 {
			n = 1
		}
		if n > 1000 {
			n = 1000
		}

		file, err := img.fsys.Open(path)
		if err != nil {
			return
		}
		defer func() { _ = file.Close() }()

		fi, err := file.Stat()
		if err != nil || !fi.IsDir() {
			return
		}

		rdf, ok := file.(fs.ReadDirFile)
		if !ok {
			return
		}

		var all []fs.DirEntry
		sawEOF := false
		for {
			entries, err := rdf.ReadDir(n)
			all = append(all, entries...)
			if err == io.EOF {
				sawEOF = true
				break
			}
			if err != nil {
				return
			}
			if len(all) > 10000 {
				return
			}
		}

		// Per fs.ReadDirFile contract, ReadDir(n>0) must return io.EOF
		// when the directory is exhausted.
		if !sawEOF {
			t.Fatal("ReadDir(n) never returned io.EOF")
		}

		// After EOF, another call must also return io.EOF with no entries.
		extra, err := rdf.ReadDir(n)
		if len(extra) != 0 {
			t.Fatalf("ReadDir after EOF returned %d entries", len(extra))
		}
		if err != io.EOF {
			t.Fatalf("ReadDir after EOF returned err=%v, want io.EOF", err)
		}

		// Compare partial read result against a full ReadDir(-1).
		fullFile, err := img.fsys.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = fullFile.Close() }()
		fullEntries, err := fullFile.(fs.ReadDirFile).ReadDir(-1)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != len(fullEntries) {
			t.Fatalf("partial ReadDir got %d entries, full ReadDir got %d", len(all), len(fullEntries))
		}
		for i := range all {
			if all[i].Name() != fullEntries[i].Name() {
				t.Fatalf("entry %d: partial=%q full=%q", i, all[i].Name(), fullEntries[i].Name())
			}
		}

		// Verify sorted order across all partial reads.
		for i := 1; i < len(all); i++ {
			if all[i-1].Name() >= all[i].Name() {
				t.Fatalf("partial ReadDir entries not sorted: %q >= %q", all[i-1].Name(), all[i].Name())
			}
		}
	})
}

func FuzzNestedPartialReadDir(f *testing.F) {
	fuzzPartialReadDir(f, initFuzzNested(f))
}

func FuzzWidePartialReadDir(f *testing.F) {
	fuzzPartialReadDir(f, initFuzzWide(f))
}

func FuzzFlatPartialReadDir(f *testing.F) {
	fuzzPartialReadDir(f, initFuzzFlat(f))
}

func FuzzDeepPartialReadDir(f *testing.F) {
	fuzzPartialReadDir(f, initFuzzDeep(f))
}

// fuzzReadAfterClose verifies that operating on a closed file/dir does
// not panic.
func fuzzReadAfterClose(f *testing.F, img fuzzImage) {
	for _, p := range img.files {
		f.Add(p)
	}
	for _, d := range img.dirs {
		f.Add(d)
	}
	for _, s := range pathSeeds() {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, path string) {
		file, err := img.fsys.Open(path)
		if err != nil {
			return
		}
		_ = file.Close()

		// These must not panic; errors are expected.
		_, _ = file.Stat()
		_, _ = file.Read(make([]byte, 1))
		if rdf, ok := file.(fs.ReadDirFile); ok {
			_, _ = rdf.ReadDir(-1)
		}
	})
}

func FuzzFlatReadAfterClose(f *testing.F) {
	fuzzReadAfterClose(f, initFuzzFlat(f))
}

func FuzzDeepReadAfterClose(f *testing.F) {
	fuzzReadAfterClose(f, initFuzzDeep(f))
}

// fuzzOpenStatReadConsistency verifies that Stat via Open().Stat() and
// fs.Stat return consistent information for the same path.
func fuzzOpenStatConsistency(f *testing.F, img fuzzImage) {
	for _, p := range img.files {
		f.Add(p)
	}
	for _, d := range img.dirs {
		f.Add(d)
	}
	for _, s := range pathSeeds() {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, path string) {
		// fs.Stat path
		fsStat, fsErr := fs.Stat(img.fsys, path)

		// Open + Stat path
		file, openErr := img.fsys.Open(path)
		if openErr != nil {
			if fsErr == nil {
				t.Fatalf("Open(%q) failed but fs.Stat succeeded", path)
			}
			return
		}
		defer func() { _ = file.Close() }()

		fileStat, fileErr := file.Stat()

		if fsErr != nil && fileErr == nil {
			t.Fatalf("fs.Stat(%q) failed but file.Stat() succeeded", path)
		}
		if fsErr == nil && fileErr != nil {
			t.Fatalf("fs.Stat(%q) succeeded but file.Stat() failed: %v", path, fileErr)
		}
		if fsErr != nil {
			return
		}

		// Both succeeded — compare.
		if fsStat.Size() != fileStat.Size() {
			t.Fatalf("size mismatch for %q: fs.Stat=%d file.Stat=%d", path, fsStat.Size(), fileStat.Size())
		}
		if fsStat.Mode() != fileStat.Mode() {
			t.Fatalf("mode mismatch for %q: fs.Stat=%v file.Stat=%v", path, fsStat.Mode(), fileStat.Mode())
		}
		if fsStat.IsDir() != fileStat.IsDir() {
			t.Fatalf("IsDir mismatch for %q", path)
		}
	})
}

func FuzzNestedOpenStatConsistency(f *testing.F) {
	fuzzOpenStatConsistency(f, initFuzzNested(f))
}

func FuzzDeepOpenStatConsistency(f *testing.F) {
	fuzzOpenStatConsistency(f, initFuzzDeep(f))
}

// fuzzReadFileSize verifies that ReadFile returns data whose length
// matches the size reported by Stat.
func fuzzReadFileSize(f *testing.F, img fuzzImage) {
	for _, p := range img.files {
		f.Add(p)
	}
	for _, s := range pathSeeds() {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, path string) {
		fi, err := fs.Stat(img.fsys, path)
		if err != nil || fi.IsDir() {
			return
		}

		data, err := fs.ReadFile(img.fsys, path)
		if err != nil {
			return
		}

		if int64(len(data)) != fi.Size() {
			t.Fatalf("ReadFile(%q) returned %d bytes but Stat reports size %d", path, len(data), fi.Size())
		}
	})
}

func FuzzNestedReadFileSize(f *testing.F) {
	fuzzReadFileSize(f, initFuzzNested(f))
}

func FuzzDeepReadFileSize(f *testing.F) {
	fuzzReadFileSize(f, initFuzzDeep(f))
}

// --- Raw image fuzzing: malformed/corrupt erofs images ---
//
// These tests fuzz the image bytes themselves to find panics, hangs, or
// unbounded allocations in the parser. This is the primary defense against
// a malicious erofs image being used to DoS a service.

// buildMinimalImage creates a small valid erofs image to use as a seed for
// raw image fuzzing. Returns the raw bytes.
func buildMinimalImage(t testing.TB) []byte {
	t.Helper()
	if _, err := erofstest.CheckMkfsVersion("1.0"); err != nil {
		t.Skipf("skipping: %v", err)
	}

	tc := erofstest.TarContext{}.WithModTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	wt := erofstest.TarAll(
		tc.Dir("/dir", 0755),
		tc.File("/dir/hello.txt", []byte("hello world\n"), 0644),
		tc.File("/empty", []byte{}, 0644),
		tc.Symlink("/dir/hello.txt", "/link"),
	)
	tarStream := erofstest.TarFromWriterTo(wt)
	defer func() { _ = tarStream.Close() }()

	path := filepath.Join(t.TempDir(), "minimal.erofs")
	if err := erofstest.ConvertTarErofs(context.Background(), tarStream, path, "", nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// exerciseFS attempts common operations on an fs.FS, recovering from panics.
// It is designed for use with potentially corrupt images where any operation
// may fail. The goal is to ensure nothing panics or hangs.
func exerciseFS(fsys fs.FS) {
	// Open and stat root
	f, err := fsys.Open(".")
	if err != nil {
		return
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return
	}
	if fi.IsDir() {
		if rdf, ok := f.(fs.ReadDirFile); ok {
			entries, _ := rdf.ReadDir(-1)
			_ = f.Close()
			// Try to open each entry (limit to avoid long runs on corrupt data)
			limit := min(len(entries), 16)
			for _, e := range entries[:limit] {
				child, err := fsys.Open(e.Name())
				if err != nil {
					continue
				}
				cfi, err := child.Stat()
				if err != nil {
					_ = child.Close()
					continue
				}
				if !cfi.IsDir() && cfi.Size() > 0 && cfi.Size() < 1<<20 {
					buf := make([]byte, 4096)
					_, _ = child.Read(buf)
				}
				if cfi.IsDir() {
					if rdf2, ok := child.(fs.ReadDirFile); ok {
						_, _ = rdf2.ReadDir(-1)
					}
				}
				_ = child.Close()
			}
		} else {
			_ = f.Close()
		}
	} else {
		if fi.Size() > 0 && fi.Size() < 1<<20 {
			buf := make([]byte, 4096)
			_, _ = f.Read(buf)
		}
		_ = f.Close()
	}

	// Try some known paths
	for _, p := range []string{".", "dir", "dir/hello.txt", "empty", "link"} {
		_ = tryOpen(fsys, p)
	}

	// Walk (with limit)
	count := 0
	_ = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		count++
		if count > 100 {
			return fs.SkipAll
		}
		_, _ = d.Info()
		return nil
	})
}

func tryOpen(fsys fs.FS, path string) error {
	f, err := fsys.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Stat()
	return nil
}

// FuzzImageOpen fuzzes the raw erofs image bytes. The fuzzer mutates the
// bytes of a valid image and we verify that Open + exerciseFS never panic.
func FuzzImageOpen(f *testing.F) {
	seed := buildMinimalImage(f)
	f.Add(seed)

	// Also add a truncated image (just superblock area)
	if len(seed) > 2048 {
		f.Add(seed[:2048])
	}
	// Add a too-small image
	f.Add(seed[:1024])
	// Add garbage
	f.Add(make([]byte, 2048))
	// Add image with zeroed superblock
	zeroed := make([]byte, len(seed))
	copy(zeroed, seed)
	for i := 1024; i < 1024+128 && i < len(zeroed); i++ {
		zeroed[i] = 0
	}
	f.Add(zeroed)

	f.Fuzz(func(t *testing.T, data []byte) {
		r := bytes.NewReader(data)
		fsys, err := erofs.Open(r)
		if err != nil {
			return
		}
		exerciseFS(fsys)
	})
}

// FuzzImageCorruptSuperblock specifically targets superblock parsing by
// mutating only the superblock region of a valid image.
func FuzzImageCorruptSuperblock(f *testing.F) {
	seed := buildMinimalImage(f)

	// Extract just the superblock (128 bytes at offset 1024)
	if len(seed) < 1024+128 {
		f.Skip("seed image too small")
	}
	sb := make([]byte, 128)
	copy(sb, seed[1024:1024+128])
	f.Add(sb)

	// Zeroed superblock with just magic
	withMagic := make([]byte, 128)
	withMagic[0] = 0xe2
	withMagic[1] = 0xe1
	withMagic[2] = 0xf5
	withMagic[3] = 0xe0
	f.Add(withMagic)

	// Superblock with extreme BlkSizeBits values
	for _, bits := range []byte{0, 8, 9, 16, 17, 255} {
		tweaked := make([]byte, 128)
		copy(tweaked, sb)
		tweaked[8] = bits // BlkSizeBits offset in SuperBlock struct
		f.Add(tweaked)
	}

	f.Fuzz(func(t *testing.T, sbBytes []byte) {
		if len(sbBytes) != 128 {
			return
		}
		img := make([]byte, len(seed))
		copy(img, seed)
		copy(img[1024:1024+128], sbBytes)

		r := bytes.NewReader(img)
		fsys, err := erofs.Open(r)
		if err != nil {
			return
		}
		exerciseFS(fsys)
	})
}

// FuzzImageCorruptInode targets inode parsing by mutating inode data
// within a valid image. This is where crafted sizes could cause OOM.
func FuzzImageCorruptInode(f *testing.F) {
	seed := buildMinimalImage(f)

	// Seed with the original image
	f.Add(seed, 0)

	// Corrupt different byte positions after superblock
	for _, off := range []int{0, 32, 64, 128, 256} {
		f.Add(seed, off)
	}

	f.Fuzz(func(t *testing.T, data []byte, corruptOffset int) {
		if len(data) < 2048 {
			return
		}
		// Only corrupt data after the superblock to keep it parseable
		metaStart := 1024 + 128
		if corruptOffset < 0 {
			corruptOffset = 0
		}
		pos := metaStart + (corruptOffset % (len(data) - metaStart))

		// Flip some bytes around the corruption point
		mutated := make([]byte, len(data))
		copy(mutated, data)
		for i := pos; i < pos+8 && i < len(mutated); i++ {
			mutated[i] ^= 0xFF
		}

		r := bytes.NewReader(mutated)
		fsys, err := erofs.Open(r)
		if err != nil {
			return
		}
		exerciseFS(fsys)
	})
}
