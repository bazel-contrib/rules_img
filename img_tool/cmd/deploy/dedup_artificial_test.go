package deploy

import (
	"bytes"
	"encoding/json"
	"testing"

	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	registrytypes "github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

// These tests cover the manifests deduplicated_push_content=blobs_and_artificial_manifests
// writes to a home repository. What they are for -- making a registry share the blob
// they reference -- is covered end to end in dedup_registry_test.go; here it is what
// they contain.

// ociLayerDescriptor is the blob an artificial manifest is written for.
func ociLayerDescriptor() api.Descriptor {
	return api.Descriptor{
		MediaType: string(registrytypes.OCILayer),
		Digest:    sharedLayerDigest,
		Size:      4096,
	}
}

// TestArtificialManifestReferencesTheBlob covers the shape of the manifest and its
// config: a real single-layer image manifest whose config records the layer's
// uncompressed digest.
func TestArtificialManifestReferencesTheBlob(t *testing.T) {
	const diffID = "sha256:9999999999999999999999999999999999999999999999999999999999999999"
	layer := ociLayerDescriptor()

	config, manifest, err := artificialManifest(layer, diffID)
	if err != nil {
		t.Fatalf("artificialManifest: %v", err)
	}

	var parsed registryv1.Manifest
	if err := json.Unmarshal(manifest.raw, &parsed); err != nil {
		t.Fatalf("parsing the artificial manifest: %v", err)
	}
	if parsed.SchemaVersion != 2 {
		t.Errorf("schemaVersion = %d, want 2", parsed.SchemaVersion)
	}
	if len(parsed.Layers) != 1 || parsed.Layers[0].Digest.String() != layer.Digest || parsed.Layers[0].Size != layer.Size {
		t.Errorf("layers = %+v, want the blob's own descriptor", parsed.Layers)
	}
	if parsed.Layers[0].MediaType != registrytypes.OCILayer {
		t.Errorf("layer media type = %s, want the blob's own %s", parsed.Layers[0].MediaType, registrytypes.OCILayer)
	}
	if parsed.Config.Digest.String() != config.descriptor.Digest || parsed.Config.Size != config.descriptor.Size {
		t.Errorf("config descriptor = %+v, want the config blob %+v", parsed.Config, config.descriptor)
	}
	if got := parsed.Annotations[artificialManifestAnnotation]; got != layer.Digest {
		t.Errorf("annotation %s = %q, want the blob it was written for", artificialManifestAnnotation, got)
	}

	// The digest the manifest is pushed under is the digest of exactly these bytes.
	if want, _, err := registryv1.SHA256(bytes.NewReader(manifest.raw)); err != nil {
		t.Fatalf("hashing the artificial manifest: %v", err)
	} else if manifest.digest != want {
		t.Errorf("manifest digest = %s, want %s", manifest.digest, want)
	}

	parsedConfig, err := registryv1.ParseConfigFile(bytes.NewReader(config.raw))
	if err != nil {
		t.Fatalf("parsing the artificial config: %v", err)
	}
	if len(parsedConfig.RootFS.DiffIDs) != 1 || parsedConfig.RootFS.DiffIDs[0].String() != diffID {
		t.Errorf("config diff ids = %v, want just %s", parsedConfig.RootFS.DiffIDs, diffID)
	}
	if parsedConfig.RootFS.Type != "layers" {
		t.Errorf("config rootfs type = %q, want layers", parsedConfig.RootFS.Type)
	}
	// Not a runnable image, and it says so rather than claiming an architecture.
	if parsedConfig.OS != "unknown" || parsedConfig.Architecture != "unknown" {
		t.Errorf("config platform = %s/%s, want unknown/unknown", parsedConfig.OS, parsedConfig.Architecture)
	}
	if got := config.descriptor.MediaType; got != string(registrytypes.OCIConfigJSON) {
		t.Errorf("config media type = %s, want %s", got, registrytypes.OCIConfigJSON)
	}
}

// TestArtificialManifestIsDeterministic is what makes re-running a deploy free: the
// same blob yields the same manifest, so the registry already has it and
// go-containerregistry's HEAD before the PUT finds it.
func TestArtificialManifestIsDeterministic(t *testing.T) {
	const diffID = "sha256:9999999999999999999999999999999999999999999999999999999999999999"
	first, firstManifest, err := artificialManifest(ociLayerDescriptor(), diffID)
	if err != nil {
		t.Fatalf("artificialManifest: %v", err)
	}
	second, secondManifest, err := artificialManifest(ociLayerDescriptor(), diffID)
	if err != nil {
		t.Fatalf("artificialManifest: %v", err)
	}
	if !bytes.Equal(first.raw, second.raw) {
		t.Errorf("config bytes differ between two builds:\n%s\n%s", first.raw, second.raw)
	}
	if !bytes.Equal(firstManifest.raw, secondManifest.raw) {
		t.Errorf("manifest bytes differ between two builds:\n%s\n%s", firstManifest.raw, secondManifest.raw)
	}
}

// TestArtificialManifestWithoutADiffIDUsesTheEmptyConfig covers the blobs with no diff
// id to record -- an artifact "layer" that is not a filesystem changeset, or an image
// whose config could not be read. A config that says nothing beats one that says
// something untrue, and the manifest references the blob either way.
func TestArtificialManifestWithoutADiffIDUsesTheEmptyConfig(t *testing.T) {
	config, manifest, err := artificialManifest(ociLayerDescriptor(), "")
	if err != nil {
		t.Fatalf("artificialManifest: %v", err)
	}
	if string(config.raw) != "{}" {
		t.Errorf("config bytes = %s, want {}", config.raw)
	}
	digest, size, err := registryv1.SHA256(bytes.NewReader(config.raw))
	if err != nil {
		t.Fatalf("hashing the empty config: %v", err)
	}
	if config.descriptor.Digest != digest.String() || config.descriptor.Size != size {
		t.Errorf("config descriptor = %+v, want the digest and size of {}", config.descriptor)
	}
	var parsed registryv1.Manifest
	if err := json.Unmarshal(manifest.raw, &parsed); err != nil {
		t.Fatalf("parsing the artificial manifest: %v", err)
	}
	if len(parsed.Layers) != 1 || parsed.Layers[0].Digest.String() != sharedLayerDigest {
		t.Errorf("layers = %+v, want the blob's own descriptor", parsed.Layers)
	}
}

// TestArtificialManifestRejectsABogusDiffID fails rather than writing a manifest whose
// config cannot be parsed: an unparseable diff id means whatever produced it is
// broken, and a registry strict enough to demand a manifest is not one to hand a
// malformed config to.
func TestArtificialManifestRejectsABogusDiffID(t *testing.T) {
	if _, _, err := artificialManifest(ociLayerDescriptor(), "not-a-digest"); err == nil {
		t.Error("artificialManifest accepted a bogus diff id")
	}
}

// TestArtificialManifestMatchesTheLayerMediaTypeFamily keeps the families apart: a
// Docker layer -- which is what a shallow base image's layers usually are -- goes into
// a Docker manifest with a Docker config, not an OCI manifest.
func TestArtificialManifestMatchesTheLayerMediaTypeFamily(t *testing.T) {
	for _, tc := range []struct {
		layerMediaType registrytypes.MediaType
		wantManifest   registrytypes.MediaType
		wantConfig     registrytypes.MediaType
	}{
		{registrytypes.OCILayer, registrytypes.OCIManifestSchema1, registrytypes.OCIConfigJSON},
		{registrytypes.OCILayerZStd, registrytypes.OCIManifestSchema1, registrytypes.OCIConfigJSON},
		{registrytypes.DockerLayer, registrytypes.DockerManifestSchema2, registrytypes.DockerConfigJSON},
		{registrytypes.DockerForeignLayer, registrytypes.DockerManifestSchema2, registrytypes.DockerConfigJSON},
		// A blob that is not a layer at all (a ztoc, an SBOM) is described as OCI.
		{"application/octet-stream", registrytypes.OCIManifestSchema1, registrytypes.OCIConfigJSON},
	} {
		t.Run(string(tc.layerMediaType), func(t *testing.T) {
			layer := ociLayerDescriptor()
			layer.MediaType = string(tc.layerMediaType)
			config, manifest, err := artificialManifest(layer, "")
			if err != nil {
				t.Fatalf("artificialManifest: %v", err)
			}
			if manifest.mediaType != tc.wantManifest {
				t.Errorf("manifest media type = %s, want %s", manifest.mediaType, tc.wantManifest)
			}
			if config.descriptor.MediaType != string(tc.wantConfig) {
				t.Errorf("config media type = %s, want %s", config.descriptor.MediaType, tc.wantConfig)
			}
			var parsed registryv1.Manifest
			if err := json.Unmarshal(manifest.raw, &parsed); err != nil {
				t.Fatalf("parsing the artificial manifest: %v", err)
			}
			if parsed.MediaType != tc.wantManifest {
				t.Errorf("manifest body media type = %s, want %s", parsed.MediaType, tc.wantManifest)
			}
		})
	}
}

// TestDiffIDIndexNilKnowsNothing covers the index a deploy that writes no artificial
// manifests passes: it answers "unknown" instead of panicking.
func TestDiffIDIndexNilKnowsNothing(t *testing.T) {
	var index *diffIDIndex
	if got := index.diffIDFor(sharedLayerDigest); got != "" {
		t.Errorf("diffIDFor on a nil index = %q, want empty", got)
	}
}
