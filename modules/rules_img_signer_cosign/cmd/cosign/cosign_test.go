package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore/pkg/cryptoutils"

	// Registers the fakekms:// provider so the KMS --key path can be tested
	// end-to-end without a real cloud KMS.
	_ "github.com/sigstore/sigstore/pkg/signature/kms/fake"
)

func writeECDSAKey(t *testing.T) string {
	t.Helper()
	path, _ := writeECDSAKeyPair(t)
	return path
}

// writeECDSAKeyPair writes an unencrypted PKCS#8 ECDSA key and returns its path
// and public key (for verifying produced signatures).
func writeECDSAKeyPair(t *testing.T) (string, *ecdsa.PublicKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshalling key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("writing key: %v", err)
	}
	return path, &key.PublicKey
}

func testSubject() v1.Descriptor {
	return v1.Descriptor{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    v1.Hash{Algorithm: "sha256", Hex: "2222222222222222222222222222222222222222222222222222222222222222"},
		Size:      777,
	}
}

// bundleLayer returns the sigstore bundle JSON layer bytes of a signature artifact.
func bundleLayer(t *testing.T, img v1.Image) []byte {
	t.Helper()
	layers, err := img.Layers()
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}
	if len(layers) != 1 {
		t.Fatalf("expected one layer, got %d", len(layers))
	}
	rc, err := layers[0].Compressed()
	if err != nil {
		t.Fatalf("Compressed: %v", err)
	}
	defer rc.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(rc); err != nil {
		t.Fatalf("reading bundle layer: %v", err)
	}
	return buf.Bytes()
}

// dsseParts extracts the DSSE envelope payload type, payload bytes, and the
// first signature from a signature artifact's bundle.
func dsseParts(t *testing.T, img v1.Image) (payloadType string, payload, sig []byte) {
	t.Helper()
	var b struct {
		DsseEnvelope struct {
			Payload     string `json:"payload"`
			PayloadType string `json:"payloadType"`
			Signatures  []struct {
				Sig string `json:"sig"`
			} `json:"signatures"`
		} `json:"dsseEnvelope"`
	}
	if err := json.Unmarshal(bundleLayer(t, img), &b); err != nil {
		t.Fatalf("parsing bundle JSON: %v", err)
	}
	if len(b.DsseEnvelope.Signatures) == 0 {
		t.Fatal("bundle has no DSSE signature")
	}
	payload, err := base64.StdEncoding.DecodeString(b.DsseEnvelope.Payload)
	if err != nil {
		t.Fatalf("decoding DSSE payload: %v", err)
	}
	sig, err = base64.StdEncoding.DecodeString(b.DsseEnvelope.Signatures[0].Sig)
	if err != nil {
		t.Fatalf("decoding DSSE signature: %v", err)
	}
	return b.DsseEnvelope.PayloadType, payload, sig
}

type parsedStatement struct {
	Type          string `json:"_type"`
	PredicateType string `json:"predicateType"`
	Subject       []struct {
		Digest      map[string]string `json:"digest"`
		Annotations map[string]string `json:"annotations"`
	} `json:"subject"`
}

func statementOf(t *testing.T, img v1.Image) parsedStatement {
	t.Helper()
	_, payload, _ := dsseParts(t, img)
	var st parsedStatement
	if err := json.Unmarshal(payload, &st); err != nil {
		t.Fatalf("parsing in-toto statement: %v", err)
	}
	return st
}

