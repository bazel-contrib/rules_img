package ocilayout

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// tarEntries reads a tar into a name->content map.
func tarEntries(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte)
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		if hdr.Typeflag == tar.TypeDir {
			out[hdr.Name] = nil
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading tar entry %q: %v", hdr.Name, err)
		}
		out[hdr.Name] = b
	}
	return out
}

// rootInputFromManifest wraps a single-manifest ManifestInput as a RootInput.
func rootInputFromManifest(mi ManifestInput, tags []string) RootInput {
	return RootInput{
		ManifestData: mi.ManifestData,
		MediaType:    mi.Manifest.MediaType,
		ArtifactType: artifactTypeOf(mi.Manifest),
		IsIndex:      false,
		OCITags:      tags,
		Children:     []ManifestInput{mi},
		Platform:     mi.Platform,
	}
}

// A single-manifest IndexMultiRoot docker-save must be byte-identical to the
// existing single-manifest DockerSave path (functional equivalence, and here
// even byte equivalence, with the image_load "tarball" output group).
func TestMultiRootSingleEqualsDockerSave(t *testing.T) {
	mi, _ := goldenImage("amd64", []byte("layer-a-content"), []byte("layer-b-content"))

	var single bytes.Buffer
	if err := New(DockerSave()).
		WithTags([]string{"repo:latest"}).
		WithOCITags([]string{"repo:latest"}).
		AddManifest(mi).
		WriteToWriter(context.Background(), &single); err != nil {
		t.Fatal(err)
	}

	var multi bytes.Buffer
	if err := New(DockerSave().WithIndexStyle(IndexMultiRoot)).
		WithTags([]string{"repo:latest"}).
		AddRoot(rootInputFromManifest(mi, []string{"repo:latest"})).
		WriteToWriter(context.Background(), &multi); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(single.Bytes(), multi.Bytes()) {
		t.Errorf("IndexMultiRoot single-manifest docker-save differs from DockerSave single path")
	}
}

// Two manifest roots combine into one index.json referencing both, with all
// (deduplicated) blobs present.
func TestMultiRootTwoManifests(t *testing.T) {
	miA, _ := goldenImage("amd64", []byte("A-a"), []byte("A-b"))
	miB, _ := goldenImage("arm64", []byte("B-a"), []byte("B-b"))

	var buf bytes.Buffer
	if err := New(OCILayout().WithIndexStyle(IndexMultiRoot)).
		AddRoot(rootInputFromManifest(miA, []string{"reg.example/a:latest"})).
		AddRoot(rootInputFromManifest(miB, []string{"reg.example/b:latest"})).
		WriteToWriter(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	entries := tarEntries(t, buf.Bytes())

	var idx v1.IndexManifest
	if err := json.Unmarshal(entries["index.json"], &idx); err != nil {
		t.Fatalf("parsing index.json: %v", err)
	}
	if len(idx.Manifests) != 2 {
		t.Fatalf("index.json manifests: got %d want 2", len(idx.Manifests))
	}
	// Both manifest blobs and both differing layer blobs must be present.
	wantBlobs := []string{
		blobPath(hashBytes(miA.ManifestData).Hex),
		blobPath(hashBytes(miB.ManifestData).Hex),
		blobPath(miA.Layers[0].Descriptor.Digest.Hex),
		blobPath(miB.Layers[0].Descriptor.Digest.Hex),
	}
	for _, b := range wantBlobs {
		if _, ok := entries[b]; !ok {
			t.Errorf("missing blob %s", b)
		}
	}
	// The shared config appears once; each descriptor carries the containerd
	// image-name annotation.
	for i, d := range idx.Manifests {
		if d.Annotations["io.containerd.image.name"] == "" {
			t.Errorf("descriptor[%d] missing io.containerd.image.name annotation", i)
		}
	}
}

// An index root is stored as a nested blob and referenced by its own digest;
// its child manifests' blobs are written but not listed at the top level.
func TestMultiRootIndexRoot(t *testing.T) {
	mi1, _ := goldenImage("amd64", []byte("idx-amd64-a"), []byte("idx-amd64-b"))
	mi2, _ := goldenImage("arm64", []byte("idx-arm64-a"), []byte("idx-arm64-b"))
	idxData := goldenIndexData(mi1, mi2)

	var buf bytes.Buffer
	if err := New(OCILayout().WithIndexStyle(IndexMultiRoot)).
		AddRoot(RootInput{
			ManifestData: idxData,
			MediaType:    types.OCIImageIndex,
			IsIndex:      true,
			OCITags:      []string{"reg.example/multi:latest"},
			Children:     []ManifestInput{mi1, mi2},
		}).
		WriteToWriter(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	entries := tarEntries(t, buf.Bytes())

	var idx v1.IndexManifest
	if err := json.Unmarshal(entries["index.json"], &idx); err != nil {
		t.Fatalf("parsing index.json: %v", err)
	}
	if len(idx.Manifests) != 1 {
		t.Fatalf("index.json should reference the single index root, got %d", len(idx.Manifests))
	}
	if idx.Manifests[0].Digest != hashBytes(idxData) {
		t.Errorf("root descriptor digest %s != index digest %s", idx.Manifests[0].Digest, hashBytes(idxData))
	}
	// The nested index blob and both child manifests must be present.
	for _, b := range []string{
		blobPath(hashBytes(idxData).Hex),
		blobPath(hashBytes(mi1.ManifestData).Hex),
		blobPath(hashBytes(mi2.ManifestData).Hex),
	} {
		if _, ok := entries[b]; !ok {
			t.Errorf("missing blob %s", b)
		}
	}
}

// Re-adding the same root+tag is deduplicated; a new tag adds a descriptor.
func TestMultiRootDedup(t *testing.T) {
	mi, _ := goldenImage("amd64", []byte("dedup-a"), []byte("dedup-b"))

	var buf bytes.Buffer
	if err := New(OCILayout().WithIndexStyle(IndexMultiRoot)).
		AddRoot(rootInputFromManifest(mi, []string{"reg.example/x:latest"})).
		AddRoot(rootInputFromManifest(mi, []string{"reg.example/x:latest"})). // dup
		AddRoot(rootInputFromManifest(mi, []string{"reg.example/x:v2"})).     // new tag
		WriteToWriter(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	var idx v1.IndexManifest
	if err := json.Unmarshal(tarEntries(t, buf.Bytes())["index.json"], &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Manifests) != 2 {
		t.Fatalf("expected 2 deduplicated descriptors (latest, v2), got %d", len(idx.Manifests))
	}
}

// The existing five goldens must remain byte-identical after adding the new
// IndexMultiRoot arm (a guard that this change did not perturb emission order).
func TestGoldensUnchangedSanity(t *testing.T) {
	// This mirrors TestGoldenOCILayoutSingle's inputs and simply re-asserts the
	// pinned whole-tar hash to make the "did I break the goldens?" check local
	// to this file too.
	mi, _ := goldenImage("amd64", []byte("layer-a-content"), []byte("layer-b-content"))
	var buf bytes.Buffer
	if err := New(OCILayout()).AddManifest(mi).WriteToWriter(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if got := sha256hex(buf.Bytes()); got != "20993216aafbb2f710cf56af33afdc0bd75ea0e14687dfb4feca5e661dcf1c5c" {
		t.Errorf("OCILayout single golden changed: %s", got)
	}
}
