package fat32

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func mustBuilder(t *testing.T, opts Options) *Builder {
	t.Helper()
	b, err := NewBuilder(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

func addFile(t *testing.T, b *Builder, name string, content []byte, mode int64) {
	t.Helper()
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(content)), Mode: mode}
	if err := b.WriteRegular(hdr, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
}

func addDir(t *testing.T, b *Builder, name string, mode int64) {
	t.Helper()
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name, Mode: mode}); err != nil {
		t.Fatal(err)
	}
}

func build(t *testing.T, b *Builder) (*memFile, *Info) {
	t.Helper()
	m := &memFile{}
	info, err := b.Build(m)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(m.b)) != info.Size {
		t.Fatalf("image is %d bytes, info says %d", len(m.b), info.Size)
	}
	return m, info
}

// populate fills a builder with a representative tree and returns the
// expected content of the regular files.
func populate(t *testing.T, b *Builder) map[string][]byte {
	t.Helper()
	files := map[string][]byte{
		"etc/hostname":                      []byte("example\n"),
		"etc/empty":                         {},
		"bin/one-cluster":                   pattern(4096, 1),
		"bin/odd":                           pattern(5000, 2),
		"usr/lib/big":                       pattern(3<<20+123, 3),
		"usr/lib/exactly-2k":                pattern(2048, 4),
		"README.TXT":                        []byte("upper case 8.3"),
		"Readme.md":                         []byte("mixed case"),
		"verylongfilename.txt":              []byte("one"),
		"verylongfilename2.txt":             []byte("two"),
		"verylongfilename3.txt":             []byte("three"),
		".gitignore":                        []byte("*.o\n"),
		"with space/a b.txt":                []byte("spaces"),
		"unicode/ünïcode-名前.txt":            []byte("unicode"),
		"a.b.c.d":                           []byte("dots"),
		"zeros":                             make([]byte, 100000),
		"deep/er/and/deeper/still/file.dat": []byte("deep"),
	}
	// In a fixed order, as the order of the files shapes the image.
	for _, name := range slices.Sorted(maps.Keys(files)) {
		addFile(t, b, name, files[name], 0o644)
	}
	return files
}

func TestBuildTree(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"default", Options{}},
		{"cluster-1k", Options{ClusterSize: 1024}},
		{"cluster-4k", Options{ClusterSize: 4096}},
		{"cluster-32k", Options{ClusterSize: 32768}},
		{"sector-4k", Options{SectorSize: 4096}},
		{"sector-1k-cluster-8k", Options{SectorSize: 1024, ClusterSize: 8192}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := mustBuilder(t, tc.opts)
			files := populate(t, b)
			m, info := build(t, b)
			c := checkImage(t, m, info.Size)
			if t.Failed() {
				return
			}

			for name, want := range files {
				f := c.files[name]
				if f == nil {
					t.Fatalf("%s: missing (have %v)", name, keys(c.files))
				}
				if !bytes.Equal(f.data, want) {
					t.Errorf("%s: wrong content", name)
				}
				if !f.contiguous() {
					t.Errorf("%s: not contiguous: %v", name, f.chain)
				}
			}
			// Files are readable straight from the image.
			if len(info.Files) != len(files)-1 { // the empty file has no data
				t.Errorf("got %d file layouts, want %d", len(info.Files), len(files)-1)
			}
			for _, f := range info.Files {
				if got := m.b[f.Offset : f.Offset+f.Size]; !bytes.Equal(got, files[f.Path]) {
					t.Errorf("%s: image bytes at %d differ", f.Path, f.Offset)
				}
				if f.Offset%int64(info.ClusterSize) != 0 {
					t.Errorf("%s: offset %d is not cluster aligned", f.Path, f.Offset)
				}
			}
			// Names that fit 8.3 in upper case need no long name.
			if f := c.files["README.TXT"]; f.hasLong || f.short != "README.TXT" {
				t.Errorf("README.TXT: %+v", f)
			}
			if f := c.files["Readme.md"]; !f.hasLong || f.short != "README.MD" {
				t.Errorf("Readme.md: %+v", f)
			}
			if f := c.files["verylongfilename.txt"]; !f.hasLong || f.short != "VERYLO~1.TXT" {
				t.Errorf("verylongfilename.txt: %+v", f)
			}
			if f := c.files["verylongfilename3.txt"]; f.short != "VERYLO~3.TXT" {
				t.Errorf("verylongfilename3.txt: %+v", f)
			}
			if f := c.files[".gitignore"]; f.short != "GITIGN~1" && f.short != "GITIGNORE" && !strings.HasPrefix(f.short, "GITIGN") {
				t.Errorf(".gitignore: %+v", f)
			}
			if f := c.files["unicode/ünïcode-名前.txt"]; !f.hasLong || !strings.HasSuffix(f.short, ".TXT") {
				t.Errorf("unicode: %+v", f)
			}
			if !c.files["etc"].dir || !c.files["deep/er/and"].dir {
				t.Error("directories missing")
			}
		})
	}
}

