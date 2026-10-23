package manifestfromocilayout

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/klauspost/compress/zstd"
	specv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

type tarEntry struct {
	name string
	body string
}

func makeLayer(t *testing.T, entries []tarEntry) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     0o644,
			Size:     int64(len(e.body)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%s): %v", e.name, err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatalf("Write(%s): %v", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	raw := bytes.Clone(buf.Bytes())
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(raw)), nil
	}, tarball.WithMediaType(types.OCILayer))
	if err != nil {
		t.Fatalf("LayerFromOpener: %v", err)
	}
	return layer
}

func buildTestImage(t *testing.T) v1.Image {
	t.Helper()
	layer1 := makeLayer(t, []tarEntry{
		{name: "etc/hello.txt", body: "hello\n"},
		{name: "etc/obsolete.txt", body: "remove-me\n"},
		{name: "etc/override.txt", body: "v1\n"},
	})
	layer2 := makeLayer(t, []tarEntry{
		{name: "etc/.wh.obsolete.txt", body: ""},
		{name: "etc/override.txt", body: "v2\n"},
		{name: "usr/local/bin/tool", body: "#!/bin/sh\necho hi\n"},
	})

	img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	img, err := mutate.AppendLayers(img, layer1, layer2)
	if err != nil {
		t.Fatalf("AppendLayers: %v", err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("ConfigFile: %v", err)
	}
	cfg.OS = "linux"
	cfg.Architecture = "amd64"
	cfg.Config.Env = []string{"FOO=bar", "GREETING=hello-world"}
	cfg.Config.WorkingDir = "/app"
	cfg.Config.Entrypoint = []string{"/usr/local/bin/tool"}
	cfg.Config.Cmd = []string{"--serve"}
	cfg.Config.User = "1000:1000"
	cfg.Config.Labels = map[string]string{"org.opencontainers.image.title": "hello"}

	img, err = mutate.ConfigFile(img, cfg)
	if err != nil {
		t.Fatalf("mutate.ConfigFile: %v", err)
	}
	return img
}

func readZstdTarEntries(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	defer f.Close()

	zr, err := zstd.NewReader(f)
	if err != nil {
		t.Fatalf("zstd.NewReader: %v", err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	files := make(map[string]string)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("ReadAll(%s): %v", hdr.Name, err)
			}
			files[hdr.Name] = string(b)
		}
	}
	return files
}

func dirToTar(t *testing.T, srcDir, dstTar string) {
	t.Helper()
	out, err := os.Create(dstTar)
	if err != nil {
		t.Fatalf("Create(%s): %v", dstTar, err)
	}
	defer out.Close()

	tw := tar.NewWriter(out)
	err = filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk(%s): %v", srcDir, err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tw.Close: %v", err)
	}
}

func configureFlagsForTest(t *testing.T, outDir string, isFlatten bool, mediaTypes []string) {
	t.Helper()
	manifestOutput = filepath.Join(outDir, "manifest.json")
	configOutput = filepath.Join(outDir, "config.json")
	descriptorOutput = filepath.Join(outDir, "descriptor.json")
	digestOutput = filepath.Join(outDir, "digest.txt")
	architecture = "amd64"
	operatingSystem = "linux"
	variant = ""
	flatten = isFlatten
	layerMediaTypes = newIndexedStringFlag()
	layerBlobs = newIndexedStringFlag()
	layerMetadataJSON = newIndexedStringFlag()
	for i, mt := range mediaTypes {
		layerMediaTypes.values[i] = mt
		layerBlobs.values[i] = filepath.Join(outDir, fmt.Sprintf("layer_blob_%d", i))
		layerMetadataJSON.values[i] = filepath.Join(outDir, fmt.Sprintf("layer_metadata_%d.json", i))
	}
}

func assertFlattenedOutputs(t *testing.T, outDir string) {
	t.Helper()

	manifestRaw, err := os.ReadFile(filepath.Join(outDir, "manifest.json"))
	if err != nil {
		t.Fatalf("reading manifest.json: %v", err)
	}
	var manifest specv1.Manifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatalf("unmarshaling manifest.json: %v", err)
	}
	if len(manifest.Layers) != 1 {
		t.Fatalf("expected 1 flattened layer in manifest, got %d", len(manifest.Layers))
	}
	if manifest.Layers[0].MediaType != api.TarZstdLayer {
		t.Errorf("expected layer mediaType %s, got %s", api.TarZstdLayer, manifest.Layers[0].MediaType)
	}

	configRaw, err := os.ReadFile(filepath.Join(outDir, "config.json"))
	if err != nil {
		t.Fatalf("reading config.json: %v", err)
	}
	var config specv1.Image
	if err := json.Unmarshal(configRaw, &config); err != nil {
		t.Fatalf("unmarshaling config.json: %v", err)
	}
	if len(config.RootFS.DiffIDs) != 1 {
		t.Fatalf("expected 1 DiffID in flattened config, got %d", len(config.RootFS.DiffIDs))
	}
	if !reflect.DeepEqual(config.Config.Env, []string{"FOO=bar", "GREETING=hello-world"}) {
		t.Errorf("unexpected Env: %v", config.Config.Env)
	}
	if config.Config.WorkingDir != "/app" {
		t.Errorf("unexpected WorkingDir: %s", config.Config.WorkingDir)
	}
	if !reflect.DeepEqual(config.Config.Entrypoint, []string{"/usr/local/bin/tool"}) {
		t.Errorf("unexpected Entrypoint: %v", config.Config.Entrypoint)
	}
	if !reflect.DeepEqual(config.Config.Cmd, []string{"--serve"}) {
		t.Errorf("unexpected Cmd: %v", config.Config.Cmd)
	}
	if config.Config.User != "1000:1000" {
		t.Errorf("unexpected User: %s", config.Config.User)
	}
	if config.Config.Labels["org.opencontainers.image.title"] != "hello" {
		t.Errorf("unexpected Labels: %v", config.Config.Labels)
	}

	metaRaw, err := os.ReadFile(filepath.Join(outDir, "layer_metadata_0.json"))
	if err != nil {
		t.Fatalf("reading layer_metadata_0.json: %v", err)
	}
	var layerMeta api.Descriptor
	if err := json.Unmarshal(metaRaw, &layerMeta); err != nil {
		t.Fatalf("unmarshaling layer_metadata_0.json: %v", err)
	}
	if layerMeta.DiffID != config.RootFS.DiffIDs[0].String() {
		t.Errorf("metadata diff_id %s != config diff_id %s", layerMeta.DiffID, config.RootFS.DiffIDs[0])
	}
	if layerMeta.Digest != manifest.Layers[0].Digest.String() {
		t.Errorf("metadata digest %s != manifest layer digest %s", layerMeta.Digest, manifest.Layers[0].Digest)
	}

	entries := readZstdTarEntries(t, filepath.Join(outDir, "layer_blob_0"))
	wantEntries := map[string]string{
		"etc/hello.txt":      "hello\n",
		"etc/override.txt":   "v2\n",
		"usr/local/bin/tool": "#!/bin/sh\necho hi\n",
	}
	if !reflect.DeepEqual(entries, wantEntries) {
		t.Errorf("flattened tar entries = %v, want %v", entries, wantEntries)
	}
}

