package deploy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// testSignatureImage builds a signature-like OCI artifact image (a single
// jose+json "envelope" layer, an empty config media type, and an OCI 1.1
// `subject` linking it to the given subject descriptor) — the shape a signer
// plugin emits and that signIntoSink captures into a sink.
func testSignatureImage(t *testing.T, subject registryv1.Descriptor) registryv1.Image {
	t.Helper()
	layer := static.NewLayer([]byte("signature-envelope"), "application/jose+json")
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatalf("AppendLayers: %v", err)
	}
	img = mutate.ConfigMediaType(img, "application/vnd.oci.empty.v1+json")
	return mutate.Subject(img, subject).(registryv1.Image)
}

// TestSignatureSinkImageDistributionReferrers verifies the core --sink signing
// behavior: a signature artifact captured into a distribution sink is written
// under its subject's repository and appears in the generated referrers listing
// for that subject (the "referrer-data").
func TestSignatureSinkImageDistributionReferrers(t *testing.T) {
	skipIfColonFilenamesUnsupported(t)
	ctx := context.Background()
	dir := t.TempDir()
	s, err := newSink(sinkDistribution, dir)
	if err != nil {
		t.Fatal(err)
	}

	// Subject image pushed under reg.example/team/app:v1.
	subjectRoot := testRoot("subject")
	subjectDigest := sha256Hash(subjectRoot.RootData)
	if err := s.AddImage(ctx, sinkImage{Refs: []string{"reg.example/team/app:v1"}, Root: subjectRoot}); err != nil {
		t.Fatal(err)
	}

	// Signature of that subject, captured as a digest-only referrer under the
	// same repository (as signIntoSink does).
	sig := testSignatureImage(t, registryv1.Descriptor{
		MediaType: types.OCIManifestSchema1,
		Digest:    subjectDigest,
		Size:      int64(len(subjectRoot.RootData)),
	})
	si, err := signatureSinkImage(sig, "reg.example", "team/app")
	if err != nil {
		t.Fatalf("signatureSinkImage: %v", err)
	}
	if err := s.AddImage(ctx, si); err != nil {
		t.Fatalf("AddImage(signature): %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	sigDigest, err := sig.Digest()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Join(dir, "reg.example", "v2", "team", "app")

	// The signature manifest blob is stored under the subject's repository.
	if _, err := os.Stat(filepath.Join(repoRoot, "manifests", "sha256:"+sigDigest.Hex)); err != nil {
		t.Errorf("signature manifest blob missing: %v", err)
	}

	// The referrers listing for the subject lists the signature.
	data, err := os.ReadFile(filepath.Join(repoRoot, "referrers", "sha256:"+subjectDigest.Hex))
	if err != nil {
		t.Fatalf("reading referrers listing for subject: %v", err)
	}
	var idx registryv1.IndexManifest
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatalf("parsing referrers listing: %v", err)
	}
	found := false
	for _, d := range idx.Manifests {
		if d.Digest == sigDigest {
			found = true
		}
	}
	if !found {
		t.Errorf("signature %s not found in referrers listing %+v", sigDigest, idx.Manifests)
	}
}

// TestSignatureSinkImageOCITar verifies a signature artifact captured into an
// oci-tar sink is present as a manifest blob and referenced from index.json, so
// it is discoverable as a referrer in the exported layout.
func TestSignatureSinkImageOCITar(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "out.tar")
	s, err := newSink(sinkOCITar, path)
	if err != nil {
		t.Fatal(err)
	}

	subjectRoot := testRoot("subject")
	subjectDigest := sha256Hash(subjectRoot.RootData)
	if err := s.AddImage(ctx, sinkImage{Refs: []string{"reg.example/team/app:v1"}, Root: subjectRoot}); err != nil {
		t.Fatal(err)
	}
	sig := testSignatureImage(t, registryv1.Descriptor{
		MediaType: types.OCIManifestSchema1,
		Digest:    subjectDigest,
		Size:      int64(len(subjectRoot.RootData)),
	})
	si, err := signatureSinkImage(sig, "reg.example", "team/app")
	if err != nil {
		t.Fatalf("signatureSinkImage: %v", err)
	}
	if err := s.AddImage(ctx, si); err != nil {
		t.Fatalf("AddImage(signature): %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	sigDigest, err := sig.Digest()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	names := tarNames(t, data)
	// The signature manifest blob must be present in the layout.
	if !names["blobs/sha256/"+sigDigest.Hex] {
		t.Errorf("oci-tar missing signature manifest blob %s", sigDigest.Hex)
	}
}
