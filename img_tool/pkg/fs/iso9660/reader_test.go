package iso9660

import (
	"archive/tar"
	"bytes"
	"io/fs"
	"strings"
	"testing"
)

func TestOpenReaderRejectsWhatIsNotAnImage(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"short", make([]byte, 100)},
		{"zeros", make([]byte, 64*BlockSize)},
		{"wrong standard identifier", func() []byte {
			b := make([]byte, 64*BlockSize)
			b[systemAreaBlocks*BlockSize] = 1
			copy(b[systemAreaBlocks*BlockSize+1:], "XX001")
			return b
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := OpenReader(&memImage{data: tc.data}); err == nil {
				t.Error("a target that is not an ISO 9660 image was accepted")
			}
		})
	}
}

// TestReaderOnASparseImage is the view a compact stream gives: the metadata is
// there, the file payloads read as zeros. The reader has to describe every
// entry anyway, which is what the mtree of a compact-stream layer needs.
func TestReaderOnASparseImage(t *testing.T) {
	img, info := build(t, Options{RockRidge: true, Joliet: true, UDF: true, DigestFiles: true}, func(b *Builder) error {
		if err := b.WriteRegular(sized(entryOf("dir/payload.bin", tar.TypeReg, 0o640), 40000), bytes.NewReader(pattern(40000, 3))); err != nil {
			return err
		}
		link := entryOf("dir/link", tar.TypeSymlink, 0o777)
		link.Linkname = "payload.bin"
		return b.WriteHeader(link)
	})

	// Zero out every file extent, the way a compact stream without its blobs
	// reconstructs.
	sparse := &memImage{data: append([]byte(nil), img.data...)}
	for _, f := range info.Files {
		clear(sparse.data[f.Offset : f.Offset+f.Size])
	}

	dense, hollow := walk(t, img), walk(t, sparse)
	if len(dense) != len(hollow) {
		t.Fatalf("the sparse image has %v, the full one %v", paths(hollow), paths(dense))
	}
	for path, want := range dense {
		got := hollow[path]
		if got == nil {
			t.Errorf("%q is missing from the sparse image", path)
			continue
		}
		if got.Mode != want.Mode || got.Size != want.Size || got.Offset != want.Offset ||
			got.Uid != want.Uid || got.Gid != want.Gid || !got.Mtime.Equal(want.Mtime) || got.Target != want.Target {
			t.Errorf("%q is %+v in the sparse image, want %+v", path, got, want)
		}
	}
	if e := hollow["dir/link"]; e == nil || e.Mode&fs.ModeSymlink == 0 || e.Target != "payload.bin" {
		t.Errorf("the symbolic link is %+v in the sparse image", e)
	}
}

// TestReaderWalksInDirectoryOrder checks the order Walk reports: the root
// first, then every directory before its content.
func TestReaderWalksInDirectoryOrder(t *testing.T) {
	img, _ := build(t, Options{RockRidge: true}, func(b *Builder) error {
		for _, name := range []string{"b/2", "a/1", "a/sub/deep", "top"} {
			if err := b.WriteRegular(sized(entryOf(name, tar.TypeReg, 0o644), 1), strings.NewReader("x")); err != nil {
				return err
			}
		}
		return nil
	})

	rd := mustOpen(t, img)
	var order []string
	if err := rd.Walk(func(e *Entry) error {
		order = append(order, e.Path)
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(order) == 0 || order[0] != "" {
		t.Fatalf("the walk starts with %v, want the root", order)
	}
	seen := map[string]bool{"": true}
	for _, p := range order[1:] {
		parent := ""
		if i := strings.LastIndexByte(p, '/'); i >= 0 {
			parent = p[:i]
		}
		if !seen[parent] {
			t.Errorf("%q came before its directory %q; the order is %v", p, parent, order)
		}
		seen[p] = true
	}
}
