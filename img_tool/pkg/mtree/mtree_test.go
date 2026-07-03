package mtree

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// allKeywords includes xattr (which is not in the default set) so tests can
// assert every field.
var allKeywords = []string{"type", "size", "mode", "uid", "uname", "gid", "gname", "sha256", "time", "link", "nlink", "xattr"}

func writeHeader(t *testing.T, tw *tar.Writer, hdr *tar.Header, body string) {
	t.Helper()
	hdr.Format = tar.FormatPAX
	if hdr.ModTime.IsZero() {
		hdr.ModTime = time.Unix(1000000000, 0).UTC()
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("WriteHeader %q: %v", hdr.Name, err)
	}
	if body != "" {
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("Write %q: %v", hdr.Name, err)
		}
	}
}

// buildTar writes a tar exercising every entry type the layer tool can emit
// (including a hardlink) and returns the raw (uncompressed) bytes.
func buildTar(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeHeader(t, tw, &tar.Header{Typeflag: tar.TypeDir, Name: "app/", Mode: 0o755}, "")
	writeHeader(t, tw, &tar.Header{Typeflag: tar.TypeReg, Name: "app/server", Mode: 0o644, Uid: 1000, Gid: 1000, Uname: "appuser", Gname: "appgroup", Size: 5}, "hello")
	writeHeader(t, tw, &tar.Header{Typeflag: tar.TypeReg, Name: "app/setuid", Mode: 0o4755, Size: 3}, "abc")
	writeHeader(t, tw, &tar.Header{Typeflag: tar.TypeSymlink, Name: "app/link", Linkname: "server"}, "")
	writeHeader(t, tw, &tar.Header{Typeflag: tar.TypeLink, Name: "app/hardlink", Linkname: "app/server"}, "")
	writeHeader(t, tw, &tar.Header{
		Typeflag:   tar.TypeReg,
		Name:       "app/xattr",
		Mode:       0o600,
		Size:       2,
		PAXRecords: map[string]string{"SCHILY.xattr.user.foo": "bar", "SCHILY.xattr.user.aaa": "zzz"},
	}, "hi")
	writeHeader(t, tw, &tar.Header{Typeflag: tar.TypeReg, Name: "passwd", Mode: 0o644, Size: 0}, "")
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	return buf.Bytes()
}

// parseSpec turns the rendered mtree into an ordered list of (name) plus a
// name->{keyword:value} map. The "#mtree" signature and blank lines are ignored.
func parseSpec(t *testing.T, spec string) ([]string, map[string]map[string]string) {
	t.Helper()
	var order []string
	out := map[string]map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(spec, "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		kw := map[string]string{}
		for _, f := range fields[1:] {
			k, v, _ := strings.Cut(f, "=")
			kw[k] = v
		}
		order = append(order, fields[0])
		out[fields[0]] = kw
	}
	return order, out
}

func render(t *testing.T, tarBytes []byte, opts Options) string {
	t.Helper()
	var out bytes.Buffer
	if err := Write(bytes.NewReader(tarBytes), &out, opts, HashContent); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return out.String()
}

