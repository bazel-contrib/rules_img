package layer

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
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
	metadataPath := filepath.Join(dir, "metadata.json")

	// Options for tar layers are accepted and ignored.
	LayerProcess(context.Background(), []string{
		"--format", "ext4",
		"--estargz",
		"--compressor-jobs", "4",
		"--compression-level", "9",
		"--compact-stream", filepath.Join(dir, "unused.cstream"),
		"--compact-stream-only",
		"--ztoc", filepath.Join(dir, "unused.ztoc"),
		"--add", "/etc/app/config.json=" + cfg,
		"--add", "/opt/app=" + appFile,
		"--add", "/opt/app-copy=" + appFile,
		"--symlink", "/usr/bin/app=/opt/app",
		"--empty-files-from-file", empties,
		"--default-metadata", `{"uid":42,"gid":43}`,
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
	info, err := writeExt4Layer(treeOut, addTree, nil, nil, nil, nil, nil, false, nil)
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
	for _, name := range []string{"unused.cstream", "unused.ztoc"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s was written", name)
		}
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
