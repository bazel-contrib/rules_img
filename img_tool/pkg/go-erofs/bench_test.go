package erofs_test

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	erofs "github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs"
)

const benchTargetSize = 250 * 1024 * 1024

// benchMergeOverlayFraction controls what fraction of the base layer the
// overlay replaces.  1/8 means ~31 MB of overlay on a 250 MB base.
const benchMergeOverlayFraction = 8

// populateBenchDir creates a realistic container-layer directory tree with
// many small config files, medium libraries, large binaries, symlinks, and
// docs totaling roughly targetSize bytes of file content.
func populateBenchDir(b *testing.B, root string, targetSize int64) {
	b.Helper()
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	written := int64(0)

	writeFile := func(name string, size int, mode os.FileMode) {
		p := filepath.Join(root, filepath.FromSlash(name))
		_ = os.MkdirAll(filepath.Dir(p), 0755)
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(i % 251)
		}
		if err := os.WriteFile(p, data, mode); err != nil {
			b.Fatal(err)
		}
		_ = os.Chtimes(p, now, now)
		written += int64(size)
	}

	writeDir := func(name string) {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(p, 0755); err != nil {
			b.Fatal(err)
		}
		_ = os.Chtimes(p, now, now)
	}

	writeSymlink := func(name, target string) {
		p := filepath.Join(root, filepath.FromSlash(name))
		_ = os.MkdirAll(filepath.Dir(p), 0755)
		if err := os.Symlink(target, p); err != nil {
			b.Fatal(err)
		}
	}

	for _, d := range []string{
		"/usr", "/usr/bin", "/usr/lib", "/usr/lib/x86_64-linux-gnu",
		"/usr/share", "/usr/share/doc", "/usr/share/man",
		"/etc", "/etc/apt", "/var", "/var/lib", "/var/cache",
	} {
		writeDir(d)
	}

	for i := 0; written < targetSize/4 && i < 2000; i++ {
		size := 100 + (i*137)%1900
		writeFile(fmt.Sprintf("/etc/conf.d/config-%04d", i), size, 0644)
	}

	for i := 0; written < targetSize*3/4; i++ {
		size := 50*1024 + (i*7919)%(450*1024)
		writeFile(fmt.Sprintf("/usr/lib/x86_64-linux-gnu/lib%04d.so", i), size, 0755)
	}

	for i := 0; written < targetSize; i++ {
		remaining := targetSize - written
		size := min(int64(2*1024*1024), remaining)
		writeFile(fmt.Sprintf("/usr/bin/binary-%04d", i), int(size), 0755)
	}

	for i := range 50 {
		writeSymlink(
			fmt.Sprintf("/usr/lib/x86_64-linux-gnu/lib%04d.so.1", i),
			fmt.Sprintf("lib%04d.so", i),
		)
	}

	for i := range 500 {
		size := 200 + (i*31)%800
		writeFile(fmt.Sprintf("/usr/share/doc/package-%04d/README", i), size, 0644)
	}
}