func TestTarLayout(t *testing.T) {
	opts := Options{PathPrefix: "", Keywords: allKeywords, Layout: LayoutTar}
	spec := render(t, buildTar(t), opts)
	t.Logf("rendered mtree:\n%s", spec)

	if !strings.HasPrefix(spec, "#mtree v2.0\n") {
		t.Errorf("expected #mtree signature line")
	}
	order, entries := parseSpec(t, spec)

	// tar layout keeps exact tar order (empty prefix => dir keeps a trailing slash).
	wantOrder := []string{"app/", "app/server", "app/setuid", "app/link", "app/hardlink", "app/xattr", "passwd"}
	if strings.Join(order, ",") != strings.Join(wantOrder, ",") {
		t.Errorf("entry order = %v, want %v", order, wantOrder)
	}

	check := func(name string, want map[string]string) {
		got, ok := entries[name]
		if !ok {
			t.Errorf("missing entry %q", name)
			return
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("entry %q keyword %q = %q, want %q (line: %v)", name, k, got[k], v, got)
			}
		}
	}
	absent := func(name, keyword string) {
		if _, ok := entries[name][keyword]; ok {
			t.Errorf("entry %q should not carry %q", name, keyword)
		}
	}

	// directory: no size, no sha256; nlink=1 (subdir counts not computed).
	check("app/", map[string]string{"type": "dir", "mode": "0755", "nlink": "1"})
	absent("app/", "size")
	absent("app/", "sha256digest")
	// regular file with 1 hardlink => nlink=2; digest of "hello".
	check("app/server", map[string]string{
		"type": "file", "size": "5", "mode": "0644", "uid": "1000", "gid": "1000",
		"uname": "appuser", "gname": "appgroup", "time": "1000000000", "nlink": "2",
		"sha256digest": "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
	})
	// setuid folds into octal mode; digest of "abc"; nlink=1.
	check("app/setuid", map[string]string{
		"type": "file", "mode": "04755", "nlink": "1",
		"sha256digest": "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
	})
	// symlink: type=link, size = len(target), link keyword set, no sha256.
	check("app/link", map[string]string{"type": "link", "size": "6", "link": "server", "nlink": "1"})
	absent("app/link", "sha256digest")
	// hardlink: a COPY of app/server (type=file, size 5, same digest, nlink=2); NO link keyword.
	check("app/hardlink", map[string]string{
		"type": "file", "size": "5", "nlink": "2",
		"sha256digest": "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
	})
	absent("app/hardlink", "link")
	// xattrs: sorted, base64-encoded, SCHILY.xattr. prefix stripped; digest of "hi".
	check("app/xattr", map[string]string{
		"type":           "file",
		"xattr.user.aaa": "enp6", // base64("zzz")
		"xattr.user.foo": "YmFy", // base64("bar")
		"sha256digest":   "8f434346648f6b96df89dda901c5176b10a6d83961dd3c1ac88b59b2dc327aa4",
	})
	// empty file: no sha256.
	check("passwd", map[string]string{"type": "file", "size": "0", "nlink": "1"})
	absent("passwd", "sha256digest")

	// xattr keywords must be sorted (user.aaa before user.foo).
	for _, line := range strings.Split(spec, "\n") {
		if strings.HasPrefix(line, "app/xattr ") {
			if a, b := strings.Index(line, "xattr.user.aaa"), strings.Index(line, "xattr.user.foo"); a < 0 || b < 0 || a > b {
				t.Errorf("xattr keywords not in sorted order: %q", line)
			}
		}
	}
}

func TestPathPrefix(t *testing.T) {
	tarBytes := buildTar(t)

	// "./" prefix: full-path entries, directories without a trailing slash.
	dotSlash := render(t, tarBytes, Options{PathPrefix: "./", Keywords: []string{"type"}, Layout: LayoutTar})
	order, _ := parseSpec(t, dotSlash)
	if !contains(order, "./app") || !contains(order, "./app/server") {
		t.Errorf(`with "./" prefix expected ./app and ./app/server, got %v`, order)
	}
	if contains(order, "./app/") {
		t.Errorf(`with "./" prefix, directories must not carry a trailing slash: %v`, order)
	}

	// "" prefix: bare paths, directories WITH a trailing slash (so they still
	// contain a slash and parse as full-path entries).
	bare := render(t, tarBytes, Options{PathPrefix: "", Keywords: []string{"type"}, Layout: LayoutTar})
	order, _ = parseSpec(t, bare)
	if !contains(order, "app/") {
		t.Errorf(`with "" prefix, directory "app" must be rendered as "app/": %v`, order)
	}
	if !contains(order, "app/server") {
		t.Errorf(`with "" prefix expected bare file path app/server: %v`, order)
	}
}

func TestOptionSelectionAndOrder(t *testing.T) {
	// Only the requested keywords, in the requested order.
	opts := Options{PathPrefix: "", Keywords: []string{"mode", "type", "uid"}, Layout: LayoutTar}
	spec := render(t, buildTar(t), opts)
	for _, line := range strings.Split(spec, "\n") {
		if strings.HasPrefix(line, "app/server ") {
			want := "app/server mode=0644 type=file uid=1000"
			if line != want {
				t.Errorf("keyword selection/order:\n got %q\nwant %q", line, want)
			}
		}
	}
}

