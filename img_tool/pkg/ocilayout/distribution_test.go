package ocilayout

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// skipIfColonFilenamesUnsupported skips tests that materialize the distribution
// layout on disk. Its paths contain a ':' (blobs/sha256:<hex>,
// manifests/sha256:<hex>) to match the OCI distribution-spec URLs, and ':' is a
// reserved character in Windows filenames.
func skipIfColonFilenamesUnsupported(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("distribution layout uses ':' in filenames, unsupported on Windows")
	}
}

func distRepoRoot(dir string, flat bool, ref DistributionRef) string {
	if flat {
		return filepath.Join(dir, "v2", ref.Name)
	}
	return filepath.Join(dir, ref.Registry, "v2", ref.Name)
}

// distImage builds a DistributionImage from a deterministic single-manifest
// image backed by in-memory blobs.
func distImage(ref DistributionRef, seed string, tags []string) DistributionImage {
	mi, _ := goldenImage("amd64", []byte(seed+"-a"), []byte(seed+"-b"))
	return DistributionImage{
		Ref:      ref,
		RootData: mi.ManifestData,
		Children: []ManifestInput{mi},
		Tags:     tags,
	}
}

func TestDistributionFreshManifest(t *testing.T) {
	skipIfColonFilenamesUnsupported(t)
	dir := t.TempDir()
	ref := DistributionRef{Registry: "reg.example", Name: "team/foo"}
	img := distImage(ref, "fresh", []string{"latest"})

	w := NewDistributionWriter(dir, false)
	if err := w.AddImage(context.Background(), img); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	repoRoot := distRepoRoot(dir, false, ref)
	manifestHex := hashBytes(img.RootData).Hex

	// Manifest stored once by digest, with the exact bytes.
	mpath := filepath.Join(repoRoot, "manifests", "sha256:"+manifestHex)
	got, err := os.ReadFile(mpath)
	if err != nil {
		t.Fatalf("reading manifest: %v", err)
	}
	if string(got) != string(img.RootData) {
		t.Errorf("manifest content mismatch")
	}

	// Tag is a same-dir relative symlink to the digest file.
	target, err := os.Readlink(filepath.Join(repoRoot, "manifests", "latest"))
	if err != nil {
		t.Fatalf("reading tag symlink: %v", err)
	}
	if target != "sha256:"+manifestHex {
		t.Errorf("tag symlink target: got %q want %q", target, "sha256:"+manifestHex)
	}

	// Config + layer blobs present under blobs/.
	cfgHex := img.Children[0].Manifest.Config.Digest.Hex
	if _, err := os.Stat(filepath.Join(repoRoot, "blobs", "sha256:"+cfgHex)); err != nil {
		t.Errorf("missing config blob: %v", err)
	}
	for _, l := range img.Children[0].Layers {
		if _, err := os.Stat(filepath.Join(repoRoot, "blobs", "sha256:"+l.Descriptor.Digest.Hex)); err != nil {
			t.Errorf("missing layer blob %s: %v", l.Descriptor.Digest.Hex, err)
		}
	}

	// tags/list generated.
	var tl struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, "tags", "list"))
	if err != nil {
		t.Fatalf("reading tags/list: %v", err)
	}
	if err := json.Unmarshal(data, &tl); err != nil {
		t.Fatal(err)
	}
	if tl.Name != ref.Name || len(tl.Tags) != 1 || tl.Tags[0] != "latest" {
		t.Errorf("tags/list = %+v, want name=%s tags=[latest]", tl, ref.Name)
	}
}

func TestDistributionFlatStripsRegistry(t *testing.T) {
	skipIfColonFilenamesUnsupported(t)
	dir := t.TempDir()
	ref := DistributionRef{Registry: "reg.example", Name: "foo"}
	w := NewDistributionWriter(dir, true)
	if err := w.AddImage(context.Background(), distImage(ref, "flat", []string{"v1"})); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// Flat layout: <dir>/v2/foo, no registry component.
	if _, err := os.Stat(filepath.Join(dir, "v2", "foo", "tags", "list")); err != nil {
		t.Errorf("flat layout missing v2/foo/tags/list: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "reg.example")); err == nil {
		t.Errorf("flat layout should not create a registry directory")
	}
}

// A second session merges tags into the existing tags/list and leaves the
// existing content in place.
func TestDistributionIncrementalMerge(t *testing.T) {
	skipIfColonFilenamesUnsupported(t)
	dir := t.TempDir()
	ref := DistributionRef{Registry: "reg.example", Name: "foo"}

	w1 := NewDistributionWriter(dir, false)
	if err := w1.AddImage(context.Background(), distImage(ref, "merge", []string{"v1"})); err != nil {
		t.Fatal(err)
	}
	if err := w1.Close(); err != nil {
		t.Fatal(err)
	}

	// Second, independent writer over the same directory, new tag on the same image.
	w2 := NewDistributionWriter(dir, false)
	if err := w2.AddImage(context.Background(), distImage(ref, "merge", []string{"v2"})); err != nil {
		t.Fatal(err)
	}
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}

	repoRoot := distRepoRoot(dir, false, ref)
	var tl struct {
		Tags []string `json:"tags"`
	}
	data, _ := os.ReadFile(filepath.Join(repoRoot, "tags", "list"))
	if err := json.Unmarshal(data, &tl); err != nil {
		t.Fatal(err)
	}
	if len(tl.Tags) != 2 || tl.Tags[0] != "v1" || tl.Tags[1] != "v2" {
		t.Errorf("merged tags/list = %v, want sorted [v1 v2]", tl.Tags)
	}
}