// TestCosignKeyBasedSign exercises the key-based, no-transparency-log path
// (needs no network) and verifies the artifact is the DSSE/in-toto bundle that
// cosign's new bundle format verifies: subject digest = the signed image digest,
// signature valid over the DSSE PAE.
func TestCosignKeyBasedSign(t *testing.T) {
	keyPath, pub := writeECDSAKeyPair(t)

	signer, err := newSigner([]string{"--key", keyPath, "--tlog-upload=false"})
	if err != nil {
		t.Fatalf("newSigner: %v", err)
	}

	subject := testSubject()
	img, err := signer.Sign(context.Background(), subject)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	manifest, err := img.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if manifest.ArtifactType != bundleMediaType {
		t.Errorf("artifactType = %q, want %q", manifest.ArtifactType, bundleMediaType)
	}
	if manifest.Subject == nil || manifest.Subject.Digest != subject.Digest {
		t.Errorf("subject not set to %s: %+v", subject.Digest, manifest.Subject)
	}
	if len(manifest.Layers) != 1 || string(manifest.Layers[0].MediaType) != bundleMediaType {
		t.Fatalf("expected one %s layer, got %+v", bundleMediaType, manifest.Layers)
	}
	// cosign identifies the bundle via these annotations.
	if manifest.Annotations[annotationBundleContent] != "dsse-envelope" {
		t.Errorf("%s = %q, want dsse-envelope", annotationBundleContent, manifest.Annotations[annotationBundleContent])
	}
	if manifest.Annotations[annotationBundlePredicateType] != cosignSignPredicateType {
		t.Errorf("%s = %q, want %q", annotationBundlePredicateType, manifest.Annotations[annotationBundlePredicateType], cosignSignPredicateType)
	}
	// Reproducible by default: no creation timestamp annotation.
	if _, ok := manifest.Annotations[annotationCreated]; ok {
		t.Errorf("did not expect %s annotation without --record-creation-timestamp", annotationCreated)
	}

	var parsed bundle.Bundle
	if err := parsed.UnmarshalJSON(bundleLayer(t, img)); err != nil {
		t.Fatalf("layer is not a valid sigstore bundle: %v", err)
	}

	// The signed content must be a DSSE in-toto statement whose subject digest is
	// the image manifest digest (this is what `cosign verify` matches against).
	payloadType, payload, sig := dsseParts(t, img)
	if payloadType != dsseIntotoPayloadType {
		t.Errorf("payloadType = %q, want %q", payloadType, dsseIntotoPayloadType)
	}
	st := statementOf(t, img)
	if st.Type != inTotoStatementType {
		t.Errorf("_type = %q, want %q", st.Type, inTotoStatementType)
	}
	if st.PredicateType != cosignSignPredicateType {
		t.Errorf("predicateType = %q, want %q", st.PredicateType, cosignSignPredicateType)
	}
	if len(st.Subject) != 1 || st.Subject[0].Digest["sha256"] != subject.Digest.Hex {
		t.Errorf("statement subject digest = %+v, want sha256=%s", st.Subject, subject.Digest.Hex)
	}

	// The DSSE signature must verify over the PAE with the signing public key —
	// exactly cosign's crypto check.
	sum := sha256.Sum256(dssePAE(payloadType, payload))
	if !ecdsa.VerifyASN1(pub, sum[:], sig) {
		t.Error("DSSE signature does not verify against the signing public key")
	}
}

// dssePAE reconstructs the DSSE v1 pre-authentication encoding.
func dssePAE(payloadType string, payload []byte) []byte {
	return []byte(fmt.Sprintf("DSSEv1 %d %s %d %s", len(payloadType), payloadType, len(payload), payload))
}

// TestRecordCreationTimestamp verifies the created annotation is only emitted
// with the flag, and that SOURCE_DATE_EPOCH is honored for reproducibility.
func TestRecordCreationTimestamp(t *testing.T) {
	keyPath := writeECDSAKey(t)
	t.Setenv("SOURCE_DATE_EPOCH", "1700000000")

	signer, err := newSigner([]string{"--key", keyPath, "--tlog-upload=false", "--record-creation-timestamp"})
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
	got := manifest.Annotations[annotationCreated]
	want := time.Unix(1700000000, 0).UTC().Format(time.RFC3339)
	if got != want {
		t.Errorf("created annotation = %q, want %q", got, want)
	}
}

