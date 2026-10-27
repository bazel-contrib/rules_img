package ext4

import (
	"archive/tar"
	"bytes"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"
)

// TestReaderAgreesWithChecker reads back images with the Reader and compares
// every entry with what the independent checker saw.
func TestReaderAgreesWithChecker(t *testing.T) {
	for _, bs := range []int{1024, 4096} {
		b := mustBuilder(t, Options{BlockSize: bs})
		files := populate(t, b)
		b.WriteRegular(&tar.Header{Typeflag: tar.TypeReg, Name: "cap", Mode: 0o755, Size: 1, Uid: 70000, ModTime: time.Unix(1<<32+5, 123456789),
			PAXRecords: map[string]string{"SCHILY.xattr.security.capability": "\x01\x00\x00\x02"}}, strings.NewReader("x"))
		b.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "large", Mode: 0o755, PAXRecords: map[string]string{
			"SCHILY.xattr.user.big": strings.Repeat("v", 300), "SCHILY.xattr.user.other": "o"}})
		acl := []byte{2, 0, 0, 0, 1, 0, 6, 0, 0xff, 0xff, 0xff, 0xff, 2, 0, 4, 0, 5, 0, 0, 0, 0x20, 0, 4, 0, 0xff, 0xff, 0xff, 0xff}
		b.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "acl", Mode: 0o644, PAXRecords: map[string]string{"SCHILY.xattr.system.posix_acl_access": string(acl)}})
		m, info := build(t, b)
		c := checkImage(t, m, info.Size)

		r, err := OpenReader(m)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		err = r.Walk(func(p string, in *Inode) error {
			seen[p] = true
			if p == "." {
				return nil
			}
			n := c.nodes[p]
			if n == nil {
				t.Errorf("%s: not in the checker's tree", p)
				return nil
			}
			if in.Number != n.ino || uint16(in.Mode&0o777) != n.mode&0o777 || in.UID != n.uid || in.GID != n.gid || in.Size != int64(n.size) ||
				in.Links != uint32(n.links) || in.Mtime.Unix() != n.mtime || uint32(in.Mtime.Nanosecond()) != n.nsec || in.Target != n.target {
				t.Errorf("%s: reader %+v, checker %+v", p, in, n)
			}
			if got, want := len(in.Xattrs), len(n.xattrs); got != want {
				t.Errorf("%s: %d xattrs, want %d", p, got, want)
			}
			for k, v := range in.Xattrs {
				want := n.xattrs[k]
				if strings.HasPrefix(k, "system.posix_acl_") {
					continue // the checker keeps the on-disk form
				}
				if v != want {
					t.Errorf("%s: xattr %s = %q, want %q", p, k, v, want)
				}
			}
			if in.Mode.IsRegular() {
				got, err := io.ReadAll(r.Content(in))
				if err != nil || !bytes.Equal(got, n.data) {
					t.Errorf("%s: content differs (%v)", p, err)
				}
				if want, ok := files[p]; ok && !bytes.Equal(got, want) {
					t.Errorf("%s: wrong content", p)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for p := range c.nodes {
			if !seen[p] && p != "" && p != "." {
				t.Errorf("%s: reader did not visit it", p)
			}
		}

		// Specific decodings.
		byPath := map[string]*Inode{}
		r.Walk(func(p string, in *Inode) error { byPath[p] = in; return nil })
		if d := byPath["dev/big"]; d == nil || d.Mode&fs.ModeCharDevice != 0 || d.Major != 259 || d.Minor != 70000 {
			t.Errorf("dev/big: %+v", d)
		}
		if d := byPath["dev/null"]; d == nil || d.Mode&fs.ModeCharDevice == 0 || d.Major != 1 || d.Minor != 3 {
			t.Errorf("dev/null: %+v", d)
		}
		if f := byPath["cap"]; f == nil || !f.Mtime.Equal(time.Unix(1<<32+5, 123456789)) {
			t.Errorf("cap: %+v", f)
		}
		if got := byPath["acl"].Xattrs["system.posix_acl_access"]; got != string(acl) {
			t.Errorf("acl round trip: %v, want %v", []byte(got), acl)
		}
		if byPath["tmp"].Mode&fs.ModeSticky == 0 {
			t.Error("tmp lost the sticky bit")
		}
		if byPath["bin/hard"].Number != byPath["bin/odd"].Number || byPath["bin/odd"].Links != 2 {
			t.Error("hard links not seen as one inode")
		}
	}
}

func TestReaderRejectsGarbage(t *testing.T) {
	if _, err := OpenReader(bytes.NewReader(make([]byte, 4096))); err == nil {
		t.Error("expected an error for zeros")
	}
	if _, err := OpenReader(bytes.NewReader(nil)); err == nil {
		t.Error("expected an error for an empty image")
	}
}
