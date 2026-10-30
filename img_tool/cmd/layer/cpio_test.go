package layer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/cpio"
	"github.com/klauspost/compress/zstd"
)

func TestCpioLayer(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, content []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	app := bytes.Repeat([]byte("application "), 5000)
	appFile := write("app", app)
	empties := write("empties.txt", []byte("var/empty\n"))

	out := filepath.Join(dir, "layer.cpio")
	metadataPath := filepath.Join(dir, "metadata.json")

	// Options for tar layers are accepted and ignored.
	LayerProcess(context.Background(), []string{
		"--layer-format", "cpio",
		"--estargz",
		"--add", "/init=" + appFile,
		"--symlink", "/sbin/init=/init",
		"--empty-files-from-file", empties,
		"--default-metadata", `{"uid":42,"gid":43}`,
		"--metadata", metadataPath,
		out,
	})

	archive, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(archive, []byte("070701")) || !bytes.Contains(archive, []byte("TRAILER!!!")) {
		t.Fatal("not a newc cpio archive")
	}
	if n := bytes.Count(archive, app); n != 1 {
		t.Errorf("the application is stored %d times", n)
	}
	got := map[string]*cpio.Entry{}
	if err := cpio.OpenReader(bytes.NewReader(archive)).Walk(func(e *cpio.Entry) error {
		c := *e
		got[e.Name] = &c
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"init", "sbin/init", "var/empty", "var", "sbin"} {
		if got[name] == nil {
			t.Errorf("member %q missing", name)
		}
	}
	if e := got["init"]; e == nil || e.UID != 42 || e.GID != 43 {
		t.Errorf("init: %+v", e)
	}
	if e := got["sbin/init"]; e == nil || e.Target != "/init" {
		t.Errorf("sbin/init: %+v", e)
	}

	var meta api.Descriptor
	raw, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive)
	if meta.Size != int64(len(archive)) || meta.Digest != fmt.Sprintf("sha256:%x", sum) || meta.Digest != meta.DiffID || meta.MediaType != api.CpioLayer {
		t.Errorf("metadata: %+v (archive is %d bytes)", meta, len(archive))
	}
}

// TestCpioLayerCompression checks the layer file of every compression, the
// metadata that describes it and the compact stream that reconstructs it.
func TestCpioLayerCompression(t *testing.T) {
	dir := t.TempDir()
	app := bytes.Repeat([]byte("application "), 5000)
	appFile := filepath.Join(dir, "app")
	if err := os.WriteFile(appFile, app, 0o644); err != nil {
		t.Fatal(err)
	}
	appSum := sha256.Sum256(app)
	blobs := blobStore{string(appSum[:]): app}

	// The plain archive is what every compression has to decompress to.
	plainPath := filepath.Join(dir, "plain.cpio")
	LayerProcess(context.Background(), []string{"--layer-format", "cpio", "--add", "/init=" + appFile, plainPath})
	plain, err := os.ReadFile(plainPath)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		format, mediaType string
		decompress        func([]byte) ([]byte, error)
	}{
		{"none", api.CpioLayer, func(b []byte) ([]byte, error) { return b, nil }},
		{"gzip", api.CpioGzipLayer, func(b []byte) ([]byte, error) {
			r, err := gzip.NewReader(bytes.NewReader(b))
			if err != nil {
				return nil, err
			}
			return io.ReadAll(r)
		}},
		{"zstd", api.CpioZstdLayer, func(b []byte) ([]byte, error) {
			r, err := zstd.NewReader(bytes.NewReader(b))
			if err != nil {
				return nil, err
			}
			defer r.Close()
			return io.ReadAll(r)
		}},
	} {
		t.Run(tc.format, func(t *testing.T) {
			out := filepath.Join(dir, tc.format+".cpio")
			cstreamPath := filepath.Join(dir, tc.format+".cstream")
			metadataPath := filepath.Join(dir, tc.format+".json")
			LayerProcess(context.Background(), []string{
				"--layer-format", "cpio", "--format", tc.format, "--compression-level", "3",
				"--add", "/init=" + appFile,
				"--compact-stream", cstreamPath, "--compact-stream-inline-threshold", "4096",
				"--metadata", metadataPath, out,
			})
			layer, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			got, err := tc.decompress(layer)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, plain) {
				t.Fatal("the layer file does not decompress to the archive")
			}
			if tc.format != "none" && len(layer) >= len(plain) {
				t.Errorf("%s layer has %d bytes, the archive %d", tc.format, len(layer), len(plain))
			}

			var meta api.Descriptor
			raw, err := os.ReadFile(metadataPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &meta); err != nil {
				t.Fatal(err)
			}
			layerSum, plainSum := sha256.Sum256(layer), sha256.Sum256(plain)
			if meta.MediaType != tc.mediaType || meta.Size != int64(len(layer)) ||
				meta.Digest != fmt.Sprintf("sha256:%x", layerSum) || meta.DiffID != fmt.Sprintf("sha256:%x", plainSum) {
				t.Errorf("metadata: %+v (layer file: %d bytes, sha256:%x; archive: sha256:%x)", meta, len(layer), layerSum, plainSum)
			}

			cstream, err := os.ReadFile(cstreamPath)
			if err != nil {
				t.Fatal(err)
			}
			csInfo, err := compactstream.Inspect(bytes.NewReader(cstream))
			if err != nil {
				t.Fatal(err)
			}
			if len(csInfo.Refs) != 1 {
				t.Errorf("compact stream references: %+v", csInfo.Refs)
			}
			if csInfo.Header.ContentFormat != compactstream.ContentFormatCpio {
				t.Errorf("content format %d, want cpio", csInfo.Header.ContentFormat)
			}
			var reconstructed bytes.Buffer
			if err := compactstream.Reconstruct(context.Background(), bytes.NewReader(cstream), blobs, &reconstructed); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(reconstructed.Bytes(), layer) {
				t.Error("the reconstructed compact stream differs from the layer file")
			}
		})
	}
}