// TestInTotoStatement checks the signed statement binds the image digest and
// records --annotations on the subject; annotations are absent when none given.
func TestInTotoStatement(t *testing.T) {
	subject := testSubject()
	payload, err := inTotoStatement(subject, map[string]string{"foo": "bar"})
	if err != nil {
		t.Fatalf("inTotoStatement: %v", err)
	}
	var st parsedStatement
	if err := json.Unmarshal(payload, &st); err != nil {
		t.Fatalf("unmarshal statement: %v", err)
	}
	if st.Type != inTotoStatementType {
		t.Errorf("_type = %q, want %q", st.Type, inTotoStatementType)
	}
	if st.PredicateType != cosignSignPredicateType {
		t.Errorf("predicateType = %q, want %q", st.PredicateType, cosignSignPredicateType)
	}
	if len(st.Subject) != 1 || st.Subject[0].Digest["sha256"] != subject.Digest.Hex {
		t.Fatalf("subject digest = %+v, want sha256=%s", st.Subject, subject.Digest.Hex)
	}
	if st.Subject[0].Annotations["foo"] != "bar" {
		t.Errorf("subject annotations = %+v, want foo=bar", st.Subject[0].Annotations)
	}

	// No annotations -> the subject omits the annotations field.
	empty, err := inTotoStatement(subject, nil)
	if err != nil {
		t.Fatalf("inTotoStatement empty: %v", err)
	}
	if bytes.Contains(empty, []byte(`"annotations"`)) {
		t.Errorf("expected no annotations field, got %s", empty)
	}
}

// TestAnnotationsBindToSignature verifies annotations are inside the signed DSSE
// payload (changing them changes the signed bytes).
func TestAnnotationsBindToSignature(t *testing.T) {
	keyPath := writeECDSAKey(t)
	payloadOf := func(args ...string) string {
		signer, err := newSigner(append([]string{"--key", keyPath, "--tlog-upload=false"}, args...))
		if err != nil {
			t.Fatalf("newSigner: %v", err)
		}
		img, err := signer.Sign(context.Background(), testSubject())
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}
		_, payload, _ := dsseParts(t, img)
		return string(payload)
	}
	base := payloadOf()
	annotated := payloadOf("-a", "foo=bar")
	if base == annotated {
		t.Error("annotations did not change the signed DSSE payload")
	}
	if !strings.Contains(annotated, `"foo"`) || !strings.Contains(annotated, `"bar"`) {
		t.Errorf("annotated payload missing foo=bar: %s", annotated)
	}
}

