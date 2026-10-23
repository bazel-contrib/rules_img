package layer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
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
		"--compact-stream-only",
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

	// The compact stream references the large files and resolves to the image.
	cstream, err := os.ReadFile(cstreamPath)
	if err != nil {
		t.Fatal(err)
	}
	csInfo, err := compactstream.Inspect(bytes.NewReader(cstream))
	if err != nil {
		t.Fatal(err)
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
	if meta.Size != int64(len(img)) || meta.Digest == "" || meta.Digest != meta.DiffID || meta.MediaType != api.Fat32Layer {
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
	_, err = writeFat32Layer(out, addFiles{{PathInImage: "/f", File: file, FileType: api.RegularFile}}, nil, nil, symlinks{{LinkName: "/link", Target: "f"}}, nil, nil, false, nil, nil, 0)
	if err == nil {
		t.Fatal("a symbolic link was accepted")
	}
}
