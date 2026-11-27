package referrers

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

const (
	amd64Manifest = "sha256:23678f31b3b3586c4fb318aecfe64a96a1f0916ba8faf9b2be2abee63fa9e827"
	arm64Manifest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

// buildKitIndex is an image index as BuildKit writes it (see the example in BuildKit's
// docs/attestations/attestation-storage.md): the platform manifests, then an attestation manifest
// for each. It also lists a descriptor of another reference type, which must be ignored.
func buildKitIndex(attestations map[string]string) []byte {
	manifests := []any{
		map[string]any{"mediaType": string(types.OCIManifestSchema1), "digest": amd64Manifest, "size": 1234, "platform": map[string]any{"architecture": "amd64", "os": "linux"}},
		map[string]any{"mediaType": string(types.OCIManifestSchema1), "digest": arm64Manifest, "size": 1234, "platform": map[string]any{"architecture": "arm64", "os": "linux"}},
	}
	for attestation, subject := range attestations {
		manifests = append(manifests, map[string]any{
			"mediaType": string(types.OCIManifestSchema1), "digest": attestation, "size": 1234,
			"annotations": map[string]any{"vnd.docker.reference.digest": subject, "vnd.docker.reference.type": "attestation-manifest"},
			"platform":    map[string]any{"architecture": "unknown", "os": "unknown"},
		})
	}
	manifests = append(manifests, map[string]any{
		"mediaType": string(types.OCIManifestSchema1), "digest": "sha256:9999999999999999999999999999999999999999999999999999999999999999", "size": 1,
		"annotations": map[string]any{"vnd.docker.reference.digest": amd64Manifest, "vnd.docker.reference.type": "something-else"},
	})
	raw, _ := json.Marshal(map[string]any{"mediaType": string(types.OCIImageIndex), "schemaVersion": 2, "manifests": manifests})
	return raw
}

func TestBuildKitAttestations(t *testing.T) {
	const amd64Attestation = "sha256:02cb9aa7600e73fcf41ee9f0f19cc03122b2d8be43d41ce4b21335118f5dd943"
	const arm64Attestation = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	index := buildKitIndex(map[string]string{amd64Attestation: amd64Manifest, arm64Attestation: arm64Manifest})

	got, err := BuildKitAttestations(index, amd64Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{amd64Attestation}; !slices.Equal(got, want) {
		t.Errorf("attestations of amd64 = %v, want %v (not those of another reference type)", got, want)
	}
	if got, _ := BuildKitAttestations(index, "sha256:3333333333333333333333333333333333333333333333333333333333333333"); len(got) != 0 {
		t.Errorf("a manifest the index has no attestation for has %v", got)
	}
	if got, err := BuildKitAttestations(nil, amd64Manifest); err != nil || len(got) != 0 {
		t.Errorf("without an index: %v, %v", got, err)
	}

	subject, ok, err := BuildKitAttestationSubject(index, arm64Attestation)
	if err != nil || !ok || subject != arm64Manifest {
		t.Errorf("subject of the arm64 attestation = %q, %v, %v", subject, ok, err)
	}
	if _, ok, _ := BuildKitAttestationSubject(index, "sha256:9999999999999999999999999999999999999999999999999999999999999999"); ok {
		t.Errorf("a descriptor of another reference type is not an attestation manifest")
	}
}

// TestDiscoverBuildKitAttestations checks that BuildKitAttestationType finds the attestation
// manifests of the index whether or not the registry lists them as referrers, and that an OCI
// artifact attestation manifest (with a subject) is also found as a referrer where the registry
// has a referrers API.
func TestDiscoverBuildKitAttestations(t *testing.T) {
	for _, referrersAPI := range []bool{true, false} {
		t.Run(map[bool]string{true: "referrers API", false: "no referrers API"}[referrersAPI], func(t *testing.T) {
			rt := handlerTransport{registry.New(registry.WithReferrersSupport(referrersAPI))}
			options := []remote.Option{remote.WithTransport(rt)}
			repo, err := nameRepository("registry.test/example/image")
			if err != nil {
				t.Fatal(err)
			}
			image := map[string]any{
				"schemaVersion": 2,
				"mediaType":     string(types.OCIManifestSchema1),
				"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111", "size": 1},
				"layers":        []any{},
			}
			raw, _ := json.Marshal(image)
			imageDigest := put(t, repo.Tag("image"), image, options)
			subject := v1.Descriptor{MediaType: types.OCIManifestSchema1, Digest: imageDigest, Size: int64(len(raw))}
			attestation := artifact(BuildKitAttestationArtifactType, subject, "sbom")
			attestationDigest := put(t, repo.Tag("attestation"), attestation, options)
			// Without the referrers API, BuildKit doesn't write a referrers tag either.
			if !referrersAPI {
				_ = remote.Delete(repo.Tag("sha256-"+imageDigest.Hex), options...)
			}

			index := buildKitIndexFor(imageDigest.String(), attestationDigest.String())
			found, err := Discover(context.Background(), repo, imageDigest.String(), []string{BuildKitAttestationType, AnyArtifactType}, Options{Transport: rt, Remote: options, Index: index})
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if want := []string{attestationDigest.String()}; !slices.Equal(found[BuildKitAttestationType], want) {
				t.Errorf("BuildKit attestations = %v, want %v", found[BuildKitAttestationType], want)
			}
			wantReferrers := []string{}
			if referrersAPI {
				wantReferrers = []string{attestationDigest.String()}
			}
			if !slices.Equal(found[AnyArtifactType], wantReferrers) {
				t.Errorf("referrers = %v, want %v", found[AnyArtifactType], wantReferrers)
			}
		})
	}
}

func buildKitIndexFor(manifest, attestation string) []byte {
	raw, _ := json.Marshal(map[string]any{"mediaType": string(types.OCIImageIndex), "schemaVersion": 2, "manifests": []any{
		map[string]any{"mediaType": string(types.OCIManifestSchema1), "digest": manifest, "size": 1, "platform": map[string]any{"architecture": "amd64", "os": "linux"}},
		map[string]any{"mediaType": string(types.OCIManifestSchema1), "digest": attestation, "size": 1,
			"annotations": map[string]any{"vnd.docker.reference.digest": manifest, "vnd.docker.reference.type": "attestation-manifest"},
			"platform":    map[string]any{"architecture": "unknown", "os": "unknown"}},
	}})
	return raw
}