// TestCosignEncryptedKey verifies interop with cosign's own encrypted key format
// (ENCRYPTED SIGSTORE PRIVATE KEY) decrypted via $COSIGN_PASSWORD.
func TestCosignEncryptedKey(t *testing.T) {
	const password = "hunter2"
	// GeneratePEMEncodedECDSAKeyPair emits the same "ENCRYPTED SIGSTORE PRIVATE
	// KEY" PEM that `cosign generate-key-pair` produces.
	privPEM, _, err := cryptoutils.GeneratePEMEncodedECDSAKeyPair(elliptic.P256(), cryptoutils.StaticPasswordFunc([]byte(password)))
	if err != nil {
		t.Fatalf("generating encrypted key: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "cosign.key")
	if err := os.WriteFile(keyPath, privPEM, 0o600); err != nil {
		t.Fatalf("writing key: %v", err)
	}

	t.Setenv("COSIGN_PASSWORD", password)
	signer, err := newSigner([]string{"--key", keyPath, "--tlog-upload=false"})
	if err != nil {
		t.Fatalf("newSigner with encrypted key: %v", err)
	}
	img, err := signer.Sign(context.Background(), testSubject())
	if err != nil {
		t.Fatalf("Sign with encrypted key: %v", err)
	}
	if st := statementOf(t, img); st.Subject[0].Digest["sha256"] != testSubject().Digest.Hex {
		t.Error("encrypted-key signature has wrong subject digest")
	}

	t.Setenv("COSIGN_PASSWORD", "wrong-password")
	if _, err := newSigner([]string{"--key", keyPath, "--tlog-upload=false"}); err == nil {
		t.Error("expected error decrypting with wrong COSIGN_PASSWORD")
	}
}

// TestKMSKeySign exercises the KMS --key path end-to-end via the fakekms://
// provider, which stands in for a cloud KMS (awskms://, gcpkms://, etc.).
func TestKMSKeySign(t *testing.T) {
	signer, err := newSigner([]string{"--key", "fakekms://test-key", "--tlog-upload=false"})
	if err != nil {
		t.Fatalf("newSigner with KMS key: %v", err)
	}
	img, err := signer.Sign(context.Background(), testSubject())
	if err != nil {
		t.Fatalf("Sign with KMS key: %v", err)
	}
	var parsed bundle.Bundle
	if err := parsed.UnmarshalJSON(bundleLayer(t, img)); err != nil {
		t.Fatalf("KMS signature is not a valid sigstore bundle: %v", err)
	}
	if st := statementOf(t, img); st.Subject[0].Digest["sha256"] != testSubject().Digest.Hex {
		t.Error("KMS signature has wrong subject digest")
	}
}

func TestIsKMSKeyRef(t *testing.T) {
	kms := []string{"awskms://k", "gcpkms://k", "azurekms://k", "hashivault://k", "fakekms://k", "custom://k"}
	files := []string{"cosign.key", "/keys/cosign.key", "./rel/key.pem", "C:\\keys\\key.pem"}
	for _, r := range kms {
		if !isKMSKeyRef(r) {
			t.Errorf("isKMSKeyRef(%q) = false, want true", r)
		}
	}
	for _, r := range files {
		if isKMSKeyRef(r) {
			t.Errorf("isKMSKeyRef(%q) = true, want false", r)
		}
	}
}

// TestK8sKeyRefRejected verifies k8s:// key references get a clear error.
func TestK8sKeyRefRejected(t *testing.T) {
	_, err := newSigner([]string{"--key", "k8s://namespace/secret", "--tlog-upload=false"})
	if err == nil {
		t.Fatal("expected error for k8s:// key reference")
	}
	if !strings.Contains(err.Error(), "Kubernetes") {
		t.Errorf("error %q should mention Kubernetes", err)
	}
}

// TestLoadKeypairRSA verifies RSA private keys are supported (not just ECDSA).
func TestLoadKeypairRSA(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshalling RSA key: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "rsa.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("writing RSA key: %v", err)
	}
	signer, err := newSigner([]string{"--key", keyPath, "--tlog-upload=false"})
	if err != nil {
		t.Fatalf("newSigner: %v", err)
	}
	img, err := signer.Sign(context.Background(), testSubject())
	if err != nil {
		t.Fatalf("Sign with RSA key: %v", err)
	}
	// Verify the DSSE signature validates with the RSA public key (PKCS1v15/SHA256).
	payloadType, payload, sig := dsseParts(t, img)
	sum := sha256.Sum256(dssePAE(payloadType, payload))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Errorf("RSA DSSE signature does not verify: %v", err)
	}
}

func TestParseSigningAlgorithm(t *testing.T) {
	for _, name := range []string{"", "ecdsa-p256", "ecdsa-p384", "ecdsa-p521", "rsa-2048", "rsa-3072", "rsa-4096", "ed25519"} {
		if _, err := parseSigningAlgorithm(name); err != nil {
			t.Errorf("parseSigningAlgorithm(%q) error: %v", name, err)
		}
	}
	if _, err := parseSigningAlgorithm("bogus"); err == nil {
		t.Error("expected error for unknown --signing-algorithm")
	}
}