func TestOCIChangesetLayout(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// Missing intermediate dirs a, a/b are synthesized.
	writeHeader(t, tw, &tar.Header{Typeflag: tar.TypeReg, Name: "a/b/c.txt", Mode: 0o644, Size: 1}, "c")
	// d/e.txt then a whiteout removing it; d remains as a (synthesized) dir.
	writeHeader(t, tw, &tar.Header{Typeflag: tar.TypeReg, Name: "d/e.txt", Mode: 0o644, Size: 1}, "e")
	writeHeader(t, tw, &tar.Header{Typeflag: tar.TypeReg, Name: "d/.wh.e.txt", Mode: 0o644, Size: 0}, "")
	// f/g.txt with an opaque whiteout in f: the marker is consumed, g.txt stays.
	writeHeader(t, tw, &tar.Header{Typeflag: tar.TypeReg, Name: "f/g.txt", Mode: 0o644, Size: 1}, "g")
	writeHeader(t, tw, &tar.Header{Typeflag: tar.TypeReg, Name: "f/.wh..wh..opq", Mode: 0o644, Size: 0}, "")
	// An explicit directory carries full metadata (unlike synthesized dirs).
	writeHeader(t, tw, &tar.Header{Typeflag: tar.TypeDir, Name: "h/", Mode: 0o700}, "")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	opts := Options{PathPrefix: "", Keywords: []string{"type", "mode", "sha256"}, Layout: LayoutOCIChangeset}
	spec := render(t, buf.Bytes(), opts)
	t.Logf("changeset mtree:\n%s", spec)
	order, entries := parseSpec(t, spec)

	// Stable, path-sorted order; whiteout markers consumed; d/e.txt removed.
	want := []string{"a/", "a/b/", "a/b/c.txt", "d/", "f/", "f/g.txt", "h/"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("changeset order = %v, want %v", order, want)
	}
	// Synthesized dirs: only type=dir.
	if got := entries["a/"]; got["type"] != "dir" || len(got) != 1 {
		t.Errorf("synthesized dir a/ = %v, want only type=dir", got)
	}
	// Explicit dir keeps its metadata.
	if got := entries["h/"]; got["type"] != "dir" || got["mode"] != "0700" {
		t.Errorf("explicit dir h/ = %v, want type=dir mode=0700", got)
	}
	// File digest present.
	if got := entries["a/b/c.txt"]["sha256digest"]; got == "" {
		t.Errorf("a/b/c.txt missing sha256digest")
	}
	if _, ok := entries["d/e.txt"]; ok {
		t.Errorf("d/e.txt should have been removed by its whiteout")
	}
	for name := range entries {
		if strings.Contains(name, ".wh.") {
			t.Errorf("whiteout marker %q must not appear in output", name)
		}
	}
}

func TestWriteWithDigester(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct{ name, body string }{{"a.txt", "AAAA"}, {"b.txt", "BBBBBB"}} {
		writeHeader(t, tw, &tar.Header{Typeflag: tar.TypeReg, Name: f.name, Mode: 0o644, Size: int64(len(f.body))}, f.body)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	// a.txt: precomputed digest returned WITHOUT reading (compact-stream ref
	// case). b.txt: hashed from the content that is read (inline case).
	const fixed = "1111111111111111111111111111111111111111111111111111111111111111"
	digester := func(hdr *tar.Header, content io.Reader) ([]byte, error) {
		if hdr.Name == "a.txt" {
			d, _ := hex.DecodeString(fixed)
			return d, nil
		}
		return HashContent(hdr, content)
	}

	var out bytes.Buffer
	if err := Write(bytes.NewReader(buf.Bytes()), &out, Options{PathPrefix: "", Keywords: []string{"type", "sha256"}, Layout: LayoutTar}, digester); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_, entries := parseSpec(t, out.String())
	if got := entries["a.txt"]["sha256digest"]; got != fixed {
		t.Errorf("a.txt sha256digest = %q, want %q (precomputed, not read)", got, fixed)
	}
	wantB := sha256.Sum256([]byte("BBBBBB"))
	if got := entries["b.txt"]["sha256digest"]; got != hex.EncodeToString(wantB[:]) {
		t.Errorf("b.txt sha256digest = %q, want %q (hashed from content)", got, hex.EncodeToString(wantB[:]))
	}
}

func TestDeterministic(t *testing.T) {
	tarBytes := buildTar(t)
	opts := Options{PathPrefix: "./", Keywords: allKeywords, Layout: LayoutTar}
	first := render(t, tarBytes, opts)
	for i := 0; i < 5; i++ {
		if got := render(t, tarBytes, opts); got != first {
			t.Fatalf("non-deterministic output on run %d", i)
		}
	}
}

func TestCompressionIndependent(t *testing.T) {
	tarBytes := buildTar(t)
	opts := Options{PathPrefix: "./", Keywords: allKeywords, Layout: LayoutTar}
	want := render(t, tarBytes, opts)

	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	if _, err := gw.Write(tarBytes); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	var zb bytes.Buffer
	zw, err := zstd.NewWriter(&zb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(tarBytes); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	for name, compressed := range map[string][]byte{"gzip": gz.Bytes(), "zstd": zb.Bytes(), "raw": tarBytes} {
		r, err := Decompress(bytes.NewReader(compressed))
		if err != nil {
			t.Fatalf("%s: Decompress: %v", name, err)
		}
		var out bytes.Buffer
		if err := Write(r, &out, opts, HashContent); err != nil {
			t.Fatalf("%s: Write: %v", name, err)
		}
		if out.String() != want {
			t.Errorf("%s mtree differs from raw", name)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
