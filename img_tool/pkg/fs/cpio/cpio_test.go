package cpio

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

type rec struct {
	name                string
	ino, mode, uid, gid uint32
	nlink, mtime        uint32
	rmaj, rmin          uint32
	dataOffset          int
	data                []byte
}

// parse is an independent reader of the newc format.
func parse(t *testing.T, b []byte) []rec {
	t.Helper()
	var out []rec
	for off := 0; ; {
		if off+headerSize > len(b) {
			t.Fatalf("archive ends at %d without a trailer", off)
		}
		if string(b[off:off+6]) != "070701" {
			t.Fatalf("bad magic at %d: %q", off, b[off:off+6])
		}
		var f [13]uint32
		for i := range f {
			v, err := strconv.ParseUint(string(b[off+6+i*8:off+14+i*8]), 16, 32)
			if err != nil {
				t.Fatal(err)
			}
			f[i] = uint32(v)
		}
		nameStart := off + headerSize
		name := string(b[nameStart : nameStart+int(f[11])-1])
		if b[nameStart+int(f[11])-1] != 0 {
			t.Fatalf("%s: name is not NUL-terminated", name)
		}
		dataStart := (nameStart + int(f[11]) + 3) &^ 3
		dataEnd := dataStart + int(f[6])
		if name == trailerName {
			if rest := b[(dataEnd+3)&^3:]; len(rest) != 0 {
				t.Fatalf("%d bytes after the trailer", len(rest))
			}
			return out
		}
		out = append(out, rec{name, f[0], f[1], f[2], f[3], f[4], f[5], f[9], f[10], dataStart, b[dataStart:dataEnd]})
		off = (dataEnd + 3) &^ 3
	}
}

func mustBuilder(t *testing.T, opts Options) *Builder {
	t.Helper()
	b, err := NewBuilder(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

func build(t *testing.T, b *Builder) ([]byte, *Info) {
	t.Helper()
	var buf bytes.Buffer
	info, err := b.Build(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if int64(buf.Len()) != info.Size {
		t.Fatalf("archive is %d bytes, info says %d", buf.Len(), info.Size)
	}
	return buf.Bytes(), info
}

func pattern(n int, seed byte) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = seed + byte(i*7)
	}
	return p
}

func addFile(t *testing.T, b *Builder, name string, content []byte, mode int64) {
	t.Helper()
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(content)), Mode: mode}
	if err := b.WriteRegular(hdr, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
}

func writeTemp(t *testing.T, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func hdrReg(name string, content string) (*tar.Header, io.Reader) {
	return &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(content)), Mode: 0o644, Uid: 1000, Gid: 100, ModTime: time.Unix(1700000000, 0)}, strings.NewReader(content)
}

func populate(t *testing.T, b *Builder) {
	t.Helper()
	for _, h := range []*tar.Header{
		{Typeflag: tar.TypeDir, Name: "etc/", Mode: 0o750, Uid: 1, Gid: 2},
		{Typeflag: tar.TypeSymlink, Name: "bin/sh", Linkname: "busybox", Mode: 0o777},
		{Typeflag: tar.TypeChar, Name: "dev/console", Mode: 0o600, Devmajor: 5, Devminor: 1},
		{Typeflag: tar.TypeBlock, Name: "dev/sda", Mode: 0o660, Devmajor: 8, Devminor: 0},
		{Typeflag: tar.TypeFifo, Name: "run/fifo", Mode: 0o644},
		{Typeflag: tar.TypeReg, Name: "empty", Mode: 0o644},
	} {
		if err := b.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{
		"bin/busybox": "ELF-ish\n",
		"etc/hosts":   "127.0.0.1 localhost\n",
		"init":        strings.Repeat("x", 4097),
	} {
		h, r := hdrReg(name, content)
		if err := b.WriteRegular(h, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "bin/ls", Linkname: "bin/busybox"}); err != nil {
		t.Fatal(err)
	}
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "bin/cat", Linkname: "bin/ls"}); err != nil {
		t.Fatal(err)
	}
}

