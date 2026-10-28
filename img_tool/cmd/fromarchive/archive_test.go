package fromarchive

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	specv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/imagearchive"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/imagearchive/archivetest"
)

func makeLayerSpecs(outDir string, mediaTypes ...string) []LayerOutputSpec {
	specs := make([]LayerOutputSpec, len(mediaTypes))
	for i, mt := range mediaTypes {
		specs[i] = LayerOutputSpec{
			MediaType:    mt,
			BlobPath:     filepath.Join(outDir, fmt.Sprintf("layer_%d.blob", i)),
			MetadataPath: filepath.Join(outDir, fmt.Sprintf("layer_%d_meta.json", i)),
		}
	}
	return specs
}

func makeTwoLayerImage(t *testing.T, osName, arch, variant string) v1.Image {
	t.Helper()
	l1 := archivetest.MakeLayer(t, types.OCILayer, archivetest.Entry{Name: "etc/hello.txt", Body: "hello\n"})
	l2 := archivetest.MakeLayer(t, types.OCILayer, archivetest.Entry{Name: "usr/local/bin/tool", Body: "#!/bin/sh\necho hi\n"})
	return archivetest.MakeImage(t, osName, arch, variant, func(cf *v1.ConfigFile) {
		cf.Config.Env = []string{"FOO=bar"}
		cf.Config.WorkingDir = "/app"
		cf.Config.Healthcheck = &v1.HealthConfig{Test: []string{"CMD", "/usr/local/bin/tool", "health"}, Retries: 3}
		cf.Config.Shell = []string{"/bin/sh", "-c"}
	}, l1, l2)
}

func TestManifestFromArchive(t *testing.T) {
	amd64Img := makeTwoLayerImage(t, "linux", "amd64", "")
	arm64Img := makeTwoLayerImage(t, "linux", "arm64", "v8")
	origDigest, err := amd64Img.Digest()
	if err != nil {
		t.Fatal(err)
	}

	ociDir, ociTar := archivetest.WriteSingleOCITar(t, amd64Img, "linux", "amd64", "")

	t.Run("oci_fast_path_preserves_exact_digest", func(t *testing.T) {
		outDir := t.TempDir()
		opts := ManifestOptions{
			SrcPath:          ociTar,
			ManifestOutput:   filepath.Join(outDir, "manifest.json"),
			ConfigOutput:     filepath.Join(outDir, "config.json"),
			DescriptorOutput: filepath.Join(outDir, "descriptor.json"),
			DigestOutput:     filepath.Join(outDir, "digest.txt"),
			Platform:         imagearchive.Platform{OS: "linux", Architecture: "amd64"},
			Layers:           makeLayerSpecs(outDir, api.TarGzipLayer, api.TarGzipLayer),
			Compression:      imagearchive.CompressionOptions{Level: -1},
		}
		if err := RunManifest(opts); err != nil {
			t.Fatalf("RunManifest fast path: %v", err)
		}
		gotDigest, _ := os.ReadFile(opts.DigestOutput)
		if string(gotDigest) != origDigest.String() {
			t.Errorf("digest = %s, want exact original %s", gotDigest, origDigest)
		}
	})

	t.Run("oci_conversion_gzip_to_zstd", func(t *testing.T) {
		outDir := t.TempDir()
		opts := ManifestOptions{
			SrcPath:          ociTar,
			ManifestOutput:   filepath.Join(outDir, "manifest.json"),
			ConfigOutput:     filepath.Join(outDir, "config.json"),
			DescriptorOutput: filepath.Join(outDir, "descriptor.json"),
			DigestOutput:     filepath.Join(outDir, "digest.txt"),
			Platform:         imagearchive.Platform{OS: "linux", Architecture: "amd64"},
			Layers:           makeLayerSpecs(outDir, api.TarZstdLayer, api.TarZstdLayer),
			Compression:      imagearchive.CompressionOptions{Level: -1},
		}
		if err := RunManifest(opts); err != nil {
			t.Fatalf("RunManifest conversion: %v", err)
		}
		var m specv1.Manifest
		raw, _ := os.ReadFile(opts.ManifestOutput)
		_ = json.Unmarshal(raw, &m)
		for i, l := range m.Layers {
			if l.MediaType != api.TarZstdLayer {
				t.Errorf("layer %d mediaType = %s, want %s", i, l.MediaType, api.TarZstdLayer)
			}
		}
		// Recompression keeps diff_ids, so the config (including Docker-only Healthcheck and Shell) must stay byte-identical.
		wantConfig, _ := amd64Img.RawConfigFile()
		if gotConfig, _ := os.ReadFile(opts.ConfigOutput); string(gotConfig) != string(wantConfig) {
			t.Errorf("config changed during recompression:\n got %s\nwant %s", gotConfig, wantConfig)
		}
	})

	t.Run("docker_save_multi_platform_selects_arm64", func(t *testing.T) {
		dockerTar := filepath.Join(t.TempDir(), "multi.tar")
		tagAmd, _ := name.NewTag("example.com/app:amd64")
		tagArm, _ := name.NewTag("example.com/app:arm64")
		if err := tarball.MultiWriteToFile(dockerTar, map[name.Tag]v1.Image{tagAmd: amd64Img, tagArm: arm64Img}); err != nil {
			t.Fatal(err)
		}
		outDir := t.TempDir()
		opts := ManifestOptions{
			SrcPath:          dockerTar,
			ManifestOutput:   filepath.Join(outDir, "manifest.json"),
			ConfigOutput:     filepath.Join(outDir, "config.json"),
			DescriptorOutput: filepath.Join(outDir, "descriptor.json"),
			DigestOutput:     filepath.Join(outDir, "digest.txt"),
			Platform:         imagearchive.Platform{OS: "linux", Architecture: "arm64"},
			Layers:           makeLayerSpecs(outDir, api.TarZstdLayer, api.TarZstdLayer),
			Compression:      imagearchive.CompressionOptions{Level: -1},
		}
		if err := RunManifest(opts); err != nil {
			t.Fatalf("RunManifest docker save: %v", err)
		}
		var desc specv1.Descriptor
		raw, _ := os.ReadFile(opts.DescriptorOutput)
		_ = json.Unmarshal(raw, &desc)
		if desc.Platform == nil || desc.Platform.Architecture != "arm64" || desc.Platform.Variant != "v8" {
			t.Fatalf("unexpected descriptor platform: %+v", desc.Platform)
		}
	})

	t.Run("directory_rejected_with_hint", func(t *testing.T) {
		opts := ManifestOptions{SrcPath: ociDir, Platform: imagearchive.Platform{OS: "linux", Architecture: "amd64"}}
		if err := RunManifest(opts); err == nil || !strings.Contains(err.Error(), "manifest-from-oci-layout") {
			t.Fatalf("expected manifest-from-oci-layout hint, got %v", err)
		}
	})
}