// populateOverlayDir creates a directory that acts as an overlay on top of
// a base layer produced by populateBenchDir. It overwrites some files,
// deletes others via whiteouts, and adds new entries.
func populateOverlayDir(b *testing.B, root string, targetSize int64) {
	b.Helper()
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	written := int64(0)

	writeFile := func(name string, size int, mode os.FileMode) {
		p := filepath.Join(root, filepath.FromSlash(name))
		_ = os.MkdirAll(filepath.Dir(p), 0755)
		data := make([]byte, size)
		for i := range data {
			data[i] = byte((i + 7) % 251)
		}
		if err := os.WriteFile(p, data, mode); err != nil {
			b.Fatal(err)
		}
		_ = os.Chtimes(p, now, now)
		written += int64(size)
	}

	writeDir := func(name string) {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(p, 0755); err != nil {
			b.Fatal(err)
		}
		_ = os.Chtimes(p, now, now)
	}

	writeWhiteout := func(name string) {
		p := filepath.Join(root, filepath.FromSlash(name))
		_ = os.MkdirAll(filepath.Dir(p), 0755)
		if err := os.WriteFile(p, nil, 0644); err != nil {
			b.Fatal(err)
		}
		_ = os.Chtimes(p, now, now)
	}

	// Whiteouts: delete some config files and a library.
	for _, wh := range []string{
		"/etc/conf.d/.wh.config-0050",
		"/etc/conf.d/.wh.config-0051",
		"/etc/conf.d/.wh.config-0052",
		"/usr/lib/x86_64-linux-gnu/.wh.lib0010.so",
	} {
		writeWhiteout(wh)
	}

	// Opaque directory: replace all docs.
	writeDir("/usr/share/doc/")
	writeWhiteout("/usr/share/doc/.wh..wh..opq")

	// Overwrite some configs.
	for i := range 20 {
		size := 200 + (i*137)%1900
		writeFile(fmt.Sprintf("/etc/conf.d/config-%04d", i), size, 0644)
	}

	// New directory tree.
	writeDir("/opt/")
	writeDir("/opt/overlay/")

	// Fill to target size with new files.
	for i := 0; written < targetSize; i++ {
		remaining := targetSize - written
		size := min(int64(512*1024), remaining)
		if size <= 0 {
			break
		}
		writeFile(fmt.Sprintf("/opt/overlay/data-%04d.bin", i), int(size), 0644)
	}
}

// prepareBenchDir builds a realistic directory tree for benchmarks.
func prepareBenchDir(b *testing.B) string {
	b.Helper()
	dirPath := filepath.Join(b.TempDir(), "root")
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		b.Fatal(err)
	}
	populateBenchDir(b, dirPath, benchTargetSize)
	return dirPath
}

// newDirFS wraps os.DirFS with Lstat-based Stat and ReadLink support.
type benchDirFS struct {
	root string
}

func (d *benchDirFS) Open(name string) (fs.File, error) {
	return os.DirFS(d.root).Open(name)
}

func (d *benchDirFS) Stat(name string) (fs.FileInfo, error) {
	return os.Lstat(filepath.Join(d.root, filepath.FromSlash(name)))
}

func (d *benchDirFS) ReadDir(name string) ([]fs.DirEntry, error) {
	return os.ReadDir(filepath.Join(d.root, filepath.FromSlash(name)))
}

func (d *benchDirFS) ReadLink(name string) (string, error) {
	return os.Readlink(filepath.Join(d.root, filepath.FromSlash(name)))
}

// disableGC disables automatic GC for accurate benchmarking and returns
// a cleanup function that re-enables it. Between iterations, call the
// returned gc function with the timer stopped to reclaim memory without
// polluting measurements.
func disableGC(b *testing.B) (gc func()) {
	b.Helper()
	prev := debug.SetGCPercent(-1)
	runtime.GC()
	b.Cleanup(func() { debug.SetGCPercent(prev) })
	return func() {
		b.StopTimer()
		runtime.GC()
		b.StartTimer()
	}
}

func BenchmarkDir(b *testing.B) {
	dirPath := prepareBenchDir(b)

	b.Run("go", func(b *testing.B) {
		gc := disableGC(b)
		b.ReportAllocs()
		for range b.N {
			gc()
			outPath := filepath.Join(b.TempDir(), "out.erofs")
			outFile, err := os.Create(outPath)
			if err != nil {
				b.Fatal(err)
			}
			w := erofs.Create(outFile)
			if err := w.CopyFrom(&benchDirFS{root: dirPath}); err != nil {
				_ = outFile.Close()
				b.Fatal(err)
			}
			if err := w.Close(); err != nil {
				_ = outFile.Close()
				b.Fatal(err)
			}
			_ = outFile.Close()
			_ = os.Remove(outPath)
		}
	})

	b.Run("mkfs.erofs", func(b *testing.B) {
		if _, err := exec.LookPath("mkfs.erofs"); err != nil {
			b.Skip("mkfs.erofs not available")
		}
		b.ReportAllocs()
		for range b.N {
			outPath := filepath.Join(b.TempDir(), "out.erofs")
			cmd := exec.Command("mkfs.erofs", "-Enoinline_data", "--quiet", outPath, dirPath)
			if out, err := cmd.CombinedOutput(); err != nil {
				b.Fatalf("mkfs.erofs: %v\n%s", err, out)
			}
			_ = os.Remove(outPath)
		}
	})
}

