package iso9660

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// memTar is an in-memory tar archive that counts how often it is opened.
type memTar struct {
	data  []byte
	opens int
}

func (m *memTar) Open() (*tar.Reader, io.Closer, error) {
	m.opens++
	return tar.NewReader(bytes.NewReader(m.data)), io.NopCloser(nil), nil
}

type tarFile struct {
	name    string
	content []byte
}

func makeTar(t *testing.T, files []tarFile) *memTar {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: f.name, Size: int64(len(f.content)), Mode: 0o644, ModTime: epoch}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &memTar{data: buf.Bytes()}
}

// pattern is deterministic content of the given size.
func pattern(size int, seed byte) []byte {
	out := make([]byte, size)
	for i := range out {
		out[i] = byte(i) ^ seed
	}
	return out
}

func TestTarEntrySourceReadsTheArchiveOnce(t *testing.T) {
	files := []tarFile{
		{"a", pattern(5000, 1)},
		{"b", pattern(3<<20, 2)},
		{"c", pattern(70000, 3)},
		{"d", pattern(70000, 3)}, // the same content as c
		{"a", pattern(100, 4)},   // replaces the first a
	}
	archive := makeTar(t, files)

	// The reference image has the same entries streamed in through a reader.
	ref, refInfo := build(t, Options{RockRidge: true, DigestFiles: true}, func(b *Builder) error {
		for _, f := range files {
			hdr := &tar.Header{Typeflag: tar.TypeReg, Name: f.name, Size: int64(len(f.content)), Mode: 0o644, ModTime: epoch}
			if err := b.WriteRegular(hdr, bytes.NewReader(f.content)); err != nil {
				return err
			}
		}
		return nil
	})

	got, info := build(t, Options{RockRidge: true, DigestFiles: true, SpoolDir: t.TempDir(), SpoolThreshold: 16}, func(b *Builder) error {
		for i, f := range files {
			hdr := &tar.Header{Typeflag: tar.TypeReg, Name: f.name, Size: int64(len(f.content)), Mode: 0o644, ModTime: epoch}
			if err := b.WriteRegularFromTarEntry(hdr, archive, i, nil); err != nil {
				return err
			}
		}
		return nil
	})
	if !bytes.Equal(got.data, ref.data) {
		t.Error("the image differs from the one built with streamed content")
	}
	if len(info.Files) != len(refInfo.Files) {
		t.Errorf("the image has %d extents, want %d", len(info.Files), len(refInfo.Files))
	}
	// The files are written in the order they were added, which is the order
	// of the archive, so it is read exactly once.
	if archive.opens != 1 {
		t.Errorf("the archive was opened %d times, want once", archive.opens)
	}
}

func TestTarEntrySourceTrustsAKnownHash(t *testing.T) {
	files := []tarFile{{"x", pattern(9000, 1)}, {"y", pattern(9000, 2)}}
	archive := makeTar(t, files)
	fake := bytes.Repeat([]byte{0xAB}, sha256.Size)
	_, info := build(t, Options{DigestFiles: true}, func(b *Builder) error {
		for i, f := range files {
			hdr := &tar.Header{Typeflag: tar.TypeReg, Name: f.name, Size: int64(len(f.content)), Mode: 0o644, ModTime: epoch}
			hash := fake
			if i == 1 {
				sum := sha256.Sum256(f.content)
				hash = sum[:]
			}
			if err := b.WriteRegularFromTarEntry(hdr, archive, i, hash); err != nil {
				return err
			}
		}
		return nil
	})
	if len(info.Files) != 2 || !bytes.Equal(info.Files[0].Digest, fake) {
		t.Errorf("a trusted digest was not taken over: %+v", info.Files)
	}
}

func TestTarEntrySourceOutOfOrderAndChanged(t *testing.T) {
	files := []tarFile{{"x", pattern(9000, 1)}, {"y", pattern(9000, 2)}}
	archive := makeTar(t, files)
	img, _ := build(t, Options{RockRidge: true}, func(b *Builder) error {
		// Added in reverse order: the archive is started over, and the image
		// is still right.
		for i := len(files) - 1; i >= 0; i-- {
			hdr := &tar.Header{Typeflag: tar.TypeReg, Name: files[i].name, Size: 9000, Mode: 0o644, ModTime: epoch}
			if err := b.WriteRegularFromTarEntry(hdr, archive, i, nil); err != nil {
				return err
			}
		}
		return nil
	})
	entries := walk(t, img)
	rd := mustOpen(t, img)
	for _, f := range files {
		if got := readContent(t, rd, entries[f.name]); got != string(f.content) {
			t.Errorf("%s has the wrong content", f.name)
		}
	}

	// A size that no longer matches the archive is an error, not garbage.
	b, err := NewBuilder(Options{Time: epoch})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	defer b.Close()
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: "x", Size: 9001, Mode: 0o644, ModTime: epoch}
	if err := b.WriteRegularFromTarEntry(hdr, archive, 0, nil); err != nil {
		t.Fatalf("adding the entry: %v", err)
	}
	if _, err := b.Build(&memImage{}); err == nil {
		t.Error("a changed archive was accepted")
	}
}

