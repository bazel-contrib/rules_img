package signer

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// TestHelperProcess is not a real test: when GO_WANT_HELPER_PROCESS=1 it acts as
// a signer plugin, reading the subject descriptor from stdin and writing a fake
// signature artifact as an OCI layout tar to stdout. TestSubprocessSignArtifacts
// runs this binary in that mode.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	var subject v1.Descriptor
	if err := json.NewDecoder(os.Stdin).Decode(&subject); err != nil {
		os.Exit(2)
	}
	img, err := buildTestArtifact(
		"application/vnd.test.signature",
		[]testArtifactLayer{{MediaType: "application/octet-stream", Data: []byte("sig-of-" + subject.Digest.Hex)}},
		&subject,
		nil,
	)
	if err != nil {
		os.Exit(3)
	}
	if err := writeArtifactTar(os.Stdout, []v1.Image{img}); err != nil {
		os.Exit(4)
	}
	os.Exit(0)
}

// TestSubprocessSignArtifacts exercises the real subprocess path: it execs this
// test binary as a plugin, feeds it the subject on stdin, and parses the OCI
// layout tar from its stdout.
func TestSubprocessSignArtifacts(t *testing.T) {
	sub := &Subprocess{
		toolPath: os.Args[0],
		args:     []string{"-test.run=TestHelperProcess"},
		env:      append(os.Environ(), "GO_WANT_HELPER_PROCESS=1"),
	}
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