// buildErofsFromDir creates an EROFS image from a directory.
func buildErofsFromDir(b *testing.B, dirPath, outPath string, opts ...erofs.CopyOpt) {
	b.Helper()
	outFile, err := os.Create(outPath)
	if err != nil {
		b.Fatal(err)
	}
	defer outFile.Close() //nolint:errcheck
	w := erofs.Create(outFile)
	if err := w.CopyFrom(&benchDirFS{root: dirPath}, opts...); err != nil {
		b.Fatal("build erofs from dir:", err)
	}
	if err := w.Close(); err != nil {
		b.Fatal("close erofs:", err)
	}
}

// mergeSources holds paths for merge benchmark inputs.
type mergeSources struct {
	// Full EROFS images (with data) for erofs-to-erofs merge benchmarks.
	goBaseFullPath    string // base layer full EROFS (go-erofs)
	goOverlayFullPath string // overlay layer full EROFS (go-erofs)

	// mkfs.erofs-built full EROFS images.
	mkfsBaseFullPath    string
	mkfsOverlayFullPath string

	// Overlay directory for go/merge benchmark.
	overlayDirPath string
}

// prepareMergeSources builds a base directory, overlay directory, and EROFS
// images for merge benchmarks.
func prepareMergeSources(b *testing.B) mergeSources {
	b.Helper()
	tmpDir := b.TempDir()

	var s mergeSources

	// Build base directory.
	baseDirPath := filepath.Join(tmpDir, "base")
	if err := os.MkdirAll(baseDirPath, 0755); err != nil {
		b.Fatal(err)
	}
	populateBenchDir(b, baseDirPath, benchTargetSize)

	// Build overlay directory.
	s.overlayDirPath = filepath.Join(tmpDir, "overlay")
	if err := os.MkdirAll(s.overlayDirPath, 0755); err != nil {
		b.Fatal(err)
	}
	populateOverlayDir(b, s.overlayDirPath, benchTargetSize/benchMergeOverlayFraction)

	// Build full EROFS images (Go) for erofs-to-erofs merge benchmarks.
	s.goBaseFullPath = filepath.Join(tmpDir, "base-full-go.erofs")
	buildErofsFromDir(b, baseDirPath, s.goBaseFullPath)
	fi, _ := os.Stat(s.goBaseFullPath)
	b.Logf("base erofs full (go): %.1f MB", float64(fi.Size())/(1024*1024))

	s.goOverlayFullPath = filepath.Join(tmpDir, "overlay-full-go.erofs")
	buildErofsFromDir(b, s.overlayDirPath, s.goOverlayFullPath)
	fi, _ = os.Stat(s.goOverlayFullPath)
	b.Logf("overlay erofs full (go): %.1f MB", float64(fi.Size())/(1024*1024))

	// Build full EROFS images (mkfs.erofs) for erofs-to-erofs merge benchmarks.
	if _, err := exec.LookPath("mkfs.erofs"); err == nil {
		for _, tc := range []struct {
			dirPath string
			field   *string
			label   string
		}{
			{baseDirPath, &s.mkfsBaseFullPath, "base"},
			{s.overlayDirPath, &s.mkfsOverlayFullPath, "overlay"},
		} {
			outPath := filepath.Join(tmpDir, tc.label+"-full-mkfs.erofs")
			cmd := exec.Command("mkfs.erofs", "-Enoinline_data", "--quiet", outPath, tc.dirPath)
			if out, err := cmd.CombinedOutput(); err != nil {
				b.Fatalf("mkfs.erofs %s: %v\n%s", tc.label, err, out)
			}
			*tc.field = outPath
			fi, _ = os.Stat(outPath)
			b.Logf("%s erofs full (mkfs): %.1f MB", tc.label, float64(fi.Size())/(1024*1024))
		}
	}

	return s
}

