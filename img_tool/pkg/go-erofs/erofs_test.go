package erofs_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs/internal/erofstest"
)

func TestErofs(t *testing.T) {
	if _, err := erofstest.CheckMkfsVersion("1.0"); err != nil {
		t.Skipf("skipping: %v", err)
	}

	minChunk := os.Getpagesize()

	for _, tc := range []struct {
		name  string
		test  erofstest.TestCase
		flags []string // extra mkfs.erofs flags
	}{
		{"Basic", erofstest.Basic, nil},
		{"FileSizes", erofstest.FileSizes, nil},
		{"UIDGIDValues", erofstest.UIDGIDValues, nil},
		{"SpecialModeBits", erofstest.SpecialModeBits, nil},
		{"LongXattrs", erofstest.LongXattrs, erofstest.XattrPrefixFlags()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, cc := range []struct {
				name string
				conv erofstest.Converter
			}{
				{"default", erofstest.MkfsErofs(tc.flags...)},
				{fmt.Sprintf("chunk-%d", minChunk), erofstest.MkfsErofs(append(tc.flags, fmt.Sprintf("--chunksize=%d", minChunk))...)},
				{fmt.Sprintf("chunk-%d", minChunk*2), erofstest.MkfsErofs(append(tc.flags, fmt.Sprintf("--chunksize=%d", minChunk*2))...)},
				{"chunk-index", erofstest.MkfsErofsBlobDev(minChunk, tc.flags...)},
			} {
				t.Run(cc.name, func(t *testing.T) {
					tc.test.Run(t, cc.conv)
				})
			}
		})
	}

	// Large file: 256MB+ to exercise chunk index overflow (run once with 4K chunks).
	t.Run("LargeFile", func(t *testing.T) {
		erofstest.LargeFile.Run(t, erofstest.MkfsErofs(fmt.Sprintf("--chunksize=%d", minChunk)))
	})

	// Sparse files require --chunksize and produce images under 1MB
	// despite 30MB of logical content.
	t.Run("SparseFiles", func(t *testing.T) {
		chunkFlag := fmt.Sprintf("--chunksize=%d", minChunk)
		erofstest.SparseFiles.Run(t, erofstest.MkfsErofsMaxSize(1024*1024, chunkFlag))
	})

	// Compression format is unimplemented — verify EroFS returns ErrNotImplemented.
	t.Run("lz4-unimplemented", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("mkfs.erofs compression is not included on Windows")
		}
		tc := erofstest.TarContext{}
		wt := erofstest.TarAll(
			tc.File("/file.txt", []byte("content\n"), 0644),
		)
		tarStream := erofstest.TarFromWriterTo(wt)
		defer func() {
			if err := tarStream.Close(); err != nil {
				t.Fatal(err)
			}
		}()

		path := filepath.Join(t.TempDir(), "compressed.erofs")
		if err := erofstest.ConvertTarErofs(context.Background(), tarStream, path, "", []string{"-zlz4"}); err != nil {
			t.Fatal(err)
		}

		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		}()

		_, err = erofs.Open(f)
		if !errors.Is(err, erofs.ErrNotImplemented) {
			t.Fatalf("expected ErrNotImplemented, got %v", err)
		}
	})
}

func BenchmarkLookup(b *testing.B) {
	if _, err := erofstest.CheckMkfsVersion("1.0"); err != nil {
		b.Skipf("skipping: %v", err)
	}

	tc := erofstest.TarContext{}.WithModTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	// Build a tar with directories of varying sizes.
	lotsOfFiles := make(chan erofstest.WriterToTar)
	go func() {
		for i := range 5000 {
			lotsOfFiles <- tc.File(fmt.Sprintf("/bigdir/%d", i), []byte{}, 0600)
		}
		close(lotsOfFiles)
	}()

	wt := erofstest.TarAll(
		tc.Dir("/a", 0755),
		tc.Dir("/a/b", 0755),
		tc.Dir("/a/b/c", 0755),
		tc.File("/a/b/c/file.txt", []byte("content\n"), 0644),
		tc.Dir("/smalldir", 0755),
		tc.File("/smalldir/file.txt", []byte("content\n"), 0644),
		tc.Dir("/bigdir", 0755),
		erofstest.TarStream(lotsOfFiles),
	)

	fsys := erofstest.MkfsErofs()(b, wt)

	for _, bc := range []struct {
		name string
		path string
	}{
		{"shallow", "smalldir/file.txt"},
		{"deep", "a/b/c/file.txt"},
		{"bigdir-first", "bigdir/0"},
		{"bigdir-last", "bigdir/4999"},
		{"bigdir-notfound", "bigdir/nonexistent"},
	} {
		b.Run(bc.name, func(b *testing.B) {
			for range b.N {
				f, err := fsys.Open(bc.path)
				if err != nil {
					if !errors.Is(err, fs.ErrNotExist) {
						b.Fatal(err)
					}
					continue
				}
				_ = f.Close()
			}
		})
	}
}

// dataRanger is used for type-asserting DataRange() from fs.FileInfo.
type dataRanger interface {
	DataRange() []erofs.DataRange
}

