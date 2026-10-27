package cpio

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type member struct {
	name                string
	ino, mode, uid, gid uint32
	nlink, mtime        uint32
	rmaj, rmin          uint32
	data                []byte
}

// parse is an independent reader of the newc format.
func parse(t *testing.T, b []byte) []member {
	t.Helper()
	var out []member
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
		if (nameStart-off+int(f[11]))%4 != 0 && dataStart%4 != 0 {
			t.Fatal("unaligned")
		}
		dataEnd := dataStart + int(f[6])
		if name == trailerName {
			if rest := b[(dataEnd+3)&^3:]; len(rest) != 0 {
				t.Fatalf("%d bytes after the trailer", len(rest))
			}
			return out
		}
		out = append(out, member{name, f[0], f[1], f[2], f[3], f[4], f[5], f[9], f[10], b[dataStart:dataEnd]})
		off = (dataEnd + 3) &^ 3
	}
}

func hdrReg(name string, content string) (*tar.Header, io.Reader) {
	return &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(content)), Mode: 0o644, Uid: 1000, Gid: 100, ModTime: time.Unix(1700000000, 0)}, strings.NewReader(content)
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
	b, err := NewBuilder(Options{SpoolThreshold: 16}) // spools init to a file
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	populate(t, b)
	archive, info := build(t, b)
	members := parse(t, archive)
	if info.Entries != len(members) {
		t.Fatalf("info says %d entries, found %d", info.Entries, len(members))
	}

	byName := map[string]member{}
	var names []string
	for _, m := range members {
		byName[m.name] = m
		names = append(names, m.name)
	}
	want := "bin bin/busybox bin/cat bin/ls bin/sh dev dev/console dev/sda empty etc etc/hosts init run run/fifo"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("members:\n got %s\nwant %s", got, want)
	}

	check := func(name string, mode, uid, gid uint32) member {
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

	// Hard links: one inode, a link count of three, content on the last.
	busybox, cat, ls := byName["bin/busybox"], byName["bin/cat"], byName["bin/ls"]
	if busybox.ino != ls.ino || ls.ino != cat.ino || busybox.nlink != 3 || cat.nlink != 3 {
		t.Errorf("hard links: ino %d/%d/%d nlink %d/%d", busybox.ino, ls.ino, cat.ino, busybox.nlink, cat.nlink)
	}
	if len(busybox.data) != 0 || len(cat.data) != 0 || string(ls.data) != "ELF-ish\n" {
		t.Errorf("hard link content: %q %q %q", busybox.data, cat.data, ls.data)
	}
	seen := map[uint32]string{}
	for _, m := range members {
		if o, dup := seen[m.ino]; dup && !(strings.HasPrefix(m.name, "bin/") && strings.HasPrefix(o, "bin/")) {
			t.Errorf("%s and %s share inode %d", o, m.name, m.ino)
		}
		seen[m.ino] = m.name
	}
}

func TestDeterministicAndReplace(t *testing.T) {
	mk := func(order []string) []byte {
		b, _ := NewBuilder(Options{})
		defer b.Close()
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

func TestFromPath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("from disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, _ := NewBuilder(Options{})
	defer b.Close()
	if err := b.WriteRegularFromPath(&tar.Header{Typeflag: tar.TypeReg, Name: "f", Size: 9, Mode: 0o644}, p); err != nil {
		t.Fatal(err)
	}
	archive, _ := build(t, b)
	if m := parse(t, archive); string(m[0].data) != "from disk" {
		t.Errorf("content %q", m[0].data)
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
		"missing link target": func(b *Builder) error {
			return b.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "x", Linkname: "nope"})
		},
		"uid out of range": func(b *Builder) error {
			return b.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "x", Uid: -1})
		},
		"unsupported type": func(b *Builder) error {
			return b.WriteHeader(&tar.Header{Typeflag: 'Z', Name: "x"})
		},
		"file as parent": func(b *Builder) error {
			if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "f"}); err != nil {
				return err
			}
			if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "f/g"}); err != nil {
				return err
			}
			_, err := b.Build(io.Discard)
			return err
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			b, _ := NewBuilder(Options{})
			defer b.Close()
			if err := fn(b); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// TestSystemTools unpacks the archive with the tools of the machine, if there
// are any that read newc.
func TestSystemTools(t *testing.T) {
	b, _ := NewBuilder(Options{})
	defer b.Close()
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
		fmt.Fprintf(os.Stderr, "verified with %s\n", tool[0])
	}
}
