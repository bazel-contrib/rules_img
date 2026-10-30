package cpio

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
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
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: f.name, Size: int64(len(f.content)), Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		tw.Write(f.content)
	}
	tw.Close()
	return &memTar{data: buf.Bytes()}
}

func TestTarEntrySource(t *testing.T) {
	files := []tarFile{
		{"a", pattern(5000, 1)},
		{"b", pattern(3<<20, 2)},
		{"c", pattern(70000, 3)},
		{"d", pattern(70000, 3)}, // same content as c
		{"a", pattern(100, 4)},   // replaces the first a
	}
	archive := makeTar(t, files)

	// Reference: the same entries streamed in.
	ref := mustBuilder(t, Options{Deduplicate: true, DigestFiles: true})
	for _, f := range files {
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: f.name, Size: int64(len(f.content)), Mode: 0o644}
		if err := ref.WriteRegularDeduplicated(hdr, bytes.NewReader(f.content)); err != nil {
			t.Fatal(err)
		}
	}
	want, wantInfo := build(t, ref)

	b := mustBuilder(t, Options{Deduplicate: true, DigestFiles: true, SpoolDir: t.TempDir(), SpoolThreshold: 16})
	for i, f := range files {
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: f.name, Size: int64(len(f.content)), Mode: 0o644}
		if err := b.WriteRegularFromTarEntry(hdr, archive, i, nil); err != nil {
			t.Fatal(err)
		}
	}
	got, info := build(t, b)
	if !bytes.Equal(got, want) {
		t.Fatal("archive differs from the one built with streamed content")
	}
	if len(info.Files) != len(wantInfo.Files) {
		t.Fatalf("%d files, want %d", len(info.Files), len(wantInfo.Files))
	}
	// Once for the digests that deduplication needs, once for writing.
	if archive.opens > 2 {
		t.Errorf("archive opened %d times, want at most 2", archive.opens)
	}
}

func TestTarEntrySourceReadsOnceWithKnownHashes(t *testing.T) {
	files := []tarFile{{"x", pattern(9000, 1)}, {"y", pattern(9000, 2)}, {"z", pattern(9000, 2)}}
	archive := makeTar(t, files)
	b := mustBuilder(t, Options{Deduplicate: true, DigestFiles: true})
	for i, f := range files {
		sum := sha256.Sum256(f.content)
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: f.name, Size: int64(len(f.content)), Mode: 0o644}
		if err := b.WriteRegularFromTarEntry(hdr, archive, i, sum[:]); err != nil {
			t.Fatal(err)
		}
	}
	_, info := build(t, b)
	if archive.opens != 1 {
		t.Errorf("archive opened %d times, want 1", archive.opens)
	}
	if len(info.Files) != 2 {
		t.Errorf("%d files, want 2 (y and z are the same)", len(info.Files))
	}
}

func TestTarEntrySourceOutOfOrderAndChanged(t *testing.T) {
	files := []tarFile{{"x", pattern(9000, 1)}, {"y", pattern(9000, 2)}}
	archive := makeTar(t, files)
	b := mustBuilder(t, Options{})
	// Added in reverse order: the archive is started over, and still right.
	for i := len(files) - 1; i >= 0; i-- {
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: files[i].name, Size: 9000, Mode: 0o644}
		if err := b.WriteRegularFromTarEntry(hdr, archive, i, nil); err != nil {
			t.Fatal(err)
		}
	}
	m, _ := build(t, b)
	for _, r := range parse(t, m) {
		want := files[0].content
		if r.name == "y" {
			want = files[1].content
		}
		if !bytes.Equal(r.data, want) {
			t.Errorf("%s: wrong content", r.name)
		}
	}

	// A size that no longer matches the archive is an error, not garbage.
	b2 := mustBuilder(t, Options{})
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: "x", Size: 9001, Mode: 0o644}
	if err := b2.WriteRegularFromTarEntry(hdr, archive, 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b2.Build(io.Discard); err == nil {
		t.Error("expected an error for a changed archive")
	}
}

func spoolCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestReplacedEntriesReleaseSpool(t *testing.T) {
	spool := t.TempDir()
	b := mustBuilder(t, Options{SpoolDir: spool, SpoolThreshold: 16})
	for i := 0; i < 20; i++ {
		addFile(t, b, "f", pattern(1000, byte(i)), 0o644)
	}
	if n := spoolCount(t, spool); n != 1 {
		t.Errorf("%d spool files after replacing one path, want 1", n)
	}

	// A subtree that is replaced by a file releases everything below it.
	addFile(t, b, "d/x", pattern(1000, 1), 0o644)
	addFile(t, b, "d/y", pattern(1000, 2), 0o644)
	if n := spoolCount(t, spool); n != 3 {
		t.Fatalf("%d spool files, want 3", n)
	}
	addFile(t, b, "d", []byte("now a file"), 0o644)
	if n := spoolCount(t, spool); n != 1 {
		t.Errorf("%d spool files after replacing a directory, want 1", n)
	}

	// A file that is still linked elsewhere keeps its content.
	addFile(t, b, "orig", pattern(1000, 5), 0o644)
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "link", Linkname: "orig"}); err != nil {
		t.Fatal(err)
	}
	addFile(t, b, "orig", []byte("other"), 0o644)
	m, _ := build(t, b)
	for _, r := range parse(t, m) {
		if r.name == "link" && !bytes.Equal(r.data, pattern(1000, 5)) {
			t.Error("link lost its content when its target path was replaced")
		}
	}
}

