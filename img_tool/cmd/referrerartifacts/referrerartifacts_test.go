package referrerartifacts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	specv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/blobstore"
)

const (
	rootDigest  = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	childDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

func testSubjects() map[string]specv1.Descriptor {
	return map[string]specv1.Descriptor{
		rootDigest: {
			MediaType: specv1.MediaTypeImageIndex,
			Digest:    digest.Digest(rootDigest),
			Size:      123,
		},
		childDigest: {
			MediaType: specv1.MediaTypeImageManifest,
			Digest:    digest.Digest(childDigest),
			Size:      456,
			Platform:  &specv1.Platform{OS: "linux", Architecture: "amd64"},
		},
	}
}

func readManifest(t *testing.T, store *blobstore.Store, dgst string) specv1.Manifest {
	t.Helper()
	raw, err := store.ReadSmall(dgst)
	if err != nil {
		t.Fatalf("reading manifest %s: %v", dgst, err)
	}
	var manifest specv1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parsing manifest: %v", err)
	}
	return manifest
}

func TestBuild(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sboms"), 0o755); err != nil {
		t.Fatal(err)
	}
	sbom := []byte(`{"spdxVersion":"SPDX-2.3"}`)
	if err := os.WriteFile(filepath.Join(dir, "sboms", "image.spdx.json"), sbom, 0o644); err != nil {
		t.Fatal(err)
	}
	store := blobstore.New(filepath.Join(t.TempDir(), "blobs"))
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}

	spec := Spec{Artifacts: []Artifact{
		{
			ArtifactType: "application/spdx+json",
			Annotations:  map[string]string{"org.example.source": "sbom-db"},
			Files:        []File{{Path: "sboms/image.spdx.json", MediaType: "application/spdx+json"}},
		},
		{
			Subject:      childDigest,
			ArtifactType: "application/vnd.example.marker",
		},
	}}

	results, err := Build(spec, dir, testSubjects(), rootDigest, store)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}

	// The first artifact defaults to the attachment's manifest, and has the file as its layer.
	sbomManifest := readManifest(t, store, results[0].Digest)
	if results[0].Subject != rootDigest || sbomManifest.Subject == nil || sbomManifest.Subject.Digest != rootDigest {
		t.Errorf("first artifact subject = %v / %v, want %s", results[0].Subject, sbomManifest.Subject, rootDigest)
	}
	if sbomManifest.Subject.MediaType != specv1.MediaTypeImageIndex || sbomManifest.Subject.Size != 123 {
		t.Errorf("subject descriptor = %+v, want the index descriptor", sbomManifest.Subject)
	}
	if sbomManifest.ArtifactType != "application/spdx+json" || sbomManifest.Annotations["org.example.source"] != "sbom-db" {
		t.Errorf("artifactType/annotations = %q/%v", sbomManifest.ArtifactType, sbomManifest.Annotations)
	}
	if sbomManifest.Config.Digest != specv1.DescriptorEmptyJSON.Digest || string(sbomManifest.Config.Data) != "{}" {
		t.Errorf("config = %+v, want the empty JSON descriptor", sbomManifest.Config)
	}
	if len(sbomManifest.Layers) != 1 {
		t.Fatalf("got %d layers, want 1", len(sbomManifest.Layers))
	}
	layer := sbomManifest.Layers[0]
	if layer.Digest != digest.FromBytes(sbom) || layer.Size != int64(len(sbom)) {
		t.Errorf("layer = %+v, want digest/size of the file", layer)
	}
	if layer.Annotations[specv1.AnnotationTitle] != "image.spdx.json" {
		t.Errorf("layer title = %q, want the base name", layer.Annotations[specv1.AnnotationTitle])
	}
	if got, err := store.ReadSmall(layer.Digest.String()); err != nil || string(got) != string(sbom) {
		t.Errorf("layer blob = %q, %v", got, err)
	}
	if got, err := store.ReadSmall(specv1.DescriptorEmptyJSON.Digest.String()); err != nil || string(got) != "{}" {
		t.Errorf("empty config blob = %q, %v", got, err)
	}

	// The subject is a pointer to the child, without the child's platform.
	marker := readManifest(t, store, results[1].Digest)
	if marker.Subject.Digest != childDigest || marker.Subject.Platform != nil {
		t.Errorf("second artifact subject = %+v, want the bare child descriptor", marker.Subject)
	}
	// An artifact without files gets the empty descriptor as its only layer.
	if len(marker.Layers) != 1 || marker.Layers[0].Digest != specv1.DescriptorEmptyJSON.Digest {
		t.Errorf("layers = %+v, want a single empty descriptor", marker.Layers)
	}

	// Rebuilding produces the same digests.
	again, err := Build(spec, dir, testSubjects(), rootDigest, store)
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Digest != results[0].Digest || again[1].Digest != results[1].Digest {
		t.Errorf("rebuild changed digests: %v vs %v", again, results)
	}
}

func TestBuildErrors(t *testing.T) {
	dir := t.TempDir()
	store := blobstore.New(filepath.Join(t.TempDir(), "blobs"))
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		artifact Artifact
		want     string
	}{
		{
			name:     "missing artifactType",
			artifact: Artifact{},
			want:     "artifactType is required",
		},
		{
			name:     "subject outside the tree",
			artifact: Artifact{ArtifactType: "a/b", Subject: "sha256:2222222222222222222222222222222222222222222222222222222222222222"},
			want:     "neither the pulled manifest nor one of its children",
		},
		{
			name:     "path escapes the spec directory",
			artifact: Artifact{ArtifactType: "a/b", Files: []File{{Path: "../secret", MediaType: "a/b"}}},
			want:     "must stay within the directory of the spec",
		},
		{
			name:     "absolute path",
			artifact: Artifact{ArtifactType: "a/b", Files: []File{{Path: "/etc/passwd", MediaType: "a/b"}}},
			want:     "must be relative to the spec",
		},
		{
			name:     "missing layer media type",
			artifact: Artifact{ArtifactType: "a/b", Files: []File{{Path: "x"}}},
			want:     "mediaType is required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Build(Spec{Artifacts: []Artifact{tc.artifact}}, dir, testSubjects(), rootDigest, store)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestReadSpecRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "spec.json")
	if err := os.WriteFile(specPath, []byte(`{"artifacts": [{"artifact_type": "a/b"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSpec(specPath); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("err = %v, want an unknown field error", err)
	}
}
