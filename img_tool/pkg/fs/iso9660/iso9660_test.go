package iso9660

import (
	"archive/tar"
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// memImage is an in-memory io.WriterAt/io.ReaderAt that reads unwritten ranges
// as zeros, like a sparse file.
type memImage struct {
	data []byte
}

func (m *memImage) WriteAt(p []byte, off int64) (int, error) {
	if end := off + int64(len(p)); end > int64(len(m.data)) {
		m.data = append(m.data, make([]byte, end-int64(len(m.data)))...)
	}
	copy(m.data[off:], p)
	return len(p), nil
}

func (m *memImage) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(m.data)) {
		clear(p)
		return 0, io.EOF
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		clear(p[n:])
		return n, io.EOF
	}
	return n, nil
}

func (m *memImage) Truncate(size int64) error {
	if size <= int64(len(m.data)) {
		m.data = m.data[:size]
		return nil
	}
	m.data = append(m.data, make([]byte, size-int64(len(m.data)))...)
	return nil
}

var epoch = time.Date(2023, 1, 2, 3, 4, 5, 0, time.UTC)

// entryOf is a tar header with sensible defaults for the tests.
func entryOf(name string, typeflag byte, mode int64) *tar.Header {
	return &tar.Header{Name: name, Typeflag: typeflag, Mode: mode, ModTime: epoch}
}

// build runs a builder over a set of operations and returns the image.
func build(t *testing.T, opts Options, add func(*Builder) error) (*memImage, *Info) {
	t.Helper()
	if opts.Time.IsZero() {
		opts.Time = epoch
	}
	b, err := NewBuilder(opts)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	defer b.Close()
	if err := add(b); err != nil {
		t.Fatalf("adding entries: %v", err)
	}
	img := &memImage{}
	info, err := b.Build(img)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if info.Size != int64(len(img.data)) {
		t.Errorf("Info.Size is %d, but the image is %d bytes", info.Size, len(img.data))
	}
	return img, info
}

