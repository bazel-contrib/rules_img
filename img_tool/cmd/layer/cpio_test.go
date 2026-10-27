package layer

import (
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
		"--compression-level", "9",
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
	for _, want := range []string{"init\x00", "sbin/init\x00", "var/empty\x00", "var\x00"} {
		if !bytes.Contains(archive, []byte(want)) {
			t.Errorf("member %q missing", want)
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
	if meta.Size != int64(len(archive)) || meta.Digest == "" || meta.Digest != meta.DiffID || meta.MediaType != api.CpioLayer {
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
