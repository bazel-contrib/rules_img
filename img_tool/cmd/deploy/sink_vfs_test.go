package deploy

import (
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

// buildLayoutAndManifest writes a real single-manifest image into an OCI layout
// directory and returns that directory plus a matching deploy manifest with one
// push operation. The VFS resolves blobs from the layout, so this exercises the
// full VFS -> resolveRoot -> sink path with no network I/O.
func buildLayoutAndManifest(t *testing.T, registry, repository string, tags []string) (string, api.DeployManifest) {
	t.Helper()
	layoutDir := t.TempDir()

	src := ocilayout.NewMemBlobSource()
	cfg := []byte(`{"architecture":"amd64","os":"linux"}`)
	layer := []byte("integration-layer-content")
	src.Add(sha256Hash(cfg).Hex, cfg).Add(sha256Hash(layer).Hex, layer)
	m := &registryv1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		Config:        registryv1.Descriptor{MediaType: types.OCIConfigJSON, Digest: sha256Hash(cfg), Size: int64(len(cfg))},
		Layers:        []registryv1.Descriptor{{MediaType: types.OCILayer, Digest: sha256Hash(layer), Size: int64(len(layer))}},
	}
	data, _ := json.Marshal(m)
	mi := ocilayout.ManifestInputFromVFS(src, m, data, nil)
	if err := ocilayout.New(ocilayout.OCILayout()).AddManifest(mi).WriteDir(context.Background(), layoutDir); err != nil {
		t.Fatalf("writing layout: %v", err)
	}

	manifestDigest := sha256Hash(data)
	op := api.PushDeployOperation{
		BaseCommandOperation: api.BaseCommandOperation{
			Command:  "push",
			RootKind: "manifest",
			Root:     api.Descriptor{MediaType: string(types.OCIManifestSchema1), Digest: manifestDigest.String(), Size: int64(len(data))},
			Manifests: []api.ManifestDeployInfo{{
				Descriptor: api.Descriptor{MediaType: string(types.OCIManifestSchema1), Digest: manifestDigest.String(), Size: int64(len(data))},
				Config:     api.Descriptor{MediaType: string(types.OCIConfigJSON), Digest: m.Config.Digest.String(), Size: m.Config.Size},
				LayerBlobs: []api.LayerBlob{{Descriptor: api.Descriptor{MediaType: string(types.OCILayer), Digest: m.Layers[0].Digest.String(), Size: m.Layers[0].Size}}},
			}},
		},
		PushTarget: api.PushTarget{Registry: registry, Repository: repository, Tags: tags},
	}
	raw, _ := json.Marshal(op)
	dm := api.DeployManifest{
		Operations: []json.RawMessage{raw},
		Settings:   api.DeploySettings{PushStrategy: "eager"},
	}
	return layoutDir, dm
}

func TestRouteToSinkOCITarFromVFS(t *testing.T) {
	layoutDir, dm := buildLayoutAndManifest(t, "reg.example", "team/foo", []string{"latest"})
	vfs, err := deployvfs.NewBuilder(dm).WithOCILayout(layoutDir).Build()
	if err != nil {
		t.Fatalf("building VFS: %v", err)
	}
	pushOps, err := dm.PushOperations()
	if err != nil {
		t.Fatal(err)
	}

	tarPath := filepath.Join(t.TempDir(), "out.tar")
	s, err := newSink(sinkOCITar, tarPath)
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

	if len(refs) != 1 || refs[0] != "reg.example/team/foo:latest" {
		t.Errorf("written refs = %v, want [reg.example/team/foo:latest]", refs)
	}
	data, err := os.ReadFile(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	names := tarNames(t, data)
	if !names["index.json"] || !names["oci-layout"] {
		t.Errorf("oci-tar missing marker/index; entries=%v", names)
	}
	// The manifest, config and layer blobs must all be streamed into the tar.
	blobCount := 0
	for n := range names {
		if len(n) > len("blobs/sha256/") && n[:len("blobs/sha256/")] == "blobs/sha256/" {
			blobCount++
		}
	}
	if blobCount != 3 {
		t.Errorf("expected 3 blobs (manifest, config, layer), got %d", blobCount)
	}
}

func TestRouteToSinkDistributionFromVFS(t *testing.T) {
	skipIfColonFilenamesUnsupported(t)
	layoutDir, dm := buildLayoutAndManifest(t, "reg.example", "team/foo", []string{"latest"})
	vfs, err := deployvfs.NewBuilder(dm).WithOCILayout(layoutDir).Build()
	if err != nil {
		t.Fatalf("building VFS: %v", err)
	}
	pushOps, err := dm.PushOperations()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	s, err := newSink(sinkDistribution, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := routeToSink(context.Background(), s, vfs, pushOps, nil, nil, sinkRouteOptions{}); err != nil {
		t.Fatalf("routeToSink: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	repoRoot := filepath.Join(dir, "reg.example", "v2", "team/foo")
	if _, err := os.Stat(filepath.Join(repoRoot, "manifests", "latest")); err != nil {
		t.Errorf("distribution sink missing tag symlink: %v", err)
	}
	var tl struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, "tags", "list"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &tl); err != nil {
		t.Fatal(err)
	}
	if tl.Name != "team/foo" || len(tl.Tags) != 1 || tl.Tags[0] != "latest" {
		t.Errorf("tags/list = %+v", tl)
	}
}

// The --registry / --repository overrides redirect the destination repository.
func TestRouteToSinkOverrides(t *testing.T) {
	skipIfColonFilenamesUnsupported(t)
	layoutDir, dm := buildLayoutAndManifest(t, "reg.example", "team/foo", []string{"latest"})
	vfs, err := deployvfs.NewBuilder(dm).WithOCILayout(layoutDir).Build()
	if err != nil {
		t.Fatal(err)
	}
	pushOps, _ := dm.PushOperations()

	dir := t.TempDir()
	s, _ := newSink(sinkDistribution, dir)
	refs, err := routeToSink(context.Background(), s, vfs, pushOps, nil, nil, sinkRouteOptions{
		overrideRegistry:   "other.example",
		overrideRepository: "bar",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0] != "other.example/bar:latest" {
		t.Errorf("overridden refs = %v", refs)
	}
	if _, err := os.Stat(filepath.Join(dir, "other.example", "v2", "bar", "manifests", "latest")); err != nil {
		t.Errorf("override not applied to distribution layout: %v", err)
	}
}

// TestDeployWithExtrasSink drives the one-shot entry point the CLI calls after
// flag parsing, confirming a --sink override lands the image in the sink with no
// registry/daemon I/O.
func TestDeployWithExtrasSink(t *testing.T) {
	layoutDir, dm := buildLayoutAndManifest(t, "reg.example", "team/foo", []string{"latest"})
	raw, err := json.Marshal(dm)
	if err != nil {
		t.Fatal(err)
	}
	tarPath := filepath.Join(t.TempDir(), "out.tar")
	if err := DeployWithExtras(context.Background(), raw, DeployOptions{
		Sink:       "oci-tar:" + tarPath,
		OCILayouts: []string{layoutDir},
		Jobs:       4,
	}); err != nil {
		t.Fatalf("DeployWithExtras: %v", err)
	}
	data, err := os.ReadFile(tarPath)
	if err != nil {
		t.Fatalf("sink tar not written: %v", err)
	}
	if names := tarNames(t, data); !names["index.json"] {
		t.Errorf("sink tar missing index.json")
	}
}
