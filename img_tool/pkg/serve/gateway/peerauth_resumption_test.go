package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"errors"

	"sync"
	"testing"
	"time"
)

// This file pins the single most easily-missed decision in the client
// authentication code: the certificate identity check is installed as
// tls.Config.VerifyConnection, not VerifyPeerCertificate.
//
// crypto/tls documents VerifyPeerCertificate as *not* invoked on resumed
// connections, and Go's server session tickets are valid for seven days. Wired
// there, an identity removed from --allowed-client-id would keep resuming — and
// therefore keep being accepted — for a week, with nothing in the logs to show it.
// The bug is invisible without a test that actually resumes a session, so this is
// that test. It runs entirely over net.Pipe: two real TLS handshakes, no listener,
// no port.

// pipeHandshake performs a TLS handshake over an in-memory connection and returns
// the server's connection state. The connection is buffered (see memconn_test.go):
// a TLS 1.3 server sends its session tickets immediately after the handshake, and
// over an unbuffered net.Pipe that write would deadlock.
func pipeHandshake(t *testing.T, serverConfig, clientConfig *tls.Config) (tls.ConnectionState, error) {
	t.Helper()
	clientConn, serverConn := memPipe("tls")
	defer clientConn.Close()
	defer serverConn.Close()

	server := tls.Server(serverConn, serverConfig)
	client := tls.Client(clientConn, clientConfig)

	var wg sync.WaitGroup
	var clientErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		clientErr = client.Handshake()
		if clientErr != nil {
			return
		}
		// TLS 1.3 delivers the session ticket after the handshake, so read once to
		// let the client store it for the next handshake. Nothing is sent, so this
		// ends at the deadline; that is the point at which the ticket has arrived.
		_ = client.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		_, _ = client.Read(make([]byte, 1))
	}()

	serverErr := server.Handshake()
	wg.Wait()

	if serverErr != nil {
		return tls.ConnectionState{}, serverErr
	}
	if clientErr != nil {
		return tls.ConnectionState{}, clientErr
	}
	return server.ConnectionState(), nil
}

func TestPeerAuthIdentityCheckRunsOnResumedConnections(t *testing.T) {
	ca := newTestCA(t, "test client CA")
	dir := t.TempDir()
	caFile := writeFile(t, dir, "ca.crt", ca.pem)
	serverCert, serverKey, _ := ca.issue(t, leafOptions{dnsNames: []string{"gw.test"}, server: true})
	material := newTestPeerTLS(t, PeerTLSOptions{
		CertFile: writeFile(t, dir, "tls.crt", serverCert),
		KeyFile:  writeFile(t, dir, "tls.key", serverKey),
		CAFile:   caFile,
	})
	clientCertPEM, clientKeyPEM, _ := ca.issue(t, leafOptions{dnsNames: []string{"worker.bb.svc"}})
	clientCert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
	if err != nil {
		t.Fatalf("loading the client keypair: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca.pem) {
		t.Fatal("building the client's root pool")
	}
	// One cache across both handshakes is what makes the second one a resumption.
	clientConfig := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		ServerName:         "gw.test",
		RootCAs:            roots,
		Certificates:       []tls.Certificate{clientCert},
		ClientSessionCache: tls.NewLRUClientSessionCache(4),
	}

	// The allow-list the server enforces, swapped between the two handshakes.
	var (
		mu      sync.Mutex
		allowed = "worker.bb.svc"
		checks  int
	)
	auth := newTestPeerAuth(t, PeerAuthOptions{TLS: material, AllowedClientIDs: []string{"worker.bb.svc"}})
	verify := func(state tls.ConnectionState) error {
		mu.Lock()
		defer mu.Unlock()
		checks++
		if len(state.PeerCertificates) == 0 {
			return nil
		}
		for _, name := range state.PeerCertificates[0].DNSNames {
			if name == allowed {
				return nil
			}
		}
		return errPeerIdentityNotAllowed
	}
	serverConfig := material.ServerConfig(auth.ClientAuthType(), verify, []string{"http/1.1"})

	first, err := pipeHandshake(t, serverConfig, clientConfig)
	if err != nil {
		t.Fatalf("first handshake: %v", err)
	}
	if first.DidResume {
		t.Fatal("the first handshake resumed a session, so this test proves nothing")
	}
	if checks != 1 {
		t.Fatalf("identity checks after the first handshake = %d, want 1", checks)
	}

	second, err := pipeHandshake(t, serverConfig, clientConfig)
	if err != nil {
		t.Fatalf("second handshake: %v", err)
	}
	if !second.DidResume {
		t.Skip("the second handshake did not resume; the resumption path cannot be exercised here")
	}
	// The load-bearing assertion: the check ran again on the resumed connection.
	// Wired to VerifyPeerCertificate this count would still be 1.
	if checks != 2 {
		t.Errorf("identity checks after a resumed handshake = %d, want 2; the check is not running on resumptions", checks)
	}

	// Narrow the allow-list and resume again: the previously admitted identity must
	// now be refused, because the allow-list is this design's only revocation
	// mechanism.
	mu.Lock()
	allowed = "someone-else.bb.svc"
	mu.Unlock()
	if _, err := pipeHandshake(t, serverConfig, clientConfig); err == nil {
		t.Error("a resumed handshake was accepted after the identity was removed from the allow-list")
	}
}