func TestBuild(t *testing.T) {
	b := mustBuilder(t, Options{SpoolThreshold: 16, SpoolDir: t.TempDir()}) // spools init to a file
	populate(t, b)
	archive, info := build(t, b)
	members := parse(t, archive)
	if info.Entries != len(members) {
		t.Fatalf("info says %d entries, found %d", info.Entries, len(members))
	}

	byName := map[string]rec{}
	var names []string
	for _, m := range members {
		byName[m.name] = m
		names = append(names, m.name)
	}
	// A directory comes before its content.
	want := "bin bin/busybox bin/cat bin/ls bin/sh dev dev/console dev/sda empty etc etc/hosts init run run/fifo"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("members:\n got %s\nwant %s", got, want)
	}

	check := func(name string, mode, uid, gid uint32) rec {
		t.Helper()
		m := byName[name]
		if m.mode != mode || m.uid != uid || m.gid != gid {
			t.Errorf("%s: mode %o uid %d gid %d, want %o %d %d", name, m.mode, m.uid, m.gid, mode, uid, gid)
		}
		return m
	}
	check("bin", 0o40755, 0, 0) // synthesized
	check("etc", 0o40750, 1, 2)
	check("bin/sh", 0o120777, 0, 0)
	if string(byName["bin/sh"].data) != "busybox" {
		t.Errorf("symlink target %q", byName["bin/sh"].data)
	}
	c := check("dev/console", 0o20600, 0, 0)
	if c.rmaj != 5 || c.rmin != 1 {
		t.Errorf("console rdev %d:%d", c.rmaj, c.rmin)
	}
	s := check("dev/sda", 0o60660, 0, 0)
	if s.rmaj != 8 || s.rmin != 0 {
		t.Errorf("sda rdev %d:%d", s.rmaj, s.rmin)
	}
	check("run/fifo", 0o10644, 0, 0)
	check("empty", 0o100644, 0, 0)
	h := check("etc/hosts", 0o100644, 1000, 100)
	if h.mtime != 1700000000 || string(h.data) != "127.0.0.1 localhost\n" {
		t.Errorf("hosts: mtime %d data %q", h.mtime, h.data)
	}
	if got := byName["init"].data; string(got) != strings.Repeat("x", 4097) {
		t.Errorf("init content has %d bytes", len(got))
	}
	if byName["empty"].mtime != 0 {
		t.Errorf("a missing timestamp is %d, want the epoch", byName["empty"].mtime)
	}

	// Hard links: one inode, a link count of three, content on the last.
	busybox, cat, ls := byName["bin/busybox"], byName["bin/cat"], byName["bin/ls"]
	if busybox.ino != ls.ino || ls.ino != cat.ino || busybox.nlink != 3 || cat.nlink != 3 {
		t.Errorf("hard links: ino %d/%d/%d nlink %d/%d", busybox.ino, ls.ino, cat.ino, busybox.nlink, cat.nlink)
	}
	if len(busybox.data) != 0 || len(cat.data) != 0 || string(ls.data) != "ELF-ish\n" {
		t.Errorf("hard link content: %q %q %q", busybox.data, cat.data, ls.data)
	}
	if byName["bin"].nlink != 2 || byName["dev"].nlink != 2 {
		t.Errorf("directory link counts: %d %d", byName["bin"].nlink, byName["dev"].nlink)
	}
	// Inodes are numbered in archive order.
	var next uint32
	seen := map[uint32]bool{}
	for _, m := range members {
		if !seen[m.ino] {
			next++
			if m.ino != next {
				t.Errorf("%s has inode %d, want %d", m.name, m.ino, next)
			}
			seen[m.ino] = true
		}
	}
}

func TestInfoFiles(t *testing.T) {
	b := mustBuilder(t, Options{DigestFiles: true, DigestArchive: true})
	populate(t, b)
	archive, info := build(t, b)

	sum := sha256.Sum256(archive)
	if !bytes.Equal(info.Digest, sum[:]) {
		t.Errorf("archive digest %x, want %x", info.Digest, sum)
	}
	got := map[string]FileLayout{}
	for _, f := range info.Files {
		got[f.Path] = f
		content := archive[f.Offset : f.Offset+f.Size]
		d := sha256.Sum256(content)
		if !bytes.Equal(f.Digest, d[:]) {
			t.Errorf("%s: digest %x does not match the content at %d", f.Path, f.Digest, f.Offset)
		}
	}
	// "bin/ls" carries the content of the hard links; empty files are not listed.
	if _, ok := got["bin/ls"]; !ok {
		t.Errorf("hard linked content not listed at the last member: %v", got)
	}
	if len(got) != 3 || got["empty"].Size != 0 {
		t.Errorf("files: %v", got)
	}
	var last int64
	for _, f := range info.Files {
		if f.Offset < last {
			t.Errorf("%s is not in archive order", f.Path)
		}
		last = f.Offset + f.Size
	}
}

