package gateway

import (
	"crypto/tls"
	"io"
	"log"
	"testing"
	"time"
)

func newTestPeerTLS(t *testing.T, opts PeerTLSOptions) *PeerTLS {
	t.Helper()
	opts.Logger = log.New(io.Discard, "", 0)
	tlsMaterial, err := NewPeerTLS(opts)
	if err != nil {
		t.Fatalf("NewPeerTLS: %v", err)
	}
	return tlsMaterial
}

// servedSerial reports the serial number of the certificate currently served.
func servedSerial(t *testing.T, material *PeerTLS) int64 {
	t.Helper()
	cert, err := material.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if cert.Leaf == nil {
		t.Fatal("the served certificate has no parsed leaf")
	}
	return cert.Leaf.SerialNumber.Int64()
}

func TestPeerTLSReloadsARotatedKeypair(t *testing.T) {
	ca := newTestCA(t, "test CA")
	dir := t.TempDir()
	certPEM, keyPEM, _ := ca.issue(t, leafOptions{serial: 10, dnsNames: []string{"gw.test"}, server: true})
	certFile := writeFile(t, dir, "tls.crt", certPEM)
	keyFile := writeFile(t, dir, "tls.key", keyPEM)

	material := newTestPeerTLS(t, PeerTLSOptions{CertFile: certFile, KeyFile: keyFile})
	if got := servedSerial(t, material); got != 10 {
		t.Fatalf("served serial = %d, want 10", got)
	}

	rotatedCert, rotatedKey, _ := ca.issue(t, leafOptions{serial: 11, dnsNames: []string{"gw.test"}, server: true})
	writeFile(t, dir, "tls.crt", rotatedCert)
	writeFile(t, dir, "tls.key", rotatedKey)
	if err := material.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := servedSerial(t, material); got != 11 {
		t.Errorf("served serial = %d, want the rotated 11", got)
	}
}

func TestPeerTLSKeepsThePreviousCertificateOnAFailedReload(t *testing.T) {
	// Kubernetes swaps the whole "..data" directory symlink, and that swap can land
	// between the two reads LoadX509KeyPair performs — so a mismatched pair is a
	// routine event, not a corruption. Serving the previous certificate is the only
	// safe response; returning nothing would take the listener down.
	ca := newTestCA(t, "test CA")
	other := newTestCA(t, "another CA")
	dir := t.TempDir()
	certPEM, keyPEM, _ := ca.issue(t, leafOptions{serial: 20, dnsNames: []string{"gw.test"}, server: true})
	certFile := writeFile(t, dir, "tls.crt", certPEM)
	keyFile := writeFile(t, dir, "tls.key", keyPEM)
	material := newTestPeerTLS(t, PeerTLSOptions{CertFile: certFile, KeyFile: keyFile})

	for _, tc := range []struct {
		name   string
		mutate func()
	}{{
		name:   "corrupt key",
		mutate: func() { writeFile(t, dir, "tls.key", []byte("not a key")) },
	}, {
		// The torn read: a new certificate paired with the old key.
		name: "mismatched pair",
		mutate: func() {
			mismatched, _, _ := other.issue(t, leafOptions{serial: 99, dnsNames: []string{"gw.test"}, server: true})
			writeFile(t, dir, "tls.crt", mismatched)
			writeFile(t, dir, "tls.key", keyPEM)
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			tc.mutate()
			if err := material.Reload(); err == nil {
				t.Fatal("Reload accepted broken material")
			}
			cert, err := material.GetCertificate(&tls.ClientHelloInfo{})
			if err != nil || cert == nil {
				t.Fatalf("GetCertificate after a failed reload: cert=%v err=%v, want the previous certificate", cert, err)
			}
			if got := servedSerial(t, material); got != 20 {
				t.Errorf("served serial = %d, want the previous 20", got)
			}
			// Restore for the next case.
			writeFile(t, dir, "tls.crt", certPEM)
			writeFile(t, dir, "tls.key", keyPEM)
		})
	}
}

func TestPeerTLSCountsReloadFailures(t *testing.T) {
	// Keeping the previous material is right, but it is also what makes a
	// persistently broken file invisible until the certificate expires. So the
	// outcome is reported, not just logged.
	ca := newTestCA(t, "test CA")
	dir := t.TempDir()
	certPEM, keyPEM, _ := ca.issue(t, leafOptions{dnsNames: []string{"gw.test"}, server: true})
	certFile := writeFile(t, dir, "tls.crt", certPEM)
	keyFile := writeFile(t, dir, "tls.key", keyPEM)

	var reloads []string
	var failures int
	material := newTestPeerTLS(t, PeerTLSOptions{
		CertFile: certFile, KeyFile: keyFile,
		OnReload: func(material string, err error) {
			reloads = append(reloads, material)
			if err != nil {
				failures++
			}
		},
	})
	if len(reloads) != 1 || reloads[0] != materialCertificate {
		t.Fatalf("reload hook calls = %v, want one for %q", reloads, materialCertificate)
	}

	writeFile(t, dir, "tls.key", []byte("broken"))
	_ = material.Reload()
	if failures != 1 {
		t.Errorf("reported reload failures = %d, want 1", failures)
	}
}