func makeIndexOutputs(t *testing.T, mediaType string) IndexOptions {
	t.Helper()
	outDir := t.TempDir()
	specFor := func(idx int, arch string) ManifestOutputSpec {
		return ManifestOutputSpec{
			Platform:         imagearchive.Platform{OS: "linux", Architecture: arch},
			ManifestOutput:   filepath.Join(outDir, fmt.Sprintf("%d_manifest.json", idx)),
			ConfigOutput:     filepath.Join(outDir, fmt.Sprintf("%d_config.json", idx)),
			DescriptorOutput: filepath.Join(outDir, fmt.Sprintf("%d_descriptor.json", idx)),
			Layers: []LayerOutputSpec{{
				MediaType:    mediaType,
				BlobPath:     filepath.Join(outDir, fmt.Sprintf("%d_l0.blob", idx)),
				MetadataPath: filepath.Join(outDir, fmt.Sprintf("%d_l0.json", idx)),
			}},
		}
	}
	return IndexOptions{
		IndexOutput:           filepath.Join(outDir, "index.json"),
		DigestOutput:          filepath.Join(outDir, "digest.txt"),
		IndexDescriptorOutput: filepath.Join(outDir, "index_descriptor.json"),
		Compression:           imagearchive.CompressionOptions{Level: -1},
		Manifests:             []ManifestOutputSpec{specFor(0, "amd64"), specFor(1, "arm64")},
	}
}