func TestReader(t *testing.T) {
	b := mustBuilder(t, Options{})
	populate(t, b)
	archive, _ := build(t, b)

	want := parse(t, archive)
	var got []*Entry
	rd := OpenReader(bytes.NewReader(archive))
	if err := rd.Walk(func(e *Entry) error {
		c := *e
		got = append(got, &c)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d entries, want %d", len(got), len(want))
	}
	for i, e := range got {
		w := want[i]
		if e.Name != w.name || e.Ino != w.ino || e.Nlink != w.nlink || e.UID != w.uid {
			t.Errorf("%s: %+v differs from %+v", w.name, e, w)
		}
		if e.Mode.IsRegular() {
			content, err := io.ReadAll(rd.Content(e))
			if err != nil {
				t.Fatal(err)
			}
			wantContent := w.data
			if w.name == "bin/busybox" || w.name == "bin/cat" {
				wantContent = []byte("ELF-ish\n") // the content of the last link
			}
			if !bytes.Equal(content, wantContent) {
				t.Errorf("%s: content %q, want %q", w.name, content, wantContent)
			}
		}
	}
	if e := got[4]; e.Name != "bin/sh" || e.Target != "busybox" {
		t.Errorf("symlink entry: %+v", e)
	}
}

func TestDeterministicAndReplace(t *testing.T) {
	mk := func(order []string) []byte {
		b := mustBuilder(t, Options{})
		for _, n := range order {
			h, r := hdrReg(n, "old")
			if err := b.WriteRegular(h, r); err != nil {
				t.Fatal(err)
			}
		}
		h, r := hdrReg("a/x", "new")
		if err := b.WriteRegular(h, r); err != nil {
			t.Fatal(err)
		}
		out, _ := build(t, b)
		return out
	}
	one, two := mk([]string{"a/x", "b"}), mk([]string{"b", "a/x"})
	if !bytes.Equal(one, two) {
		t.Error("the archive depends on the order of entries")
	}
	for _, m := range parse(t, one) {
		if m.name == "a/x" && string(m.data) != "new" {
			t.Errorf("a/x = %q, want the replacement", m.data)
		}
	}
}

func TestDirectoryMetadataIsKept(t *testing.T) {
	b := mustBuilder(t, Options{})
	addFile(t, b, "d/f", []byte("x"), 0o644)
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "d/", Mode: 0o700, Uid: 5}); err != nil {
		t.Fatal(err)
	}
	archive, _ := build(t, b)
	m := parse(t, archive)
	if len(m) != 2 || m[0].name != "d" || m[0].mode != 0o40700 || m[0].uid != 5 || m[1].name != "d/f" {
		t.Errorf("members: %+v", m)
	}
}

func TestFromPath(t *testing.T) {
	p := writeTemp(t, []byte("from disk"))
	b := mustBuilder(t, Options{})
	if err := b.WriteRegularFromPath(&tar.Header{Typeflag: tar.TypeReg, Name: "f", Size: 9, Mode: 0o644}, p); err != nil {
		t.Fatal(err)
	}
	archive, _ := build(t, b)
	if m := parse(t, archive); string(m[0].data) != "from disk" {
		t.Errorf("content %q", m[0].data)
	}
}

func TestDeduplicate(t *testing.T) {
	content := pattern(5000, 1)
	for _, dedup := range []bool{true, false} {
		b := mustBuilder(t, Options{Deduplicate: dedup})
		for _, name := range []string{"a", "b", "c"} {
			hdr := &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(content)), Mode: 0o644}
			if err := b.WriteRegularDeduplicated(hdr, bytes.NewReader(content)); err != nil {
				t.Fatal(err)
			}
		}
		// Different metadata: not merged.
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: "d", Size: int64(len(content)), Mode: 0o600}
		if err := b.WriteRegularDeduplicated(hdr, bytes.NewReader(content)); err != nil {
			t.Fatal(err)
		}
		// Not added through a Deduplicated method: not merged.
		addFile(t, b, "e", content, 0o644)
		archive, info := build(t, b)
		m := parse(t, archive)
		byName := map[string]rec{}
		for _, r := range m {
			byName[r.name] = r
		}
		if dedup {
			if byName["a"].ino != byName["b"].ino || byName["b"].ino != byName["c"].ino || byName["a"].nlink != 3 {
				t.Errorf("a, b and c should be hard links: %+v", m)
			}
			if len(byName["a"].data) != 0 || len(byName["c"].data) != len(content) {
				t.Errorf("content should be at the last link")
			}
			if byName["d"].ino == byName["a"].ino || byName["e"].ino == byName["a"].ino {
				t.Errorf("d and e must stay separate")
			}
			if len(info.Files) != 3 {
				t.Errorf("%d files with content, want 3", len(info.Files))
			}
		} else {
			if byName["a"].ino == byName["b"].ino || len(info.Files) != 5 {
				t.Errorf("without deduplication every file is stored: %d", len(info.Files))
			}
		}
	}
}