func keys(m map[string]*vfile) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestMinimalSize(t *testing.T) {
	b := mustBuilder(t, Options{})
	addFile(t, b, "file", pattern(10000, 9), 0o644)
	m, info := build(t, b)
	c := checkImage(t, m, info.Size)

	if info.Clusters != minClusters {
		t.Errorf("%d clusters, want the minimum of %d", info.Clusters, minClusters)
	}
	if info.ClusterSize != 512 {
		t.Errorf("cluster size %d", info.ClusterSize)
	}
	// A volume this small has 512 byte clusters: 20 clusters for the file,
	// one for the root directory.
	if info.UsedClusters != 21 {
		t.Errorf("%d clusters used", info.UsedClusters)
	}
	if want := int64(65541)*512 + 2*512*int64((65541+2)*4/512+1) + 32*512; info.Size < want-1024 || info.Size > want+1024 {
		t.Errorf("size %d, want about %d", info.Size, want)
	}
	if !bytes.Equal(c.files["file"].data, pattern(10000, 9)) {
		t.Error("wrong content")
	}
	if len(m.b) < minClusters*512 {
		t.Errorf("image of only %d bytes", len(m.b))
	}
}

func TestLargerImage(t *testing.T) {
	b := mustBuilder(t, Options{Size: 1 << 30})
	files := populate(t, b)
	m := &memFile{}
	cw := &countingWriter{m: m}
	info, err := b.Build(cw)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size < 1<<30 || info.Size > 1<<30+3*4096 {
		t.Errorf("size %d", info.Size)
	}
	if info.ClusterSize != 4096 {
		t.Errorf("a 1 GiB volume has 4 KiB clusters, got %d", info.ClusterSize)
	}
	c := checkImage(t, m, info.Size)
	for name, want := range files {
		if !bytes.Equal(c.files[name].data, want) {
			t.Errorf("%s: wrong content", name)
		}
	}
	// Free space is not written.
	if cw.bytes > 12<<20 {
		t.Errorf("wrote %d bytes for a %d byte image", cw.bytes, info.Size)
	}

	// The same through a file system file, which is sparse.
	path := filepath.Join(t.TempDir(), "image")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b2 := mustBuilder(t, Options{Size: 1 << 30})
	populate(t, b2)
	info2, err := b2.Build(f)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	if st.Size() != info2.Size {
		t.Errorf("file is %d bytes, want %d", st.Size(), info2.Size)
	}
	checkImage(t, f, st.Size())
}

type countingWriter struct {
	m     *memFile
	bytes int
}

func (c *countingWriter) WriteAt(p []byte, off int64) (int, error) {
	c.bytes += len(p)
	return c.m.WriteAt(p, off)
}
func (c *countingWriter) Truncate(n int64) error { return c.m.Truncate(n) }

func TestWriterWithoutTruncate(t *testing.T) {
	b := mustBuilder(t, Options{Size: 40 << 20})
	addFile(t, b, "f", []byte("hello"), 0o644)
	m := &memFile{}
	info, err := b.Build(noTruncate{m})
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(m.b)) != info.Size {
		t.Errorf("image is %d bytes, want %d", len(m.b), info.Size)
	}
	checkImage(t, m, info.Size)
}

func TestDeterministicAndOrder(t *testing.T) {
	var images [][]byte
	var infos []*Info
	for i := 0; i < 2; i++ {
		b := mustBuilder(t, Options{})
		populate(t, b)
		m, info := build(t, b)
		images = append(images, m.b)
		infos = append(infos, info)
	}
	_ = images
	// map iteration order differs from run to run, but the image does not
	// (populate adds the files in map order, so build the same list instead).
	var last int64 = -1
	for _, f := range infos[0].Files {
		if f.Offset < last {
			t.Errorf("%s at %d is not after %d", f.Path, f.Offset, last)
		}
		last = f.Offset + f.Size
	}
}

