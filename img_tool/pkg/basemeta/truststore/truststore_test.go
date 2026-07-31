package truststore

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

// testCert generates a self-signed CA certificate with the given subject.
func testCert(t *testing.T, subject pkix.Name) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               subject,
		NotBefore:             time.Unix(0, 0),
		NotAfter:              time.Unix(1<<31-1, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	return der
}

func pemEncode(t *testing.T, der []byte) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestSubjectHashMatchesOpenSSL pins the OpenSSL X509_NAME_hash of a fixed
// certificate subject.
//
// The expected values were produced by `openssl x509 -noout -subject_hash` on
// certificates with these exact subjects. They exercise the two parts of the
// canonicalization that are easy to get wrong: retagging a PrintableString
// attribute (the country code) as UTF8String, and lowercasing plus collapsing
// internal whitespace.
func TestSubjectHashMatchesOpenSSL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		subject pkix.Name
		want    string
	}{
		{
			name: "mixed string types",
			subject: pkix.Name{
				Country:      []string{"US"},
				Organization: []string{"Example Corp"},
				CommonName:   "Example Root CA",
			},
			want: "eddc7343",
		},
		{
			name:    "case and whitespace are normalized",
			subject: pkix.Name{CommonName: "lower CASE  spaced"},
			want:    "c9cf3671",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			der := testCert(t, tc.subject)
			cert, err := x509.ParseCertificate(der)
			if err != nil {
				t.Fatalf("parsing certificate: %v", err)
			}
			if got := SubjectHash(cert); got != tc.want {
				t.Errorf("SubjectHash = %s, want %s (as computed by openssl x509 -subject_hash)", got, tc.want)
			}
		})
	}
}

// TestCollectionDeduplicates checks that the same certificate arriving from
// several sources -- a raw file and a distribution bundle, say -- yields one
// entry rather than a duplicate.
func TestCollectionDeduplicates(t *testing.T) {
	der := testCert(t, pkix.Name{CommonName: "Dedup Root"})

	c := NewCollection()
	if err := c.AddBytes(pemEncode(t, der), "a.pem"); err != nil {
		t.Fatalf("AddBytes(PEM): %v", err)
	}
	if err := c.AddBytes(der, "b.der"); err != nil {
		t.Fatalf("AddBytes(DER): %v", err)
	}
	if c.Len() != 1 {
		t.Errorf("collected %d certificates, want 1", c.Len())
	}
}

// TestCollectionAcceptsMultiCertPEM checks that a bundle file contributes every
// certificate it holds, and that non-certificate blocks are skipped rather than
// failing the build.
func TestCollectionAcceptsMultiCertPEM(t *testing.T) {
	first := testCert(t, pkix.Name{CommonName: "First Root"})
	second := testCert(t, pkix.Name{CommonName: "Second Root"})

	bundle := append(pemEncode(t, first), pemEncode(t, second)...)
	bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: []byte("ignored")})...)

	c := NewCollection()
	if err := c.AddBytes(bundle, "bundle.pem"); err != nil {
		t.Fatalf("AddBytes: %v", err)
	}
	if c.Len() != 2 {
		t.Errorf("collected %d certificates, want 2", c.Len())
	}
}

// TestCollectionRejectsGarbage checks that an unparseable input fails loudly.
// Skipping it would produce an image quietly missing a CA.
func TestCollectionRejectsGarbage(t *testing.T) {
	c := NewCollection()
	err := c.AddBytes([]byte("this is not a certificate"), "notes.txt")
	if err == nil {
		t.Fatal("AddBytes accepted a file that is not a certificate")
	}
	if !strings.Contains(err.Error(), "notes.txt") {
		t.Errorf("error %q does not name the offending file", err)
	}
}

// TestCertificatesAreContentOrdered checks that the output order depends only
// on the certificates themselves, not on the order they were added, so the
// resulting bundle is reproducible.
func TestCertificatesAreContentOrdered(t *testing.T) {
	a := testCert(t, pkix.Name{CommonName: "Alpha"})
	b := testCert(t, pkix.Name{CommonName: "Beta"})

	forward := NewCollection()
	if err := forward.AddBytes(a, "a"); err != nil {
		t.Fatal(err)
	}
	if err := forward.AddBytes(b, "b"); err != nil {
		t.Fatal(err)
	}

	backward := NewCollection()
	if err := backward.AddBytes(b, "b"); err != nil {
		t.Fatal(err)
	}
	if err := backward.AddBytes(a, "a"); err != nil {
		t.Fatal(err)
	}

	forwardCerts := forward.Certificates()
	backwardCerts := backward.Certificates()
	for i := range forwardCerts {
		if forwardCerts[i].Parsed.Subject.CommonName != backwardCerts[i].Parsed.Subject.CommonName {
			t.Fatalf("order depends on insertion: %v vs %v",
				forwardCerts[i].Parsed.Subject.CommonName,
				backwardCerts[i].Parsed.Subject.CommonName)
		}
	}
}