func TestCpioLayerWithoutCompactStream(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "layer.cpio")
	LayerProcess(context.Background(), []string{"--layer-format", "cpio", "--add", "/f=" + file, out})
	archive, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(archive, []byte("content")) {
		t.Error("no archive written")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("unexpected files: %v", entries)
	}
}

// TestCpioLayerCompactStreamOnly checks that the archive can be left out: the
// compact stream and the metadata are the same as when it is written, with and
// without compression.
func TestCpioLayerCompactStreamOnly(t *testing.T) {
	dir := t.TempDir()
	big := bytes.Repeat([]byte("0123456789abcdef"), 4000)
	file := filepath.Join(dir, "big")
	if err := os.WriteFile(file, big, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"none", "gzip"} {
		run := func(only bool) (cstream []byte, metadata []byte, entries int) {
			work := t.TempDir()
			args := []string{"--layer-format", "cpio", "--format", format, "--add", "/big=" + file, "--compact-stream", filepath.Join(work, "layer.cstream"), "--metadata", filepath.Join(work, "metadata.json")}
			if only {
				args = append(args, "--compact-stream-only")
			} else {
				args = append(args, filepath.Join(work, "layer.cpio"))
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
		withArchive, mdWith, nWith := run(false)
		only, mdOnly, nOnly := run(true)
		if nWith != 3 || nOnly != 2 {
			t.Errorf("%s: files written: %d with the archive, %d without; want 3 and 2 (no leftover temporary archive)", format, nWith, nOnly)
		}
		if !bytes.Equal(withArchive, only) {
			t.Errorf("%s: the compact stream depends on whether the archive is written", format)
		}
		if !bytes.Equal(mdWith, mdOnly) {
			t.Errorf("%s: metadata differs:\n%s\n%s", format, mdWith, mdOnly)
		}
	}
}

// TestCpioLayerImportTar checks that a tar is read when the archive is written,
// not copied, and that the result depends on deduplication as asked.
func TestCpioLayerImportTar(t *testing.T) {
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
		out := filepath.Join(dir, "dedup-"+dedup+".cpio")
		LayerProcess(context.Background(), []string{"--layer-format", "cpio", "--cpio-deduplicate=" + dedup, "--import-tar", tarPath, out})
		archive, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		for name, content := range contents {
			if !bytes.Contains(archive, content) {
				t.Errorf("dedup=%s: %s missing from the archive", dedup, name)
			}
		}
		// a and c are the same file: stored once with deduplication.
		want := 1
		if dedup == "false" {
			want = 2
		}
		if n := bytes.Count(archive, contents["a"]); n != want {
			t.Errorf("dedup=%s: %d copies of the repeated file, want %d", dedup, n, want)
		}
		sum := sha256.Sum256(archive)
		digests[fmt.Sprintf("%x", sum)] = true
	}
	if len(digests) != 2 {
		t.Error("turning deduplication off does not change the archive")
	}
}

// TestCpioLayerTreeArtifactDeduplication checks that two tree artifacts with
// the same content are stored once, as a symlink to the first, like in a tar
// layer -- and only when it is asked for.
func TestCpioLayerTreeArtifactDeduplication(t *testing.T) {
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
		out := filepath.Join(dir, handling+".cpio")
		LayerProcess(context.Background(), []string{
			"--layer-format", "cpio", "--layer-tree-artifact-handling", handling,
			"--add", "/a=" + first, "--add", "/b=" + second, out,
		})
		f, err := os.Open(out)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		targets := map[string]string{}
		if err := cpio.OpenReader(f).Walk(func(e *cpio.Entry) error {
			targets[e.Name] = e.Target
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