func TestPeerAuthVerifyConnectionAllowsAnAnonymousHandshake(t *testing.T) {
	// A client with no certificate has to get through the handshake: it may still
	// authenticate with a bearer token, and an unauthenticated health probe has to
	// reach /healthz. That is why the listener uses VerifyClientCertIfGiven.
	ca := newTestCA(t, "test client CA")
	dir := t.TempDir()
	serverCert, serverKey, _ := ca.issue(t, leafOptions{dnsNames: []string{"gw.test"}, server: true})
	material := newTestPeerTLS(t, PeerTLSOptions{
		CertFile: writeFile(t, dir, "tls.crt", serverCert),
		KeyFile:  writeFile(t, dir, "tls.key", serverKey),
		CAFile:   writeFile(t, dir, "ca.crt", ca.pem),
	})
	auth := newTestPeerAuth(t, PeerAuthOptions{TLS: material, AllowedClientIDs: []string{"worker.bb.svc"}})

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.pem)
	state, err := pipeHandshake(t,
		material.ServerConfig(auth.ClientAuthType(), auth.VerifyConnection, []string{"http/1.1"}),
		&tls.Config{MinVersion: tls.VersionTLS13, ServerName: "gw.test", RootCAs: roots})
	if err != nil {
		t.Fatalf("handshake without a client certificate: %v", err)
	}
	if len(state.PeerCertificates) != 0 {
		t.Fatal("the client presented a certificate")
	}
	// ...and the request is then rejected at the request level, not at the
	// handshake.
	if _, err := auth.Authenticate(tokenRequest("")); !errors.Is(err, errNoPeerCredential) {
		t.Errorf("Authenticate error = %v, want %v", err, errNoPeerCredential)
	}
}

func TestPeerAuthVerifyConnectionRejectsADisallowedIdentity(t *testing.T) {
	ca := newTestCA(t, "test client CA")
	dir := t.TempDir()
	serverCert, serverKey, _ := ca.issue(t, leafOptions{dnsNames: []string{"gw.test"}, server: true})
	material := newTestPeerTLS(t, PeerTLSOptions{
		CertFile: writeFile(t, dir, "tls.crt", serverCert),
		KeyFile:  writeFile(t, dir, "tls.key", serverKey),
		CAFile:   writeFile(t, dir, "ca.crt", ca.pem),
	})
	auth := newTestPeerAuth(t, PeerAuthOptions{TLS: material, AllowedClientIDs: []string{"worker.bb.svc"}})

	clientCertPEM, clientKeyPEM, _ := ca.issue(t, leafOptions{dnsNames: []string{"stranger.test"}})
	clientCert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
	if err != nil {
		t.Fatalf("loading the client keypair: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.pem)

	_, err = pipeHandshake(t,
		material.ServerConfig(auth.ClientAuthType(), auth.VerifyConnection, []string{"http/1.1"}),
		&tls.Config{
			MinVersion:   tls.VersionTLS13,
			ServerName:   "gw.test",
			RootCAs:      roots,
			Certificates: []tls.Certificate{clientCert},
		})
	if err == nil {
		t.Error("the handshake was accepted with an identity outside the allow-list")
	}
}