// BenchmarkMerge measures the cost of merging an overlay on top of a
// metadata-only base layer.
//
// Sub-benchmarks:
//
//	go/merge           — MetadataOnly(base) + Merge(overlay dir) → EROFS
//	go/erofs           — merge two full EROFS images
//	go/erofs-meta      — merge two full EROFS images as metadata-only
//	mkfs.erofs/erofs   — mkfs.erofs merge: two full EROFS images
func BenchmarkMerge(b *testing.B) {
	src := prepareMergeSources(b)

	// go/merge: full EROFS base + Merge overlay dir.
	b.Run("go/merge", func(b *testing.B) {
		gc := disableGC(b)
		b.ReportAllocs()
		for range b.N {
			gc()
			baseErofsFile, err := os.Open(src.goBaseFullPath)
			if err != nil {
				b.Fatal(err)
			}
			baseFS, err := erofs.Open(baseErofsFile)
			if err != nil {
				_ = baseErofsFile.Close()
				b.Fatal(err)
			}

			outPath := filepath.Join(b.TempDir(), "out.erofs")
			outFile, err := os.Create(outPath)
			if err != nil {
				b.Fatal(err)
			}

			w := erofs.Create(outFile)
			if err := w.CopyFrom(baseFS); err != nil {
				b.Fatal(err)
			}
			if err := w.CopyFrom(&benchDirFS{root: src.overlayDirPath}, erofs.Merge()); err != nil {
				b.Fatal(err)
			}
			if err := w.Close(); err != nil {
				b.Fatal(err)
			}

			_ = outFile.Close()
			_ = baseErofsFile.Close()
			_ = os.Remove(outPath)
		}
	})

	// go/erofs: merge two full EROFS images (base + overlay).
	b.Run("go/erofs", func(b *testing.B) {
		gc := disableGC(b)
		b.ReportAllocs()
		for range b.N {
			gc()
			baseFile, err := os.Open(src.goBaseFullPath)
			if err != nil {
				b.Fatal(err)
			}
			baseFS, err := erofs.Open(baseFile)
			if err != nil {
				_ = baseFile.Close()
				b.Fatal(err)
			}
			overlayFile, err := os.Open(src.goOverlayFullPath)
			if err != nil {
				_ = baseFile.Close()
				b.Fatal(err)
			}
			overlayFS, err := erofs.Open(overlayFile)
			if err != nil {
				_ = baseFile.Close()
				_ = overlayFile.Close()
				b.Fatal(err)
			}

			outPath := filepath.Join(b.TempDir(), "out.erofs")
			outFile, err := os.Create(outPath)
			if err != nil {
				b.Fatal(err)
			}

			w := erofs.Create(outFile)
			if err := w.CopyFrom(baseFS); err != nil {
				b.Fatal(err)
			}
			if err := w.CopyFrom(overlayFS, erofs.Merge()); err != nil {
				b.Fatal(err)
			}
			if err := w.Close(); err != nil {
				b.Fatal(err)
			}

			_ = outFile.Close()
			_ = baseFile.Close()
			_ = overlayFile.Close()
			_ = os.Remove(outPath)
		}
	})

	// go/erofs-meta: merge two full EROFS images as metadata-only
	// (snapshotter index pattern — no data copied, only metadata + chunk refs).
	b.Run("go/erofs-meta", func(b *testing.B) {
		gc := disableGC(b)
		b.ReportAllocs()
		for range b.N {
			gc()
			baseFile, err := os.Open(src.goBaseFullPath)
			if err != nil {
				b.Fatal(err)
			}
			baseFS, err := erofs.Open(baseFile)
			if err != nil {
				_ = baseFile.Close()
				b.Fatal(err)
			}
			overlayFile, err := os.Open(src.goOverlayFullPath)
			if err != nil {
				_ = baseFile.Close()
				b.Fatal(err)
			}
			overlayFS, err := erofs.Open(overlayFile)
			if err != nil {
				_ = baseFile.Close()
				_ = overlayFile.Close()
				b.Fatal(err)
			}

			outPath := filepath.Join(b.TempDir(), "out.erofs")
			outFile, err := os.Create(outPath)
			if err != nil {
				b.Fatal(err)
			}

			w := erofs.Create(outFile)
			if err := w.CopyFrom(baseFS, erofs.MetadataOnly()); err != nil {
				b.Fatal(err)
			}
			if err := w.CopyFrom(overlayFS, erofs.MetadataOnly(), erofs.Merge()); err != nil {
				b.Fatal(err)
			}
			if err := w.Close(); err != nil {
				b.Fatal(err)
			}

			_ = outFile.Close()
			_ = baseFile.Close()
			_ = overlayFile.Close()
			_ = os.Remove(outPath)
		}
	})

	// mkfs.erofs/erofs: merge two full EROFS images via mkfs.erofs.
	b.Run("mkfs.erofs/erofs", func(b *testing.B) {
		if src.mkfsBaseFullPath == "" {
			b.Skip("mkfs.erofs not available")
		}
		b.ReportAllocs()
		for range b.N {
			outPath := filepath.Join(b.TempDir(), "out.erofs")
			cmd := exec.Command("mkfs.erofs",
				"--aufs", "--ovlfs-strip=1", "--quiet", "-Enoinline_data",
				outPath, src.mkfsBaseFullPath, src.mkfsOverlayFullPath)
			out, err := cmd.CombinedOutput()
			_ = os.Remove(outPath)
			if err != nil {
				b.Fatalf("mkfs.erofs erofs merge: %v\n%s", err, out)
			}
		}
	})
}

