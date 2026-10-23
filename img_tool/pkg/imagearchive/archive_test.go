package imagearchive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/types"
	specv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/imagearchive/archivetest"
)

func TestIndexedTarRandomAccessAndTemporalHardlinks(t *testing.T) {
	tarPath := filepath.Join(t.TempDir(), "sample.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	tw := tar.NewWriter(f)
	writeReg := func(name, content string) {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("WriteHeader(%s): %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("Write(%s): %v", name, err)
		}
	}

	writeReg("./index.json", `{"schemaVersion":2}`)
	writeReg("blobs/sha256/aaaa", strings.Repeat("first-payload-", 300))
	writeReg("/blobs/sha256/bbbb", "original-bbbb")

	if err := tw.WriteHeader(&tar.Header{
		Name:     "blobs/sha256/hardlink_to_bbbb",
		Linkname: "blobs/sha256/bbbb",
		Typeflag: tar.TypeLink,
	}); err != nil {
		t.Fatalf("WriteHeader hardlink: %v", err)
	}
	// Overwriting bbbb later in the stream must not alter an earlier POSIX hardlink.
	writeReg("blobs/sha256/bbbb", "overwritten-bbbb")

	if err := tw.WriteHeader(&tar.Header{
		Name:     "refs/latest",
		Linkname: "../blobs/sha256/aaaa",
		Typeflag: tar.TypeSymlink,
	}); err != nil {
		t.Fatalf("WriteHeader symlink: %v", err)
	}
	_ = tw.Close()
	_ = f.Close()

	idx, err := OpenIndexedTar(tarPath)
	if err != nil {
		t.Fatalf("OpenIndexedTar: %v", err)
	}
	defer idx.Close()

	gotHardlink, err := idx.ReadBytes("blobs/sha256/hardlink_to_bbbb")
	if err != nil || string(gotHardlink) != "original-bbbb" {
		t.Fatalf("hardlink = %q (err=%v), want %q", gotHardlink, err, "original-bbbb")
	}
	gotLatestB, err := idx.ReadBytes("blobs/sha256/bbbb")
	if err != nil || string(gotLatestB) != "overwritten-bbbb" {
		t.Fatalf("overwritten entry = %q (err=%v), want %q", gotLatestB, err, "overwritten-bbbb")
	}
	sr, sz, err := idx.OpenEntry("refs/latest")
	if err != nil {
		t.Fatalf("OpenEntry symlink: %v", err)
	}
	gotSymlink, _ := io.ReadAll(sr)
	wantAAAA := strings.Repeat("first-payload-", 300)
	if sz != int64(len(wantAAAA)) || string(gotSymlink) != wantAAAA {
		t.Errorf("symlink size=%d len=%d", sz, len(gotSymlink))
	}
}

func TestIndexedTarRejectsCompressedAndUnsafePaths(t *testing.T) {
	gzPath := filepath.Join(t.TempDir(), "archive.tar.gz")
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, _ = gw.Write([]byte("not an uncompressed tar"))
	_ = gw.Close()
	if err := os.WriteFile(gzPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenIndexedTar(gzPath); err == nil || !strings.Contains(err.Error(), "gzip-compressed") {
		t.Fatalf("expected gzip error, got %v", err)
	}

	tarPath := filepath.Join(t.TempDir(), "unsafe.tar")
	f, _ := os.Create(tarPath)
	tw := tar.NewWriter(f)
	_ = tw.WriteHeader(&tar.Header{Name: "../escape.txt", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("evil"))
	_ = tw.Close()
	_ = f.Close()
	if _, err := OpenIndexedTar(tarPath); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("expected unsafe path error, got %v", err)
	}
}

func TestMultiTagUnplatformedOCISource(t *testing.T) {
	layer := archivetest.MakeLayer(t, types.OCILayer, archivetest.Entry{Name: "a.txt", Body: "ok"})
	img := archivetest.MakeImage(t, "linux", "amd64", "", nil, layer)
	ociDir, _ := archivetest.WriteSingleOCITar(t, img, "linux", "amd64", "")

	// Rewrite index.json with two tag descriptors that both omit Platform (mirrors `img docker-save` multi-tag output).
	rawIdx, err := os.ReadFile(filepath.Join(ociDir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx specv1.Index
	if err := json.Unmarshal(rawIdx, &idx); err != nil {
		t.Fatal(err)
	}
	d1 := idx.Manifests[0]
	d1.Platform = nil
	d1.Annotations = map[string]string{specv1.AnnotationRefName: "app:v1"}
	d2 := d1
	d2.Annotations = map[string]string{specv1.AnnotationRefName: "app:latest"}
	idx.Manifests = []specv1.Descriptor{d1, d2}
	updated, _ := json.Marshal(idx)
	_ = os.WriteFile(filepath.Join(ociDir, "index.json"), updated, 0o644)

	src, err := OpenSource(ociDir)
	if err != nil {
		t.Fatalf("OpenSource: %v", err)
	}
	defer src.Close()
	if _, err := src.ImageForPlatform(Platform{OS: "linux", Architecture: "amd64"}); err != nil {
		t.Fatalf("ImageForPlatform multi-tag unplatformed: %v", err)
	}
}

func TestConvertImageRejectsManifestDigestMismatch(t *testing.T) {
	layer := archivetest.MakeLayer(t, types.OCILayer, archivetest.Entry{Name: "a.txt", Body: "ok"})
	img := archivetest.MakeImage(t, "linux", "amd64", "", nil, layer)
	ociDir, _ := archivetest.WriteSingleOCITar(t, img, "linux", "amd64", "")

	// Rewrite the manifest blob in place with an extra annotation, leaving the digest in
	// index.json pointing at bytes it no longer describes.
	rawIdx, err := os.ReadFile(filepath.Join(ociDir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx specv1.Index
	if err := json.Unmarshal(rawIdx, &idx); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(ociDir, "blobs", string(idx.Manifests[0].Digest.Algorithm()), idx.Manifests[0].Digest.Encoded())
	rawManifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest specv1.Manifest
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Annotations = map[string]string{"tampered": "yes"}
	tampered, _ := json.Marshal(manifest)
	if err := os.WriteFile(manifestPath, tampered, 0o644); err != nil {
		t.Fatal(err)
	}

	src, err := OpenSource(ociDir)
	if err != nil {
		t.Fatalf("OpenSource: %v", err)
	}
	defer src.Close()
	loaded, err := src.ImageForPlatform(Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatalf("ImageForPlatform: %v", err)
	}

	out := t.TempDir()
	_, err = ConvertImage(
		loaded,
		[]LayerOutputSpec{{MediaType: loaded.Manifest.Layers[0].MediaType, BlobPath: filepath.Join(out, "l0.blob"), MetadataPath: filepath.Join(out, "m0.json")}},
		CompressionOptions{Level: -1},
		filepath.Join(out, "manifest.json"),
		filepath.Join(out, "config.json"),
		filepath.Join(out, "descriptor.json"),
		filepath.Join(out, "digest"),
		Platform{OS: "linux", Architecture: "amd64"},
	)
	if err == nil || !strings.Contains(err.Error(), "manifest digest mismatch") {
		t.Fatalf("ConvertImage(tampered manifest) error = %v, want manifest digest mismatch", err)
	}
}

func TestValidateTarLayers(t *testing.T) {
	erofs := &LoadedImage{Manifest: specv1.Manifest{Layers: []specv1.Descriptor{{MediaType: api.ErofsLayer}}}}
	if err := ValidateTarLayers(erofs); err == nil {
		t.Error("ValidateTarLayers(erofs layer) = nil, want error")
	}
	tarred := &LoadedImage{Manifest: specv1.Manifest{Layers: []specv1.Descriptor{{MediaType: api.TarGzipLayer}, {MediaType: string(types.DockerLayer)}}}}
	if err := ValidateTarLayers(tarred); err != nil {
		t.Errorf("ValidateTarLayers(tar layers) = %v, want nil", err)
	}
	// Classic Docker save archives carry no parsed manifest.
	if err := ValidateTarLayers(&LoadedImage{}); err != nil {
		t.Errorf("ValidateTarLayers(docker save) = %v, want nil", err)
	}
}

func TestContainerdPlatformMatchingAndHermeticConversion(t *testing.T) {
	if !(Platform{OS: "linux", Architecture: "aarch64"}).Matches(Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}) {
		t.Fatal("expected containerd normalization to match linux/aarch64 with linux/arm64/v8")
	}
	if !(Platform{OS: "linux", Architecture: "x86_64"}).Matches(Platform{OS: "linux", Architecture: "amd64", Variant: "v1"}) {
		t.Fatal("expected containerd normalization to match linux/x86_64 with linux/amd64/v1")
	}
	if !(Platform{OS: "linux", Architecture: "arm"}).Matches(Platform{OS: "linux", Architecture: "arm", Variant: "v7"}) {
		t.Fatal("expected containerd normalization to match linux/arm with linux/arm/v7")
	}

	layer := archivetest.MakeLayer(t, types.DockerLayer, archivetest.Entry{Name: "app/bin", Body: strings.Repeat("deterministic-", 512)})
	img := archivetest.MakeImage(t, "linux", "arm64", "v8", nil, layer)
	ociDir, ociTar := archivetest.WriteSingleOCITar(t, img, "linux", "arm64", "v8")

	origProcs := runtime.GOMAXPROCS(0)
	t.Cleanup(func() { runtime.GOMAXPROCS(origProcs) })

	runOnce := func(srcPath string, procs int, jobs string) string {
		runtime.GOMAXPROCS(procs)
		src, err := OpenSource(srcPath)
		if err != nil {
			t.Fatalf("OpenSource(%s): %v", srcPath, err)
		}
		defer src.Close()
		loaded, err := src.ImageForPlatform(Platform{OS: "linux", Architecture: "arm64"})
		if err != nil {
			t.Fatalf("ImageForPlatform: %v", err)
		}
		out := t.TempDir()
		desc, err := ConvertImage(
			loaded,
			[]LayerOutputSpec{{MediaType: api.TarZstdLayer, BlobPath: filepath.Join(out, "l0.tzst"), MetadataPath: filepath.Join(out, "m0.json")}},
			CompressionOptions{Jobs: jobs, Level: 1},
			filepath.Join(out, "manifest.json"),
			filepath.Join(out, "config.json"),
			filepath.Join(out, "descriptor.json"),
			filepath.Join(out, "digest"),
			Platform{OS: "linux", Architecture: "arm64"},
		)
		if err != nil {
			t.Fatalf("ConvertImage: %v", err)
		}
		return desc.Digest.String()
	}

	d1 := runOnce(ociDir, 1, "1")
	d2 := runOnce(ociTar, 4, "4")
	if d1 != d2 {
		t.Fatalf("non-hermetic conversion digest: layout/1-core=%s vs tar/4-core=%s", d1, d2)
	}

	// Simulate Bazel sandbox / sparse_oci_layout where every file in the OCI directory is a symlink.
	symlinkDir := filepath.Join(t.TempDir(), "symlink_oci")
	if err := filepath.Walk(ociDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(ociDir, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(symlinkDir, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		return os.Symlink(p, dst)
	}); err != nil {
		t.Fatalf("constructing symlinked OCI layout: %v", err)
	}
	d3 := runOnce(symlinkDir, 2, "2")
	if d3 != d1 {
		t.Fatalf("symlinked OCI layout digest=%s, want %s", d3, d1)
	}
}
