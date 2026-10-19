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

// manifestWithoutMediaType is makeTestManifest's manifest with the top-level
// mediaType omitted, as image_manifest writes it with omit_media_type.
func manifestWithoutMediaType(t *testing.T) ManifestInput {
	t.Helper()
	manifest, _, src, _, _ := makeTestManifest()
	manifest.MediaType = ""
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["mediaType"]; ok {
		t.Fatalf("test manifest still has a mediaType: %s", data)
	}
	return manifestInputFromSource(manifest, data, src)
}

func readIndex(t *testing.T, path string) v1.IndexManifest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var idx v1.IndexManifest
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return idx
}

func checkManifestDescriptor(t *testing.T, where string, d v1.Descriptor, mi ManifestInput) {
	t.Helper()
	if d.MediaType != types.OCIManifestSchema1 {
		t.Errorf("%s: mediaType = %q, want %s", where, d.MediaType, types.OCIManifestSchema1)
	}
	if d.Digest != hashBytes(mi.ManifestData) {
		t.Errorf("%s: digest = %s, want %s", where, d.Digest, hashBytes(mi.ManifestData))
	}
}

func TestManifestWithoutMediaTypeIndexStyles(t *testing.T) {
	ctx := context.Background()
	mi := manifestWithoutMediaType(t)

	t.Run("clean", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "layout")
		if err := New(OCILayout()).AddManifest(mi).WriteDir(ctx, out); err != nil {
			t.Fatalf("WriteDir: %v", err)
		}
		idx := readIndex(t, filepath.Join(out, "index.json"))
		if len(idx.Manifests) != 1 {
			t.Fatalf("got %d index entries, want 1", len(idx.Manifests))
		}
		checkManifestDescriptor(t, "index.json", idx.Manifests[0], mi)
	})

	t.Run("annotated", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "layout")
		if err := New(DockerSave()).WithTags([]string{"repo:v1"}).WithOCITags([]string{"repo:v1"}).AddManifest(mi).WriteDir(ctx, out); err != nil {
			t.Fatalf("WriteDir: %v", err)
		}
		idx := readIndex(t, filepath.Join(out, "index.json"))
		if len(idx.Manifests) == 0 {
			t.Fatal("index.json has no entries")
		}
		for _, d := range idx.Manifests {
			checkManifestDescriptor(t, "index.json", d, mi)
		}
	})

	t.Run("root descriptor", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "sparse")
		if err := New(SparseOCILayout()).AddManifest(mi).WriteDir(ctx, out); err != nil {
			t.Fatalf("WriteDir: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(out, "root.descriptor.json"))
		if err != nil {
			t.Fatal(err)
		}
		var d v1.Descriptor
		if err := json.Unmarshal(data, &d); err != nil {
			t.Fatal(err)
		}
		checkManifestDescriptor(t, "root.descriptor.json", d, mi)
	})

	t.Run("editor", func(t *testing.T) {
		layout := filepath.Join(t.TempDir(), "layout")
		ed, err := CreateDir(layout, OCILayout())
		if err != nil {
			t.Fatalf("CreateDir: %v", err)
		}
		if err := ed.AddManifest(ctx, mi, "repo:v1"); err != nil {
			t.Fatalf("AddManifest: %v", err)
		}
		if err := ed.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		idx := readIndex(t, filepath.Join(layout, "index.json"))
		if len(idx.Manifests) != 1 {
			t.Fatalf("got %d index entries, want 1", len(idx.Manifests))
		}
		checkManifestDescriptor(t, "index.json", idx.Manifests[0], mi)
	})
}

func TestDistributionReferrerWithoutMediaType(t *testing.T) {
	skipIfColonFilenamesUnsupported(t)
	dir := t.TempDir()
	ref := DistributionRef{Registry: "reg.example", Name: "foo"}

	subject := v1.Hash{Algorithm: "sha256", Hex: "1111111111111111111111111111111111111111111111111111111111111111"}
	cfg := []byte(`{}`)
	src := NewMemBlobSource().Add(hashBytes(cfg).Hex, cfg)
	m := &v1.Manifest{
		SchemaVersion: 2,
		Config:        v1.Descriptor{MediaType: "application/vnd.example.sig", Digest: hashBytes(cfg), Size: int64(len(cfg))},
		Subject:       &v1.Descriptor{MediaType: types.OCIManifestSchema1, Digest: subject, Size: 100},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	sig := ManifestInput{Manifest: m, ManifestData: data, Config: BlobFromSource(src, m.Config.Digest.Hex, m.Config.Size)}

	w := NewDistributionWriter(dir, false)
	if err := w.AddImage(context.Background(), DistributionImage{Ref: ref, RootData: data, Children: []ManifestInput{sig}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	idx := readIndex(t, filepath.Join(distRepoRoot(dir, false, ref), "referrers", subject.String()))
	if len(idx.Manifests) != 1 {
		t.Fatalf("got %d referrers, want 1", len(idx.Manifests))
	}
	checkManifestDescriptor(t, "referrers", idx.Manifests[0], sig)
}
