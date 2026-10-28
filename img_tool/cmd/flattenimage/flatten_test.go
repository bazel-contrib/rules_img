package flattenimage

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	specv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fileopener"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/imagearchive"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/imagearchive/archivetest"
)

func buildTestImage(t *testing.T) v1.Image {
	t.Helper()
	l1 := archivetest.MakeLayer(t, types.OCILayer,
		archivetest.Entry{Name: "etc/hello.txt", Body: "hello\n"},
		archivetest.Entry{Name: "etc/obsolete.txt", Body: "remove-me\n"},
		archivetest.Entry{Name: "etc/override.txt", Body: "v1\n"},
	)
	l2 := archivetest.MakeLayer(t, types.OCILayer,
		archivetest.Entry{Name: "etc/.wh.obsolete.txt", Body: ""},
		archivetest.Entry{Name: "etc/override.txt", Body: "v2\n"},
		archivetest.Entry{Name: "usr/local/bin/tool", Body: "#!/bin/sh\necho hi\n"},
	)
	return archivetest.MakeImage(t, "linux", "amd64", "", func(cfg *v1.ConfigFile) {
		cfg.Config.Env = []string{"FOO=bar", "GREETING=hello-world"}
		cfg.Config.WorkingDir = "/app"
		cfg.Config.Entrypoint = []string{"/usr/local/bin/tool"}
		cfg.Config.Cmd = []string{"--serve"}
		cfg.Config.User = "1000:1000"
		cfg.Config.Labels = map[string]string{"org.opencontainers.image.title": "hello"}
		cfg.Config.Healthcheck = &v1.HealthConfig{Test: []string{"CMD", "/usr/local/bin/tool", "health"}, Retries: 3}
		cfg.Config.OnBuild = []string{"RUN echo onbuild"}
		cfg.Config.Shell = []string{"/bin/sh", "-c"}
	}, l1, l2)
}

func readZstdTarEntries(t *testing.T, p string) map[string]string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := fileopener.CompressionReader(f)
	if err != nil {
		t.Fatal(err)
	}
	if rc, ok := r.(io.Closer); ok {
		defer rc.Close()
	}

	tr := tar.NewReader(r)
	files := make(map[string]string)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA {
			b, _ := io.ReadAll(tr)
			files[hdr.Name] = string(b)
		}
	}
	return files
}

func newOutputOpts(outDir string) Options {
	return Options{
		Format:              "zstd",
		Compression:         imagearchive.CompressionOptions{Level: -1},
		ManifestOutput:      filepath.Join(outDir, "manifest.json"),
		ConfigOutput:        filepath.Join(outDir, "config.json"),
		DescriptorOutput:    filepath.Join(outDir, "descriptor.json"),
		DigestOutput:        filepath.Join(outDir, "digest.txt"),
		LayerBlobOutput:     filepath.Join(outDir, "flat.tzst"),
		LayerMetadataOutput: filepath.Join(outDir, "flat_metadata.json"),
	}
}

func assertFlattenedOutputs(t *testing.T, opts Options) {
	t.Helper()
	var manifest specv1.Manifest
	rawManifest, err := os.ReadFile(opts.ManifestOutput)
	if err != nil || json.Unmarshal(rawManifest, &manifest) != nil {
		t.Fatalf("reading/unmarshaling manifest: %v", err)
	}
	if len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != api.TarZstdLayer {
		t.Fatalf("unexpected manifest layers: %+v", manifest.Layers)
	}

	var config specv1.Image
	rawConfig, err := os.ReadFile(opts.ConfigOutput)
	if err != nil || json.Unmarshal(rawConfig, &config) != nil {
		t.Fatalf("reading/unmarshaling config: %v", err)
	}
	if len(config.RootFS.DiffIDs) != 1 ||
		!reflect.DeepEqual(config.Config.Env, []string{"FOO=bar", "GREETING=hello-world"}) ||
		config.Config.WorkingDir != "/app" ||
		!reflect.DeepEqual(config.Config.Entrypoint, []string{"/usr/local/bin/tool"}) ||
		!reflect.DeepEqual(config.Config.Cmd, []string{"--serve"}) ||
		config.Config.User != "1000:1000" ||
		config.Config.Labels["org.opencontainers.image.title"] != "hello" {
		t.Fatalf("unexpected config: %+v", config.Config)
	}
	dockerCfg, err := v1.ParseConfigFile(bytes.NewReader(rawConfig))
	if err != nil || dockerCfg.Config.Healthcheck == nil || len(dockerCfg.Config.OnBuild) != 1 || len(dockerCfg.Config.Shell) != 2 {
		t.Fatalf("Docker-only config fields (Healthcheck, OnBuild, Shell) were dropped: %s (err=%v)", rawConfig, err)
	}

	var layerMeta api.Descriptor
	rawMeta, _ := os.ReadFile(opts.LayerMetadataOutput)
	_ = json.Unmarshal(rawMeta, &layerMeta)
	if layerMeta.DiffID != config.RootFS.DiffIDs[0].String() || layerMeta.Digest != manifest.Layers[0].Digest.String() {
		t.Fatalf("metadata mismatch: %+v", layerMeta)
	}

	wantEntries := map[string]string{
		"etc/hello.txt":      "hello\n",
		"etc/override.txt":   "v2\n",
		"usr/local/bin/tool": "#!/bin/sh\necho hi\n",
	}
	if got := readZstdTarEntries(t, opts.LayerBlobOutput); !reflect.DeepEqual(got, wantEntries) {
		t.Errorf("flattened tar entries = %v, want %v", got, wantEntries)
	}
}

