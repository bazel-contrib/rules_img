package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/serve/gateway"
)

// Helpers shared by the tests in this package.

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing %q: %v", raw, err)
	}
	return u
}

// newEmptyPeerTLS returns TLS material with nothing configured, which is what a
// forwarder that authenticates with a bearer token alone uses.
func newEmptyPeerTLS(t *testing.T) *gateway.PeerTLS {
	t.Helper()
	material, err := gateway.NewPeerTLS(gateway.PeerTLSOptions{})
	if err != nil {
		t.Fatalf("NewPeerTLS: %v", err)
	}
	return material
}

func writeTempFile(t *testing.T, name, contents string) string {
	t.Helper()
	return writeTempFileIn(t, t.TempDir(), name, []byte(contents))
}

// writeTempFileIn writes into a directory the caller chose, for a test that needs
// several files side by side.
func writeTempFileIn(t *testing.T, dir, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// testKeypair mints a throwaway self-signed server certificate and returns it as
// PEM, together with the key and a CA bundle that verifies it (itself, since it is
// self-signed). It exists so a test can hand the peer listener real TLS material
// without a fixture to rotate.
func testKeypair(t *testing.T) (certPEM, keyPEM, caPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "oci-distribution-gateway.test"},
		DNSNames:              []string{"oci-distribution-gateway.test"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshalling key: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, certPEM
}

// testTCPAddr builds a *net.TCPAddr without resolving anything.
type testTCPAddr struct {
	ip   string
	port int
}

func (a *testTCPAddr) addr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP(a.ip), Port: a.port}
}
