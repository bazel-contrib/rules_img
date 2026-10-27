package layer

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/ext4"
)

func TestExt4Layer(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, content []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	app := bytes.Repeat([]byte("application "), 50000)
	cfg := write("config.json", []byte(`{"a":1}`))
	appFile := write("app", app)
	tree := filepath.Join(dir, "tree")
	if err := os.Mkdir(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	treeContent := bytes.Repeat([]byte{7}, 9000)
	if err := os.WriteFile(filepath.Join(tree, "data.bin"), treeContent, 0o644); err != nil {
		t.Fatal(err)
	}
	empties := write("empties.txt", []byte("var/empty\n"))

	out := filepath.Join(dir, "layer.ext4")
	cstreamPath := filepath.Join(dir, "layer.cstream")
	metadataPath := filepath.Join(dir, "metadata.json")

	// Options for tar layers are accepted and ignored.
	LayerProcess(context.Background(), []string{
		"--layer-format", "ext4",
		"--estargz",
		"--compressor-jobs", "4",
		"--compression-level", "9",
		"--ztoc", filepath.Join(dir, "unused.ztoc"),
		"--add", "/etc/app/config.json=" + cfg,
		"--add", "/opt/app=" + appFile,
		"--add", "/opt/app-copy=" + appFile,
		"--symlink", "/usr/bin/app=/opt/app",
		"--empty-files-from-file", empties,
		"--default-metadata", `{"uid":42,"gid":43}`,
		"--compact-stream", cstreamPath,
		"--compact-stream-inline-threshold", "4096",
		"--metadata", metadataPath,
		out,
	})
	// A tree artifact goes through the same sink.
	addTree := addFiles{{PathInImage: "/opt/tree", File: tree, FileType: api.Directory}}
	treeOut, err := os.Create(filepath.Join(dir, "tree.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	defer treeOut.Close()
	info, err := writeExt4Layer(treeOut, ext4Settings{deduplicate: true}, addTree, nil, nil, nil, nil, nil, false, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Files) != 1 || info.Files[0].Path != "opt/tree/data.bin" {
		t.Fatalf("tree files: %+v", info.Files)
	}
	got := make([]byte, info.Files[0].Size)
	if _, err := treeOut.ReadAt(got, info.Files[0].Offset); err != nil || !bytes.Equal(got, treeContent) {
		t.Errorf("tree content wrong (%v)", err)
	}

	img, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if magic := binary.LittleEndian.Uint16(img[1024+0x38:]); magic != 0xEF53 {
		t.Fatalf("not an ext4 image: magic %#x", magic)
	}
	// Two files with the same content and metadata are stored once.
	if n := bytes.Count(img, app); n != 1 {
		t.Errorf("the application is stored %d times in the image, want once", n)
	}
	if !bytes.Contains(img, []byte(`{"a":1}`)) {
		t.Error("config file missing from the image")
	}
	for _, name := range []string{"unused.ztoc"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s was written", name)
		}
	}

	// The compact stream stands in for the image: the large file is a reference,
	// the small one stays inline, and resolving the references gives the image.
	cstream, err := os.ReadFile(cstreamPath)
	if err != nil {
		t.Fatal(err)
	}
	csInfo, err := compactstream.Inspect(bytes.NewReader(cstream))
	if err != nil {
		t.Fatal(err)
	}
	if len(csInfo.Refs) != 1 || csInfo.Refs[0].Size != uint64(len(app)) {
		t.Errorf("compact stream references: %+v", csInfo.Refs)
	}
	sum := sha256.Sum256(app)
	var reconstructed bytes.Buffer
	if err := compactstream.Reconstruct(context.Background(), bytes.NewReader(cstream), blobStore{string(sum[:]): app}, &reconstructed); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reconstructed.Bytes(), img) {
		t.Error("reconstructed compact stream differs from the image")
	}

	var meta api.Descriptor
	raw, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Size != int64(len(img)) || meta.Digest == "" || meta.Digest != meta.DiffID {
		t.Errorf("metadata: %+v (image is %d bytes)", meta, len(img))
	}
}

type blobStore map[string][]byte

func (s blobStore) ReaderForBlob(_ context.Context, digest []byte, size int64) (io.ReadCloser, error) {
	blob, ok := s[string(digest)]
	if !ok || int64(len(blob)) != size {
		return nil, fmt.Errorf("blob %x (%d bytes) not available", digest, size)
	}
	return io.NopCloser(bytes.NewReader(blob)), nil
}

func TestExt4LayerWithoutCompactStream(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "layer.ext4")
	LayerProcess(context.Background(), []string{"--layer-format", "ext4", "--add", "/f=" + file, out})
	img, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(img) == 0 || !bytes.Contains(img, []byte("content")) {
		t.Error("no image written")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("unexpected files: %v", entries)
	}
}

