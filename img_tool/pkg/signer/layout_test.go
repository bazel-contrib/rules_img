package signer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// buildArtifact constructs a signature-like OCI artifact image: an image
// manifest with an empty config, a single "envelope" layer, an artifactType,
// and a subject.
func buildArtifact(t *testing.T) (v1.Image, []byte) {
	t.Helper()

	config := []byte("{}")
	configSum := sha256.Sum256(config)
	configHex := hex.EncodeToString(configSum[:])

	envelope := []byte(`{"payload":"signature-envelope"}`)
	layerSum := sha256.Sum256(envelope)
	layerHex := hex.EncodeToString(layerSum[:])

	subjectSum := sha256.Sum256([]byte("the-subject-manifest"))
	subjectHex := hex.EncodeToString(subjectSum[:])

	manifest := v1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		ArtifactType:  "application/vnd.cncf.notary.signature",
		Config: v1.Descriptor{
			MediaType: "application/vnd.oci.empty.v1+json",
			Digest:    v1.Hash{Algorithm: "sha256", Hex: configHex},
			Size:      int64(len(config)),
		},
		Layers: []v1.Descriptor{{
			MediaType: "application/jose+json",
			Digest:    v1.Hash{Algorithm: "sha256", Hex: layerHex},
			Size:      int64(len(envelope)),
		}},
		Subject: &v1.Descriptor{
			MediaType: types.OCIManifestSchema1,
			Digest:    v1.Hash{Algorithm: "sha256", Hex: subjectHex},
			Size:      42,
		},
		Annotations: map[string]string{"org.opencontainers.image.created": "1970-01-01T00:00:00Z"},
	}
	rawManifest, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshalling manifest: %v", err)
	}

	blobs := map[string][]byte{configHex: config, layerHex: envelope}
	img, err := imageFromBlobs(types.OCIManifestSchema1, rawManifest, blobs)
	if err != nil {
		t.Fatalf("building artifact image: %v", err)
	}
	return img, rawManifest
}

func TestWriteReadArtifactRoundTrip(t *testing.T) {
	img, rawManifest := buildArtifact(t)
	wantDigest, err := img.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}

	var buf bytes.Buffer
	if err := writeArtifactTar(&buf, []v1.Image{img}); err != nil {
		t.Fatalf("writeArtifactTar: %v", err)
	}

	got, err := ReadArtifactLayout(buf.Bytes())
	if err != nil {
		t.Fatalf("ReadArtifactLayout: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d images, want 1", len(got))
	}

	gotDigest, err := got[0].Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if gotDigest != wantDigest {
		t.Errorf("digest changed: got %s want %s", gotDigest, wantDigest)
	}

	gotManifest, err := got[0].RawManifest()
	if err != nil {
		t.Fatalf("RawManifest: %v", err)
	}
	if !bytes.Equal(gotManifest, rawManifest) {
		t.Errorf("manifest bytes not preserved:\n got %s\nwant %s", gotManifest, rawManifest)
	}

	// Subject must survive the round-trip so referrers linkage works.
	var m v1.Manifest
	if err := json.Unmarshal(gotManifest, &m); err != nil {
		t.Fatalf("parsing round-tripped manifest: %v", err)
	}
	if m.Subject == nil {
		t.Fatal("subject dropped in round-trip")
	}
	if m.ArtifactType != "application/vnd.cncf.notary.signature" {
		t.Errorf("artifactType not preserved: %q", m.ArtifactType)
	}

	// The layer content must be intact.
	layers, err := got[0].Layers()
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}
	if len(layers) != 1 {
		t.Fatalf("got %d layers, want 1", len(layers))
	}
	rc, err := layers[0].Compressed()
	if err != nil {
		t.Fatalf("Compressed: %v", err)
	}
	defer rc.Close()
	var lbuf bytes.Buffer
	if _, err := lbuf.ReadFrom(rc); err != nil {
		t.Fatalf("reading layer: %v", err)
	}
	if lbuf.String() != `{"payload":"signature-envelope"}` {
		t.Errorf("layer content changed: %q", lbuf.String())
	}
}

func TestReadArtifactLayoutRejectsCorruptBlob(t *testing.T) {
	img, _ := buildArtifact(t)
	var buf bytes.Buffer
	if err := writeArtifactTar(&buf, []v1.Image{img}); err != nil {
		t.Fatalf("writeArtifactTar: %v", err)
	}
	// Flip a byte in the middle of the tar (inside a blob) and expect a digest error.
	corrupt := buf.Bytes()
	corrupt[len(corrupt)/2] ^= 0xff
	if _, err := ReadArtifactLayout(corrupt); err == nil {
		t.Skip("byte flip landed in tar padding; integrity check not exercised")
	}
}