// The same blob shared by two repositories is hardlinked (or, on filesystems
// without hardlink support, at least present with identical content).
func TestDistributionBlobHardlink(t *testing.T) {
	skipIfColonFilenamesUnsupported(t)
	dir := t.TempDir()
	refA := DistributionRef{Registry: "reg.example", Name: "a"}
	refB := DistributionRef{Registry: "reg.example", Name: "b"}
	imgA := distImage(refA, "shared", []string{"latest"})
	// Same content, different repository.
	imgB := imgA
	imgB.Ref = refB

	w := NewDistributionWriter(dir, false)
	if err := w.AddImage(context.Background(), imgA); err != nil {
		t.Fatal(err)
	}
	if err := w.AddImage(context.Background(), imgB); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	layerHex := imgA.Children[0].Layers[0].Descriptor.Digest.Hex
	pathA := filepath.Join(distRepoRoot(dir, false, refA), "blobs", "sha256:"+layerHex)
	pathB := filepath.Join(distRepoRoot(dir, false, refB), "blobs", "sha256:"+layerHex)
	statA, err := os.Stat(pathA)
	if err != nil {
		t.Fatal(err)
	}
	statB, err := os.Stat(pathB)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(statA, statB) {
		// Fallback: content must at least be identical.
		a, _ := os.ReadFile(pathA)
		b, _ := os.ReadFile(pathB)
		if string(a) != string(b) {
			t.Errorf("shared blob differs between repos and is not hardlinked")
		}
	}
}

// A manifest with a subject is recorded in referrers/sha256:<subject>.
func TestDistributionReferrers(t *testing.T) {
	skipIfColonFilenamesUnsupported(t)
	dir := t.TempDir()
	ref := DistributionRef{Registry: "reg.example", Name: "foo"}

	subject := v1.Hash{Algorithm: "sha256", Hex: "1111111111111111111111111111111111111111111111111111111111111111"}
	cfg := []byte(`{}`)
	src := NewMemBlobSource().Add(hashBytes(cfg).Hex, cfg)
	m := &v1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		Config:        v1.Descriptor{MediaType: "application/vnd.example.sig", Digest: hashBytes(cfg), Size: int64(len(cfg))},
		Subject:       &v1.Descriptor{MediaType: types.OCIManifestSchema1, Digest: subject, Size: 100},
	}
	data, _ := json.Marshal(m)
	sig := ManifestInput{Manifest: m, ManifestData: data, Config: BlobFromSource(src, m.Config.Digest.Hex, m.Config.Size)}

	w := NewDistributionWriter(dir, false)
	if err := w.AddImage(context.Background(), DistributionImage{
		Ref:      ref,
		RootData: data,
		Children: []ManifestInput{sig},
		Tags:     nil,
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	repoRoot := distRepoRoot(dir, false, ref)
	refData, err := os.ReadFile(filepath.Join(repoRoot, "referrers", subject.String()))
	if err != nil {
		t.Fatalf("reading referrers file: %v", err)
	}
	var idx v1.IndexManifest
	if err := json.Unmarshal(refData, &idx); err != nil {
		t.Fatal(err)
	}
	if idx.MediaType != types.OCIImageIndex {
		t.Errorf("referrers index media type = %s", idx.MediaType)
	}
	if len(idx.Manifests) != 1 {
		t.Fatalf("referrers manifests: got %d want 1", len(idx.Manifests))
	}
	d := idx.Manifests[0]
	if d.Digest != hashBytes(data) {
		t.Errorf("referrer digest = %s want %s", d.Digest, hashBytes(data))
	}
	if d.ArtifactType != "application/vnd.example.sig" {
		t.Errorf("referrer artifactType = %q want application/vnd.example.sig", d.ArtifactType)
	}
}

// Adding two tags to one digest stores the manifest once and creates both tag
// symlinks.
func TestDistributionContentOnceTwoTags(t *testing.T) {
	skipIfColonFilenamesUnsupported(t)
	dir := t.TempDir()
	ref := DistributionRef{Registry: "reg.example", Name: "foo"}
	img := distImage(ref, "twotags", []string{"a", "b"})

	w := NewDistributionWriter(dir, false)
	if err := w.AddImage(context.Background(), img); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	repoRoot := distRepoRoot(dir, false, ref)
	entries, err := os.ReadDir(filepath.Join(repoRoot, "manifests"))
	if err != nil {
		t.Fatal(err)
	}
	digestFiles, tagLinks := 0, 0
	for _, e := range entries {
		if e.Name() == "sha256:"+hashBytes(img.RootData).Hex {
			digestFiles++
		} else {
			tagLinks++
		}
	}
	if digestFiles != 1 {
		t.Errorf("expected the manifest stored once, found %d digest files", digestFiles)
	}
	if tagLinks != 2 {
		t.Errorf("expected 2 tag symlinks, found %d", tagLinks)
	}
}