func TestImportTarAndAddFS(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "d/", Mode: 0o755})
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "d/f", Size: 3, Mode: 0o644})
	tw.Write([]byte("abc"))
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "d/l", Linkname: "f"})
	tw.Close()

	b := mustBuilder(t, Options{})
	if err := b.ImportTar(&buf); err != nil {
		t.Fatal(err)
	}
	if err := b.AddFS(fstest.MapFS{
		"x/y":   {Data: []byte("from fs"), Mode: 0o755},
		"x/lnk": {Data: []byte("y"), Mode: 0o777 | os.ModeSymlink},
	}, "fs"); err == nil {
		// MapFS has no ReadLink in older Go versions; either outcome is fine
		// as long as the regular file arrives.
	}
	archive, _ := build(t, b)
	got := map[string]string{}
	for _, m := range parse(t, archive) {
		got[m.name] = string(m.data)
	}
	if got["d/f"] != "abc" || got["d/l"] != "f" || got["fs/x/y"] != "from fs" {
		t.Errorf("members: %v", got)
	}
}

func TestErrors(t *testing.T) {
	cases := map[string]func(b *Builder) error{
		"escape": func(b *Builder) error {
			return b.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "../x"})
		},
		"short content": func(b *Builder) error {
			return b.WriteRegular(&tar.Header{Typeflag: tar.TypeReg, Name: "x", Size: 10}, strings.NewReader("abc"))
		},
		"short content in a file": func(b *Builder) error {
			p := writeTemp(t, []byte("abc"))
			if err := b.WriteRegularFromPath(&tar.Header{Typeflag: tar.TypeReg, Name: "x", Size: 10}, p); err != nil {
				return err
			}
			_, err := b.Build(io.Discard)
			return err
		},
		"missing link target": func(b *Builder) error {
			return b.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "x", Linkname: "nope"})
		},
		"link to a directory": func(b *Builder) error {
			if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "d"}); err != nil {
				return err
			}
			return b.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "x", Linkname: "d"})
		},
		"uid out of range": func(b *Builder) error {
			return b.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "x", Uid: -1})
		},
		"symlink target too long": func(b *Builder) error {
			return b.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "x", Linkname: strings.Repeat("a", 4096)})
		},
		"device number out of range": func(b *Builder) error {
			return b.WriteHeader(&tar.Header{Typeflag: tar.TypeChar, Name: "x", Devmajor: 1 << 33})
		},
		"file too large": func(b *Builder) error {
			return b.WriteRegularFromPath(&tar.Header{Typeflag: tar.TypeReg, Name: "x", Size: 1 << 32}, "/nonexistent")
		},
		"regular file needs content": func(b *Builder) error {
			return b.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "x", Size: 3})
		},
		"unsupported type": func(b *Builder) error {
			return b.WriteHeader(&tar.Header{Typeflag: 'Z', Name: "x"})
		},
		"root is a file": func(b *Builder) error {
			return b.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: ".", Linkname: "x"})
		},
		"NUL in a name": func(b *Builder) error {
			return b.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "a\x00b"})
		},
		"name too long": func(b *Builder) error {
			return b.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: strings.Repeat("a/", 3000)})
		},
		"file as parent": func(b *Builder) error {
			if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "f"}); err != nil {
				return err
			}
			return b.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "f/g"})
		},
		"built twice": func(b *Builder) error {
			if _, err := b.Build(io.Discard); err != nil {
				return err
			}
			_, err := b.Build(io.Discard)
			return err
		},
		"add after build": func(b *Builder) error {
			if _, err := b.Build(io.Discard); err != nil {
				return err
			}
			return b.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "x"})
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			b := mustBuilder(t, Options{})
			if err := fn(b); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestOptionsValidate(t *testing.T) {
	if _, err := NewBuilder(Options{SpoolThreshold: -1}); err == nil {
		t.Error("expected an error for a negative spool threshold")
	}
}

