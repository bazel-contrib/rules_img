package mtree

import (
	"archive/tar"
	"bytes"
	"strings"
	"testing"
)

// renderString renders inputs to an mtree string.
func renderString(t *testing.T, opts Options, inputs []Input) string {
	t.Helper()
	var out bytes.Buffer
	if err := WriteMulti(&out, opts, inputs); err != nil {
		t.Fatalf("WriteMulti: %v", err)
	}
	return out.String()
}

func singleFileTar(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeHeader(t, tw, &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(body))}, body)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarInput(b []byte) Input {
	return Input{Kind: TarInput, Reader: bytes.NewReader(b), Digester: HashContent}
}
func mtreeInput(s string) Input { return Input{Kind: MtreeInput, Reader: strings.NewReader(s)} }

// TestMultiTarConcat verifies that multiple tar inputs are concatenated in order
// in the tar layout.
func TestMultiTarConcat(t *testing.T) {
	opts := Options{PathPrefix: "", Keywords: []string{"type", "size"}, Layout: LayoutTar}
	spec := renderString(t, opts, []Input{
		tarInput(singleFileTar(t, "a.txt", "AAAA")),
		tarInput(singleFileTar(t, "b.txt", "BB")),
	})
	order, _ := parseSpec(t, spec)
	if strings.Join(order, ",") != "a.txt,b.txt" {
		t.Errorf("multi-tar order = %v, want [a.txt b.txt]", order)
	}
}

// TestMtreeInputRoundTripAndStrip verifies an mtree input is folded back in with
// its paths re-normalized to the target layout and its keywords filtered to the
// requested option set (nlink here is present in the input but not requested, so
// it is stripped).
func TestMtreeInputRoundTripAndStrip(t *testing.T) {
	// Render buildTar with every field and the "./" prefix.
	full := renderString(t, Options{PathPrefix: "./", Keywords: allKeywords, Layout: LayoutTar}, []Input{tarInput(buildTar(t))})

	// Feed it back as an mtree input, this time with bare paths and no nlink.
	noNlink := []string{"type", "size", "mode", "uid", "uname", "gid", "gname", "sha256", "time", "link", "xattr"}
	out := renderString(t, Options{PathPrefix: "", Keywords: noNlink, Layout: LayoutTar}, []Input{mtreeInput(full)})
	order, entries := parseSpec(t, out)

	// nlink stripped everywhere.
	for name, kw := range entries {
		if _, ok := kw["nlink"]; ok {
			t.Errorf("nlink was not stripped from %q", name)
		}
	}
	// Path re-normalization: dir gets a trailing slash under the empty prefix.
	if !contains(order, "app/") {
		t.Errorf("directory should be re-normalized to app/ (empty prefix): %v", order)
	}
	// Preserved fields survive the round trip (values kept verbatim, re-filtered).
	check := func(name string, want map[string]string) {
		got := entries[name]
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%q %s = %q, want %q", name, k, got[k], v)
			}
		}
	}
	// hardlink stayed a copy (type=file, target's digest, no link).
	check("app/hardlink", map[string]string{
		"type":         "file",
		"sha256digest": "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
	})
	if _, ok := entries["app/hardlink"]["link"]; ok {
		t.Errorf("hardlink copy must not carry a link keyword")
	}
	// symlink kept its link target and type.
	check("app/link", map[string]string{"type": "link", "link": "server"})
	// xattr preserved.
	check("app/xattr", map[string]string{"xattr.user.aaa": "enp6", "xattr.user.foo": "YmFy"})
}

// TestInterleavedTarAndMtreeChangeset verifies that tar and mtree inputs are
// applied together to the changeset tree, including a whiteout carried by an
// mtree input and intermediate directories synthesized for both origins.
func TestInterleavedTarAndMtreeChangeset(t *testing.T) {
	// Tar input adds d/e.txt (synthesizing d) and keep/file.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeHeader(t, tw, &tar.Header{Typeflag: tar.TypeReg, Name: "d/e.txt", Mode: 0o644, Size: 1}, "e")
	writeHeader(t, tw, &tar.Header{Typeflag: tar.TypeReg, Name: "keep/file", Mode: 0o644, Size: 1}, "k")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	// A later mtree input: a whiteout removing d/e.txt, plus a new file under a
	// missing intermediate directory x/y.
	spec := "#mtree v2.0\n" +
		"./d/.wh.e.txt type=file mode=0644\n" +
		"./x/y/z.txt type=file mode=0600\n"

	opts := Options{PathPrefix: "", Keywords: []string{"type", "mode"}, Layout: LayoutOCIChangeset}
	out := renderString(t, opts, []Input{tarInput(buf.Bytes()), mtreeInput(spec)})
	order, entries := parseSpec(t, out)
	t.Logf("interleaved changeset:\n%s", out)

	want := []string{"d/", "keep/", "keep/file", "x/", "x/y/", "x/y/z.txt"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("changeset order = %v, want %v", order, want)
	}
	if _, ok := entries["d/e.txt"]; ok {
		t.Errorf("d/e.txt should have been removed by the mtree input's whiteout")
	}
	// Synthesized dirs (from both origins) carry only type=dir.
	for _, d := range []string{"d/", "x/", "x/y/"} {
		if got := entries[d]; got["type"] != "dir" || len(got) != 1 {
			t.Errorf("synthesized dir %q = %v, want only type=dir", d, got)
		}
	}
	// The mtree-input file made it into the tree with its (filtered) metadata.
	if got := entries["x/y/z.txt"]; got["type"] != "file" || got["mode"] != "0600" {
		t.Errorf("x/y/z.txt = %v, want type=file mode=0600", got)
	}
	for name := range entries {
		if strings.Contains(name, ".wh.") {
			t.Errorf("whiteout marker %q must not appear in changeset output", name)
		}
	}
}
