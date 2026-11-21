package referrers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// dsseAttestation returns a DSSE envelope as `cosign attest` writes it under
// the .att tag: an in-toto statement about subject, whose predicate (an SPDX
// document) is a JSON string, as distroless publishes it.
func dsseAttestation(t *testing.T, subject string) []byte {
	t.Helper()
	algorithm, encoded, _ := strings.Cut(subject, ":")
	statement, err := json.Marshal(map[string]any{
		"_type":         "https://in-toto.io/Statement/v0.1",
		"predicateType": "https://spdx.dev/Document",
		"subject":       []any{map[string]any{"name": "example.com/image", "digest": map[string]string{algorithm: encoded}}},
		"predicate":     `{"spdxVersion":"SPDX-2.3","SPDXID":"SPDXRef-DOCUMENT"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]any{
		"payloadType": "application/vnd.in-toto+json",
		"payload":     base64.StdEncoding.EncodeToString(statement),
		"signatures":  []any{map[string]string{"keyid": "", "sig": "c2ln"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func simpleSigning(subject string) []byte {
	return []byte(fmt.Sprintf(`{"critical":{"identity":{"docker-reference":"example.com/image"},"image":{"docker-manifest-digest":%q},"type":"cosign container image signature"},"optional":null}`, subject))
}

// pushCosignArtifact pushes a manifest with the given layers under cosign's
// tag for the artifacts of subject with suffix, and returns its digest.
func pushCosignArtifact(t *testing.T, repo name.Repository, subject, suffix string, layers map[types.MediaType][]byte, options []remote.Option) string {
	t.Helper()
	img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.OCIConfigJSON)
	for mediaType, data := range layers {
		var err error
		img, err = mutate.Append(img, mutate.Addendum{
			Layer:       static.NewLayer(data, mediaType),
			Annotations: map[string]string{"predicateType": "https://spdx.dev/Document"},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := remote.Write(repo.Tag(CosignTag(subject, suffix)), img, options...); err != nil {
		t.Fatal(err)
	}
	digest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return digest.String()
}

func newCosignRegistry(t *testing.T) (name.Repository, Options, string) {
	t.Helper()
	// The conditional registry (see push_test.go) also gives every request a
	// body, which the in-memory registry assumes.
	rt := handlerTransport{handler: newConditionalRegistry(false)}
	options := Options{Transport: rt, Remote: []remote.Option{remote.WithTransport(rt)}}
	repo := testRepo(t)
	image, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := image.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(repo.Digest(digest.String()), image, options.Remote...); err != nil {
		t.Fatal(err)
	}
	return repo, options, digest.String()
}

func TestDiscoverCosignTags(t *testing.T) {
	repo, options, subject := newCosignRegistry(t)
	att := pushCosignArtifact(t, repo, subject, CosignAttestations, map[types.MediaType][]byte{mediaTypeDSSE: dsseAttestation(t, subject)}, options.Remote)
	sig := pushCosignArtifact(t, repo, subject, CosignSignatures, map[types.MediaType][]byte{mediaTypeSimpleSigning: simpleSigning(subject)}, options.Remote)

	found, err := Discover(context.Background(), repo, subject, []string{CosignTagType(CosignAttestations), CosignTagType(CosignSignatures), "application/spdx+json"}, options)
	if err != nil {
		t.Fatalf("Discover() = %v", err)
	}
	want := map[string][]string{
		"cosign:att":            {att},
		"cosign:sig":            {sig},
		"application/spdx+json": {},
	}
	for artifactType, digests := range want {
		if got := found[artifactType]; fmt.Sprint(got) != fmt.Sprint(digests) {
			t.Errorf("Discover()[%q] = %v, want %v", artifactType, got, digests)
		}
	}
}

func TestDiscoverCosignTagMissing(t *testing.T) {
	repo, options, subject := newCosignRegistry(t)
	found, err := Discover(context.Background(), repo, subject, []string{CosignTagType(CosignAttestations)}, options)
	if err != nil {
		t.Fatalf("Discover() = %v", err)
	}
	if got := found["cosign:att"]; len(got) != 0 {
		t.Errorf("Discover() found %v without a .att tag", got)
	}
}

// A cosign tag is only a naming convention: an artifact under it whose signed
// payload is about another manifest (or that isn't cosign's) is an error.
func TestDiscoverCosignTagRejectsForeignArtifacts(t *testing.T) {
	other := "sha256:" + strings.Repeat("0", 64)
	for _, tc := range []struct {
		name   string
		suffix string
		layers func(subject string) map[types.MediaType][]byte
		want   string
	}{
		{name: "attestation of another manifest", suffix: CosignAttestations, layers: func(string) map[types.MediaType][]byte {
			return map[types.MediaType][]byte{mediaTypeDSSE: dsseAttestation(t, other)}
		}, want: "don't name"},
		{name: "signature of another manifest", suffix: CosignSignatures, layers: func(string) map[types.MediaType][]byte {
			return map[types.MediaType][]byte{mediaTypeSimpleSigning: simpleSigning(other)}
		}, want: "don't name"},
		{name: "not cosign's", suffix: CosignAttestations, layers: func(string) map[types.MediaType][]byte {
			return map[types.MediaType][]byte{"application/vnd.oci.image.layer.v1.tar": []byte("tar")}
		}, want: "not one of cosign's"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, options, subject := newCosignRegistry(t)
			pushCosignArtifact(t, repo, subject, tc.suffix, tc.layers(subject), options.Remote)
			_, err := Discover(context.Background(), repo, subject, []string{CosignTagType(tc.suffix)}, options)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Discover() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestCosignTag(t *testing.T) {
	subject := "sha256:1a64f46364620465ef9e9a8f5413757dfdf22695e050bbfd7303eeaf17a59383"
	if got, want := CosignTag(subject, CosignAttestations), "sha256-1a64f46364620465ef9e9a8f5413757dfdf22695e050bbfd7303eeaf17a59383.att"; got != want {
		t.Errorf("CosignTag() = %q, want %q", got, want)
	}
}

// A pinned cosign artifact has no tag to go by: it is about the manifest of the
// tree its signed payloads name.
func TestAttributeCosignArtifact(t *testing.T) {
	image := "sha256:" + strings.Repeat("1", 64)
	child := "sha256:" + strings.Repeat("2", 64)
	outside := "sha256:" + strings.Repeat("3", 64)
	tree := []string{image, child}

	artifactOf := func(mediaType types.MediaType, layers ...[]byte) ([]byte, func(v1.Descriptor) ([]byte, error)) {
		blobs := map[string][]byte{}
		var descriptors []v1.Descriptor
		for _, data := range layers {
			digest, size, err := v1.SHA256(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			blobs[digest.String()] = data
			descriptors = append(descriptors, v1.Descriptor{MediaType: mediaTypeDSSE, Digest: digest, Size: size})
		}
		config := v1.Descriptor{MediaType: types.OCIConfigJSON, Digest: v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("c", 64)}, Size: 2}
		raw, err := json.Marshal(v1.Manifest{SchemaVersion: 2, MediaType: mediaType, Config: config, Layers: descriptors})
		if err != nil {
			t.Fatal(err)
		}
		return raw, func(layer v1.Descriptor) ([]byte, error) { return blobs[layer.Digest.String()], nil }
	}
	artifact := func(layers ...[]byte) ([]byte, func(v1.Descriptor) ([]byte, error)) {
		return artifactOf(types.OCIManifestSchema1, layers...)
	}

	raw, read := artifact(dsseAttestation(t, child), dsseAttestation(t, child))
	got, subject, err := AttributeCosignArtifact(raw, read, tree)
	if err != nil || subject != child || got.Suffix != CosignAttestations {
		t.Errorf("AttributeCosignArtifact() = %+v, %q, %v; want an attestation of %s", got, subject, err, child)
	}

	// The mediaType field is optional in a manifest.
	raw, read = artifactOf("", dsseAttestation(t, child))
	if _, subject, err := AttributeCosignArtifact(raw, read, tree); err != nil || subject != child {
		t.Errorf("AttributeCosignArtifact() of a manifest without a mediaType = %q, %v; want an attestation of %s", subject, err, child)
	}

	raw, read = artifact(dsseAttestation(t, outside))
	if _, _, err := AttributeCosignArtifact(raw, read, tree); err == nil || !strings.Contains(err.Error(), "none of which is part of the image") {
		t.Errorf("AttributeCosignArtifact() of an attestation of another image = %v, want an error", err)
	}

	// Layers about different manifests name none in common.
	raw, read = artifact(dsseAttestation(t, image), dsseAttestation(t, child))
	if _, _, err := AttributeCosignArtifact(raw, read, tree); err == nil {
		t.Error("AttributeCosignArtifact() of layers about different manifests succeeded")
	}
}