// checkDataRangeCoverage asserts that the DataRange() slices returned by
// Stat() on name cover exactly fileSize bytes, with all positive sizes.
func checkDataRangeCoverage(t *testing.T, fsys fs.FS, name string, fileSize int) {
	t.Helper()
	f, err := fsys.Open(name)
	if err != nil {
		t.Fatalf("Open %s: %v", name, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat %s: %v", name, err)
	}
	if info.Size() != int64(fileSize) {
		t.Fatalf("Size = %d, want %d", info.Size(), fileSize)
	}
	// All erofs.fileInfo values implement DataRange(); no type-assert guard needed.
	dr := info.(dataRanger)
	ranges := dr.DataRange()
	if len(ranges) == 0 {
		t.Fatalf("DataRange() returned nil for non-empty file %s", name)
	}
	var total int64
	for _, r := range ranges {
		if r.Size <= 0 {
			t.Errorf("range has non-positive Size %d: %+v", r.Size, r)
		}
		total += r.Size
	}
	if total != info.Size() {
		t.Errorf("total DataRange size = %d, want %d; ranges = %+v", total, info.Size(), ranges)
	}
}

// TestDataRangeMultiBlockCoverage verifies that DataRange() total size equals
// the file size for multi-block files across the layouts mkfs.erofs produces.
//
// The tar pipeline (all platforms) exercises FlatPlain. The dir-source
// subtest (non-Windows only) exercises FlatInline multi-block, which is the
// layout that contained the headSize bug (inodeData treated as block count
// instead of block address).
func TestDataRangeMultiBlockCoverage(t *testing.T) {
	if _, err := erofstest.CheckMkfsVersion("1.0"); err != nil {
		t.Skipf("skipping: %v", err)
	}

	const blockSize = 4096

	sizes := []struct {
		name     string
		fileSize int
	}{
		{"2-block", blockSize + 100},
		{"3-block", blockSize*2 + 200},
		{"4-block", blockSize*3 + 50},
	}

	// tar pipeline: works on all platforms; mkfs.erofs uses -Enoinline_data
	// so files land in FlatPlain layout (single contiguous range).
	t.Run("tar-pipeline", func(t *testing.T) {
		for _, tc := range sizes {
			t.Run(tc.name, func(t *testing.T) {
				content := bytes.Repeat([]byte("x"), tc.fileSize)
				tarCtx := erofstest.TarContext{}
				wt := erofstest.TarAll(tarCtx.File("/file.bin", content, 0o644))
				fsys := erofstest.MkfsErofs()(t, wt)
				checkDataRangeCoverage(t, fsys, "file.bin", tc.fileSize)
			})
		}
	})

	// dir-source pipeline: mkfs.erofs with a directory argument produces
	// FlatInline layout for files whose tail fits in the inode block, giving
	// multi-block FlatInline files. This is what the headSize fix targets.
	// Skipped on Windows because the cross-compiled mkfs.erofs there only
	// supports --tar input.
	t.Run("dir-source-flatinline", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("mkfs.erofs directory-source mode not available on Windows")
		}
		for _, tc := range sizes {
			t.Run(tc.name, func(t *testing.T) {
				dir := t.TempDir()
				content := bytes.Repeat([]byte("x"), tc.fileSize)
				if err := os.WriteFile(filepath.Join(dir, "file.bin"), content, 0o644); err != nil {
					t.Fatal(err)
				}
				imgPath := filepath.Join(t.TempDir(), "test.erofs")
				out, err := exec.Command("mkfs.erofs", imgPath, dir).CombinedOutput()
				if err != nil {
					t.Fatalf("mkfs.erofs: %s: %v", out, err)
				}
				imgData, err := os.ReadFile(imgPath)
				if err != nil {
					t.Fatal(err)
				}
				fsys, err := erofs.Open(bytes.NewReader(imgData))
				if err != nil {
					t.Fatal("Open:", err)
				}
				checkDataRangeCoverage(t, fsys, "file.bin", tc.fileSize)
			})
		}
	})
}

// TestDataRangeSparseChunkBased verifies that DataRange() correctly represents
// sparse (hole-containing) chunk-based EROFS files.  It uses the existing
// testdata fixture which contains known sparse files.
//
// Invariants checked:
//   - sum(Size) == info.Size() for every regular file.
//   - Hole entries have Offset == -1.
//   - At least one file in the fixture has a hole so we are actually
//     exercising the hole-emission path in buildChunkDataRanges.
func TestDataRangeSparseChunkBased(t *testing.T) {
	// basic-chunk-index.erofs stores file data on a separate blob device.
	metaData, err := os.ReadFile("testdata/basic-chunk-index.erofs")
	if err != nil {
		t.Skipf("skipping: testdata not available: %v", err)
	}
	blobData, err := os.ReadFile("testdata/basic-chunk-index-data.erofs")
	if err != nil {
		t.Skipf("skipping: testdata blob not available: %v", err)
	}

	fsys, err := erofs.Open(bytes.NewReader(metaData),
		erofs.WithExtraDevices(bytes.NewReader(blobData)))
	if err != nil {
		t.Fatal("Open:", err)
	}

	foundHole := false
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := fsys.Open(p)
		if err != nil {
			return fmt.Errorf("Open %s: %w", p, err)
		}
		defer func() { _ = f.Close() }()

		info, err := f.Stat()
		if err != nil {
			return fmt.Errorf("Stat %s: %w", p, err)
		}

		dr := info.(dataRanger)
		ranges := dr.DataRange()

		// Every non-empty regular file must have at least one range.
		if info.Size() > 0 && len(ranges) == 0 {
			t.Errorf("%s: Size=%d but DataRange() returned nil", p, info.Size())
			return nil
		}

		// Invariant: sum(Size) == file size.
		var total int64
		for j, r := range ranges {
			if r.Size <= 0 {
				t.Errorf("%s: DataRange[%d].Size = %d, want > 0", p, j, r.Size)
			}
			total += r.Size
			if r.Offset == -1 {
				foundHole = true
			}
		}
		if total != info.Size() {
			t.Errorf("%s: sum(DataRange.Size) = %d, want %d", p, total, info.Size())
		}
		return nil
	})
	if err != nil {
		t.Fatal("WalkDir:", err)
	}

	if !foundHole {
		t.Error("no hole entries found in fixture — buildChunkDataRanges hole path was not exercised")
	}
}
