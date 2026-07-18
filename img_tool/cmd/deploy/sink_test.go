package deploy

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/ocilayout"
)

// skipIfColonFilenamesUnsupported skips tests that materialize the distribution
// layout, whose paths contain a ':' (blobs/sha256:<hex>) that Windows disallows.
func skipIfColonFilenamesUnsupported(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("distribution layout uses ':' in filenames, unsupported on Windows")
	}
}

func TestParseSink(t *testing.T) {
	cases := []struct {
		spec     string
		wantKind sinkKind
		wantPath string
		wantErr  bool
	}{
		{"oci-tar:/tmp/out.tar", sinkOCITar, "/tmp/out.tar", false},
		{"docker-save:/tmp/out.tar", sinkDockerSave, "/tmp/out.tar", false},
		{"oci:/tmp/dir", sinkOCIDir, "/tmp/dir", false},
		{"distribution:/tmp/reg", sinkDistribution, "/tmp/reg", false},
		{"distribution-flat:/tmp/reg", sinkDistributionFlat, "/tmp/reg", false},
		{"oci-tar:", sinkNone, "", true},
		{"bogus:/tmp/x", sinkNone, "", true},
		{"noseparator", sinkNone, "", true},
	}
	for _, tc := range cases {
		kind, path, err := parseSink(tc.spec)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseSink(%q) err=%v wantErr=%v", tc.spec, err, tc.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if kind != tc.wantKind || path != tc.wantPath {
			t.Errorf("parseSink(%q) = (%d, %q), want (%d, %q)", tc.spec, kind, path, tc.wantKind, tc.wantPath)
		}
	}
}

func TestSinkGlobalOnly(t *testing.T) {
	global := []sinkKind{sinkOCIDir, sinkDistribution, sinkDistributionFlat}
	perRequest := []sinkKind{sinkOCITar, sinkDockerSave}
	for _, k := range global {
		if !k.globalOnly() {
			t.Errorf("kind %d should be global-only", k)
		}
	}
	for _, k := range perRequest {
		if k.globalOnly() {
			t.Errorf("kind %d should be allowed per-request", k)
		}
	}
}

func TestParseDistributionRef(t *testing.T) {
	ref, tag, err := parseDistributionRef("reg.example/team/foo:v1")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Registry != "reg.example" || ref.Name != "team/foo" || tag != "v1" {
		t.Errorf("parseDistributionRef = (%+v, %q)", ref, tag)
	}
}

func sha256Hash(b []byte) registryv1.Hash {
	h, _, _ := registryv1.SHA256(bytes.NewReader(b))
	return h
}

// testRoot builds a resolvedRoot for a deterministic single-manifest image
// backed by in-memory blobs.
func testRoot(seed string) resolvedRoot {
	src := ocilayout.NewMemBlobSource()
	cfg := []byte(`{"architecture":"amd64","os":"linux"}`)
	layer := []byte(seed + "-layer")
	src.Add(sha256Hash(cfg).Hex, cfg).Add(sha256Hash(layer).Hex, layer)
	m := &registryv1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		Config:        registryv1.Descriptor{MediaType: types.OCIConfigJSON, Digest: sha256Hash(cfg), Size: int64(len(cfg))},
		Layers:        []registryv1.Descriptor{{MediaType: types.OCILayer, Digest: sha256Hash(layer), Size: int64(len(layer))}},
	}
	data, _ := json.Marshal(m)
	return resolvedRoot{
		RootData:  data,
		MediaType: types.OCIManifestSchema1,
		IsIndex:   false,
		Children:  []ocilayout.ManifestInput{ocilayout.ManifestInputFromVFS(src, m, data, nil)},
	}
}

func tarNames(t *testing.T, data []byte) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names[hdr.Name] = true
		if hdr.Typeflag != tar.TypeDir {
			io.Copy(io.Discard, tr)
		}
	}
	return names
}

func TestOCITarSink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.tar")
	s, err := newSink(sinkOCITar, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddImage(context.Background(), sinkImage{Refs: []string{"reg.example/foo:latest"}, Root: testRoot("a")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	names := tarNames(t, data)
	for _, want := range []string{"oci-layout", "index.json"} {
		if !names[want] {
			t.Errorf("oci-tar missing %s", want)
		}
	}
	if names["manifest.json"] {
		t.Errorf("oci-tar must not contain a Docker manifest.json")
	}
}

func TestDockerSaveSink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.tar")
	s, err := newSink(sinkDockerSave, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddImage(context.Background(), sinkImage{Refs: []string{"reg.example/foo:latest"}, Root: testRoot("a")}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddImage(context.Background(), sinkImage{Refs: []string{"reg.example/bar:latest"}, Root: testRoot("b")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	names := tarNames(t, data)
	if !names["manifest.json"] {
		t.Errorf("docker-save missing manifest.json")
	}
}

func TestOCIDirSinkIncremental(t *testing.T) {
	dir := t.TempDir()
	s, err := newSink(sinkOCIDir, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddImage(context.Background(), sinkImage{Refs: []string{"reg.example/foo:latest"}, Root: testRoot("a")}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddImage(context.Background(), sinkImage{Refs: []string{"reg.example/bar:latest"}, Root: testRoot("b")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx registryv1.IndexManifest
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Manifests) != 2 {
		t.Fatalf("index.json manifests: got %d want 2", len(idx.Manifests))
	}
	for i, d := range idx.Manifests {
		if d.Annotations["io.containerd.image.name"] == "" {
			t.Errorf("descriptor[%d] missing image-name annotation", i)
		}
	}

	// Re-opening the existing directory and adding a tag must merge, not clobber.
	s2, err := newSink(sinkOCIDir, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.AddImage(context.Background(), sinkImage{Refs: []string{"reg.example/foo:v2"}, Root: testRoot("a")}); err != nil {
		t.Fatal(err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "index.json"))
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Manifests) != 3 {
		t.Fatalf("after merge: got %d descriptors want 3", len(idx.Manifests))
	}
}

func TestDistributionSinkRoutes(t *testing.T) {
	skipIfColonFilenamesUnsupported(t)
	dir := t.TempDir()
	s, err := newSink(sinkDistribution, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddImage(context.Background(), sinkImage{
		Refs:       []string{"reg.example/team/foo:latest"},
		Registry:   "reg.example",
		Repository: "team/foo",
		Root:       testRoot("a"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "reg.example", "v2", "team/foo", "tags", "list")); err != nil {
		t.Errorf("distribution sink did not write tags/list: %v", err)
	}
}

func TestDeduplicateAndSortTags(t *testing.T) {
	got := deduplicateAndSortTags([]string{"b", "a", "", "b", "c"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}
