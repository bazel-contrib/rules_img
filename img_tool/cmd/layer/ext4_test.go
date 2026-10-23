package layer

import (
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
		"--compact-stream-only",
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
	info, err := writeExt4Layer(treeOut, addTree, nil, nil, nil, nil, nil, false, nil, nil, 0)
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