// layerSpec describes a container image layer for the 10-layer benchmark.
type layerSpec struct {
	name      string
	size      int64    // approximate target size
	whiteouts []string // .wh.<name> entries
	opaques   []string // directories to make opaque
}

// BenchmarkMerge10Layer simulates a realistic container image with 10 layers:
//
//	Layer 0: base OS (~250 MB)
//	Layers 1-9: progressively smaller, some with whiteouts/opaques
//
// Each layer is a full EROFS image. The benchmark merges all 10 into a single
// metadata-only index with 10 blob devices.
func BenchmarkMerge10Layer(b *testing.B) {
	layers := []layerSpec{
		{name: "base-os", size: 250 * 1024 * 1024},
		{name: "runtime", size: 30 * 1024 * 1024},
		{name: "deps", size: 15 * 1024 * 1024,
			whiteouts: []string{"/usr/share/doc/.wh..wh..opq"}},
		{name: "app-v1", size: 8 * 1024 * 1024},
		{name: "app-v2", size: 5 * 1024 * 1024,
			whiteouts: []string{"/opt/app/.wh.old-binary"},
			opaques:   []string{"/tmp/"}},
		{name: "config", size: 50 * 1024},
		{name: "secrets", size: 4 * 1024},
		{name: "hotfix1", size: 2 * 1024 * 1024,
			whiteouts: []string{"/usr/lib/x86_64-linux-gnu/.wh.libold.so"}},
		{name: "hotfix2", size: 500 * 1024},
		{name: "metadata", size: 10 * 1024},
	}

	tmpDir := b.TempDir()
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	// Build dir + EROFS for each layer.
	type layerFiles struct {
		goErofsPath   string
		mkfsErofsPath string
	}
	built := make([]layerFiles, len(layers))

	for li, spec := range layers {
		// Generate directory.
		dirPath := filepath.Join(tmpDir, fmt.Sprintf("layer%d", li))
		if err := os.MkdirAll(dirPath, 0755); err != nil {
			b.Fatal(err)
		}

		func() {
			written := int64(0)

			writeDir := func(name string) {
				p := filepath.Join(dirPath, filepath.FromSlash(name))
				_ = os.MkdirAll(p, 0755)
				_ = os.Chtimes(p, now, now)
			}
			writeFile := func(name string, size int) {
				p := filepath.Join(dirPath, filepath.FromSlash(name))
				_ = os.MkdirAll(filepath.Dir(p), 0755)
				data := make([]byte, size)
				for i := range data {
					data[i] = byte((i + li*7) % 251)
				}
				_ = os.WriteFile(p, data, 0644)
				_ = os.Chtimes(p, now, now)
				written += int64(size)
			}
			writeWhiteout := func(name string) {
				p := filepath.Join(dirPath, filepath.FromSlash(name))
				_ = os.MkdirAll(filepath.Dir(p), 0755)
				_ = os.WriteFile(p, nil, 0644)
				_ = os.Chtimes(p, now, now)
			}

			// Whiteouts first (before dirs they reference).
			for _, wh := range spec.whiteouts {
				writeWhiteout(wh)
			}
			for _, op := range spec.opaques {
				writeDir(op)
				writeWhiteout(op + ".wh..wh..opq")
			}

			if li == 0 {
				// Base OS layer: realistic tree with 5000+ files at various depths.
				dirs := []string{
					"/", "/bin", "/sbin", "/lib", "/lib/x86_64-linux-gnu",
					"/lib/x86_64-linux-gnu/security",
					"/usr", "/usr/bin", "/usr/sbin", "/usr/lib",
					"/usr/lib/x86_64-linux-gnu",
					"/usr/lib/python3", "/usr/lib/python3/dist-packages",
					"/usr/lib/python3/dist-packages/pip",
					"/usr/lib/python3/dist-packages/pip/internal",
					"/usr/lib/python3/dist-packages/setuptools",
					"/usr/share", "/usr/share/doc", "/usr/share/man",
					"/usr/share/man/man1", "/usr/share/man/man5", "/usr/share/man/man8",
					"/usr/share/locale", "/usr/share/locale/en", "/usr/share/locale/de",
					"/usr/share/zoneinfo", "/usr/share/zoneinfo/US",
					"/usr/share/zoneinfo/Europe",
					"/usr/include", "/usr/include/linux", "/usr/include/x86_64-linux-gnu",
					"/etc", "/etc/apt", "/etc/apt/sources.list.d",
					"/etc/default", "/etc/init.d", "/etc/cron.d",
					"/etc/ssl", "/etc/ssl/certs",
					"/etc/systemd", "/etc/systemd/system",
					"/var", "/var/lib", "/var/lib/apt", "/var/lib/dpkg",
					"/var/lib/dpkg/info", "/var/cache", "/var/cache/apt",
					"/var/log", "/var/tmp",
					"/opt", "/opt/app", "/tmp", "/run",
				}
				for _, d := range dirs {
					writeDir(d)
				}

				// ~2000 small config/metadata files (50-2000 bytes)
				for i := 0; i < 800 && written < spec.size/8; i++ {
					sz := 100 + (i*137)%1900
					writeFile(fmt.Sprintf("/etc/apt/sources.list.d/source-%04d.list", i), sz)
				}
				for i := 0; i < 600 && written < spec.size/5; i++ {
					sz := 50 + (i*31)%500
					writeFile(fmt.Sprintf("/var/lib/dpkg/info/pkg-%04d.md5sums", i), sz)
				}
				for i := 0; i < 600 && written < spec.size*3/10; i++ {
					sz := 200 + (i*41)%1800
					writeFile(fmt.Sprintf("/usr/share/doc/package-%04d/README", i), sz)
				}

				// ~1000 medium Python/locale files (1-20 KB)
				for i := 0; i < 400 && written < spec.size*2/5; i++ {
					sz := 1024 + (i*997)%(19*1024)
					writeFile(fmt.Sprintf("/usr/lib/python3/dist-packages/pip/internal/mod_%04d.py", i), sz)
				}
				for i := 0; i < 300 && written < spec.size/2; i++ {
					sz := 2048 + (i*773)%(18*1024)
					writeFile(fmt.Sprintf("/usr/lib/python3/dist-packages/setuptools/cmd_%04d.py", i), sz)
				}
				for i := 0; i < 200 && written < spec.size*11/20; i++ {
					sz := 500 + (i*251)%(4*1024)
					writeFile(fmt.Sprintf("/usr/include/linux/header_%04d.h", i), sz)
				}
				for i := 0; i < 50; i++ {
					sz := 5*1024 + (i*3571)%(45*1024)
					writeFile(fmt.Sprintf("/usr/share/locale/en/messages_%04d.mo", i), sz)
					writeFile(fmt.Sprintf("/usr/share/locale/de/messages_%04d.mo", i), sz)
				}
				for i := 0; i < 300 && written < spec.size*13/20; i++ {
					sz := 100 + (i*59)%900
					writeFile(fmt.Sprintf("/usr/share/man/man1/cmd_%04d.1", i), sz)
				}
				for i := 0; i < 200 && written < spec.size*3/4; i++ {
					sz := 200 + (i*67)%1200
					writeFile(fmt.Sprintf("/etc/ssl/certs/cert_%04d.pem", i), sz)
				}

				// ~200 shared libraries (50-500 KB)
				for i := 0; i < 200 && written < spec.size*17/20; i++ {
					sz := 50*1024 + (i*7919)%(450*1024)
					writeFile(fmt.Sprintf("/usr/lib/x86_64-linux-gnu/lib%04d.so", i), sz)
				}

				// ~50 security/PAM modules (10-100 KB)
				for i := 0; i < 50; i++ {
					sz := 10*1024 + (i*1009)%(90*1024)
					writeFile(fmt.Sprintf("/lib/x86_64-linux-gnu/security/pam_%04d.so", i), sz)
				}

				// ~100 timezone files (200 bytes - 2 KB)
				for i := 0; i < 50; i++ {
					sz := 200 + (i*127)%1800
					writeFile(fmt.Sprintf("/usr/share/zoneinfo/US/zone_%04d", i), sz)
					writeFile(fmt.Sprintf("/usr/share/zoneinfo/Europe/zone_%04d", i), sz)
				}

				// Additional small files to ensure 5000+ total.
				for i := 0; i < 500; i++ {
					sz := 64 + (i*37)%400
					writeFile(fmt.Sprintf("/var/lib/apt/lists/pkg_%04d", i), sz)
				}
				for i := 0; i < 300; i++ {
					sz := 100 + (i*53)%600
					writeFile(fmt.Sprintf("/etc/cron.d/job_%04d", i), sz)
				}

				// Large binaries to fill remaining.
				for i := 0; written < spec.size; i++ {
					remaining := spec.size - written
					sz := min(int64(2*1024*1024), remaining)
					if sz < 1 {
						break
					}
					writeFile(fmt.Sprintf("/usr/bin/binary-%04d", i), int(sz))
				}
			} else {
				// Upper layers: simpler structure.
				for _, d := range []string{"/", "/usr", "/usr/lib", "/usr/lib/x86_64-linux-gnu",
					"/usr/share", "/usr/share/doc", "/opt", "/opt/app", "/tmp", "/etc"} {
					writeDir(d)
				}
				for i := 0; written < spec.size; i++ {
					remaining := spec.size - written
					sz := min(int64(512*1024), remaining)
					if sz < 1 {
						break
					}
					writeFile(fmt.Sprintf("/opt/app/%s-%04d.bin", spec.name, i), int(sz))
				}
			}
		}()

		// Build Go EROFS.
		goPath := filepath.Join(tmpDir, fmt.Sprintf("layer%d-go.erofs", li))
		buildErofsFromDir(b, dirPath, goPath)
		built[li].goErofsPath = goPath

		// Build mkfs.erofs EROFS.
		if _, err := exec.LookPath("mkfs.erofs"); err == nil {
			mkfsPath := filepath.Join(tmpDir, fmt.Sprintf("layer%d-mkfs.erofs", li))
			cmd := exec.Command("mkfs.erofs", "-Enoinline_data", "--quiet", mkfsPath, dirPath)
			if out, err := cmd.CombinedOutput(); err != nil {
				b.Fatalf("mkfs.erofs layer %d: %v\n%s", li, err, out)
			}
			built[li].mkfsErofsPath = mkfsPath
		}

		fi, _ := os.Stat(goPath)
		// Count inodes by opening the image.
		inodes := uint64(0)
		if gf, err := os.Open(goPath); err == nil {
			if efs, err := erofs.Open(gf); err == nil {
				_ = fs.WalkDir(efs, ".", func(_ string, _ fs.DirEntry, _ error) error {
					inodes++
					return nil
				})
			}
			_ = gf.Close()
		}
		b.Logf("layer %d (%s): target %s, erofs %.1f MB, %d files",
			li, spec.name, formatSize(spec.size), float64(fi.Size())/(1024*1024), inodes)
	}

	// go: merge all 10 layers as MetadataOnly into a single index.
	b.Run("go", func(b *testing.B) {
		gc := disableGC(b)
		b.ReportAllocs()
		for range b.N {
			gc()
			var files []*os.File
			outPath := filepath.Join(b.TempDir(), "merged.erofs")
			outFile, err := os.Create(outPath)
			if err != nil {
				b.Fatal(err)
			}

			w := erofs.Create(outFile)
			for li, lf := range built {
				f, err := os.Open(lf.goErofsPath)
				if err != nil {
					b.Fatal(err)
				}
				files = append(files, f)
				efs, err := erofs.Open(f)
				if err != nil {
					b.Fatal(err)
				}
				opts := []erofs.CopyOpt{erofs.MetadataOnly()}
				if li > 0 {
					opts = append(opts, erofs.Merge())
				}
				if err := w.CopyFrom(efs, opts...); err != nil {
					b.Fatal(err)
				}
			}
			if err := w.Close(); err != nil {
				b.Fatal(err)
			}

			_ = outFile.Close()
			for _, f := range files {
				_ = f.Close()
			}
			_ = os.Remove(outPath)
		}
	})

	// mkfs.erofs: merge all 10 layers.
	b.Run("mkfs.erofs", func(b *testing.B) {
		if built[0].mkfsErofsPath == "" {
			b.Skip("mkfs.erofs not available")
		}
		b.ReportAllocs()
		for range b.N {
			outPath := filepath.Join(b.TempDir(), "merged.erofs")
			args := []string{"--aufs", "--ovlfs-strip=1", "--quiet", "-Enoinline_data", outPath}
			for _, lf := range built {
				args = append(args, lf.mkfsErofsPath)
			}
			cmd := exec.Command("mkfs.erofs", args...)
			out, err := cmd.CombinedOutput()
			_ = os.Remove(outPath)
			if err != nil {
				b.Fatalf("mkfs.erofs: %v\n%s", err, out)
			}
		}
	})
}

func formatSize(b int64) string {
	switch {
	case b >= 1024*1024:
		return fmt.Sprintf("%.0f MB", float64(b)/(1024*1024))
	case b >= 1024:
		return fmt.Sprintf("%.0f KB", float64(b)/1024)
	default:
		return fmt.Sprintf("%d B", b)
	}
}