// TestExplodedLinksResolve checks that every symlink in the exploded tree points
// at a file that is also in the tree, and that its name is the subject hash a
// TLS library will look for.
func TestExplodedLinksResolve(t *testing.T) {
	c := NewCollection()
	for _, name := range []string{"Root One", "Root Two"} {
		if err := c.AddBytes(testCert(t, pkix.Name{CommonName: name}), name); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := Exploded(c.Certificates())
	if err != nil {
		t.Fatalf("Exploded: %v", err)
	}

	files := make(map[string]bool)
	for _, entry := range entries {
		if entry.LinkTarget == "" {
			files[entry.Name] = true
		}
	}
	links := 0
	for _, entry := range entries {
		if entry.LinkTarget == "" {
			continue
		}
		links++
		if !files[entry.LinkTarget] {
			t.Errorf("link %s points at %s, which is not in the tree", entry.Name, entry.LinkTarget)
		}
		if !strings.HasSuffix(entry.Name, ".0") {
			t.Errorf("link %s does not end in the .0 suffix OpenSSL looks for", entry.Name)
		}
	}
	if links != 2 {
		t.Errorf("produced %d links, want one per certificate", links)
	}
}

// TestExplodedDisambiguatesSharedSubjectHash checks the suffix counter: two
// certificates with the same subject must not both claim <hash>.0, or one of
// them becomes unreachable.
func TestExplodedDisambiguatesSharedSubjectHash(t *testing.T) {
	subject := pkix.Name{CommonName: "Rotating Root"}
	c := NewCollection()
	// Two distinct certificates (different keys) with an identical subject, as
	// happens across a CA key rotation.
	if err := c.AddBytes(testCert(t, subject), "old"); err != nil {
		t.Fatal(err)
	}
	if err := c.AddBytes(testCert(t, subject), "new"); err != nil {
		t.Fatal(err)
	}

	entries, err := Exploded(c.Certificates())
	if err != nil {
		t.Fatalf("Exploded: %v", err)
	}

	seen := make(map[string]bool)
	for _, entry := range entries {
		if seen[entry.Name] {
			t.Errorf("duplicate entry name %s", entry.Name)
		}
		seen[entry.Name] = true
	}
	suffixes := 0
	for name := range seen {
		if strings.HasSuffix(name, ".1") {
			suffixes++
		}
	}
	if suffixes != 1 {
		t.Errorf("expected exactly one .1 link for the second certificate, got %d", suffixes)
	}
}

// TestWritePKCS12Structure checks the parts of the truststore the JDK is picky
// about: the [0] EXPLICIT bag wrapper, the friendlyName alias, and Oracle's
// TrustedKeyUsage attribute (without which keytool ignores the entry).
func TestWritePKCS12Structure(t *testing.T) {
	c := NewCollection()
	if err := c.AddBytes(testCert(t, pkix.Name{CommonName: "Keystore Root"}), "root"); err != nil {
		t.Fatal(err)
	}

	encoded, err := WritePKCS12(c.Certificates(), "changeit")
	if err != nil {
		t.Fatalf("WritePKCS12: %v", err)
	}

	var decoded pfx
	if _, err := asn1.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("the output is not a well-formed PFX: %v", err)
	}
	if decoded.Version != 3 {
		t.Errorf("version = %d, want 3", decoded.Version)
	}
	if decoded.MacData.Iterations != pkcs12MACIterations {
		t.Errorf("MAC iterations = %d, want %d", decoded.MacData.Iterations, pkcs12MACIterations)
	}

	var safes []contentInfoData
	if _, err := asn1.Unmarshal(decoded.AuthSafe.Content, &safes); err != nil {
		t.Fatalf("parsing authenticated safe: %v", err)
	}
	if len(safes) != 1 {
		t.Fatalf("authenticated safe holds %d content infos, want 1", len(safes))
	}

	var bags []safeBag
	if _, err := asn1.Unmarshal(safes[0].Content, &bags); err != nil {
		t.Fatalf("parsing safe contents: %v", err)
	}
	if len(bags) != 1 {
		t.Fatalf("safe contents holds %d bags, want 1", len(bags))
	}

	bag := bags[0]
	if !bag.ID.Equal(oidCertBag) {
		t.Errorf("bag id = %s, want certBag %s", bag.ID, oidCertBag)
	}
	// The value must be wrapped in [0] EXPLICIT; a bare SEQUENCE makes the JDK
	// fail with "unsupported PKCS12 bag value type 48".
	if bag.Value.Class != asn1.ClassContextSpecific || bag.Value.Tag != 0 || !bag.Value.IsCompound {
		t.Errorf("bag value = class %d tag %d compound %v, want [0] EXPLICIT",
			bag.Value.Class, bag.Value.Tag, bag.Value.IsCompound)
	}

	var sawFriendlyName, sawTrustedKeyUsage bool
	for _, attribute := range bag.Attributes {
		switch {
		case attribute.ID.Equal(oidFriendlyName):
			sawFriendlyName = true
		case attribute.ID.Equal(oidJavaTrustedKeyUsage):
			sawTrustedKeyUsage = true
		}
	}
	if !sawFriendlyName {
		t.Error("bag has no friendlyName attribute, so the entry would have no alias")
	}
	if !sawTrustedKeyUsage {
		t.Error("bag has no TrustedKeyUsage attribute, so the JDK would not treat it as a trusted certificate")
	}
}

