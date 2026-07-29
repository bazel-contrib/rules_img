package main

import (
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"

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
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// testTCPAddr builds a *net.TCPAddr without resolving anything.
type testTCPAddr struct {
	ip   string
	port int
}

func (a *testTCPAddr) addr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP(a.ip), Port: a.port}
}