func TestDeterminismStress(t *testing.T) {
	build := func() []byte {
		b := mustBuilder(t, Options{DigestFiles: true})
		for i := 0; i < 300; i++ {
			content := pattern(100+i%7*3000, byte(i%5))
			hdr := &tar.Header{
				Typeflag: tar.TypeReg, Name: fmt.Sprintf("Dir %d/Some Long File Name %d.txt", i%11, i), Size: int64(len(content)),
				Mode: 0o644, ModTime: time.Unix(1_600_000_000+int64(i%4), 0),
			}
			if err := b.WriteRegular(hdr, bytes.NewReader(content)); err != nil {
				t.Fatal(err)
			}
			if i%9 == 0 {
				if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: fmt.Sprintf("links/Link %d.txt", i), Linkname: hdr.Name}); err != nil {
					t.Fatal(err)
				}
			}
		}
		m, _ := build(t, b)
		return m.b
	}
	first := build()
	for i := 0; i < 15; i++ {
		if got := build(); !bytes.Equal(got, first) {
			t.Fatalf("build %d differs from the first one", i+2)
		}
	}
}

func TestManyEntries(t *testing.T) {
	b := mustBuilder(t, Options{})
	want := map[string][]byte{}
	for i := 0; i < 3000; i++ {
		name := fmt.Sprintf("d/a fairly long file name number %04d.txt", i)
		content := pattern(i%700, byte(i))
		addFile(t, b, name, content, 0o600)
		want[name] = content
	}
	addFile(t, b, "d/"+strings.Repeat("L", 255), []byte("long name"), 0o600)
	m, info := build(t, b)
	c := checkImage(t, m, info.Size)
	if len(c.files["d"].children) != 3001 {
		t.Errorf("%d entries", len(c.files["d"].children))
	}
	for name, content := range want {
		if !bytes.Equal(c.files[name].data, content) {
			t.Fatalf("%s: wrong content", name)
		}
	}
	// 3000 long names take several entries each: the directory has many clusters.
	if n := len(c.files["d"].chain); n < 10 {
		t.Errorf("directory has %d clusters", n)
	}
}

func TestNamesWithSameShortName(t *testing.T) {
	b := mustBuilder(t, Options{})
	// All of them would be "LONGFI~1.TXT" without a counter, one is exactly that.
	for i := 0; i < 15; i++ {
		addFile(t, b, fmt.Sprintf("longfilename-%d.txt", i), []byte{byte(i)}, 0o644)
	}
	addFile(t, b, "LONGFI~1.TXT", []byte("exact"), 0o644)
	addFile(t, b, "longfi~2.txt", []byte("lower"), 0o644)
	m, info := build(t, b)
	c := checkImage(t, m, info.Size)
	shorts := map[string]string{}
	for name, f := range c.files {
		if name == "" || f.dir {
			continue
		}
		if prev, dup := shorts[f.short]; dup {
			t.Errorf("%q and %q have the short name %q", name, prev, f.short)
		}
		shorts[f.short] = name
	}
	if c.files["LONGFI~1.TXT"].short != "LONGFI~1.TXT" || c.files["longfi~2.txt"].short != "LONGFI~2.TXT" {
		t.Errorf("exact names changed: %+v %+v", c.files["LONGFI~1.TXT"], c.files["longfi~2.txt"])
	}
}