func TestDeduplicateReleasesSpool(t *testing.T) {
	spool := t.TempDir()
	b := mustBuilder(t, Options{SpoolDir: spool, SpoolThreshold: 16, Deduplicate: true})
	for _, name := range []string{"a", "b", "c"} {
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: 1000, Mode: 0o644}
		if err := b.WriteRegularDeduplicated(hdr, bytes.NewReader(pattern(1000, 1))); err != nil {
			t.Fatal(err)
		}
	}
	if n := spoolCount(t, spool); n != 3 {
		t.Fatalf("%d spool files, want 3", n)
	}
	build(t, b)
	// The duplicates were merged away before the data was written.
	if n := spoolCount(t, spool); n != 1 {
		t.Errorf("%d spool files after merging, want 1", n)
	}
}

func TestCloseRemovesSpool(t *testing.T) {
	spool := t.TempDir()
	b, err := NewBuilder(Options{SpoolDir: spool, SpoolThreshold: 16})
	if err != nil {
		t.Fatal(err)
	}
	addFile(t, b, "f", pattern(1000, 1), 0o644)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if n := spoolCount(t, spool); n != 0 {
		t.Errorf("%d spool files after Close", n)
	}
}

func TestKnownHashAndPathDigestAreReused(t *testing.T) {
	// A trusted digest is reported as given and not computed again.
	b := mustBuilder(t, Options{DigestFiles: true, Deduplicate: true})
	fake := bytes.Repeat([]byte{0xAB}, 32)
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: "f", Size: 5000, Mode: 0o644}
	if err := b.WriteRegularDeduplicatedKnownHash(hdr, bytes.NewReader(pattern(5000, 1)), fake); err != nil {
		t.Fatal(err)
	}
	_, info := build(t, b)
	if len(info.Files) != 1 || !bytes.Equal(info.Files[0].Digest, fake) {
		t.Errorf("digest %x was not taken over", info.Files[0].Digest)
	}

	// The same path added twice is hashed once.
	p := writeTemp(t, pattern(5000, 2))
	b2 := mustBuilder(t, Options{DigestFiles: true, Deduplicate: true})
	for _, name := range []string{"a", "b"} {
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: 5000, Mode: 0o644}
		if err := b2.WriteRegularFromPathDeduplicated(hdr, p); err != nil {
			t.Fatal(err)
		}
	}
	_, info2 := build(t, b2)
	want := sha256.Sum256(pattern(5000, 2))
	if len(info2.Files) != 1 || !bytes.Equal(info2.Files[0].Digest, want[:]) {
		t.Errorf("wrong digest for a file added twice: %+v", info2.Files)
	}
	if len(b2.pathDigests) != 1 {
		t.Errorf("%d remembered path digests, want 1", len(b2.pathDigests))
	}
}

func TestUnsortedTarIsReadOnce(t *testing.T) {
	// The archive order is not the name order in which the files are written.
	files := []tarFile{{"z", pattern(9000, 1)}, {"a", pattern(9000, 2)}, {"m", pattern(9000, 3)}}
	archive := makeTar(t, files)
	spool := t.TempDir()
	b := mustBuilder(t, Options{SpoolDir: spool})
	for i, f := range files {
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: f.name, Size: int64(len(f.content)), Mode: 0o644}
		if err := b.WriteRegularFromTarEntry(hdr, archive, i, nil); err != nil {
			t.Fatal(err)
		}
	}
	m, _ := build(t, b)
	if archive.opens != 1 {
		t.Errorf("archive opened %d times, want 1", archive.opens)
	}
	for _, r := range parse(t, m) {
		for _, f := range files {
			if r.name == f.name && !bytes.Equal(r.data, f.content) {
				t.Errorf("%s: wrong content", r.name)
			}
		}
	}
}

func TestSameTarEntryAddedTwice(t *testing.T) {
	files := []tarFile{{"b", pattern(9000, 1)}, {"a", pattern(9000, 2)}}
	archive := makeTar(t, files)
	b := mustBuilder(t, Options{})
	for _, name := range []string{"x", "y"} {
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: 9000, Mode: 0o644}
		if err := b.WriteRegularFromTarEntry(hdr, archive, 0, nil); err != nil {
			t.Fatal(err)
		}
	}
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: "a", Size: 9000, Mode: 0o644}
	if err := b.WriteRegularFromTarEntry(hdr, archive, 1, nil); err != nil {
		t.Fatal(err)
	}
	m, _ := build(t, b)
	for _, r := range parse(t, m) {
		if r.name != "a" && !bytes.Equal(r.data, files[0].content) {
			t.Errorf("%s: wrong content", r.name)
		}
	}
}

func TestMemoryBudget(t *testing.T) {
	spool := t.TempDir()
	b := mustBuilder(t, Options{SpoolDir: spool})
	const files = memoryBudget/defaultSpoolThreshold + 8
	for i := 0; i < files; i++ {
		addFile(t, b, fmt.Sprintf("f%03d", i), pattern(defaultSpoolThreshold, byte(i)), 0o644)
	}
	if b.memUsed > memoryBudget {
		t.Errorf("%d bytes held in memory, budget is %d", b.memUsed, memoryBudget)
	}
	if n := spoolCount(t, spool); n < 8 {
		t.Errorf("%d spool files, want at least 8", n)
	}
	// Replacing a file gives its memory back.
	used := b.memUsed
	addFile(t, b, "f000", []byte("x"), 0o644)
	if b.memUsed >= used {
		t.Errorf("memory in use did not drop: %d -> %d", used, b.memUsed)
	}
}
