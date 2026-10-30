package layer

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/fat32"
)

func TestFat32Layer(t *testing.T) {
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

	out := filepath.Join(dir, "layer.fat32")
	cstreamPath := filepath.Join(dir, "layer.cstream")
	metadataPath := filepath.Join(dir, "metadata.json")

	// Options for tar layers are accepted and ignored.
	LayerProcess(context.Background(), []string{
		"--layer-format", "fat32",
		"--estargz",
		"--compressor-jobs", "4",
		"--compression-level", "9",
		"--ztoc", filepath.Join(dir, "unused.ztoc"),
		"--add", "/etc/app/config.json=" + cfg,
		"--add", "/opt/app=" + appFile,
		"--add", "/opt/tree=" + tree,
		"--empty-files-from-file", empties,
		"--default-metadata", `{"uid":42,"gid":43}`,
		"--compact-stream", cstreamPath,
		"--compact-stream-inline-threshold", "4096",
		"--metadata", metadataPath,
		out,
	})

	img, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if img[510] != 0x55 || img[511] != 0xAA || string(img[82:90]) != "FAT32   " {
		t.Fatalf("not a FAT32 image")
	}
	if got := binary.LittleEndian.Uint32(img[32:]); int64(got)*512 != int64(len(img)) {
		t.Errorf("boot sector says %d sectors for an image of %d bytes", got, len(img))
	}
	// The files are stored contiguously, so their content shows up whole.
	if n := bytes.Count(img, app); n != 1 {
		t.Errorf("the application is stored %d times", n)
	}
	if !bytes.Contains(img, treeContent) || !bytes.Contains(img, []byte(`{"a":1}`)) {
		t.Error("tree artifact or config file missing from the image")
	}
	if _, err := os.Stat(filepath.Join(dir, "unused.ztoc")); err == nil {
		t.Error("a ztoc was written")
	}
	rd, err := fat32.OpenReader(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*fat32.Entry{}
	if err := rd.Walk(func(e *fat32.Entry) error {
		c := *e
		got[e.Path] = &c
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"etc/app/config.json", "opt/app", "opt/tree/data.bin", "var/empty", "var"} {
		if got[name] == nil {
			t.Errorf("%q missing", name)
		}
	}

	// The compact stream references the large files and resolves to the image.
	cstream, err := os.ReadFile(cstreamPath)
	if err != nil {
		t.Fatal(err)
	}
	csInfo, err := compactstream.Inspect(bytes.NewReader(cstream))
	if err != nil {
		t.Fatal(err)
	}
	if csInfo.Header.ContentFormat != compactstream.ContentFormatFat32 {
		t.Errorf("content format %d", csInfo.Header.ContentFormat)
	}
	if len(csInfo.Refs) != 2 {
		t.Errorf("compact stream references: %+v", csInfo.Refs)
	}
	appSum, treeSum := sha256.Sum256(app), sha256.Sum256(treeContent)
	var reconstructed bytes.Buffer
	blobs := blobStore{string(appSum[:]): app, string(treeSum[:]): treeContent}
	if err := compactstream.Reconstruct(context.Background(), bytes.NewReader(cstream), blobs, &reconstructed); err != nil {
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
	imgSum := sha256.Sum256(img)
	if meta.Size != int64(len(img)) || meta.Digest != fmt.Sprintf("sha256:%x", imgSum) || meta.Digest != meta.DiffID || meta.MediaType != api.Fat32Layer {
		t.Errorf("metadata: %+v (image is %d bytes)", meta, len(img))
	}
}

func TestFat32LayerRejectsWhatFATCannotStore(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(filepath.Join(dir, "layer.fat32"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	_, err = writeFat32Layer(out, fat32Settings{}, addFiles{{PathInImage: "/f", File: file, FileType: api.RegularFile}}, nil, nil, symlinks{{LinkName: "/link", Target: "f"}}, nil, nil, false, nil, nil, 0)
	if err == nil {
		t.Fatal("a symbolic link was accepted")
	}
}

func TestFat32LayerSectorSize(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{0, 512, 4096} {
		out := filepath.Join(dir, fmt.Sprintf("layer-%d.fat32", size))
		LayerProcess(context.Background(), []string{"--layer-format", "fat32", "--fat32-sector-size", fmt.Sprint(size), "--add", "/f=" + file, out})
		img, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		want := size
		if want == 0 {
			want = 512
		}
		if got := int(binary.LittleEndian.Uint16(img[11:])); got != want {
			t.Errorf("--fat32-sector-size %d: the file system has %d byte sectors", size, got)
		}
	}
}

// TestFat32LayerCompactStreamOnly checks that the image can be left out: the
// compact stream and the metadata are the same as when it is written.
func TestFat32LayerCompactStreamOnly(t *testing.T) {
	dir := t.TempDir()
	big := bytes.Repeat([]byte("0123456789abcdef"), 4000)
	file := filepath.Join(dir, "big")
	if err := os.WriteFile(file, big, 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(only bool) (cstream []byte, metadata []byte, entries int) {
		work := t.TempDir()
		args := []string{"--layer-format", "fat32", "--add", "/big=" + file, "--compact-stream", filepath.Join(work, "layer.cstream"), "--metadata", filepath.Join(work, "metadata.json")}
		if only {
			args = append(args, "--compact-stream-only")
		} else {
			args = append(args, filepath.Join(work, "layer.fat32"))
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
}

// TestFat32LayerImportTar checks that a tar is read when the image is written,
// and that identical files are stored again: FAT cannot share content.
func TestFat32LayerImportTar(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	contents := map[string][]byte{"a": bytes.Repeat([]byte("a"), 30000), "b": bytes.Repeat([]byte("b"), 5000), "c": bytes.Repeat([]byte("a"), 30000)}
	for _, name := range []string{"a", "b", "c"} {
		tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(contents[name])), Mode: 0o644})
		tw.Write(contents[name])
	}
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "link-to-b", Linkname: "b"})
	tw.Close()
	tarPath := filepath.Join(dir, "in.tar")
	if err := os.WriteFile(tarPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "layer.fat32")
	LayerProcess(context.Background(), []string{"--layer-format", "fat32", "--import-tar", tarPath, out})
	img, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(img, contents["a"]); n != 2 {
		t.Errorf("%d copies of the repeated file, want 2", n)
	}
	if n := bytes.Count(img, contents["b"]); n != 2 {
		t.Errorf("%d copies of the hard-linked file, want 2 (a hard link becomes a copy)", n)
	}
	rd, err := fat32.OpenReader(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	rd.Walk(func(e *fat32.Entry) error { seen[e.Path] = true; return nil })
	for _, name := range []string{"a", "b", "c", "link-to-b"} {
		if !seen[name] {
			t.Errorf("%s missing", name)
		}
	}
}

// TestFat32LayerTreeArtifacts checks that a tree artifact whose content was
// stored before is stored again instead of becoming a symlink, which FAT could
// not hold.
func TestFat32LayerTreeArtifacts(t *testing.T) {
	dir := t.TempDir()
	content := bytes.Repeat([]byte{9}, 6000)
	var trees []string
	for _, name := range []string{"tree1", "tree2"} {
		p := filepath.Join(dir, name)
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "f"), content, 0o644); err != nil {
			t.Fatal(err)
		}
		trees = append(trees, p)
	}
	out := filepath.Join(dir, "layer.fat32")
	LayerProcess(context.Background(), []string{
		"--layer-format", "fat32", "--layer-tree-artifact-handling", "deduplicate_symlink",
		"--add", "/one=" + trees[0], "--add", "/two=" + trees[1], out,
	})
	img, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(img, content); n != 2 {
		t.Errorf("%d copies of the tree content, want 2", n)
	}
}