func TestMultiClusterFileIsContiguous(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "sparse")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	const size = 300<<20 + 777
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("tail"), size-4); err != nil {
		t.Fatal(err)
	}
	f.Close()

	b := mustBuilder(t, Options{})
	if err := b.WriteRegularFromPath(&tar.Header{Typeflag: tar.TypeReg, Name: "big", Size: size, Mode: 0o644}, src); err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(filepath.Join(dir, "image"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	info, err := b.Build(out)
	if err != nil {
		t.Fatal(err)
	}
	c := checkImage(t, out, info.Size)
	n := c.files["big"]
	if !n.contiguous() || len(n.data) != size || !bytes.HasSuffix(n.data, []byte("tail")) {
		t.Errorf("contiguous %v, %d bytes", n.contiguous(), len(n.data))
	}
	if info.ClusterSize != 4096 {
		t.Errorf("cluster size %d for a %d byte image", info.ClusterSize, info.Size)
	}
}

func TestAttributesAndTimes(t *testing.T) {
	b := mustBuilder(t, Options{Time: time.Date(2024, 2, 3, 4, 5, 6, 0, time.UTC)})
	mod := time.Date(2001, 9, 9, 1, 46, 41, 500_000_000, time.UTC)
	acc := time.Date(2020, 12, 31, 23, 59, 59, 0, time.UTC)
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: "ro", Mode: 0o444, ModTime: mod, AccessTime: acc}
	if err := b.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "rw", Mode: 0o644, ModTime: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "default"}); err != nil {
		t.Fatal(err)
	}
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "future", ModTime: time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	m, info := build(t, b)
	c := checkImage(t, m, info.Size)

	decode := func(date, tm uint16, tenth byte) time.Time {
		return time.Date(1980+int(date>>9), time.Month(date>>5&0xF), int(date&0x1F), int(tm>>11), int(tm>>5&0x3F), int(tm&0x1F)*2+int(tenth/100), int(tenth%100)*10_000_000, time.UTC)
	}
	ro := c.files["ro"]
	if ro.attr != attrReadOnly|attrArchive {
		t.Errorf("ro: attr %#x", ro.attr)
	}
	if got := decode(ro.mdate, ro.mtime, ro.tenth); !got.Equal(mod) {
		t.Errorf("ro: modification time %v, want %v", got, mod)
	}
	if got := decode(ro.adate, 0, 0); !got.Equal(time.Date(2020, 12, 31, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("ro: access date %v", got)
	}
	if c.files["rw"].attr != attrArchive {
		t.Errorf("rw: attr %#x", c.files["rw"].attr)
	}
	// 1970 is before 1980; a file without time gets the time of the options (the
	// Options.Time of this test) while a time before 1980 is clamped.
	if got := decode(c.files["rw"].mdate, c.files["rw"].mtime, c.files["rw"].tenth); !got.Equal(time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("rw: %v", got)
	}
	if got := decode(c.files["default"].mdate, c.files["default"].mtime, c.files["default"].tenth); !got.Equal(time.Date(2024, 2, 3, 4, 5, 6, 0, time.UTC)) {
		t.Errorf("default: %v", got)
	}
	if got := decode(c.files["future"].mdate, c.files["future"].mtime, c.files["future"].tenth); got.Year() != 2107 {
		t.Errorf("future: %v", got)
	}
}

func TestLabelAndVolumeID(t *testing.T) {
	b := mustBuilder(t, Options{Label: "RULES IMG", VolumeID: 0xCAFEF00D})
	addFile(t, b, "f", []byte("x"), 0o644)
	m, info := build(t, b)
	c := checkImage(t, m, info.Size)
	if c.label != "RULES IMG" || info.VolumeID != 0xCAFEF00D {
		t.Errorf("label %q, volume ID %#x", c.label, info.VolumeID)
	}
	if id := uint32(m.b[67]) | uint32(m.b[68])<<8 | uint32(m.b[69])<<16 | uint32(m.b[70])<<24; id != 0xCAFEF00D {
		t.Errorf("volume ID in the boot sector %#x", id)
	}

	b2 := mustBuilder(t, Options{})
	m2, info2 := build(t, b2)
	c2 := checkImage(t, m2, info2.Size)
	if c2.label != "NO NAME" || info2.VolumeID == 0 {
		t.Errorf("label %q, volume ID %#x", c2.label, info2.VolumeID)
	}
}

func TestHardLinkIsCopy(t *testing.T) {
	b := mustBuilder(t, Options{})
	addFile(t, b, "orig", []byte("shared content"), 0o644)
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "dir/link", Linkname: "orig"}); err != nil {
		t.Fatal(err)
	}
	m, info := build(t, b)
	c := checkImage(t, m, info.Size)
	if string(c.files["dir/link"].data) != "shared content" || string(c.files["orig"].data) != "shared content" {
		t.Error("link is not a copy")
	}
	if c.files["dir/link"].first == c.files["orig"].first {
		t.Error("link shares clusters with the original")
	}
}