func TestIndexFromArchive(t *testing.T) {
	amd64Img := archivetest.MakeImage(t, "linux", "amd64", "", nil, archivetest.MakeLayer(t, types.OCILayer, archivetest.Entry{Name: "a", Body: "amd"}))
	arm64Img := archivetest.MakeImage(t, "linux", "arm64", "v8", nil, archivetest.MakeLayer(t, types.OCILayer, archivetest.Entry{Name: "a", Body: "arm"}))
	wantAMD, _ := amd64Img.Digest()
	wantARM, _ := arm64Img.Digest()

	idx := mutate.AppendManifests(
		empty.Index,
		mutate.IndexAddendum{Add: amd64Img, Descriptor: v1.Descriptor{MediaType: types.OCIManifestSchema1, Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}},
		mutate.IndexAddendum{Add: arm64Img, Descriptor: v1.Descriptor{MediaType: types.OCIManifestSchema1, Platform: &v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}}},
	)
	layoutDir := filepath.Join(t.TempDir(), "oci-index")
	if _, err := layout.Write(layoutDir, idx); err != nil {
		t.Fatal(err)
	}
	ociTar := filepath.Join(t.TempDir(), "multiarch-oci.tar")
	archivetest.WriteDirAsTar(t, layoutDir, ociTar)

	t.Run("oci_fast_path_and_zstd_conversion", func(t *testing.T) {
		fastOpts := makeIndexOutputs(t, api.TarGzipLayer)
		fastOpts.SrcPath = ociTar
		if err := RunIndex(fastOpts); err != nil {
			t.Fatalf("RunIndex(fast): %v", err)
		}
		var outIdx specv1.Index
		raw, _ := os.ReadFile(fastOpts.IndexOutput)
		_ = json.Unmarshal(raw, &outIdx)
		if len(outIdx.Manifests) != 2 || outIdx.Manifests[0].Digest.String() != wantAMD.String() || outIdx.Manifests[1].Digest.String() != wantARM.String() {
			t.Fatalf("unexpected fast-path index manifests: %+v", outIdx.Manifests)
		}

		zstdOpts := makeIndexOutputs(t, api.TarZstdLayer)
		zstdOpts.SrcPath = ociTar
		if err := RunIndex(zstdOpts); err != nil {
			t.Fatalf("RunIndex(zstd): %v", err)
		}
	})

	t.Run("multi_entry_docker_save", func(t *testing.T) {
		tagAMD, _ := name.NewTag("example.com/multi:amd64")
		tagARM, _ := name.NewTag("example.com/multi:arm64")
		dockerTar := filepath.Join(t.TempDir(), "multi-docker.tar")
		if err := tarball.MultiWriteToFile(dockerTar, map[name.Tag]v1.Image{tagAMD: amd64Img, tagARM: arm64Img}); err != nil {
			t.Fatal(err)
		}

		// Read entry order from manifest.json.
		f, _ := os.Open(dockerTar)
		defer f.Close()
		tr := tar.NewReader(f)
		files := map[string][]byte{}
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if !strings.HasSuffix(hdr.Name, ".tar") && !strings.HasSuffix(hdr.Name, ".tar.gz") {
				files[hdr.Name], _ = io.ReadAll(tr)
			}
		}
		var entries []struct{ Config string }
		_ = json.Unmarshal(files["manifest.json"], &entries)

		opts := makeIndexOutputs(t, api.TarZstdLayer)
		opts.SrcPath = dockerTar
		for i, e := range entries {
			var cfg specv1.Image
			_ = json.Unmarshal(files[e.Config], &cfg)
			opts.Manifests[i].Platform = imagearchive.Platform{OS: cfg.OS, Architecture: cfg.Architecture}
		}
		if err := RunIndex(opts); err != nil {
			t.Fatalf("RunIndex(docker save): %v", err)
		}
	})

	t.Run("validation_errors", func(t *testing.T) {
		swapped := makeIndexOutputs(t, api.TarGzipLayer)
		swapped.SrcPath = ociTar
		swapped.Manifests[0].Platform.Architecture = "arm64"
		swapped.Manifests[1].Platform.Architecture = "amd64"
		if err := RunIndex(swapped); err == nil || !strings.Contains(err.Error(), "architecture mismatch") {
			t.Fatalf("expected architecture mismatch, got %v", err)
		}

		wrongCount := makeIndexOutputs(t, api.TarGzipLayer)
		wrongCount.SrcPath = ociTar
		wrongCount.Manifests = wrongCount.Manifests[:1]
		if err := RunIndex(wrongCount); err == nil || !strings.Contains(err.Error(), "manifest count mismatch") {
			t.Fatalf("expected manifest count mismatch, got %v", err)
		}

		dirOpts := makeIndexOutputs(t, api.TarGzipLayer)
		dirOpts.SrcPath = layoutDir
		if err := RunIndex(dirOpts); err == nil || !strings.Contains(err.Error(), "index-from-oci-layout") {
			t.Fatalf("expected index-from-oci-layout hint, got %v", err)
		}
	})
}