func TestImportTar(t *testing.T) {
	files := []tarFile{{"dir/a", pattern(100, 1)}, {"dir/b", pattern(200, 2)}}
	archive := makeTar(t, files)
	img, _ := build(t, Options{RockRidge: true}, func(b *Builder) error {
		return b.ImportTar(bytes.NewReader(archive.data))
	})
	entries := walk(t, img)
	rd := mustOpen(t, img)
	for _, f := range files {
		if got := readContent(t, rd, entries[f.name]); got != string(f.content) {
			t.Errorf("%s has the wrong content", f.name)
		}
	}
}

func TestReplacedEntriesReleaseSpooledContent(t *testing.T) {
	spool := t.TempDir()
	b, err := NewBuilder(Options{RockRidge: true, Time: epoch, SpoolDir: spool, SpoolThreshold: 16})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	defer b.Close()
	addFile := func(name string, content []byte) {
		t.Helper()
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(content)), Mode: 0o644, ModTime: epoch}
		if err := b.WriteRegular(hdr, bytes.NewReader(content)); err != nil {
			t.Fatalf("adding %s: %v", name, err)
		}
	}
	spoolCount := func() int {
		t.Helper()
		entries, err := os.ReadDir(spool)
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}

	for i := 0; i < 20; i++ {
		addFile("f", pattern(1000, byte(i)))
	}
	if n := spoolCount(); n != 1 {
		t.Errorf("%d spool files after replacing one path, want 1", n)
	}

	// A subtree that is replaced by a file releases everything below it.
	addFile("d/x", pattern(1000, 1))
	addFile("d/y", pattern(1000, 2))
	if n := spoolCount(); n != 3 {
		t.Fatalf("%d spool files, want 3", n)
	}
	addFile("d", []byte("now a file"))
	if n := spoolCount(); n != 1 {
		t.Errorf("%d spool files after replacing a directory, want 1", n)
	}

	// A hard link is another name for the same content: it survives the
	// replacement of the name it was made from, and the content goes away with
	// the last name.
	addFile("orig", pattern(1000, 5))
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "link", Linkname: "orig", ModTime: epoch}); err != nil {
		t.Fatal(err)
	}
	addFile("orig", []byte("other"))
	if n := spoolCount(); n != 2 {
		t.Errorf("%d spool files while the link is still there, want 2", n)
	}
	addFile("link", []byte("gone"))
	if n := spoolCount(); n != 1 {
		t.Errorf("%d spool files after replacing the last name, want 1", n)
	}
}

func TestHardLinkSurvivesReplacingItsTarget(t *testing.T) {
	img, _ := build(t, Options{RockRidge: true, SpoolDir: t.TempDir(), SpoolThreshold: 16}, func(b *Builder) error {
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: "orig", Size: 1000, Mode: 0o644, ModTime: epoch}
		if err := b.WriteRegular(hdr, bytes.NewReader(pattern(1000, 5))); err != nil {
			return err
		}
		if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "link", Linkname: "orig", ModTime: epoch}); err != nil {
			return err
		}
		return b.WriteRegular(&tar.Header{Typeflag: tar.TypeReg, Name: "orig", Size: 5, Mode: 0o644, ModTime: epoch}, bytes.NewReader([]byte("other")))
	})
	entries := walk(t, img)
	rd := mustOpen(t, img)
	if got := readContent(t, rd, entries["link"]); got != string(pattern(1000, 5)) {
		t.Error("the link lost its content when the name it was made from was replaced")
	}
	if got := readContent(t, rd, entries["orig"]); got != "other" {
		t.Errorf("the replaced file has the content %q", got)
	}
}

func TestPathDigestIsReused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "content")
	if err := os.WriteFile(path, pattern(5000, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := NewBuilder(Options{DigestFiles: true, Time: epoch})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	defer b.Close()
	for _, name := range []string{"a", "b"} {
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: 5000, Mode: 0o644, ModTime: epoch}
		if err := b.WriteRegularFromPath(hdr, path); err != nil {
			t.Fatal(err)
		}
	}
	info, err := b.Build(&memImage{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := sha256Of(pattern(5000, 2))
	if len(info.Files) != 2 || !bytes.Equal(info.Files[0].Digest, want) || !bytes.Equal(info.Files[1].Digest, want) {
		t.Errorf("wrong digests for a file added twice: %+v", info.Files)
	}
	if len(b.pathDigests) != 1 {
		t.Errorf("%d remembered path digests, want 1", len(b.pathDigests))
	}
}

func TestImageDigestDoesNotChangeTheImage(t *testing.T) {
	var images [2][]byte
	for i, digest := range []bool{false, true} {
		img, _ := build(t, Options{RockRidge: true, Joliet: true, UDF: true, DigestImage: digest}, func(b *Builder) error {
			hdr := &tar.Header{Typeflag: tar.TypeReg, Name: "a", Size: 5000, Mode: 0o644, ModTime: epoch}
			return b.WriteRegular(hdr, bytes.NewReader(pattern(5000, 7)))
		})
		images[i] = img.data
	}
	if !bytes.Equal(images[0], images[1]) {
		t.Error("asking for the image digest changed the image")
	}
}