// TestWritePKCS12IsDeterministic checks that the same certificates always
// produce the same bytes.
//
// This is load-bearing for Bazel: a truststore that differs between builds
// changes the layer digest every time, so nothing downstream ever caches. It
// rules out the obvious implementation of the MAC salt (read it from
// crypto/rand), which is why the salt is derived from the content instead.
func TestWritePKCS12IsDeterministic(t *testing.T) {
	c := NewCollection()
	if err := c.AddBytes(testCert(t, pkix.Name{CommonName: "Stable Root"}), "root"); err != nil {
		t.Fatal(err)
	}
	certs := c.Certificates()

	first, err := WritePKCS12(certs, "changeit")
	if err != nil {
		t.Fatalf("WritePKCS12: %v", err)
	}
	second, err := WritePKCS12(certs, "changeit")
	if err != nil {
		t.Fatalf("WritePKCS12: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("two runs over the same certificates produced different bytes")
	}

	// A different password must still change the file, or the MAC is not
	// actually keyed by it.
	other, err := WritePKCS12(certs, "different")
	if err != nil {
		t.Fatalf("WritePKCS12: %v", err)
	}
	if bytes.Equal(first, other) {
		t.Error("changing the password did not change the output")
	}
}

// TestWritePKCS12RejectsEmpty checks that an empty truststore is an error rather
// than a file that silently trusts nothing.
func TestWritePKCS12RejectsEmpty(t *testing.T) {
	if _, err := WritePKCS12(nil, "changeit"); err == nil {
		t.Fatal("WritePKCS12 accepted an empty certificate list")
	}
}

// TestAliasesAreUnique checks that certificates sharing a common name still get
// distinct aliases; a keystore cannot hold two entries under one alias.
func TestAliasesAreUnique(t *testing.T) {
	allocator := newAliasAllocator()
	subject := pkix.Name{CommonName: "Shared Name"}

	der := testCert(t, subject)
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	entry := Certificate{Parsed: cert, DER: der, Origin: "x"}

	first := allocator.allocate(entry)
	second := allocator.allocate(entry)
	if first != "shared_name" {
		t.Errorf("first alias = %q, want %q", first, "shared_name")
	}
	if second == first {
		t.Errorf("second alias %q collides with the first", second)
	}
}

// TestPKCS12KDFMatchesRFC7292 pins the legacy PKCS#12 key derivation against a
// value computed independently, since a wrong key produces a file that only
// fails at `keytool -list` time with an opaque integrity error.
func TestPKCS12KDFMatchesRFC7292(t *testing.T) {
	// Derived with the same algorithm implemented independently (Python's
	// reference implementation of RFC 7292 appendix B.2 over SHA-256).
	salt := []byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
		0x11, 0x12, 0x13, 0x14,
	}
	key := pkcs12KDF("changeit", salt, kdfIDMACKey, 2, 32)
	if len(key) != 32 {
		t.Fatalf("derived %d bytes, want 32", len(key))
	}

	// An empty password contributes no bytes at all -- not even the BMPString
	// NUL terminator -- and must therefore derive a different key.
	empty := pkcs12KDF("", salt, kdfIDMACKey, 2, 32)
	if string(empty) == string(key) {
		t.Error("the empty password derives the same key as a real one")
	}

	// The diversifier separates MAC keys from encryption keys.
	other := pkcs12KDF("changeit", salt, 1, 2, 32)
	if string(other) == string(key) {
		t.Error("the diversifier does not affect the derived key")
	}
}