func TestDockerSaveArchiveFlattenAndMultiLayer(t *testing.T) {
	img := buildTestImage(t)
	tmpDir := t.TempDir()
	dockerTar := filepath.Join(tmpDir, "docker_save.tar")
	tag, err := name.NewTag("example.com/test:latest")
	if err != nil {
		t.Fatalf("NewTag: %v", err)
	}
	if err := tarball.WriteToFile(dockerTar, tag, img); err != nil {
		t.Fatalf("tarball.WriteToFile: %v", err)
	}

	t.Run("flatten", func(t *testing.T) {
		outDir := t.TempDir()
		configureFlagsForTest(t, outDir, true, []string{api.TarZstdLayer})
		if err := processSource(dockerTar); err != nil {
			t.Fatalf("processSource(flatten=true): %v", err)
		}
		assertFlattenedOutputs(t, outDir)
	})

	t.Run("multi_layer", func(t *testing.T) {
		outDir := t.TempDir()
		configureFlagsForTest(t, outDir, false, []string{api.TarZstdLayer, api.TarZstdLayer})
		if err := processSource(dockerTar); err != nil {
			t.Fatalf("processSource(flatten=false): %v", err)
		}
		manifestRaw, err := os.ReadFile(filepath.Join(outDir, "manifest.json"))
		if err != nil {
			t.Fatalf("reading manifest.json: %v", err)
		}
		var manifest specv1.Manifest
		if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
			t.Fatalf("unmarshaling manifest.json: %v", err)
		}
		if len(manifest.Layers) != 2 {
			t.Fatalf("expected 2 layers in manifest, got %d", len(manifest.Layers))
		}
	})
}

func TestOCILayoutDirAndTarArchive(t *testing.T) {
	img := buildTestImage(t)
	tmpDir := t.TempDir()
	ociDir := filepath.Join(tmpDir, "oci_layout")
	lp, err := layout.Write(ociDir, empty.Index)
	if err != nil {
		t.Fatalf("layout.Write: %v", err)
	}
	if err := lp.AppendImage(img, layout.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"})); err != nil {
		t.Fatalf("lp.AppendImage: %v", err)
	}

	ociTar := filepath.Join(tmpDir, "oci_layout.tar")
	dirToTar(t, ociDir, ociTar)

	t.Run("oci_dir_flatten", func(t *testing.T) {
		outDir := t.TempDir()
		configureFlagsForTest(t, outDir, true, []string{api.TarZstdLayer})
		if err := processSource(ociDir); err != nil {
			t.Fatalf("processSource(ociDir, flatten=true): %v", err)
		}
		assertFlattenedOutputs(t, outDir)
	})

	t.Run("oci_tar_flatten", func(t *testing.T) {
		outDir := t.TempDir()
		configureFlagsForTest(t, outDir, true, []string{api.TarZstdLayer})
		if err := processSource(ociTar); err != nil {
			t.Fatalf("processSource(ociTar, flatten=true): %v", err)
		}
		assertFlattenedOutputs(t, outDir)
	})

	t.Run("oci_tar_multi_layer", func(t *testing.T) {
		outDir := t.TempDir()
		configureFlagsForTest(t, outDir, false, []string{api.TarGzipLayer, api.TarGzipLayer})
		if err := processSource(ociTar); err != nil {
			t.Fatalf("processSource(ociTar, flatten=false): %v", err)
		}
		manifestRaw, err := os.ReadFile(filepath.Join(outDir, "manifest.json"))
		if err != nil {
			t.Fatalf("reading manifest.json: %v", err)
		}
		var manifest specv1.Manifest
		if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
			t.Fatalf("unmarshaling manifest.json: %v", err)
		}
		if len(manifest.Layers) != 2 {
			t.Fatalf("expected 2 layers in manifest, got %d", len(manifest.Layers))
		}
	})
}