// TestExt4LayerCompactStreamOnly checks that the image can be left out: the
// compact stream and the metadata are the same as when it is written.
func TestExt4LayerCompactStreamOnly(t *testing.T) {
	dir := t.TempDir()
	big := bytes.Repeat([]byte("0123456789abcdef"), 4000)
	file := filepath.Join(dir, "big")
	if err := os.WriteFile(file, big, 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(only bool) (cstream []byte, metadata []byte, entries int) {
		work := t.TempDir()
		args := []string{"--layer-format", "ext4", "--add", "/big=" + file, "--compact-stream", filepath.Join(work, "layer.cstream"), "--metadata", filepath.Join(work, "metadata.json")}
		if only {
			args = append(args, "--compact-stream-only")
		} else {
			args = append(args, filepath.Join(work, "layer.ext4"))
		}
		LayerProcess(context.Background(), args)
		cs, err := os.ReadFile(filepath.Join(work, "layer.cstream"))
		if err != nil {
			t.Fatal(err)
		}
		md, err := os.ReadFile(filepath.Join(work, "metadata.json"))
		if err != nil {
			t.Fatal(err)
		}
		list, _ := os.ReadDir(work)
		return cs, md, len(list)
	}
	withImage, mdWith, nWith := run(false)
	only, mdOnly, nOnly := run(true)
	if nWith != 3 || nOnly != 2 {
		t.Errorf("files written: %d with the image, %d without; want 3 and 2 (no leftover temporary image)", nWith, nOnly)
	}
	if !bytes.Equal(withImage, only) {
		t.Error("the compact stream depends on whether the image is written")
	}
	if !bytes.Equal(mdWith, mdOnly) {
		t.Errorf("metadata differs:\n%s\n%s", mdWith, mdOnly)
	}
	info, err := compactstream.Inspect(bytes.NewReader(only))
	if err != nil || info.Header.ContentFormat != compactstream.ContentFormatExt4 {
		t.Errorf("content format: %+v (%v)", info, err)
	}
}

// TestExt4LayerImportTar checks that a tar is read when the image is written,
// not copied, and that the result does not depend on how it is compressed.
func TestExt4LayerImportTar(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	contents := map[string][]byte{"a": bytes.Repeat([]byte("a"), 30000), "b": bytes.Repeat([]byte("b"), 5000), "c": bytes.Repeat([]byte("a"), 30000)}
	for _, name := range []string{"a", "b", "c"} {
		tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(contents[name])), Mode: 0o644})
		tw.Write(contents[name])
	}
	tw.Close()
	tarPath := filepath.Join(dir, "in.tar")
	if err := os.WriteFile(tarPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	digests := map[string]bool{}
	for _, dedup := range []string{"true", "false"} {
		out := filepath.Join(dir, "dedup-"+dedup+".ext4")
		LayerProcess(context.Background(), []string{"--layer-format", "ext4", "--ext4-deduplicate=" + dedup, "--import-tar", tarPath, out})
		img, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		for name, content := range contents {
			if !bytes.Contains(img, content) {
				t.Errorf("dedup=%s: %s missing from the image", dedup, name)
			}
		}
		// a and c are the same file: stored once with deduplication.
		want := 1
		if dedup == "false" {
			want = 2
		}
		if n := bytes.Count(img, contents["a"]); n != want {
			t.Errorf("dedup=%s: %d copies of the repeated file, want %d", dedup, n, want)
		}
		sum := sha256.Sum256(img)
		digests[fmt.Sprintf("%x", sum)] = true
	}
	if len(digests) != 2 {
		t.Error("turning deduplication off does not change the image")
	}
}