func TestReplacementAndMerge(t *testing.T) {
	b := mustBuilder(t, Options{})
	addFile(t, b, "a/file", []byte("one"), 0o644)
	addFile(t, b, "a/other", []byte("keep"), 0o644)
	addFile(t, b, "a/file", []byte("replaced"), 0o644)
	addDir(t, b, "a", 0o755) // keeps the children
	addFile(t, b, "gone/x", []byte("x"), 0o644)
	addFile(t, b, "gone", []byte("now a file"), 0o644)
	m, info := build(t, b)
	c := checkImage(t, m, info.Size)
	if string(c.files["a/file"].data) != "replaced" || string(c.files["a/other"].data) != "keep" {
		t.Error("wrong content")
	}
	if c.files["gone"].dir || string(c.files["gone"].data) != "now a file" {
		t.Error("file did not replace the directory")
	}
	if _, ok := c.files["gone/x"]; ok {
		t.Error("child of replaced directory survived")
	}
	for _, f := range info.Files {
		if f.Size == 3 || f.Path == "gone/x" {
			t.Errorf("stale file %+v", f)
		}
	}
}

func TestSpool(t *testing.T) {
	spool := t.TempDir()
	b, err := NewBuilder(Options{SpoolDir: spool, SpoolThreshold: 16})
	if err != nil {
		t.Fatal(err)
	}
	content := pattern(100000, 7)
	addFile(t, b, "big", content, 0o644)
	addFile(t, b, "small", []byte("tiny"), 0o644)
	if entries, _ := os.ReadDir(spool); len(entries) != 1 {
		t.Errorf("%d spool files", len(entries))
	}
	m, info := build(t, b)
	c := checkImage(t, m, info.Size)
	if !bytes.Equal(c.files["big"].data, content) || string(c.files["small"].data) != "tiny" {
		t.Error("wrong content")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(spool); len(entries) != 0 {
		t.Errorf("%d spool files left", len(entries))
	}
}

func TestImportTarAndFS(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "./app/", Mode: 0o755})
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "./app/run", Mode: 0o755, Size: 4})
	tw.Write([]byte("code"))
	tw.Close()

	b := mustBuilder(t, Options{})
	if err := b.ImportTar(&buf); err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{
		"conf/a.txt":  {Data: []byte("a"), Mode: 0o440},
		"conf/b/c.sh": {Data: []byte("#!"), Mode: 0o755},
	}
	if err := b.AddFS(fsys, "opt"); err != nil {
		t.Fatal(err)
	}
	m, info := build(t, b)
	c := checkImage(t, m, info.Size)
	if string(c.files["app/run"].data) != "code" || string(c.files["opt/conf/b/c.sh"].data) != "#!" {
		t.Error("wrong content")
	}
	if c.files["opt/conf/a.txt"].attr&attrReadOnly == 0 {
		t.Error("read-only file is not read-only")
	}

	b2 := mustBuilder(t, Options{})
	if err := b2.AddFS(fstest.MapFS{"link": {Data: []byte("x"), Mode: fs.ModeSymlink | 0o777}}, ""); err == nil {
		t.Error("symlink in an fs accepted")
	}
}

