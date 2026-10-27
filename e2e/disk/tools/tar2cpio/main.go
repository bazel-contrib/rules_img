// Command tar2cpio converts a tar archive into a gzip-compressed "newc" cpio archive,
// the format of a Linux initramfs. Ownership is reset to root and timestamps to zero,
// so the output depends only on the names, modes and contents of the entries.
package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: tar2cpio INPUT.tar[.gz] OUTPUT.cpio.gz")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "tar2cpio:", err)
		os.Exit(1)
	}
}

func run(inPath, outPath string) error {
	in, err := os.Open(inPath)
	if err != nil {
		return err
	}
	defer in.Close()
	br := bufio.NewReader(in)
	var r io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	}

	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(out, 1<<20)
	gzw, err := gzip.NewWriterLevel(bw, gzip.BestCompression)
	if err != nil {
		return err
	}
	if err := convert(tar.NewReader(r), gzw); err != nil {
		return err
	}
	if err := gzw.Close(); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	return out.Close()
}

const (
	modeDir     = 0o040000
	modeRegular = 0o100000
	modeSymlink = 0o120000
)

type entry struct {
	name   string
	mode   uint32
	target string // symlink target
	data   []byte
}

// convert writes the tar entries as a cpio archive. Entries are emitted sorted by path, so
// that directories come before their contents and the output is independent of tar order.
// A later entry replaces an earlier one with the same path.
func convert(tr *tar.Reader, w io.Writer) error {
	entries := map[string]*entry{}
	contents := map[string][]byte{} // for hard links
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := path.Clean("/" + h.Name)[1:]
		if name == "" || name == "." {
			continue
		}
		perm := uint32(h.Mode) & 0o7777
		switch h.Typeflag {
		case tar.TypeDir:
			entries[name] = &entry{name: name, mode: modeDir | perm}
		case tar.TypeReg:
			data, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			contents[name] = data
			entries[name] = &entry{name: name, mode: modeRegular | perm, data: data}
		case tar.TypeLink:
			src := path.Clean("/" + h.Linkname)[1:]
			data, ok := contents[src]
			if !ok {
				return fmt.Errorf("%s: hard link to unknown file %s", name, h.Linkname)
			}
			contents[name] = data
			entries[name] = &entry{name: name, mode: modeRegular | perm, data: data}
		case tar.TypeSymlink:
			entries[name] = &entry{name: name, mode: modeSymlink | 0o777, target: h.Linkname}
		default:
			// Device nodes, FIFOs and so on are created by devtmpfs and systemd.
		}
	}
	// Parent directories that the tar leaves implicit.
	for name := range entries {
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			if _, ok := entries[dir]; !ok {
				entries[dir] = &entry{name: dir, mode: modeDir | 0o755}
			}
		}
	}
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)

	cw := &cpioWriter{w: w}
	for i, n := range names {
		e := entries[n]
		var body []byte
		switch {
		case e.mode&0o170000 == modeSymlink:
			body = []byte(e.target)
		default:
			body = e.data
		}
		nlink := uint32(1)
		if e.mode&0o170000 == modeDir {
			nlink = 2
		}
		if err := cw.write(uint32(i+1), e.mode, nlink, e.name, body); err != nil {
			return err
		}
	}
	return cw.write(0, 0, 1, "TRAILER!!!", nil)
}

type cpioWriter struct {
	w io.Writer
}

func pad4(n int) int { return (4 - n%4) % 4 }

// write emits one entry in the "newc" (070701) format.
func (c *cpioWriter) write(ino, mode, nlink uint32, name string, body []byte) error {
	nameLen := len(name) + 1
	hdr := fmt.Sprintf("070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
		ino, mode, 0, 0, nlink, 0, len(body), 0, 0, 0, 0, nameLen, 0)
	var sb strings.Builder
	sb.WriteString(hdr)
	sb.WriteString(name)
	sb.WriteByte(0)
	sb.WriteString(strings.Repeat("\x00", pad4(len(hdr)+nameLen)))
	if _, err := io.WriteString(c.w, sb.String()); err != nil {
		return err
	}
	if _, err := c.w.Write(body); err != nil {
		return err
	}
	_, err := io.WriteString(c.w, strings.Repeat("\x00", pad4(len(body))))
	return err
}