func TestFlattenImageAllInputModes(t *testing.T) {
	img := buildTestImage(t)
	dockerTar := filepath.Join(t.TempDir(), "docker_save.tar")
	tag, _ := name.NewTag("example.com/test:latest")
	if err := tarball.WriteToFile(dockerTar, tag, img); err != nil {
		t.Fatal(err)
	}
	ociDir, ociTar := archivetest.WriteSingleOCITar(t, img, "linux", "amd64", "")

	for _, tc := range []struct {
		name string
		src  string
	}{
		{"docker_save_tar", dockerTar},
		{"oci_layout_tar", ociTar},
		{"oci_layout_dir", ociDir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := newOutputOpts(t.TempDir())
			opts.SrcPath = tc.src
			opts.Platform = imagearchive.Platform{OS: "linux", Architecture: "amd64"}
			if err := Run(opts); err != nil {
				t.Fatalf("Run(%s): %v", tc.name, err)
			}
			assertFlattenedOutputs(t, opts)
		})
	}

	t.Run("manifest_config_layers_and_omit_history", func(t *testing.T) {
		rawManifest, _ := img.RawManifest()
		rawConfig, _ := img.RawConfigFile()
		manifestObj, _ := img.Manifest()

		srcDir := t.TempDir()
		srcManifest := filepath.Join(srcDir, "src_manifest.json")
		srcConfig := filepath.Join(srcDir, "src_config.json")
		_ = os.WriteFile(srcManifest, rawManifest, 0o644)
		_ = os.WriteFile(srcConfig, rawConfig, 0o644)
		layerPaths := make([]string, len(manifestObj.Layers))
		for i, l := range manifestObj.Layers {
			layerPaths[i] = filepath.Join(ociDir, "blobs", l.Digest.Algorithm, l.Digest.Hex)
		}

		opts := newOutputOpts(t.TempDir())
		opts.SourceManifest = srcManifest
		opts.SourceConfig = srcConfig
		opts.Layers = layerPaths
		opts.OmitHistory = true
		if err := Run(opts); err != nil {
			t.Fatalf("Run(manifest_config_and_layers): %v", err)
		}
		assertFlattenedOutputs(t, opts)

		var cfg specv1.Image
		rawCfg, _ := os.ReadFile(opts.ConfigOutput)
		_ = json.Unmarshal(rawCfg, &cfg)
		if len(cfg.History) != 0 {
			t.Errorf("expected empty config.History when OmitHistory=true, got %+v", cfg.History)
		}
	})
}

// Non-tar layers (EROFS) read as an empty tar stream instead of failing, which would silently
// flatten the image into an empty filesystem. Both input modes must reject them.
func TestFlattenImageRejectsNonTarLayers(t *testing.T) {
	erofsLayer := archivetest.MakeLayer(t, types.MediaType(api.ErofsLayer),
		archivetest.Entry{Name: "etc/hello.txt", Body: "hello\n"},
	)
	img := archivetest.MakeImage(t, "linux", "amd64", "", nil, erofsLayer)
	ociDir, ociTar := archivetest.WriteSingleOCITar(t, img, "linux", "amd64", "")

	t.Run("src", func(t *testing.T) {
		opts := newOutputOpts(t.TempDir())
		opts.SrcPath = ociTar
		opts.Platform = imagearchive.Platform{OS: "linux", Architecture: "amd64"}
		if err := Run(opts); err == nil || !strings.Contains(err.Error(), "not a tar changeset") {
			t.Fatalf("Run(src with EROFS layer) error = %v, want non-tar changeset error", err)
		}
	})

	t.Run("source_manifest", func(t *testing.T) {
		rawManifest, _ := img.RawManifest()
		rawConfig, _ := img.RawConfigFile()
		manifestObj, _ := img.Manifest()

		srcDir := t.TempDir()
		srcManifest := filepath.Join(srcDir, "src_manifest.json")
		srcConfig := filepath.Join(srcDir, "src_config.json")
		_ = os.WriteFile(srcManifest, rawManifest, 0o644)
		_ = os.WriteFile(srcConfig, rawConfig, 0o644)
		layerPaths := make([]string, len(manifestObj.Layers))
		for i, l := range manifestObj.Layers {
			layerPaths[i] = filepath.Join(ociDir, "blobs", l.Digest.Algorithm, l.Digest.Hex)
		}

		opts := newOutputOpts(t.TempDir())
		opts.SourceManifest = srcManifest
		opts.SourceConfig = srcConfig
		opts.Layers = layerPaths
		if err := Run(opts); err == nil || !strings.Contains(err.Error(), "not a tar changeset") {
			t.Fatalf("Run(source-manifest with EROFS layer) error = %v, want non-tar changeset error", err)
		}
	})
}
