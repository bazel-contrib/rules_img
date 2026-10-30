package fat32

import (
	"archive/tar"
	"bytes"
	"io"
	"io/fs"
	"testing"
	"time"
)

// TestReaderAgreesWithChecker reads back images with the Reader and compares
// every entry with what the independent checker saw.
func TestReaderAgreesWithChecker(t *testing.T) {
	for _, opts := range []Options{{}, {SectorSize: 4096}, {ClusterSize: 8192}} {
		b := mustBuilder(t, opts)
		files := populate(t, b)
		mod := time.Date(2001, 9, 9, 1, 46, 41, 500_000_000, time.UTC)
		acc := time.Date(2020, 12, 31, 0, 0, 0, 0, time.UTC)
		if err := b.WriteRegular(&tar.Header{Typeflag: tar.TypeReg, Name: "ro", Mode: 0o444, Size: 1, ModTime: mod, AccessTime: acc}, bytes.NewReader([]byte("x"))); err != nil {
			t.Fatal(err)
		}
		m, info := build(t, b)
		c := checkImage(t, m, info.Size)

		r, err := OpenReader(m)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		err = r.Walk(func(e *Entry) error {
			seen[e.Path] = true
			if e.Path == "" {
				if !e.Mode.IsDir() {
					t.Errorf("root: mode %v", e.Mode)
				}
				return nil
			}
			f := c.files[e.Path]
			if f == nil {
				t.Errorf("%s: not in the checker's tree", e.Path)
				return nil
			}
			if e.Mode.IsDir() != f.dir || e.Size != int64(f.size) {
				t.Errorf("%s: reader %+v, checker %+v", e.Path, e, f)
			}
			if (f.attr&attrReadOnly != 0) != (!e.Mode.IsDir() && e.Mode.Perm()&0o200 == 0) {
				t.Errorf("%s: mode %v, attr %#x", e.Path, e.Mode, f.attr)
			}
			if e.Mode.IsRegular() {
				got, err := io.ReadAll(r.Content(e))
				if err != nil || !bytes.Equal(got, f.data) {
					t.Errorf("%s: content differs (%v)", e.Path, err)
				}
				if want, ok := files[e.Path]; ok && !bytes.Equal(got, want) {
					t.Errorf("%s: wrong content", e.Path)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for p := range c.files {
			if !seen[p] && p != "" {
				t.Errorf("%s: reader did not visit it", p)
			}
		}

		byPath := map[string]*Entry{}
		r.Walk(func(e *Entry) error { byPath[e.Path] = e; return nil })
		ro := byPath["ro"]
		if ro == nil || ro.Mode != 0o444 || !ro.Mtime.Equal(mod) || !ro.Atime.Equal(acc) {
			t.Errorf("ro: %+v", ro)
		}
		if long := byPath["verylongfilename.txt"]; long == nil || long.Mode != fs.FileMode(0o644) {
			t.Errorf("verylongfilename.txt: %+v", long)
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