// TestExt4LayerTreeArtifactDeduplication checks that two tree artifacts with
// the same content are stored once, as a symlink to the first, like in a tar
// layer -- and only when it is asked for.
func TestExt4LayerTreeArtifactDeduplication(t *testing.T) {
	dir := t.TempDir()
	content := bytes.Repeat([]byte("tree payload "), 1000)
	tree := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Join(p, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "sub", "data.bin"), content, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	first, second := tree("one"), tree("two")

	for _, handling := range []string{"full", "deduplicate_symlink"} {
		out := filepath.Join(dir, handling+".ext4")
		LayerProcess(context.Background(), []string{
			"--layer-format", "ext4", "--layer-tree-artifact-handling", handling,
			"--add", "/a=" + first, "--add", "/b=" + second, out,
		})
		f, err := os.Open(out)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		rd, err := ext4.OpenReader(f)
		if err != nil {
			t.Fatal(err)
		}
		targets := map[string]string{}
		if err := rd.Walk(func(p string, in *ext4.Inode) error {
			targets[p] = in.Target
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, ok := targets["a/sub/data.bin"]; !ok {
			t.Errorf("%s: the first tree is missing", handling)
		}
		if handling == "full" {
			if _, ok := targets["b/sub/data.bin"]; !ok {
				t.Errorf("%s: the second tree is missing", handling)
			}
			continue
		}
		if _, ok := targets["b/sub/data.bin"]; ok {
			t.Errorf("%s: the second tree was stored again", handling)
		}
		if got := targets["b"]; got != "a" {
			t.Errorf("%s: b is a symlink to %q, want %q", handling, got, "a")
		}
	}
}

// TestExt4LayerOptions checks that the geometry of the image can be configured.
func TestExt4LayerOptions(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "layer.ext4")
	LayerProcess(context.Background(), []string{
		"--layer-format", "ext4",
		"--ext4-block-size", "1024",
		"--ext4-min-size", "8MiB",
		"--ext4-bytes-per-inode", "4KiB",
		"--ext4-extra-inodes", "32",
		"--ext4-label", "rules_img",
		"--ext4-uuid", "01234567-89ab-cdef-0123-456789abcdef",
		"--ext4-omit-lost-and-found",
		"--add", "/f=" + file, out,
	})
	img, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(img) != 8<<20 {
		t.Errorf("image is %d bytes, want %d", len(img), 8<<20)
	}
	sb := img[1024:2048]
	if got := 1024 << binary.LittleEndian.Uint32(sb[0x18:]); got != 1024 {
		t.Errorf("block size %d, want 1024", got)
	}
	if got := string(bytes.TrimRight(sb[0x78:0x88], "\x00")); got != "rules_img" {
		t.Errorf("label %q", got)
	}
	wantUUID := []byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}
	if got := sb[0x68:0x78]; !bytes.Equal(got, wantUUID) {
		t.Errorf("uuid %x, want %x", got, wantUUID)
	}
	// 8 MiB at 4096 bytes per inode, plus the 32 extra.
	if got := binary.LittleEndian.Uint32(sb[0x00:]); got < 8<<20/4096 {
		t.Errorf("%d inodes, want at least %d", got, 8<<20/4096)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rd, err := ext4.OpenReader(f)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	rd.Walk(func(p string, _ *ext4.Inode) error { seen[p] = true; return nil })
	if seen["lost+found"] {
		t.Error("lost+found was written despite --ext4-omit-lost-and-found")
	}
	if !seen["f"] {
		t.Error("the file is missing")
	}
}

func TestParseUUID(t *testing.T) {
	want := [16]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10}
	for _, raw := range []string{
		"01234567-89ab-cdef-fedc-ba9876543210",
		"0123456789abcdeffedcba9876543210",
	} {
		got, err := parseUUID(raw)
		if err != nil || got != want {
			t.Errorf("parseUUID(%q) = %x, %v", raw, got, err)
		}
	}
	for _, raw := range []string{
		"",
		"0123",
		"00000000-0000-0000-0000-000000000000",
		"0123456789abcdeffedcba987654321z",
	} {
		if _, err := parseUUID(raw); err == nil {
			t.Errorf("parseUUID(%q) was accepted", raw)
		}
	}
}
