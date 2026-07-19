package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/notaryproject/notation-core-go/signature"
	"github.com/notaryproject/notation-core-go/signature/cose"
	"github.com/notaryproject/notation-core-go/signature/jws"
)

func writeTestKeyAndCert(t *testing.T) (keyPath, certPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "rules_img test signer"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating cert: %v", err)
	}
	dir := t.TempDir()
	keyPath = filepath.Join(dir, "key.pem")
	certPath = filepath.Join(dir, "cert.pem")

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshalling key: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("writing key: %v", err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatalf("writing cert: %v", err)
	}
	return keyPath, certPath
}

func TestNotationSignProducesVerifiableArtifact(t *testing.T) {
	keyPath, certPath := writeTestKeyAndCert(t)

	signer, err := newSigner([]string{"--key", keyPath, "--certificate-chain", certPath, "--signature-format", "jws"})
	if err != nil {
		t.Fatalf("newSigner: %v", err)
	}

	subject := v1.Descriptor{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    v1.Hash{Algorithm: "sha256", Hex: "1111111111111111111111111111111111111111111111111111111111111111"},
		Size:      512,
	}
	img, err := signer.Sign(context.Background(), subject)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	manifest, err := img.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if manifest.ArtifactType != artifactTypeNotation {
		t.Errorf("artifactType = %q, want %q", manifest.ArtifactType, artifactTypeNotation)
	}
	if manifest.Subject == nil || manifest.Subject.Digest != subject.Digest {
		t.Errorf("subject not set to %s: %+v", subject.Digest, manifest.Subject)
	}
	if _, ok := manifest.Annotations[annotationThumbprint]; !ok {
		t.Errorf("missing %s annotation", annotationThumbprint)
	}
	if len(manifest.Layers) != 1 || string(manifest.Layers[0].MediaType) != jws.MediaTypeEnvelope {
		t.Fatalf("expected one %s layer, got %+v", jws.MediaTypeEnvelope, manifest.Layers)
	}

	layers, err := img.Layers()
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}
	rc, err := layers[0].Compressed()
	if err != nil {
		t.Fatalf("Compressed: %v", err)
	}
	defer rc.Close()
	envelopeBytes := make([]byte, manifest.Layers[0].Size)
	if _, err := readFull(rc, envelopeBytes); err != nil {
		t.Fatalf("reading envelope: %v", err)
	}

	env, err := signature.ParseEnvelope(jws.MediaTypeEnvelope, envelopeBytes)
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	content, err := env.Verify()
	if err != nil {
		t.Fatalf("envelope verification failed: %v", err)
	}
	if content.Payload.ContentType != payloadContentType {
		t.Errorf("payload content type = %q, want %q", content.Payload.ContentType, payloadContentType)
	}

	var payload struct {
		TargetArtifact struct {
			Digest string `json:"digest"`
		} `json:"targetArtifact"`
	}
	if err := json.Unmarshal(content.Payload.Content, &payload); err != nil {
		t.Fatalf("parsing payload: %v", err)
	}
	if payload.TargetArtifact.Digest != subject.Digest.String() {
		t.Errorf("payload targetArtifact digest = %q, want %q", payload.TargetArtifact.Digest, subject.Digest.String())
	}
}

func readFull(r interface{ Read([]byte) (int, error) }, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			if total == len(buf) {
				return total, nil
			}
			return total, err
		}
	}
	return total, nil
}

// testTargetArtifact mirrors the "targetArtifact" object inside the signed
// Notary payload, so tests can assert on the signed content.
type testTargetArtifact struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations"`
}

// signedTargetArtifact signs subject, extracts the signature envelope from the
// produced artifact, verifies it, and returns the parsed payload's
// targetArtifact.
func signedTargetArtifact(t *testing.T, img v1.Image, envType string) testTargetArtifact {
	t.Helper()
	manifest, err := img.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if len(manifest.Layers) != 1 {
		t.Fatalf("expected one layer, got %d", len(manifest.Layers))
	}
	layers, err := img.Layers()
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}
	rc, err := layers[0].Compressed()
	if err != nil {
		t.Fatalf("Compressed: %v", err)
	}
	defer rc.Close()
	envelopeBytes := make([]byte, manifest.Layers[0].Size)
	if _, err := readFull(rc, envelopeBytes); err != nil {
		t.Fatalf("reading envelope: %v", err)
	}
	env, err := signature.ParseEnvelope(envType, envelopeBytes)
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	content, err := env.Verify()
	if err != nil {
		t.Fatalf("envelope verification failed: %v", err)
	}
	var payload struct {
		TargetArtifact testTargetArtifact `json:"targetArtifact"`
	}
	if err := json.Unmarshal(content.Payload.Content, &payload); err != nil {
		t.Fatalf("parsing payload: %v", err)
	}
	return payload.TargetArtifact
}