// TestSystemTools unpacks the archive with the tools of the machine, if there
// are any that read newc.
func TestSystemTools(t *testing.T) {
	b := mustBuilder(t, Options{})
	for _, n := range []string{"etc/hosts", "init"} {
		h, r := hdrReg(n, "content of "+n)
		if err := b.WriteRegular(h, r); err != nil {
			t.Fatal(err)
		}
	}
	archive, _ := build(t, b)

	for _, tool := range [][]string{{"bsdtar", "-t", "-f", "-"}, {"cpio", "-it"}} {
		path, err := exec.LookPath(tool[0])
		if err != nil {
			continue
		}
		cmd := exec.Command(path, tool[1:]...)
		cmd.Stdin = bytes.NewReader(archive)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("%s: %v\n%s", tool[0], err, out)
			continue
		}
		for _, want := range []string{"etc/hosts", "init"} {
			if !strings.Contains(string(out), want) {
				t.Errorf("%s did not list %s:\n%s", tool[0], want, out)
			}
		}
		t.Logf("verified with %s", tool[0])
	}
}

func TestTimestampsAreClamped(t *testing.T) {
	b := mustBuilder(t, Options{})
	for name, ts := range map[string]int64{"old": -86400, "new": 1 << 33, "ok": 1700000000} {
		if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name, ModTime: time.Unix(ts, 0)}); err != nil {
			t.Fatal(err)
		}
	}
	m, _ := build(t, b)
	got := map[string]int64{}
	err := OpenReader(bytes.NewReader(m)).Walk(func(e *Entry) error {
		got[e.Name] = e.Mtime.Unix()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["old"] != 0 || got["new"] != 1<<32-1 || got["ok"] != 1700000000 {
		t.Errorf("mtimes %v", got)
	}
}

func TestHardLinkToSymlinkIsACopy(t *testing.T) {
	b := mustBuilder(t, Options{})
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "s", Linkname: "target", Mode: 0o777}); err != nil {
		t.Fatal(err)
	}
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "l", Linkname: "s"}); err != nil {
		t.Fatal(err)
	}
	m, _ := build(t, b)
	seen := 0
	err := OpenReader(bytes.NewReader(m)).Walk(func(e *Entry) error {
		seen++
		if e.Nlink != 1 || e.Target != "target" || e.Mode&fs.ModeSymlink == 0 {
			t.Errorf("%s: nlink %d target %q mode %v", e.Name, e.Nlink, e.Target, e.Mode)
		}
		return nil
	})
	if err != nil || seen != 2 {
		t.Fatalf("seen %d entries, err %v", seen, err)
	}
}

func TestReaderBoundsAndDevices(t *testing.T) {
	record := func(ino, devminor uint32, mode uint32, nlink uint32, name string, data string) string {
		fields := []uint32{ino, mode, 0, 0, nlink, 0, uint32(len(data)), 0, devminor, 0, 0, uint32(len(name) + 1), 0}
		s := magic
		for _, f := range fields {
			s += fmt.Sprintf("%08x", f)
		}
		s += name + "\x00"
		s += strings.Repeat("\x00", int(pad4(int64(len(s)))))
		s += data
		s += strings.Repeat("\x00", int(pad4(int64(len(data)))))
		return s
	}
	trailer := record(0, 0, 0, 1, trailerName, "")

	// A symbolic link that claims to be huge is rejected, not allocated.
	huge := record(1, 0, modeSymlink|0o777, 1, "s", "")
	huge = huge[:6+6*8] + "ffffffff" + huge[6+7*8:]
	if err := OpenReader(strings.NewReader(huge + trailer)).Walk(func(*Entry) error { return nil }); err == nil {
		t.Error("expected an error for an oversized symbolic link")
	}

	// The same inode number on two devices is not one file.
	arch := record(5, 1, modeReg|0o644, 2, "a", "AAAA") + record(5, 2, modeReg|0o644, 2, "b", "BBBBBB") + trailer
	sizes := map[string]int64{}
	if err := OpenReader(strings.NewReader(arch)).Walk(func(e *Entry) error {
		sizes[e.Name] = e.Size
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if sizes["a"] != 4 || sizes["b"] != 6 {
		t.Errorf("sizes %v", sizes)
	}
}