func TestPeerTLSClientCertificateSharesTheReloader(t *testing.T) {
	ca := newTestCA(t, "test CA")
	dir := t.TempDir()
	certPEM, keyPEM, _ := ca.issue(t, leafOptions{serial: 30, dnsNames: []string{"client.test"}})
	material := newTestPeerTLS(t, PeerTLSOptions{
		CertFile: writeFile(t, dir, "tls.crt", certPEM),
		KeyFile:  writeFile(t, dir, "tls.key", keyPEM),
	})

	clientCert, err := material.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		t.Fatalf("GetClientCertificate: %v", err)
	}
	if clientCert.Leaf == nil || clientCert.Leaf.SerialNumber.Int64() != 30 {
		t.Errorf("GetClientCertificate returned %v, want the loaded certificate", clientCert.Leaf)
	}
}

func TestPeerTLSWithoutACertificateOffersNone(t *testing.T) {
	// A forwarder authenticating by bearer token alone still has to complete the
	// handshake, so "no certificate" must be an empty certificate rather than an
	// error.
	material := newTestPeerTLS(t, PeerTLSOptions{})
	cert, err := material.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		t.Fatalf("GetClientCertificate: %v", err)
	}
	if len(cert.Certificate) != 0 {
		t.Errorf("GetClientCertificate returned a certificate, want an empty one")
	}
	if _, err := material.GetCertificate(&tls.ClientHelloInfo{}); err == nil {
		t.Error("GetCertificate succeeded without a configured keypair")
	}
}

func TestPeerTLSRotatesTheCABundle(t *testing.T) {
	// x509.CertPool is append-only and http.Server clones its TLSConfig once, so a
	// trust anchor can only be *removed* by handing out a fresh config per
	// handshake. Without that, rotating a CA would mean restarting every worker
	// pod's sidecar.
	first := newTestCA(t, "first CA")
	second := newTestCA(t, "second CA")
	dir := t.TempDir()
	caFile := writeFile(t, dir, "ca.crt", first.pem)
	material := newTestPeerTLS(t, PeerTLSOptions{CAFile: caFile})

	config := material.ServerConfig(tls.VerifyClientCertIfGiven, nil, []string{"h2", "http/1.1"})
	before, err := config.GetConfigForClient(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetConfigForClient: %v", err)
	}
	if before.ClientCAs == nil {
		t.Fatal("the handshake config has no client CAs")
	}
	// Recursion guard: the config handed out must not point back at itself.
	if before.GetConfigForClient != nil {
		t.Error("the handshake config still has GetConfigForClient set")
	}
	// ALPN has to be carried across explicitly: ServeTLS only fixes up the config
	// it clones, so a missing "h2" here would silently give up HTTP/2.
	if len(before.NextProtos) == 0 || before.NextProtos[0] != "h2" {
		t.Errorf("handshake config NextProtos = %v, want h2 first", before.NextProtos)
	}

	writeFile(t, dir, "ca.crt", second.pem)
	if err := material.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	after, err := config.GetConfigForClient(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetConfigForClient after reload: %v", err)
	}
	if after.ClientCAs.Equal(before.ClientCAs) {
		t.Error("the rotated CA bundle did not reach a new handshake")
	}
}

func TestPeerTLSRejectsAnIncompleteKeypair(t *testing.T) {
	if _, err := NewPeerTLS(PeerTLSOptions{CertFile: "cert.pem"}); err == nil {
		t.Error("NewPeerTLS accepted a certificate without a key")
	}
	if _, err := NewPeerTLS(PeerTLSOptions{KeyFile: "key.pem"}); err == nil {
		t.Error("NewPeerTLS accepted a key without a certificate")
	}
}

func TestPeerTLSRejectsABrokenCABundleAtStartup(t *testing.T) {
	// At startup a bad file is fatal: the process must not come up believing it can
	// verify clients when it cannot.
	dir := t.TempDir()
	if _, err := NewPeerTLS(PeerTLSOptions{CAFile: writeFile(t, dir, "ca.crt", []byte("junk"))}); err == nil {
		t.Error("NewPeerTLS accepted a CA bundle with no certificates")
	}
	if _, err := NewPeerTLS(PeerTLSOptions{CAFile: writeFile(t, dir, "empty.crt", nil)}); err == nil {
		t.Error("NewPeerTLS accepted an empty CA bundle")
	}
}

func TestPeerTLSWatchPicksUpAChange(t *testing.T) {
	ca := newTestCA(t, "test CA")
	dir := t.TempDir()
	certPEM, keyPEM, _ := ca.issue(t, leafOptions{serial: 40, dnsNames: []string{"gw.test"}, server: true})
	certFile := writeFile(t, dir, "tls.crt", certPEM)
	keyFile := writeFile(t, dir, "tls.key", keyPEM)
	material := newTestPeerTLS(t, PeerTLSOptions{CertFile: certFile, KeyFile: keyFile})

	done := make(chan struct{})
	defer close(done)
	go material.Watch(done, time.Millisecond)

	rotatedCert, rotatedKey, _ := ca.issue(t, leafOptions{serial: 41, dnsNames: []string{"gw.test"}, server: true})
	writeFile(t, dir, "tls.crt", rotatedCert)
	writeFile(t, dir, "tls.key", rotatedKey)
	// Polling compares size and modification time; the sizes here are equal, so
	// bump the timestamps rather than relying on filesystem granularity.
	touch(t, certFile)
	touch(t, keyFile)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if servedSerial(t, material) == 41 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Errorf("the watcher did not pick up the rotated certificate (serving serial %d)", servedSerial(t, material))
}