func TestResolveIdentityToken(t *testing.T) {
	t.Setenv("SIGSTORE_ID_TOKEN", "")
	got, err := resolveIdentityToken("literal-token")
	if err != nil || got != "literal-token" {
		t.Fatalf("literal token = %q, %v; want literal-token", got, err)
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("token-from-file\n"), 0o600); err != nil {
		t.Fatalf("writing token file: %v", err)
	}
	got, err = resolveIdentityToken(tokenFile)
	if err != nil || got != "token-from-file" {
		t.Fatalf("token from file = %q, %v; want token-from-file", got, err)
	}
	t.Setenv("SIGSTORE_ID_TOKEN", "env-token")
	got, err = resolveIdentityToken("")
	if err != nil || got != "env-token" {
		t.Fatalf("token from env = %q, %v; want env-token", got, err)
	}
}

// makeCert creates a certificate signed by parent (or self-signed if parent is nil).
func makeCert(t *testing.T, cn string, serial int64, isCA bool, parent *x509.Certificate, parentKey crypto.Signer) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  isCA,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	signerCert, signerKey := tmpl, crypto.Signer(key)
	if parent != nil {
		signerCert, signerKey = parent, parentKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signerCert, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatalf("creating cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing cert: %v", err)
	}
	return cert, key
}

func writeCertsPEM(t *testing.T, certs ...*x509.Certificate) string {
	t.Helper()
	var buf bytes.Buffer
	for _, c := range certs {
		if err := pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}); err != nil {
			t.Fatalf("encoding cert: %v", err)
		}
	}
	path := filepath.Join(t.TempDir(), "certs.pem")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("writing certs: %v", err)
	}
	return path
}

func TestCertificateProviders(t *testing.T) {
	root, rootKey := makeCert(t, "root", 1, true, nil, nil)
	intermediate, intKey := makeCert(t, "intermediate", 2, true, root, rootKey)
	leaf, _ := makeCert(t, "leaf", 3, false, intermediate, intKey)

	// Single leaf certificate -> CertificateProvider.
	certProvider, chainProvider, err := certificateProviders(writeCertsPEM(t, leaf), "")
	if err != nil {
		t.Fatalf("leaf-only: %v", err)
	}
	if certProvider == nil || chainProvider != nil {
		t.Fatalf("leaf-only: want cert provider only, got cert=%v chain=%v", certProvider != nil, chainProvider != nil)
	}
	if got, _ := certProvider.GetCertificate(context.Background(), nil, nil); !bytes.Equal(got, leaf.Raw) {
		t.Error("leaf-only: certificate DER mismatch")
	}

	// Leaf + chain (intermediate, root) -> ChainProvider with the root stripped.
	certProvider, chainProvider, err = certificateProviders(writeCertsPEM(t, leaf), writeCertsPEM(t, intermediate, root))
	if err != nil {
		t.Fatalf("leaf+chain: %v", err)
	}
	if certProvider != nil || chainProvider == nil {
		t.Fatalf("leaf+chain: want chain provider only, got cert=%v chain=%v", certProvider != nil, chainProvider != nil)
	}
	chain, _ := chainProvider.GetCertificateChain(context.Background(), nil, nil)
	if len(chain) != 2 || !bytes.Equal(chain[0], leaf.Raw) || !bytes.Equal(chain[1], intermediate.Raw) {
		t.Errorf("leaf+chain: expected [leaf, intermediate] with root stripped, got %d certs", len(chain))
	}

	// Leaf + only a self-signed root -> root stripped -> single leaf CertificateProvider.
	certProvider, chainProvider, err = certificateProviders(writeCertsPEM(t, leaf), writeCertsPEM(t, root))
	if err != nil {
		t.Fatalf("leaf+root: %v", err)
	}
	if certProvider == nil || chainProvider != nil {
		t.Fatalf("leaf+root: want cert provider only after stripping root, got cert=%v chain=%v", certProvider != nil, chainProvider != nil)
	}
}

// TestCertificateFlagsRequireKey verifies --certificate without --key is rejected.
func TestCertificateFlagsRequireKey(t *testing.T) {
	if _, err := newSigner([]string{"--certificate", "/nonexistent.pem"}); err == nil {
		t.Error("expected error for --certificate without --key")
	}
}