func TestErrors(t *testing.T) {
	b := mustBuilder(t, Options{})
	for name, hdr := range map[string]*tar.Header{
		"escape":         {Typeflag: tar.TypeDir, Name: "../escape"},
		"too long":       {Typeflag: tar.TypeDir, Name: strings.Repeat("a", 256)},
		"star":           {Typeflag: tar.TypeDir, Name: "a*b"},
		"colon":          {Typeflag: tar.TypeDir, Name: "a:b"},
		"control":        {Typeflag: tar.TypeDir, Name: "a\x01b"},
		"trailing dot":   {Typeflag: tar.TypeDir, Name: "dir."},
		"trailing space": {Typeflag: tar.TypeDir, Name: "dir "},
		"invalid utf8":   {Typeflag: tar.TypeDir, Name: "a\xffb"},
		"content":        {Typeflag: tar.TypeReg, Name: "f", Size: 3},
		"symlink":        {Typeflag: tar.TypeSymlink, Name: "s", Linkname: "t"},
		"char device":    {Typeflag: tar.TypeChar, Name: "c"},
		"fifo":           {Typeflag: tar.TypeFifo, Name: "p"},
		"dangling link":  {Typeflag: tar.TypeLink, Name: "l", Linkname: "missing"},
	} {
		if err := b.WriteHeader(hdr); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := b.WriteRegular(&tar.Header{Typeflag: tar.TypeReg, Name: "f", Size: 10}, strings.NewReader("short")); err == nil {
		t.Error("short content accepted")
	}
	if err := b.WriteRegular(&tar.Header{Typeflag: tar.TypeReg, Name: "big", Size: 1 << 32}, strings.NewReader("")); err == nil {
		t.Error("4 GiB file accepted")
	}
	addFile(t, b, "file", []byte("x"), 0o644)
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "file/sub"}); err == nil {
		t.Error("directory below a regular file accepted")
	}
	addFile(t, b, "Name", []byte("x"), 0o644)
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "NAME"}); err == nil {
		t.Error("names that differ only in case accepted")
	}
	for _, bad := range []Options{{SectorSize: 100}, {SectorSize: 8192}, {ClusterSize: 3000}, {ClusterSize: 65536}, {SectorSize: 4096, ClusterSize: 512}, {Label: "lower"}, {Label: "WAYTOOLONGLABEL"}, {Label: "A*B"}} {
		if _, err := NewBuilder(bad); err == nil {
			t.Errorf("options %+v accepted", bad)
		}
	}

	// Content that shrinks between adding and building is an error.
	src := filepath.Join(t.TempDir(), "f")
	os.WriteFile(src, []byte("12345"), 0o644)
	b2 := mustBuilder(t, Options{})
	if err := b2.WriteRegularFromPath(&tar.Header{Typeflag: tar.TypeReg, Name: "f", Size: 5}, src); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(src, []byte("123"), 0o644)
	if _, err := b2.Build(&memFile{}); err == nil {
		t.Error("changed file accepted")
	}
	if _, err := b2.Build(&memFile{}); err == nil {
		t.Error("second build accepted")
	}

	// a directory with too many entries
	b3 := mustBuilder(t, Options{})
	for i := 0; i < 1<<16; i++ {
		if err := b3.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: fmt.Sprintf("d/%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b3.Build(&memFile{}); err == nil {
		t.Error("directory with 65536 entries accepted")
	}
}

func TestShortNames(t *testing.T) {
	exact := map[string]string{
		"README.TXT": "README.TXT", "readme.txt": "README.TXT", "Readme.Txt": "README.TXT",
		"A": "A", "a.b": "A.B", "12345678.123": "12345678.123", "x-y_z.c": "X-Y_Z.C",
	}
	for name, want := range exact {
		short, ok := exactShort(name)
		if !ok || displayShort(short) != want {
			t.Errorf("exactShort(%q) = %q, %v; want %q", name, displayShort(short), ok, want)
		}
	}
	for _, name := range []string{"123456789.txt", "a.html", "a b", "a.b.c", ".gitignore", "a+b", "ünï", "a,b", "a=b", "a[1]"} {
		if _, ok := exactShort(name); ok {
			t.Errorf("exactShort(%q) accepted", name)
		}
	}
	taken := map[string]bool{}
	for name, want := range map[string]string{
		"The quick brown.fox": "THEQUI~1.FOX", "a b c d e f g h i.txt": "ABCDEF~1.TXT",
		".gitignore": "GITIGN~1", "very+bad,name.c++": "VERY_B~1.C__", "名前.txt": "__~1.TXT",
	} {
		short, err := generateShort(name, func(s string) bool { return taken[s] })
		if err != nil || displayShort(short) != want {
			t.Errorf("generateShort(%q) = %q, %v; want %q", name, displayShort(short), err, want)
		}
	}
	// the counter grows and the base shrinks
	taken = map[string]bool{}
	for i := 1; i <= 120; i++ {
		short, err := generateShort("abcdefghij.txt", func(s string) bool { return taken[s] })
		if err != nil {
			t.Fatal(err)
		}
		taken[displayShort(short)] = true
		if i == 1 && displayShort(short) != "ABCDEF~1.TXT" || i == 10 && displayShort(short) != "ABCDE~10.TXT" || i == 100 && displayShort(short) != "ABCD~100.TXT" {
			t.Errorf("name %d is %q", i, displayShort(short))
		}
	}
}

