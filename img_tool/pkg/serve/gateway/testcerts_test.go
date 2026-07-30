package gateway

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// This file mints throwaway certificates for the TLS and client-authentication
// tests. It is a test helper on purpose: the gateway itself never creates
// certificates, and generating them here keeps the tests hermetic (no fixtures to
// rotate, nothing to bind, no network).

// testCA is a self-signed CA that can issue leaves.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

// newTestCA creates a CA valid for an hour.
func newTestCA(t *testing.T, commonName string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating CA certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing CA certificate: %v", err)
	}
	return &testCA{
		cert: cert,
		key:  key,
		pem:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

// leafOptions describes a leaf certificate to issue.
type leafOptions struct {
	commonName string
	dnsNames   []string
	uris       []string
	// serial distinguishes two certificates for the same name, so a test can tell
	// a reloaded keypair from the one it replaced.
	serial int64
	// notBefore and notAfter default to a one-hour window around now.
	notBefore, notAfter time.Time
	// server issues a server certificate instead of a client one.
	server bool
}

// issue returns a leaf signed by the CA, as PEM certificate and key.
func (ca *testCA) issue(t *testing.T, opts leafOptions) (certPEM, keyPEM []byte, cert *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating leaf key: %v", err)
	}
	if opts.serial == 0 {
		opts.serial = 2
	}
	if opts.notBefore.IsZero() {
		opts.notBefore = time.Now().Add(-time.Minute)
	}
	if opts.notAfter.IsZero() {
		opts.notAfter = time.Now().Add(time.Hour)
	}
	usage := x509.ExtKeyUsageClientAuth
	if opts.server {
		usage = x509.ExtKeyUsageServerAuth
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(opts.serial),
		Subject:      pkix.Name{CommonName: opts.commonName},
		NotBefore:    opts.notBefore,
		NotAfter:     opts.notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		DNSNames:     opts.dnsNames,
	}
	for _, raw := range opts.uris {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parsing URI SAN %q: %v", raw, err)
		}
		template.URIs = append(template.URIs, u)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("creating leaf certificate: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing leaf certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshalling leaf key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		parsed
}

// writeFile writes data into dir/name and returns the path.
func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// touch bumps a file's modification time far enough into the future that a
// change poll notices it without the test having to sleep.
func touch(t *testing.T, path string) {
	t.Helper()
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("touching %s: %v", path, err)
	}
}