// walk collects every entry of an image by path.
func walk(t *testing.T, img *memImage) map[string]*Entry {
	t.Helper()
	rd, err := OpenReader(img)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	out := map[string]*Entry{}
	if err := rd.Walk(func(e *Entry) error {
		if _, dup := out[e.Path]; dup {
			t.Errorf("the path %q appears twice", e.Path)
		}
		copied := *e
		out[e.Path] = &copied
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return out
}

func TestSimpleImage(t *testing.T) {
	img, info := build(t, Options{VolumeID: "TESTVOL"}, func(b *Builder) error {
		if err := b.WriteHeader(entryOf("etc/", tar.TypeDir, 0o755)); err != nil {
			return err
		}
		if err := b.WriteRegular(sized(entryOf("etc/hello.txt", tar.TypeReg, 0o644), 5), strings.NewReader("hello")); err != nil {
			return err
		}
		return b.WriteRegular(sized(entryOf("README", tar.TypeReg, 0o644), 5), strings.NewReader("world"))
	})

	if info.Blocks*BlockSize != uint32(info.Size) {
		t.Errorf("Info.Blocks %d does not match the size %d", info.Blocks, info.Size)
	}
	rd, err := OpenReader(img)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	if rd.VolumeID != "TESTVOL" {
		t.Errorf("volume id is %q, want %q", rd.VolumeID, "TESTVOL")
	}
	if rd.RockRidge() {
		t.Error("the image announces Rock Ridge, which was not asked for")
	}

	entries := walk(t, img)
	// Without Rock Ridge the names are the mangled ISO 9660 identifiers.
	for _, want := range []string{"ETC", "ETC/HELLO.TXT", "README"} {
		if _, ok := entries[want]; !ok {
			t.Errorf("the image has no entry %q; it has %v", want, paths(entries))
		}
	}
	if e := entries["ETC/HELLO.TXT"]; e != nil {
		if e.Size != 5 {
			t.Errorf("size is %d, want 5", e.Size)
		}
		if got := readContent(t, rd, e); got != "hello" {
			t.Errorf("content is %q, want %q", got, "hello")
		}
	}
}

func TestRockRidgeMetadata(t *testing.T) {
	img, _ := build(t, Options{RockRidge: true}, func(b *Builder) error {
		dir := entryOf("Some Dir", tar.TypeDir, 0o750)
		dir.Uid, dir.Gid = 1000, 2000
		if err := b.WriteHeader(dir); err != nil {
			return err
		}
		file := sized(entryOf("Some Dir/a long file name with spaces.txt", tar.TypeReg, 0o640), 7)
		file.Uid, file.Gid = 1000, 2000
		if err := b.WriteRegular(file, strings.NewReader("content")); err != nil {
			return err
		}
		link := entryOf("link", tar.TypeSymlink, 0o777)
		link.Linkname = "Some Dir"
		return b.WriteHeader(link)
	})

	entries := walk(t, img)
	file := entries["Some Dir/a long file name with spaces.txt"]
	if file == nil {
		t.Fatalf("the real name is missing; the image has %v", paths(entries))
	}
	if file.Mode.Perm() != 0o640 {
		t.Errorf("mode is %v, want 0640", file.Mode.Perm())
	}
	if file.Uid != 1000 || file.Gid != 2000 {
		t.Errorf("owner is %d:%d, want 1000:2000", file.Uid, file.Gid)
	}
	if !file.Mtime.Equal(epoch) {
		t.Errorf("mtime is %v, want %v", file.Mtime, epoch)
	}
	dir := entries["Some Dir"]
	if dir == nil || !dir.Mode.IsDir() || dir.Mode.Perm() != 0o750 {
		t.Errorf("the directory is %+v, want a 0750 directory", dir)
	}
}

func TestRockRidgeSymlink(t *testing.T) {
	targets := []string{
		"file",
		"../sibling",
		"/absolute/path",
		"./here",
		strings.Repeat("a/", 200) + "deep",
	}
	img, _ := build(t, Options{RockRidge: true}, func(b *Builder) error {
		for i, target := range targets {
			hdr := entryOf("link"+string(rune('0'+i)), tar.TypeSymlink, 0o777)
			hdr.Linkname = target
			if err := b.WriteHeader(hdr); err != nil {
				return err
			}
		}
		return nil
	})

	entries := walk(t, img)
	for i, want := range targets {
		name := "link" + string(rune('0'+i))
		e := entries[name]
		if e == nil {
			t.Errorf("%s is missing", name)
			continue
		}
		if e.Mode&fs.ModeSymlink == 0 {
			t.Errorf("%s is not a symbolic link: %v", name, e.Mode)
		}
		if e.Target != want {
			t.Errorf("%s points at %q, want %q", name, e.Target, want)
		}
	}
}

func TestRockRidgeSpecialFiles(t *testing.T) {
	img, _ := build(t, Options{RockRidge: true}, func(b *Builder) error {
		dev := entryOf("dev/null", tar.TypeChar, 0o666)
		dev.Devmajor, dev.Devminor = 1, 3
		if err := b.WriteHeader(dev); err != nil {
			return err
		}
		disk := entryOf("dev/sda", tar.TypeBlock, 0o660)
		disk.Devmajor, disk.Devminor = 8, 0
		if err := b.WriteHeader(disk); err != nil {
			return err
		}
		return b.WriteHeader(entryOf("dev/pipe", tar.TypeFifo, 0o644))
	})

	entries := walk(t, img)
	if e := entries["dev/null"]; e == nil || e.Mode&fs.ModeCharDevice == 0 || e.Major != 1 || e.Minor != 3 {
		t.Errorf("dev/null is %+v, want a 1:3 character device", e)
	}
	if e := entries["dev/sda"]; e == nil || e.Mode&fs.ModeDevice == 0 || e.Mode&fs.ModeCharDevice != 0 || e.Major != 8 {
		t.Errorf("dev/sda is %+v, want an 8:0 block device", e)
	}
	if e := entries["dev/pipe"]; e == nil || e.Mode&fs.ModeNamedPipe == 0 {
		t.Errorf("dev/pipe is %+v, want a FIFO", e)
	}
}

func TestSpecialFilesNeedRockRidge(t *testing.T) {
	b, err := NewBuilder(Options{})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	defer b.Close()
	link := entryOf("link", tar.TypeSymlink, 0o777)
	link.Linkname = "target"
	if err := b.WriteHeader(link); err == nil || !strings.Contains(err.Error(), "Rock Ridge") {
		t.Errorf("adding a symbolic link without Rock Ridge gave %v, want a Rock Ridge error", err)
	}
	if err := b.WriteHeader(entryOf("pipe", tar.TypeFifo, 0o644)); err == nil || !strings.Contains(err.Error(), "Rock Ridge") {
		t.Errorf("adding a FIFO without Rock Ridge gave %v, want a Rock Ridge error", err)
	}
}

func TestHardLinksShareTheirExtent(t *testing.T) {
	img, info := build(t, Options{RockRidge: true}, func(b *Builder) error {
		if err := b.WriteRegular(sized(entryOf("a", tar.TypeReg, 0o644), 5), strings.NewReader("hello")); err != nil {
			return err
		}
		link := entryOf("b", tar.TypeLink, 0o644)
		link.Linkname = "a"
		return b.WriteHeader(link)
	})

	if len(info.Files) != 1 {
		t.Errorf("Info.Files has %d entries, want one shared extent", len(info.Files))
	}
	entries := walk(t, img)
	a, bEntry := entries["a"], entries["b"]
	if a == nil || bEntry == nil {
		t.Fatalf("both names should exist; the image has %v", paths(entries))
	}
	if a.Offset != bEntry.Offset {
		t.Errorf("the two names point at %d and %d, want the same extent", a.Offset, bEntry.Offset)
	}
	if a.Nlink != 2 {
		t.Errorf("the link count is %d, want 2", a.Nlink)
	}
}

func TestDeduplicationSharesExtents(t *testing.T) {
	add := func(b *Builder) error {
		for _, name := range []string{"a", "b", "c"} {
			if err := b.WriteRegularDeduplicated(sized(entryOf(name, tar.TypeReg, 0o644), 5), strings.NewReader("hello")); err != nil {
				return err
			}
		}
		return b.WriteRegularDeduplicated(sized(entryOf("d", tar.TypeReg, 0o644), 5), strings.NewReader("other"))
	}
	_, deduped := build(t, Options{RockRidge: true, Deduplicate: true}, add)
	if len(deduped.Files) != 2 {
		t.Errorf("with deduplication the image has %d extents, want 2", len(deduped.Files))
	}
	_, plain := build(t, Options{RockRidge: true}, add)
	if len(plain.Files) != 4 {
		t.Errorf("without deduplication the image has %d extents, want 4", len(plain.Files))
	}
}

func TestJolietHierarchy(t *testing.T) {
	img, _ := build(t, Options{Joliet: true}, func(b *Builder) error {
		if err := b.WriteHeader(entryOf("Mixed Case Dir", tar.TypeDir, 0o755)); err != nil {
			return err
		}
		return b.WriteRegular(sized(entryOf("Mixed Case Dir/Readme.md", tar.TypeReg, 0o644), 2), strings.NewReader("hi"))
	})

	j, err := openJoliet(img)
	if err != nil {
		t.Fatalf("opening the Joliet hierarchy: %v", err)
	}
	names := j.names(t)
	for _, want := range []string{"Mixed Case Dir", "Mixed Case Dir/Readme.md"} {
		if !names[want] {
			t.Errorf("the Joliet hierarchy has no %q; it has %v", want, keys(names))
		}
	}
	// The primary hierarchy still carries the mangled identifiers.
	if entries := walk(t, img); entries["MIXED_CASE_DIR/README.MD"] == nil {
		t.Errorf("the primary hierarchy is missing the mangled name; it has %v", paths(entries))
	}
}

func TestLongJolietNameNeedsLongNames(t *testing.T) {
	name := strings.Repeat("x", 80) + ".txt"
	_, err := NewBuilder(Options{Joliet: true})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	// 84 characters are truncated to the 64 the specification allows, so the
	// name is not what went in; with long names it survives.
	for _, long := range []bool{false, true} {
		b, err := NewBuilder(Options{Joliet: true, JolietLongNames: long, Time: epoch})
		if err != nil {
			t.Fatalf("NewBuilder: %v", err)
		}
		if err := b.WriteRegular(sized(entryOf(name, tar.TypeReg, 0o644), 1), strings.NewReader("x")); err != nil {
			t.Fatalf("adding the file: %v", err)
		}
		img := &memImage{}
		if _, err := b.Build(img); err != nil {
			t.Fatalf("Build: %v", err)
		}
		b.Close()
		j, err := openJoliet(img)
		if err != nil {
			t.Fatalf("opening the Joliet hierarchy: %v", err)
		}
		got := keys(j.names(t))
		if len(got) != 1 {
			t.Fatalf("the Joliet hierarchy has %v, want one name", got)
		}
		if long && got[0] != name {
			t.Errorf("with long names the identifier is %q, want %q", got[0], name)
		}
		if !long && len(got[0]) != maxJolietChars {
			t.Errorf("without long names the identifier is %d characters, want %d", len(got[0]), maxJolietChars)
		}
	}
}

func TestDeterministicAndSparse(t *testing.T) {
	add := func(b *Builder) error {
		if err := b.WriteRegular(sized(entryOf("zeros", tar.TypeReg, 0o644), 1<<16), bytes.NewReader(make([]byte, 1<<16))); err != nil {
			return err
		}
		return b.WriteRegular(sized(entryOf("data", tar.TypeReg, 0o644), 4), strings.NewReader("data"))
	}
	first, firstInfo := build(t, Options{RockRidge: true, Joliet: true, UDF: true, DigestImage: true}, add)
	second, secondInfo := build(t, Options{RockRidge: true, Joliet: true, UDF: true, DigestImage: true}, add)
	if !bytes.Equal(first.data, second.data) {
		t.Error("two builds of the same input gave different images")
	}
	if !bytes.Equal(firstInfo.Digest, secondInfo.Digest) {
		t.Error("two builds of the same input gave different digests")
	}
	if got := sha256Of(first.data); !bytes.Equal(got, firstInfo.Digest) {
		t.Errorf("Info.Digest %x does not match the image %x", firstInfo.Digest, got)
	}
	// The all-zero file was never written, so the image is sparse.
	entries := walk(t, first)
	e := entries["zeros"]
	if e == nil {
		t.Fatalf("the all-zero file is missing")
	}
	if got := readContent(t, mustOpen(t, first), e); got != strings.Repeat("\x00", 1<<16) {
		t.Errorf("the all-zero file reads back as %d bytes of something else", len(got))
	}
}

func TestMinimumSize(t *testing.T) {
	const want = 4 << 20
	_, info := build(t, Options{Size: want}, func(b *Builder) error {
		return b.WriteRegular(sized(entryOf("a", tar.TypeReg, 0o644), 1), strings.NewReader("a"))
	})
	if info.Size < want {
		t.Errorf("the image is %d bytes, want at least %d", info.Size, want)
	}
	if info.Size >= want+BlockSize {
		t.Errorf("the image is %d bytes, more than one block past the requested %d", info.Size, want)
	}
}

func TestDigestFiles(t *testing.T) {
	_, info := build(t, Options{DigestFiles: true}, func(b *Builder) error {
		return b.WriteRegular(sized(entryOf("a", tar.TypeReg, 0o644), 5), strings.NewReader("hello"))
	})
	if len(info.Files) != 1 {
		t.Fatalf("Info.Files has %d entries, want 1", len(info.Files))
	}
	if got, want := info.Files[0].Digest, sha256Of([]byte("hello")); !bytes.Equal(got, want) {
		t.Errorf("the digest is %x, want %x", got, want)
	}
	if info.Files[0].Offset%BlockSize != 0 {
		t.Errorf("the extent starts at %d, which is not a block boundary", info.Files[0].Offset)
	}
}

func TestFromPathAndFS(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("from a path"), 0o644); err != nil {
		t.Fatal(err)
	}
	img, _ := build(t, Options{RockRidge: true}, func(b *Builder) error {
		if err := b.WriteRegularFromPath(sized(entryOf("file", tar.TypeReg, 0o644), 11), filepath.Join(dir, "file")); err != nil {
			return err
		}
		return b.AddFS(os.DirFS(dir), "tree")
	})
	entries := walk(t, img)
	rd := mustOpen(t, img)
	if e := entries["file"]; e == nil || readContent(t, rd, e) != "from a path" {
		t.Errorf("the file added by path is %+v", e)
	}
	if e := entries["tree/file"]; e == nil || readContent(t, rd, e) != "from a path" {
		t.Errorf("the file added from an fs.FS is %+v", e)
	}
}

func TestReplacingEntries(t *testing.T) {
	img, _ := build(t, Options{RockRidge: true}, func(b *Builder) error {
		if err := b.WriteRegular(sized(entryOf("a", tar.TypeReg, 0o644), 3), strings.NewReader("old")); err != nil {
			return err
		}
		if err := b.WriteRegular(sized(entryOf("a", tar.TypeReg, 0o600), 3), strings.NewReader("new")); err != nil {
			return err
		}
		// Adding a directory again keeps its children and takes the metadata.
		if err := b.WriteHeader(entryOf("d/", tar.TypeDir, 0o755)); err != nil {
			return err
		}
		if err := b.WriteRegular(sized(entryOf("d/child", tar.TypeReg, 0o644), 1), strings.NewReader("c")); err != nil {
			return err
		}
		return b.WriteHeader(entryOf("d/", tar.TypeDir, 0o700))
	})

	entries := walk(t, img)
	rd := mustOpen(t, img)
	if e := entries["a"]; e == nil || readContent(t, rd, e) != "new" || e.Mode.Perm() != 0o600 {
		t.Errorf("the replaced file is %+v", e)
	}
	if e := entries["d"]; e == nil || e.Mode.Perm() != 0o700 {
		t.Errorf("the directory is %+v, want mode 0700", e)
	}
	if entries["d/child"] == nil {
		t.Error("re-adding the directory dropped its child")
	}
}

func TestNameClashesAreMangledApart(t *testing.T) {
	img, _ := build(t, Options{RockRidge: true}, func(b *Builder) error {
		for _, name := range []string{"file.txt", "FILE.TXT", "file!txt", "fileXtxt"} {
			if err := b.WriteRegular(sized(entryOf(name, tar.TypeReg, 0o644), 1), strings.NewReader("x")); err != nil {
				return err
			}
		}
		return nil
	})
	entries := walk(t, img)
	if len(entries) != 5 { // the four files and the root
		t.Errorf("the image has %v, want four distinct files", paths(entries))
	}
}

func TestDeepAndWideTree(t *testing.T) {
	// ISO 9660 limits the hierarchy to eight levels; the writer does not
	// relocate deep directories, which every current reader copes with.
	deep := ""
	for i := 0; i < 20; i++ {
		deep = filepath.ToSlash(filepath.Join(deep, "level"+string(rune('a'+i))))
	}
	img, _ := build(t, Options{RockRidge: true, Joliet: true, UDF: true}, func(b *Builder) error {
		if err := b.WriteRegular(sized(entryOf(deep+"/file", tar.TypeReg, 0o644), 1), strings.NewReader("x")); err != nil {
			return err
		}
		// A directory with enough children to need several blocks.
		for i := 0; i < 200; i++ {
			name := "wide/" + strings.Repeat("n", 60) + string(rune('A'+i%26)) + string(rune('A'+i/26))
			if err := b.WriteRegular(sized(entryOf(name, tar.TypeReg, 0o644), 1), strings.NewReader("x")); err != nil {
				return err
			}
		}
		return nil
	})
	entries := walk(t, img)
	if entries[deep+"/file"] == nil {
		t.Errorf("the deeply nested file is missing")
	}
	wide := 0
	for p := range entries {
		if strings.HasPrefix(p, "wide/") {
			wide++
		}
	}
	if wide != 200 {
		t.Errorf("the wide directory has %d entries, want 200", wide)
	}
}

func TestInvalidNames(t *testing.T) {
	b, err := NewBuilder(Options{})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	defer b.Close()
	if err := b.WriteHeader(entryOf("../escape", tar.TypeDir, 0o755)); err == nil {
		t.Error("a path that escapes the root was accepted")
	}
	long := strings.Repeat("x", 300)
	if err := b.WriteHeader(entryOf(long, tar.TypeDir, 0o755)); err == nil {
		t.Error("an over-long path component was accepted")
	}
}

func TestBuildOnlyOnce(t *testing.T) {
	b, err := NewBuilder(Options{Time: epoch})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	defer b.Close()
	if _, err := b.Build(&memImage{}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := b.Build(&memImage{}); err == nil {
		t.Error("the image was built twice")
	}
	if err := b.WriteHeader(entryOf("late", tar.TypeDir, 0o755)); err == nil {
		t.Error("an entry was added after the image was built")
	}
}

func TestDigestImageNeedsAReadableTarget(t *testing.T) {
	b, err := NewBuilder(Options{DigestImage: true, Time: epoch})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	defer b.Close()
	if _, err := b.Build(writeOnly{}); err == nil {
		t.Error("DigestImage was accepted with a target that cannot be read back")
	}
}

type writeOnly struct{}

func (writeOnly) WriteAt(p []byte, off int64) (int, error) { return len(p), nil }

// Helpers.

func sized(hdr *tar.Header, size int64) *tar.Header {
	hdr.Size = size
	return hdr
}

func mustOpen(t *testing.T, img *memImage) *Reader {
	t.Helper()
	rd, err := OpenReader(img)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	return rd
}

func readContent(t *testing.T, rd *Reader, e *Entry) string {
	t.Helper()
	data, err := io.ReadAll(rd.Content(e))
	if err != nil {
		t.Fatalf("reading %q: %v", e.Path, err)
	}
	return string(data)
}

func paths(entries map[string]*Entry) []string { return keys(toSet(entries)) }

func toSet[V any](m map[string]V) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}

// TestImageSizeBeyondTheFormat covers the limit of the 32 bit block counts: a
// requested size past it has to be an error, not an image whose block count
// wrapped around into something small.
func TestImageSizeBeyondTheFormat(t *testing.T) {
	for _, size := range []int64{maxImageSize + 1, 1 << 43, 1 << 62} {
		if _, err := NewBuilder(Options{Size: size}); err == nil {
			t.Errorf("Size %d was accepted, but it is larger than the %d bytes the format can describe", size, int64(maxImageSize))
		}
	}
	// The largest size the format can describe is still accepted, and planning
	// it does not wrap the block count.
	b, err := NewBuilder(Options{Size: maxImageSize, Time: epoch})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	defer b.Close()
	l, err := b.plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if l.totalBlocks != maxVolumeBlocks {
		t.Errorf("the plan has %d blocks, want %d", l.totalBlocks, uint32(maxVolumeBlocks))
	}
}

// TestJolietNameClash covers the identifier counter in the Joliet hierarchy:
// names that only differ past the limit collide, and the counter that tells
// them apart has to cut the name on a character boundary, not a byte one.
func TestJolietNameClash(t *testing.T) {
	// Three-byte characters, so a byte-wise cut would split one.
	long := strings.Repeat("ä", maxJolietChars) // two bytes per character
	wide := strings.Repeat("日", maxJolietChars) // three bytes per character
	names := []string{
		long + "-one.txt",
		long + "-two.txt",
		wide + "-one.txt",
		wide + "-two.txt",
	}
	img, _ := build(t, Options{Joliet: true, RockRidge: true}, func(b *Builder) error {
		for _, name := range names {
			if err := b.WriteRegular(sized(entryOf(name, tar.TypeReg, 0o644), 1), strings.NewReader("x")); err != nil {
				return err
			}
		}
		return nil
	})
	check(t, img)

	j, err := openJoliet(img)
	if err != nil {
		t.Fatalf("opening the Joliet hierarchy: %v", err)
	}
	got := keys(j.names(t))
	if len(got) != len(names) {
		t.Fatalf("the Joliet hierarchy has %d names, want %d: %q", len(got), len(names), got)
	}
	for _, name := range got {
		if !utf8.ValidString(name) {
			t.Errorf("the identifier %q is not valid UTF-8", name)
		}
		if strings.ContainsRune(name, utf8.RuneError) {
			t.Errorf("the identifier %q holds a replacement character, so a name was cut inside one", name)
		}
		if n := utf16Len(name); n > maxJolietChars {
			t.Errorf("the identifier %q is %d characters, more than the %d allowed", name, n, maxJolietChars)
		}
	}
}

func TestCompareIdentifiers(t *testing.T) {
	// A name is less than every name it is a prefix of, because the shorter
	// one is padded with spaces and a space is below every character a name
	// may hold.
	for _, tc := range []struct {
		name string
		a, b string
		unit int
		want int
	}{
		{"prefix is less", "A", "AB", 1, -1},
		{"longer is more", "AB", "A", 1, 1},
		{"equal", "ABC", "ABC", 1, 0},
		{"by character", "AC", "AB", 1, 1},
		{"space pads below", "A", "A_", 1, -1},
		{"version suffix", "A.TXT;1", "AB.TXT;1", 1, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := compareIdentifiers([]byte(tc.a), []byte(tc.b), 1); got != tc.want {
				t.Errorf("compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
			// The same holds for Joliet, where a character is two bytes and the
			// padding is the UCS-2 space rather than a single 0x20 byte.
			if got := compareIdentifiers(ucs2(tc.a), ucs2(tc.b), 2); got != tc.want {
				t.Errorf("compare(ucs2(%q), ucs2(%q)) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestJolietPrefixNamesAreSorted is the same property on a real image: a
// directory holding a name and a name it is a prefix of has to be sorted, which
// is what the checker verifies for both hierarchies.
func TestJolietPrefixNamesAreSorted(t *testing.T) {
	// Added in an order that is neither sorted nor reverse sorted.
	names := []string{"report.txt", "re", "report", "r", "rep"}
	img, _ := build(t, Options{Joliet: true, RockRidge: true}, func(b *Builder) error {
		for _, name := range names {
			if err := b.WriteRegular(sized(entryOf("dir/"+name, tar.TypeReg, 0o644), 1), strings.NewReader("x")); err != nil {
				return err
			}
		}
		return nil
	})
	check(t, img)

	j, err := openJoliet(img)
	if err != nil {
		t.Fatalf("opening the Joliet hierarchy: %v", err)
	}
	for _, name := range names {
		if !j.names(t)["dir/"+name] {
			t.Errorf("the Joliet hierarchy has no %q", "dir/"+name)
		}
	}
}

// TestMultiExtentPlan covers interchange level 3: a file larger than one
// extent gets one directory record per extent, all with the same identifier,
// all but the last marked as continuing, and the extents contiguous. It only
// plans the image, so no content has to exist.
func TestMultiExtentPlan(t *testing.T) {
	const big = 3*maxExtentLength + 5000
	b, err := NewBuilder(Options{RockRidge: true, Joliet: true, Time: epoch})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	defer b.Close()
	if err := b.WriteRegularFromPath(sized(entryOf("big.bin", tar.TypeReg, 0o644), big), "/nonexistent"); err != nil {
		t.Fatalf("adding the file: %v", err)
	}
	if err := b.WriteRegularFromPath(sized(entryOf("small.bin", tar.TypeReg, 0o644), 10), "/nonexistent"); err != nil {
		t.Fatalf("adding the file: %v", err)
	}
	l, err := b.plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	for _, h := range l.hierarchies() {
		var records []*record
		for _, rec := range h.root().records {
			if rec.node.kind == kindReg && rec.node.size == big {
				records = append(records, rec)
			}
		}
		if len(records) != 4 {
			t.Fatalf("the %d byte file has %d records, want 4", int64(big), len(records))
		}
		var total int64
		var want uint32
		for i, rec := range records {
			lba, length := rec.location()
			total += int64(length)
			last := i == len(records)-1
			if rec.lastExtent != last {
				t.Errorf("record %d: lastExtent is %v, want %v", i, rec.lastExtent, last)
			}
			if !last && length != maxExtentLength {
				t.Errorf("record %d is %d bytes, want the full %d", i, length, int64(maxExtentLength))
			}
			if last && length != 5000 {
				t.Errorf("the last record is %d bytes, want 5000", length)
			}
			if i == 0 {
				want = lba
			}
			if lba != want {
				t.Errorf("record %d starts at block %d, want %d (the extents have to be contiguous)", i, lba, want)
			}
			want = lba + length/BlockSize
			if !bytes.Equal(rec.id, records[0].id) {
				t.Errorf("record %d has a different identifier than the first", i)
			}
		}
		if total != big {
			t.Errorf("the records cover %d bytes, want %d", total, int64(big))
		}
	}
}

// TestMultiExtentImage writes a real image with a file larger than one extent
// and reads it back, which is what proves the records, the multi-extent flags
// and the reader's coalescing agree. The content is a sparse file, so the test
// moves bytes but allocates none.
func TestMultiExtentImage(t *testing.T) {
	if testing.Short() {
		t.Skip("writing a file larger than 4 GiB")
	}
	const big = maxExtentLength + 5000
	dir := t.TempDir()
	path := filepath.Join(dir, "big.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// A sparse file: the tail is written, everything before it reads as zeros.
	if _, err := f.WriteAt([]byte("end of the file"), big-15); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	b, err := NewBuilder(Options{RockRidge: true, Joliet: true, UDF: true, Time: epoch})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	defer b.Close()
	if err := b.WriteRegularFromPath(sized(entryOf("big.bin", tar.TypeReg, 0o644), big), path); err != nil {
		t.Fatalf("adding the file: %v", err)
	}
	if err := b.WriteRegular(sized(entryOf("after.txt", tar.TypeReg, 0o644), 5), strings.NewReader("after")); err != nil {
		t.Fatalf("adding the file: %v", err)
	}

	out, err := os.Create(filepath.Join(dir, "image.iso"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	info, err := b.Build(out)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(info.Files) != 2 {
		t.Fatalf("Info.Files has %d entries, want 2", len(info.Files))
	}
	// The file is one contiguous run, whatever the number of records, which is
	// what lets a compact stream reference it as a single range.
	if info.Files[0].Size != big {
		t.Errorf("Info.Files[0] is %d bytes, want %d", info.Files[0].Size, int64(big))
	}

	rd, err := OpenReader(out)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	entries := map[string]*Entry{}
	if err := rd.Walk(func(e *Entry) error {
		copied := *e
		entries[e.Path] = &copied
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	e := entries["big.bin"]
	if e == nil {
		t.Fatalf("the large file is missing; the image has %v", paths(entries))
	}
	if e.Size != big {
		t.Errorf("the reader reports %d bytes, want %d (the extents were not added up)", e.Size, int64(big))
	}
	if e.Offset != info.Files[0].Offset {
		t.Errorf("the reader puts the file at %d, want %d", e.Offset, info.Files[0].Offset)
	}
	// The entry after the large file is still found, so the records did not
	// confuse the walk.
	if entries["after.txt"] == nil {
		t.Error("the entry after the large file is missing")
	}
	tail := make([]byte, 15)
	if _, err := out.ReadAt(tail, e.Offset+big-15); err != nil {
		t.Fatal(err)
	}
	if string(tail) != "end of the file" {
		t.Errorf("the last bytes of the file are %q", tail)
	}
}

func TestRationalizedRockRidge(t *testing.T) {
	add := func(b *Builder) error {
		data := entryOf("data.txt", tar.TypeReg, 0o640)
		data.Uid, data.Gid = 1000, 2000
		if err := b.WriteRegular(sized(data, 4), strings.NewReader("data")); err != nil {
			return err
		}
		// Only the group execute bit is set, and setuid, setgid and sticky are
		// all on: all three have to go, and the execute bit has to reach every
		// class.
		exe := entryOf("tool", tar.TypeReg, 0o7010)
		exe.Uid, exe.Gid = 1000, 2000
		if err := b.WriteRegular(sized(exe, 4), strings.NewReader("tool")); err != nil {
			return err
		}
		dir := entryOf("sub", tar.TypeDir, 0o700)
		dir.Uid, dir.Gid = 1000, 2000
		if err := b.WriteHeader(dir); err != nil {
			return err
		}
		// A directory nothing describes, which the writer invents.
		return b.WriteRegular(sized(entryOf("made/up/file", tar.TypeReg, 0o604), 2), strings.NewReader("up"))
	}

	plain, _ := build(t, Options{RockRidge: true}, add)
	rational, _ := build(t, Options{RockRidge: true, RationalizeRockRidge: true}, add)
	check(t, rational)

	for path, want := range map[string]fs.FileMode{
		"data.txt":     0o444,
		"tool":         0o555,
		"sub":          0o555,
		"made":         0o555,
		"made/up/file": 0o444,
	} {
		got := walk(t, rational)[path]
		if got == nil {
			t.Errorf("%q is missing", path)
			continue
		}
		if got.Mode.Perm() != want {
			t.Errorf("%q has mode %v, want %v", path, got.Mode.Perm(), want)
		}
		if got.Uid != 0 || got.Gid != 0 {
			t.Errorf("%q is owned by %d:%d, want root", path, got.Uid, got.Gid)
		}
		if got.Mode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
			t.Errorf("%q keeps a setuid, setgid or sticky bit: %v", path, got.Mode)
		}
	}
	// Without the option the metadata is whatever was asked for.
	if e := walk(t, plain)["data.txt"]; e == nil || e.Mode.Perm() != 0o640 || e.Uid != 1000 {
		t.Errorf("without rationalizing, data.txt is %+v", e)
	}
}
