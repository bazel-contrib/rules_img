package deployvfs

import (
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

const testRegistryDigest = "sha256:4444444444444444444444444444444444444444444444444444444444444444"

func layerDesc() api.Descriptor {
	return api.Descriptor{
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Digest:    testRegistryDigest,
		Size:      123,
	}
}

// TestLayerFromRegistryPerLayerSources verifies the source-selection routing of
// layerFromRegistry (the blob Opener itself performs network access and is not
// invoked here).
func TestLayerFromRegistryPerLayerSources(t *testing.T) {
	b := NewBuilder(api.DeployManifest{})
	sources := []api.LayerSource{
		{Registry: "index.docker.io", Repository: "library/ubuntu"},
		{Registry: "mirror.example.com", Repository: "library/ubuntu"},
	}

	entry, err := b.layerFromRegistry(sources, layerDesc())
	if err != nil {
		t.Fatalf("expected per-layer sources to resolve, got error: %v", err)
	}
	if entry.Location != "registry" {
		t.Errorf("entry.Location = %q, want %q", entry.Location, "registry")
	}
}

// TestLayerFromRegistryUnconfigured verifies that a layer with no upstream
// sources is not resolvable from a registry.
func TestLayerFromRegistryUnconfigured(t *testing.T) {
	b := NewBuilder(api.DeployManifest{})
	if _, err := b.layerFromRegistry(nil, layerDesc()); err == nil {
		t.Error("expected error when no sources are configured")
	} else if bse, ok := err.(*BlobSourceError); !ok || bse.Kind != BlobSourceUnconfigured {
		t.Errorf("expected BlobSourceUnconfigured, got %v", err)
	}
}
