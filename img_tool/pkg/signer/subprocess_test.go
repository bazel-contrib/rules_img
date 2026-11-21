package signer

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// TestMain makes the test binary act as a signer plugin when
// GO_WANT_HELPER_PROCESS=1 (see helperPlugin). It can't be a test selected with
// -test.run: the plugin protocol puts the subcommand first, which ends the test
// binary's flag parsing.
func TestMain(m *testing.M) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") == "1" {
		os.Exit(helperPlugin(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// helperPlugin is a fake signer plugin. sign-oci-artifact reads the subject
// descriptor from stdin and writes a fake signature artifact; attest-oci-artifact
// reads an AttestRequest and writes an artifact whose layer is the predicate.
func helperPlugin(args []string) int {
	if len(args) == 0 {
		return 1
	}
	var img v1.Image
	var err error
	switch args[0] {
	case SignSubcommand:
		var subject v1.Descriptor
		if err := json.NewDecoder(os.Stdin).Decode(&subject); err != nil {
			return 2
		}
		img, err = buildTestArtifact(
			"application/vnd.test.signature",
			[]testArtifactLayer{{MediaType: "application/octet-stream", Data: []byte("sig-of-" + subject.Digest.Hex)}},
			&subject,
			nil,
		)
	case AttestSubcommand:
		var request AttestRequest
		if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil {
			return 2
		}
		img, err = buildTestArtifact(
			"application/vnd.test.attestation",
			[]testArtifactLayer{{MediaType: "application/json", Data: request.Predicate}},
			&request.Subject,
			map[string]string{"predicateType": request.PredicateType},
		)
	default:
		return 1
	}
	if err != nil {
		return 3
	}
	if err := writeArtifactTar(os.Stdout, []v1.Image{img}); err != nil {
		return 4
	}
	return 0
}

func helperSubprocess() *Subprocess {
	return &Subprocess{toolPath: os.Args[0], env: append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")}
}

// TestSubprocessSignArtifacts exercises the real subprocess path: it execs this
// test binary as a plugin, feeds it the subject on stdin, and parses the OCI
// layout tar from its stdout.
func TestSubprocessSignArtifacts(t *testing.T) {
	sub := helperSubprocess()
	subject := v1.Descriptor{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    v1.Hash{Algorithm: "sha256", Hex: "3333333333333333333333333333333333333333333333333333333333333333"},
		Size:      99,
	}

	imgs, err := sub.SignArtifacts(context.Background(), subject)
	if err != nil {
		t.Fatalf("SignArtifacts: %v", err)
	}
	if len(imgs) != 1 {
		t.Fatalf("got %d artifacts, want 1", len(imgs))
	}
	manifest, err := imgs[0].Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if manifest.ArtifactType != "application/vnd.test.signature" {
		t.Errorf("artifactType = %q", manifest.ArtifactType)
	}
	if manifest.Subject == nil || manifest.Subject.Digest != subject.Digest {
		t.Errorf("subject = %+v, want %s", manifest.Subject, subject.Digest)
	}
}

// TestSubprocessAttestArtifacts runs the attest-oci-artifact subcommand of a
// plugin: the request carries the subject, the predicate type, and the
// predicate.
func TestSubprocessAttestArtifacts(t *testing.T) {
	subject := v1.Descriptor{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    v1.Hash{Algorithm: "sha256", Hex: "4444444444444444444444444444444444444444444444444444444444444444"},
		Size:      42,
	}
	predicate := json.RawMessage(`{"bomFormat":"CycloneDX","specVersion":"1.6"}`)
	imgs, err := helperSubprocess().AttestArtifacts(context.Background(), subject, CycloneDXPredicateType, predicate)
	if err != nil {
		t.Fatalf("AttestArtifacts: %v", err)
	}
	if len(imgs) != 1 {
		t.Fatalf("got %d artifacts, want 1", len(imgs))
	}
	manifest, err := imgs[0].Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Subject == nil || manifest.Subject.Digest != subject.Digest || manifest.Annotations["predicateType"] != CycloneDXPredicateType {
		t.Errorf("the attestation refers to %+v with annotations %v, want %s and the predicate type %s", manifest.Subject, manifest.Annotations, subject.Digest, CycloneDXPredicateType)
	}
	layers, err := imgs[0].Layers()
	if err != nil {
		t.Fatal(err)
	}
	rc, err := layers[0].Compressed()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(predicate) {
		t.Errorf("the plugin got the predicate %s, want %s", got, predicate)
	}
}
