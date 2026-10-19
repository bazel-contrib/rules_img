package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/deployvfs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/ocilayout"
)

// TestSinksManifestWithoutMediaType routes a manifest that omits its top-level
// mediaType (image_manifest's omit_media_type) to each layout sink. The bytes
// must be stored unchanged, and the index.json entry must still say it is an
// image manifest.
func TestSinksManifestWithoutMediaType(t *testing.T) {
	src := ocilayout.NewMemBlobSource()
	cfg := []byte(`{"name":"chart","version":"1.0.0"}`)
	layer := []byte("chart-content")
	src.Add(sha256Hash(cfg).Hex, cfg).Add(sha256Hash(layer).Hex, layer)
	m := &registryv1.Manifest{
		SchemaVersion: 2,
		Config:        registryv1.Descriptor{MediaType: "application/vnd.cncf.helm.config.v1+json", Digest: sha256Hash(cfg), Size: int64(len(cfg))},
		Layers:        []registryv1.Descriptor{{MediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip", Digest: sha256Hash(layer), Size: int64(len(layer))}},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"mediaType":"`+string(types.OCIManifestSchema1)+`"`)) {
		t.Fatalf("test manifest has a top-level mediaType: %s", data)
	}
	layoutDir := t.TempDir()
	if err := ocilayout.New(ocilayout.OCILayout()).AddManifest(ocilayout.ManifestInputFromVFS(src, m, data, nil)).WriteDir(context.Background(), layoutDir); err != nil {
		t.Fatalf("writing layout: %v", err)
	}

	// The request descriptors are what deploy-metadata writes for this manifest.
	manifestDigest := sha256Hash(data)
	descriptor := api.Descriptor{MediaType: string(types.OCIManifestSchema1), Digest: manifestDigest.String(), Size: int64(len(data))}
	op := api.PushDeployOperation{
		BaseCommandOperation: api.BaseCommandOperation{
			Command:  "push",
			RootKind: "manifest",
			Root:     descriptor,
			Manifests: []api.ManifestDeployInfo{{
				Descriptor: descriptor,
				Config:     api.Descriptor{MediaType: string(m.Config.MediaType), Digest: m.Config.Digest.String(), Size: m.Config.Size},
				LayerBlobs: []api.LayerBlob{{Descriptor: api.Descriptor{MediaType: string(m.Layers[0].MediaType), Digest: m.Layers[0].Digest.String(), Size: m.Layers[0].Size}}},
			}},
		},
		PushTarget: api.PushTarget{Registry: "reg.example", Repository: "charts/chart", Tags: []string{"1.0.0"}},
	}
	raw, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	dm := api.DeployManifest{Operations: []json.RawMessage{raw}, Settings: api.DeploySettings{PushStrategy: "eager"}}

	route := func(t *testing.T, kind sinkKind, path string) {
		t.Helper()
		vfs, err := deployvfs.NewBuilder(dm).WithOCILayout(layoutDir).Build()
		if err != nil {
			t.Fatalf("building VFS: %v", err)
		}
		pushOps, err := dm.PushOperations()
		if err != nil {
			t.Fatal(err)
		}
		s, err := newSink(kind, path)
		if err != nil {
			t.Fatal(err)
		}
		refs, err := routeToSink(context.Background(), s, vfs, pushOps, nil, nil, sinkRouteOptions{})
		if err != nil {
			t.Fatalf("routeToSink: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if len(refs) != 1 || refs[0] != "reg.example/charts/chart:1.0.0" {
			t.Errorf("written refs = %v, want [reg.example/charts/chart:1.0.0]", refs)
		}
	}
	check := func(t *testing.T, indexJSON, storedManifest []byte) {
		t.Helper()
		if !bytes.Equal(storedManifest, data) {
			t.Errorf("stored manifest differs from the built one:\ngot:  %s\nwant: %s", storedManifest, data)
		}
		var idx registryv1.IndexManifest
		if err := json.Unmarshal(indexJSON, &idx); err != nil {
			t.Fatalf("parsing index.json: %v", err)
		}
		if len(idx.Manifests) == 0 {
			t.Fatal("index.json has no entries")
		}
		for _, d := range idx.Manifests {
			if d.MediaType != types.OCIManifestSchema1 || d.Digest != manifestDigest {
				t.Errorf("index.json entry = %s %s, want %s %s", d.MediaType, d.Digest, types.OCIManifestSchema1, manifestDigest)
			}
		}
	}

	t.Run("oci-tar", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "out.tar")
		route(t, sinkOCITar, path)
		tarData, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		check(t, tarFile(t, tarData, "index.json"), tarFile(t, tarData, "blobs/sha256/"+manifestDigest.Hex))
	})

	t.Run("oci", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "layout")
		route(t, sinkOCIDir, dir)
		indexJSON, err := os.ReadFile(filepath.Join(dir, "index.json"))
		if err != nil {
			t.Fatal(err)
		}
		storedManifest, err := os.ReadFile(filepath.Join(dir, "blobs", "sha256", manifestDigest.Hex))
		if err != nil {
			t.Fatal(err)
		}
		check(t, indexJSON, storedManifest)
	})

	t.Run("docker-save", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "out.tar")
		route(t, sinkDockerSave, path)
		tarData, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		check(t, tarFile(t, tarData, "index.json"), tarFile(t, tarData, "blobs/sha256/"+manifestDigest.Hex))
	})

	t.Run("distribution", func(t *testing.T) {
		skipIfColonFilenamesUnsupported(t)
		dir := t.TempDir()
		route(t, sinkDistribution, dir)
		// A distribution layout has no index.json; the manifest is stored under
		// its digest and linked from its tag.
		for _, name := range []string{manifestDigest.String(), "1.0.0"} {
			stored, err := os.ReadFile(filepath.Join(dir, "reg.example", "v2", "charts/chart", "manifests", name))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(stored, data) {
				t.Errorf("manifests/%s differs from the built manifest:\ngot:  %s\nwant: %s", name, stored, data)
			}
		}
	})
}
