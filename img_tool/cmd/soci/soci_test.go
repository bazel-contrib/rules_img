package soci

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	specv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

// writeLayerMetadata writes an api.Descriptor JSON file and returns its path.
func writeLayerMetadata(t *testing.T, dir, name string, desc api.Descriptor) string {
	t.Helper()
	path := filepath.Join(dir, name)
	data, err := json.Marshal(desc)
	if err != nil {
		t.Fatalf("marshaling layer metadata: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing layer metadata: %v", err)
	}
	return path
}

// writeZtoc writes a fake ztoc blob (the soci-index command only hashes it) and
// returns its path plus the digest/size it should produce.
func writeZtoc(t *testing.T, dir, name string, content []byte) (string, string, int64) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("writing ztoc: %v", err)
	}
	sum := sha256.Sum256(content)
	return path, "sha256:" + hex(sum[:]), int64(len(content))
}

func hex(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = digits[c>>4]
		out[i*2+1] = digits[c&0xf]
	}
	return string(out)
}

func TestBuildSociIndex(t *testing.T) {
	dir := t.TempDir()

	// Two layers: one 20 MiB (above the 10 MiB floor), one 1 KiB (below it).
	bigMeta := writeLayerMetadata(t, dir, "big.metadata.json", api.Descriptor{
		MediaType: api.TarGzipLayer,
		Digest:    "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Size:      20 << 20,
	})
	smallMeta := writeLayerMetadata(t, dir, "small.metadata.json", api.Descriptor{
		MediaType: api.TarGzipLayer,
		Digest:    "sha256:2222222222222222222222222222222222222222222222222222222222222222",
		Size:      1 << 10,
	})
	bigZtoc, bigZtocDigest, bigZtocSize := writeZtoc(t, dir, "big.ztoc", []byte("big-ztoc-bytes"))
	smallZtoc, _, _ := writeZtoc(t, dir, "small.ztoc", []byte("small-ztoc-bytes"))

	pairs := layerZtocPairs{
		{metadataPath: bigMeta, ztocPath: bigZtoc},
		{metadataPath: smallMeta, ztocPath: smallZtoc},
	}

	const spanSize = 4 << 20
	const minLayerSize = 10 << 20
	result, err := buildSociIndex(pairs, spanSize, minLayerSize, "test-tool", &specv1.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatalf("buildSociIndex: %v", err)
	}

	var manifest specv1.Manifest
	if err := json.Unmarshal(result.manifest, &manifest); err != nil {
		t.Fatalf("unmarshaling manifest: %v", err)
	}

	if manifest.SchemaVersion != 2 {
		t.Errorf("schemaVersion = %d, want 2", manifest.SchemaVersion)
	}
	if manifest.MediaType != specv1.MediaTypeImageManifest {
		t.Errorf("mediaType = %q, want %q", manifest.MediaType, specv1.MediaTypeImageManifest)
	}
	// The type is conveyed via the config media type, not a top-level artifactType.
	if manifest.ArtifactType != "" {
		t.Errorf("manifest artifactType = %q, want empty (conveyed via config)", manifest.ArtifactType)
	}
	if manifest.Subject != nil {
		t.Errorf("subject = %v, want nil (v2 does not use referrers)", manifest.Subject)
	}

	// Config: the "{}" blob with the v2 artifact media type.
	emptySHA := sha256.Sum256([]byte("{}"))
	wantConfigDigest := "sha256:" + hex(emptySHA[:])
	if manifest.Config.MediaType != api.SociIndexArtifactTypeV2 {
		t.Errorf("config mediaType = %q, want %q", manifest.Config.MediaType, api.SociIndexArtifactTypeV2)
	}
	if manifest.Config.Digest.String() != wantConfigDigest {
		t.Errorf("config digest = %q, want %q", manifest.Config.Digest, wantConfigDigest)
	}
	if manifest.Config.Size != 2 {
		t.Errorf("config size = %d, want 2", manifest.Config.Size)
	}

	// Only the big layer should be indexed (small one is below the floor).
	if len(manifest.Layers) != 1 {
		t.Fatalf("len(layers) = %d, want 1 (small layer filtered)", len(manifest.Layers))
	}
	ztocDesc := manifest.Layers[0]
	if ztocDesc.MediaType != api.SociLayerMediaType {
		t.Errorf("ztoc mediaType = %q, want %q", ztocDesc.MediaType, api.SociLayerMediaType)
	}
	if ztocDesc.Digest.String() != bigZtocDigest {
		t.Errorf("ztoc digest = %q, want %q", ztocDesc.Digest, bigZtocDigest)
	}
	if ztocDesc.Size != bigZtocSize {
		t.Errorf("ztoc size = %d, want %d", ztocDesc.Size, bigZtocSize)
	}
	if got := ztocDesc.Annotations[api.SociImageLayerDigestAnnotation]; got != "sha256:1111111111111111111111111111111111111111111111111111111111111111" {
		t.Errorf("image-layer-digest annotation = %q", got)
	}
	if got := ztocDesc.Annotations[api.SociImageLayerMediaTypeAnnotation]; got != api.TarGzipLayer {
		t.Errorf("image-layer-mediaType annotation = %q, want %q", got, api.TarGzipLayer)
	}
	if got := ztocDesc.Annotations[api.SociSpanSizeAnnotation]; got != "4194304" {
		t.Errorf("span-size annotation = %q, want 4194304", got)
	}

	if got := manifest.Annotations[api.SociBuildToolIdentifierAnnotation]; got != "test-tool" {
		t.Errorf("build-tool-identifier annotation = %q, want test-tool", got)
	}

	// Descriptor: image-manifest media type, v2 artifactType, matching digest+size.
	var descriptor specv1.Descriptor
	if err := json.Unmarshal(result.descriptor, &descriptor); err != nil {
		t.Fatalf("unmarshaling descriptor: %v", err)
	}
	if descriptor.MediaType != specv1.MediaTypeImageManifest {
		t.Errorf("descriptor mediaType = %q, want %q", descriptor.MediaType, specv1.MediaTypeImageManifest)
	}
	if descriptor.ArtifactType != api.SociIndexArtifactTypeV2 {
		t.Errorf("descriptor artifactType = %q, want %q", descriptor.ArtifactType, api.SociIndexArtifactTypeV2)
	}
	if descriptor.Digest != result.digest {
		t.Errorf("descriptor digest = %q, want %q", descriptor.Digest, result.digest)
	}
	if descriptor.Size != int64(len(result.manifest)) {
		t.Errorf("descriptor size = %d, want %d", descriptor.Size, len(result.manifest))
	}
	if descriptor.Platform == nil || descriptor.Platform.OS != "linux" || descriptor.Platform.Architecture != "amd64" {
		t.Errorf("descriptor platform = %+v, want linux/amd64", descriptor.Platform)
	}

	if string(result.config) != "{}" {
		t.Errorf("config blob = %q, want {}", result.config)
	}
}