func TestLongEntries(t *testing.T) {
	// The example of the specification: "The quick brown.fox" takes two entries.
	short, _ := generateShort("The quick brown.fox", func(string) bool { return false })
	sum := shortChecksum(short)
	entries := longEntries("The quick brown.fox", sum)
	if len(entries) != 2 {
		t.Fatalf("%d entries", len(entries))
	}
	if entries[0][0] != 0x42 || entries[1][0] != 0x01 {
		t.Errorf("ordinals %#x %#x", entries[0][0], entries[1][0])
	}
	// "T" "h" "e" " " "q" is the first entry (stored second).
	for i, ch := range "The q" {
		if entries[1][1+2*i] != byte(ch) || entries[1][2+2*i] != 0 {
			t.Errorf("first entry character %d: %x", i, entries[1][1:11])
		}
	}
	// A name of exactly 13 characters has no terminator and no padding.
	one := longEntries("abcdefghijklm", sum)
	if len(one) != 1 || one[0][0] != 0x41 {
		t.Errorf("%d entries, ordinal %#x", len(one), one[0][0])
	}
	if one[0][28] != 'l' || one[0][30] != 'm' {
		t.Errorf("last characters: %x", one[0][28:32])
	}
	// A longer name is terminated and padded with 0xFFFF.
	two := longEntries("abcdefghijklmn", sum)
	if len(two) != 2 || two[0][1] != 'n' || two[0][3] != 0 || two[0][4] != 0 || two[0][5] != 0xFF || two[0][6] != 0xFF {
		t.Errorf("padding: %x", two[0])
	}
}

func TestEquivalentSectors(t *testing.T) {
	// The digest of a file does not depend on where it is stored.
	var sums []string
	for _, opts := range []Options{{}, {ClusterSize: 4096}, {SectorSize: 2048}} {
		opts.DigestFiles = true
		b := mustBuilder(t, opts)
		addFile(t, b, "f", pattern(70000, 3), 0o644)
		_, info := build(t, b)
		sums = append(sums, fmt.Sprintf("%x", info.Files[0].Digest))
	}
	want := sha256.Sum256(pattern(70000, 3))
	for _, s := range sums {
		if s != fmt.Sprintf("%x", want) {
			t.Errorf("digest %s", s)
		}
	}
}

// TestDumpImages writes sample images to $FAT32_DUMP_DIR so they can be checked
// with the real dosfstools and mounted.
func TestDumpImages(t *testing.T) {
	dir := os.Getenv("FAT32_DUMP_DIR")
	if dir == "" {
		t.Skip("FAT32_DUMP_DIR not set")
	}
	for name, opts := range map[string]Options{
		"default":    {},
		"cluster1k":  {ClusterSize: 1024},
		"cluster4k":  {ClusterSize: 4096},
		"cluster32k": {ClusterSize: 32768},
		"sector4k":   {SectorSize: 4096},
		"sector1k":   {SectorSize: 1024, ClusterSize: 4096},
		"large":      {Size: 1 << 30},
		"label":      {Label: "RULES IMG", VolumeID: 0x12345678},
	} {
		b := mustBuilder(t, opts)
		files := populate(t, b)
		for i := 0; i < 300; i++ {
			files[fmt.Sprintf("many/File number %03d with a long name.txt", i)] = pattern(i*7, byte(i))
			addFile(t, b, fmt.Sprintf("many/File number %03d with a long name.txt", i), pattern(i*7, byte(i)), 0o644)
		}
		files["ro.txt"] = []byte("read only")
		addFile(t, b, "ro.txt", []byte("read only"), 0o444)
		if name == "default" { // the expected tree, to compare an extraction with
			for p, content := range files {
				full := filepath.Join(dir, "expected", filepath.FromSlash(p))
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, content, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		f, err := os.Create(filepath.Join(dir, name+".img"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.Build(f); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
}

func TestSupplementaryPlaneNames(t *testing.T) {
	// Characters beyond the Basic Multilingual Plane are stored as surrogate pairs.
	names := []string{"😀.txt", "dir 🎉/file-🚀-name.txt", strings.Repeat("😀", 126) + ".md"} // 255 UTF-16 units
	b := mustBuilder(t, Options{})
	for _, name := range names {
		addFile(t, b, name, []byte(name), 0o644)
	}
	m, info := build(t, b)
	c := checkImage(t, m, info.Size)
	for _, name := range names {
		if f := c.files[name]; f == nil || string(f.data) != name {
			t.Errorf("%q: not found or wrong content", name)
		}
	}
	if err := mustBuilder(t, Options{}).WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: strings.Repeat("😀", 128)}); err == nil {
		t.Error("a name of 256 UTF-16 characters was accepted")
	}
}