func testSubject() v1.Descriptor {
	return v1.Descriptor{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    v1.Hash{Algorithm: "sha256", Hex: "1111111111111111111111111111111111111111111111111111111111111111"},
		Size:      512,
	}
}

func TestUserMetadataAddedToPayload(t *testing.T) {
	keyPath, certPath := writeTestKeyAndCert(t)
	signer, err := newSigner([]string{
		"--key", keyPath, "--certificate-chain", certPath,
		"--user-metadata", "buildId=42", "-m", "commit=abcdef",
	})
	if err != nil {
		t.Fatalf("newSigner: %v", err)
	}
	img, err := signer.Sign(context.Background(), testSubject())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	target := signedTargetArtifact(t, img, jws.MediaTypeEnvelope)
	if got := target.Annotations["buildId"]; got != "42" {
		t.Errorf("annotation buildId = %q, want %q", got, "42")
	}
	if got := target.Annotations["commit"]; got != "abcdef" {
		t.Errorf("annotation commit = %q, want %q", got, "abcdef")
	}
}

func TestNoUserMetadataOmitsAnnotations(t *testing.T) {
	keyPath, certPath := writeTestKeyAndCert(t)
	signer, err := newSigner([]string{"--key", keyPath, "--certificate-chain", certPath})
	if err != nil {
		t.Fatalf("newSigner: %v", err)
	}
	img, err := signer.Sign(context.Background(), testSubject())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	target := signedTargetArtifact(t, img, jws.MediaTypeEnvelope)
	if target.Annotations != nil {
		t.Errorf("expected no annotations in payload, got %v", target.Annotations)
	}
}

func TestUserMetadataValidation(t *testing.T) {
	keyPath, certPath := writeTestKeyAndCert(t)
	cases := []struct {
		name string
		args []string
	}{
		{"missing separator", []string{"--user-metadata", "novalue"}},
		{"empty key", []string{"--user-metadata", "=value"}},
		{"empty value", []string{"--user-metadata", "key="}},
		{"reserved prefix", []string{"--user-metadata", "io.cncf.notary.foo=bar"}},
		{"duplicate key", []string{"--user-metadata", "k=1", "--user-metadata", "k=2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--key", keyPath, "--certificate-chain", certPath}, tc.args...)
			if _, err := newSigner(args); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

func TestExpiryValidation(t *testing.T) {
	keyPath, certPath := writeTestKeyAndCert(t)
	if _, err := newSigner([]string{"--key", keyPath, "--certificate-chain", certPath, "--expiry", "-1h"}); err == nil {
		t.Error("expected error for negative expiry")
	}
	if _, err := newSigner([]string{"--key", keyPath, "--certificate-chain", certPath, "--expiry", "500ms"}); err == nil {
		t.Error("expected error for sub-second expiry granularity")
	}
	if _, err := newSigner([]string{"--key", keyPath, "--certificate-chain", certPath, "-e", "12h"}); err != nil {
		t.Errorf("valid -e expiry rejected: %v", err)
	}
}

func TestTimestampFlagsRequiredTogether(t *testing.T) {
	keyPath, certPath := writeTestKeyAndCert(t)
	if _, err := newSigner([]string{"--key", keyPath, "--certificate-chain", certPath, "--timestamp-url", "https://tsa.example"}); err == nil {
		t.Error("expected error when --timestamp-url is set without --timestamp-root-cert")
	}
	if _, err := newSigner([]string{"--key", keyPath, "--certificate-chain", certPath, "--timestamp-root-cert", certPath}); err == nil {
		t.Error("expected error when --timestamp-root-cert is set without --timestamp-url")
	}
}

func TestCOSEFormat(t *testing.T) {
	keyPath, certPath := writeTestKeyAndCert(t)
	signer, err := newSigner([]string{"--key", keyPath, "--certificate-chain", certPath, "--signature-format", "cose"})
	if err != nil {
		t.Fatalf("newSigner: %v", err)
	}
	img, err := signer.Sign(context.Background(), testSubject())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	manifest, err := img.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if string(manifest.Layers[0].MediaType) != cose.MediaTypeEnvelope {
		t.Errorf("layer media type = %q, want %q", manifest.Layers[0].MediaType, cose.MediaTypeEnvelope)
	}
	// The COSE envelope must verify and carry the expected subject digest.
	target := signedTargetArtifact(t, img, cose.MediaTypeEnvelope)
	if target.Digest != testSubject().Digest.String() {
		t.Errorf("payload digest = %q, want %q", target.Digest, testSubject().Digest.String())
	}
}

func TestLegacyEnvVarFallback(t *testing.T) {
	keyPath, certPath := writeTestKeyAndCert(t)
	t.Setenv("NOTATION_KEY", keyPath)
	t.Setenv("NOTATION_CERTIFICATE_CHAIN", certPath)
	if _, err := newSigner(nil); err != nil {
		t.Errorf("legacy NOTATION_KEY/NOTATION_CERTIFICATE_CHAIN fallback failed: %v", err)
	}
}
