package ocilayout

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func TestEditorAddBlobIdempotent(t *testing.T) {
	dir := t.TempDir()
	ed, err := CreateDir(dir, OCILayout())
	if err != nil {
		t.Fatalf("CreateDir: %v", err)
	}

	data := []byte("some blob content")
	digest := hashBytes(data)
	ctx := context.Background()

	if err := ed.AddBlob(ctx, digest, BlobFromBytes(data)); err != nil {
		t.Fatalf("AddBlob: %v", err)
	}
	blobFile := filepath.Join(dir, "blobs/sha256", digest.Hex)
	info1, err := os.Stat(blobFile)
	if err != nil {
		t.Fatalf("blob not written: %v", err)
	}

	// Second add of the same blob must be a no-op (file unchanged, no error
	// even if the source now differs).
	if err := ed.AddBlob(ctx, digest, BlobFromBytes([]byte("DIFFERENT"))); err != nil {
		t.Fatalf("second AddBlob: %v", err)
	}
	info2, err := os.Stat(blobFile)
	if err != nil {
		t.Fatalf("blob vanished: %v", err)
	}
	if !info1.ModTime().Equal(info2.ModTime()) || info1.Size() != info2.Size() {
		t.Errorf("blob was rewritten on idempotent AddBlob")
	}
	got, _ := os.ReadFile(blobFile)
	if string(got) != string(data) {
		t.Errorf("blob content changed: %q", got)
	}
}

func TestEditorAddManifestUnionAndDedup(t *testing.T) {
	manifest, manifestData, _, _, _ := makeTestManifest()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	os.WriteFile(configPath, []byte(`{"architecture":"amd64","os":"linux"}`), 0o644)
	layerPath := filepath.Join(dir, "layer")
	os.WriteFile(layerPath, []byte("fake layer content"), 0o644)

	layout := filepath.Join(dir, "layout")
	ed, err := CreateDir(layout, OCILayout())
	if err != nil {
		t.Fatalf("CreateDir: %v", err)
	}
	ctx := context.Background()

	mi := ManifestInput{Manifest: manifest, ManifestData: manifestData, Config: BlobFromPath(configPath)}
	mi.Layers = []LayerInput{{Descriptor: manifest.Layers[0], Blob: BlobFromPath(layerPath), Present: true}}

	if err := ed.AddManifest(ctx, mi, "repo:v1"); err != nil {
		t.Fatalf("AddManifest: %v", err)
	}
	// Re-add identical manifest+tag: index must not grow.
	if err := ed.AddManifest(ctx, mi, "repo:v1"); err != nil {
		t.Fatalf("AddManifest (re-add): %v", err)
	}
	// Add same manifest with a different tag: index grows by one.
	if err := ed.AddManifest(ctx, mi, "repo:v2"); err != nil {
		t.Fatalf("AddManifest (v2): %v", err)
	}
	if err := ed.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	idxData, err := os.ReadFile(filepath.Join(layout, "index.json"))
	if err != nil {
		t.Fatalf("reading index.json: %v", err)
	}
	var idx v1.IndexManifest
	if err := json.Unmarshal(idxData, &idx); err != nil {
		t.Fatalf("parsing index.json: %v", err)
	}
	if len(idx.Manifests) != 2 {
		t.Fatalf("expected 2 descriptors (v1, v2), got %d", len(idx.Manifests))
	}
	manifestHex := hashBytes(manifestData).Hex
	if _, err := os.Stat(filepath.Join(layout, "blobs/sha256", manifestHex)); err != nil {
		t.Errorf("manifest blob missing: %v", err)
	}
}

func TestEditorOpenExisting(t *testing.T) {
	// Build a base layout with the Builder, then open and append a manifest.
	manifest, manifestData, _, _, _ := makeTestManifest()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	os.WriteFile(configPath, []byte(`{"architecture":"amd64","os":"linux"}`), 0o644)
	layerPath := filepath.Join(dir, "layer")
	os.WriteFile(layerPath, []byte("fake layer content"), 0o644)
	layout := filepath.Join(dir, "layout")

	base := ManifestInput{Manifest: manifest, ManifestData: manifestData, Config: BlobFromPath(configPath)}
	base.Layers = []LayerInput{{Descriptor: manifest.Layers[0], Blob: BlobFromPath(layerPath), Present: true}}
	if err := New(OCILayout()).AddManifest(base).WriteDir(context.Background(), layout); err != nil {
		t.Fatalf("WriteDir: %v", err)
	}

	ed, err := OpenDir(layout)
	if err != nil {
		t.Fatalf("OpenDir: %v", err)
	}
	// The base index has one (clean) descriptor.
	if len(ed.index.Manifests) != 1 {
		t.Fatalf("expected 1 existing descriptor, got %d", len(ed.index.Manifests))
	}

	// Append a second, distinct manifest.
	cfg2 := []byte(`{"architecture":"arm64","os":"linux"}`)
	cfg2Path := filepath.Join(dir, "config2")
	os.WriteFile(cfg2Path, cfg2, 0o644)
	m2 := &v1.Manifest{
		SchemaVersion: 2, MediaType: types.OCIManifestSchema1,
		Config: v1.Descriptor{MediaType: types.OCIConfigJSON, Digest: hashBytes(cfg2), Size: int64(len(cfg2))},
	}
	m2d, _ := json.Marshal(m2)
	mi2 := ManifestInput{Manifest: m2, ManifestData: m2d, Config: BlobFromPath(cfg2Path)}
	if err := ed.AddManifest(context.Background(), mi2, "repo:arm64"); err != nil {
		t.Fatalf("AddManifest: %v", err)
	}
	if err := ed.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	idxData, _ := os.ReadFile(filepath.Join(layout, "index.json"))
	var idx v1.IndexManifest
	json.Unmarshal(idxData, &idx)
	if len(idx.Manifests) != 2 {
		t.Fatalf("expected 2 descriptors after append, got %d", len(idx.Manifests))
	}
}

func TestEditorRejectsAddManifestOnSparse(t *testing.T) {
	dir := t.TempDir()
	layout := filepath.Join(dir, "sparse")
	if _, err := CreateDir(layout, SparseOCILayout()); err != nil {
		t.Fatalf("CreateDir sparse: %v", err)
	}
	// A sparse layout written by CreateDir still writes index.json (empty),
	// so to exercise the rejection we simulate a root.descriptor.json-only
	// layout by removing index.json.
	os.Remove(filepath.Join(layout, "index.json"))

	ed, err := OpenDir(layout)
	if err != nil {
		t.Fatalf("OpenDir: %v", err)
	}
	manifest, manifestData, _, _, _ := makeTestManifest()
	mi := ManifestInput{Manifest: manifest, ManifestData: manifestData, Config: BlobFromBytes([]byte("x"))}
	if err := ed.AddManifest(context.Background(), mi, "repo:v1"); err == nil {
		t.Error("expected AddManifest to fail without an index.json root")
	}
}
