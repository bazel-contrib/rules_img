package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests cover the peer listener: the second socket a serving gateway can
// carry cache replication on, so that its clients and its peers are authenticated
// differently. The configuration it exists for is "clients plaintext and
// anonymous, instances mTLS to each other", which no single listener can express.

// TestPeerListenerFlagValidation covers the startup checks. They are fail-closed
// in the same way the rest of the gateway's are: a combination that could only
// half-work refuses to start rather than serving with a control that looks
// configured but is not.
func TestPeerListenerFlagValidation(t *testing.T) {
	base := serveFlags{address: "0.0.0.0", port: 8080}

	with := func(mutate func(*serveFlags)) serveFlags {
		flags := base
		mutate(&flags)
		return flags
	}

	for _, tc := range []struct {
		name  string
		flags serveFlags
		want  string // a substring of the expected error; empty means it must pass
		// check runs on the resolved flags after a passing validation, so the
		// inheritance from the client listener is asserted rather than assumed.
		check func(*testing.T, *serveFlags)
	}{{
		name:  "no peer listener asked for",
		flags: base,
	}, {
		// Its port is what a discovered peer is dialled on, so it cannot be the
		// arbitrary one a zero port would take.
		name:  "material without a port",
		flags: with(func(f *serveFlags) { f.peerClientCAFile = "/tls/ca.crt" }),
		want:  "needs an explicit --peer-port",
	}, {
		name: "half a keypair",
		flags: with(func(f *serveFlags) {
			f.peerPort = 8443
			f.peerTLSCertFile = "/tls/tls.crt"
		}),
		want: "must be given together",
	}, {
		// The configuration this whole listener exists for: anonymous plaintext
		// clients, mTLS peers. The peer listener's keypair is named explicitly
		// because the client listener has none to inherit.
		name: "plaintext clients and mTLS peers",
		flags: with(func(f *serveFlags) {
			f.peerPort = 8443
			f.peerTLSCertFile, f.peerTLSKeyFile = "/tls/tls.crt", "/tls/tls.key"
			f.peerClientCAFile = "/tls/ca.crt"
			f.allowUnauthenticatedText = true
		}),
		check: func(t *testing.T, f *serveFlags) {
			if f.peerAddress != f.address {
				t.Errorf("peer address = %q, want it to default to --address %q", f.peerAddress, f.address)
			}
		},
	}, {
		// A gateway that already serves TLS to its clients need not say so twice.
		name: "the keypair is inherited from the client listener",
		flags: with(func(f *serveFlags) {
			f.tlsCertFile, f.tlsKeyFile = "/tls/tls.crt", "/tls/tls.key"
			f.peerPort = 8443
			f.peerClientCAFile = "/tls/peer-ca.crt"
		}),
		check: func(t *testing.T, f *serveFlags) {
			if f.peerTLSCertFile != "/tls/tls.crt" || f.peerTLSKeyFile != "/tls/tls.key" {
				t.Errorf("peer keypair = %q/%q, want it inherited from --tls-cert-file", f.peerTLSCertFile, f.peerTLSKeyFile)
			}
		},
	}, {
		name: "a client CA with no certificate to present it over",
		flags: with(func(f *serveFlags) {
			f.peerPort = 8443
			f.peerClientCAFile = "/tls/ca.crt"
		}),
		want: "requires a certificate for the peer listener",
	}, {
		name: "both listeners on the same address and port",
		flags: with(func(f *serveFlags) {
			f.peerPort = 8080
			f.allowUnauthenticatedPeerListener = true
		}),
		want: "give the peer listener a port of its own",
	}, {
		// Same port, different address, is two distinct sockets.
		name: "the same port on a different address",
		flags: with(func(f *serveFlags) {
			f.peerAddress = "127.0.0.1"
			f.peerPort = 8080
		}),
	}} {
		t.Run(tc.name, func(t *testing.T) {
			flags := tc.flags
			err := flags.validatePeerListener()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("validatePeerListener() = %v, want it to pass", err)
			case tc.want == "":
				if tc.check != nil {
					tc.check(t, &flags)
				}
			case err == nil:
				t.Fatalf("validatePeerListener() passed, want an error containing %q", tc.want)
			case !strings.Contains(err.Error(), tc.want):
				t.Errorf("validatePeerListener() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// TestPeerListenerRefusesToBeAnonymousOnTheNetwork covers the one check that
// cannot be made from the flags alone. The peer listener is a write path into this
// instance's blob existence cache, and a client that inserts a fact about a blob
// that is not there makes push clients skip an upload they still owe — so an
// unauthenticated one on a reachable address needs the operator to say so.
func TestPeerListenerRefusesToBeAnonymousOnTheNetwork(t *testing.T) {
	for _, tc := range []struct {
		name    string
		address string
		allow   bool
		wantErr bool
	}{
		{name: "reachable and anonymous", address: "0.0.0.0", wantErr: true},
		{name: "reachable, anonymous, and declared safe", address: "0.0.0.0", allow: true},
		{name: "loopback needs no declaration", address: "localhost"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flags := serveFlags{
				address:                          tc.address,
				port:                             8080,
				peerAddress:                      tc.address,
				peerPort:                         8443,
				allowUnauthenticatedPeerListener: tc.allow,
			}
			if err := flags.validatePeerListener(); err != nil {
				t.Fatalf("validatePeerListener() = %v", err)
			}
			listener, err := flags.buildPeerListener(nil)
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("buildPeerListener() opened an anonymous listener on %s, want it refused", tc.address)
			case tc.wantErr:
				if !strings.Contains(err.Error(), "without authenticating its peers") {
					t.Errorf("buildPeerListener() = %v, want it to say the peers are unauthenticated", err)
				}
			case err != nil:
				t.Fatalf("buildPeerListener() = %v, want it to pass", err)
			case !listener.enabled():
				t.Fatal("buildPeerListener() returned no listener")
			}
		})
	}
}

// TestPeerListenerServesMTLSWhileClientsGoPlaintext is the end-to-end check of the
// configuration this listener exists for. It assembles the peer listener's real
// server and drives real TLS handshakes over it: a peer presenting a certificate
// from the CA is served, a certificate from anywhere else is refused by the
// handshake, and one with no certificate still connects (so the health probe is
// answerable) and is left to the handler to authenticate — while the client
// listener alongside it is plain HTTP with no credential at all.
func TestPeerListenerServesMTLSWhileClientsGoPlaintext(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM, caPEM := testKeypair(t)
	flags := serveFlags{
		address:          "0.0.0.0",
		port:             8080,
		peerPort:         8443,
		peerTLSCertFile:  writeTempFileIn(t, dir, "tls.crt", certPEM),
		peerTLSKeyFile:   writeTempFileIn(t, dir, "tls.key", keyPEM),
		peerClientCAFile: writeTempFileIn(t, dir, "ca.crt", caPEM),
		// The clients: anonymous, plaintext, network-reachable.
		allowUnauthenticatedText: true,
	}
	if err := flags.validatePeerListener(); err != nil {
		t.Fatalf("validatePeerListener() = %v", err)
	}
	peers, err := flags.buildPeerListener(nil)
	if err != nil {
		t.Fatalf("buildPeerListener() = %v", err)
	}

	server, serveOn := peers.server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "peer\n")
	}))
	if server.TLSConfig == nil {
		t.Fatal("the peer listener assembled no TLS config")
	}
	// A client certificate is requested and verified, which is exactly what the
	// client listener does not do.
	if got := server.TLSConfig.ClientAuth; got != tls.VerifyClientCertIfGiven {
		t.Errorf("peer listener ClientAuth = %v, want VerifyClientCertIfGiven", got)
	}

	socket := newPipeListener()
	defer socket.Close()
	go func() { _ = serveOn(socket) }()
	defer server.Close()

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("the CA bundle holds no certificates")
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("loading the client keypair: %v", err)
	}

	for _, tc := range []struct {
		name        string
		certificate []tls.Certificate
		wantBody    string
	}{
		{name: "a peer with a certificate from the CA", certificate: []tls.Certificate{pair}, wantBody: "peer"},
		// No certificate still completes the handshake — that is what keeps the
		// health probe answerable — but the handler authenticates the request, which
		// this bare handler stands in for.
		{name: "a peer with no certificate", wantBody: "peer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := socket.dial()
			if err != nil {
				t.Fatalf("dialling the peer listener: %v", err)
			}
			defer conn.Close()
			client := tls.Client(conn, &tls.Config{
				RootCAs:      pool,
				ServerName:   "oci-distribution-gateway.test",
				Certificates: tc.certificate,
				MinVersion:   tls.VersionTLS13,
			})
			if err := client.Handshake(); err != nil {
				t.Fatalf("TLS handshake: %v", err)
			}
			if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatalf("setting a deadline: %v", err)
			}
			if _, err := io.WriteString(client, "GET /healthz HTTP/1.1\r\nHost: peer\r\nConnection: close\r\n\r\n"); err != nil {
				t.Fatalf("writing the request: %v", err)
			}
			response, err := io.ReadAll(client)
			if err != nil {
				t.Fatalf("reading the response: %v", err)
			}
			if !strings.Contains(string(response), tc.wantBody) {
				t.Errorf("response = %q, want it to contain %q", response, tc.wantBody)
			}
		})
	}

	// And a peer whose certificate is signed by somebody else is refused by the
	// handshake, before any request is read.
	otherCert, otherKey, _ := testKeypair(t)
	other, err := tls.X509KeyPair(otherCert, otherKey)
	if err != nil {
		t.Fatalf("loading the foreign keypair: %v", err)
	}
	conn, err := socket.dial()
	if err != nil {
		t.Fatalf("dialling the peer listener: %v", err)
	}
	defer conn.Close()
	client := tls.Client(conn, &tls.Config{
		RootCAs:      pool,
		ServerName:   "oci-distribution-gateway.test",
		Certificates: []tls.Certificate{other},
		MinVersion:   tls.VersionTLS13,
	})
	if err := client.Handshake(); err != nil {
		// TLS 1.3 reports the server's rejection of the client certificate on the
		// first read rather than in Handshake, so either is the expected outcome.
		return
	}
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(client, "GET /healthz HTTP/1.1\r\nHost: peer\r\nConnection: close\r\n\r\n"); err == nil {
		if body, err := io.ReadAll(client); err == nil && strings.Contains(string(body), "peer") {
			t.Error("the peer listener served a client whose certificate the CA did not sign")
		}
	}
}

// TestReplicationFollowsThePeerListener checks the consequence of the split that
// is easiest to get wrong: with a peer listener, the hop between instances is that
// listener's — its port is the one a discovered peer is dialled on, and its TLS
// decides whether the hop is https. The client listener's plaintext must not make
// the gateway refuse (or downgrade) a replication hop that is in fact encrypted.
func TestReplicationFollowsThePeerListener(t *testing.T) {
	// A gateway serving plaintext to its clients, with peer discovery. Without a
	// peer listener this cannot work: replication would be plaintext too.
	flags := serveFlags{
		address:            "0.0.0.0",
		port:               8080,
		blobCacheTTL:       time.Hour,
		blobCacheMaxMemory: defaultBlobCacheMemory,
		cachePeerService:   "img-gateway/gw",
	}
	if _, err := flags.cacheReplication(nil, nil); err == nil || !strings.Contains(err.Error(), "serves replication over plaintext") {
		t.Fatalf("cacheReplication() without a peer listener = %v, want it to refuse a plaintext hop", err)
	}

	// The same gateway with a TLS peer listener: the hop is that listener's, so it
	// is https and it is dialled on --peer-port. Discovery itself needs a pod's
	// environment, which a test is not — reaching that error is the assertion that
	// the plaintext and port checks both passed.
	dir := t.TempDir()
	certPEM, keyPEM, caPEM := testKeypair(t)
	flags.peerPort = 8443
	flags.peerTLSCertFile = writeTempFileIn(t, dir, "tls.crt", certPEM)
	flags.peerTLSKeyFile = writeTempFileIn(t, dir, "tls.key", keyPEM)
	flags.peerClientCAFile = writeTempFileIn(t, dir, "ca.crt", caPEM)
	if err := flags.validatePeerListener(); err != nil {
		t.Fatalf("validatePeerListener() = %v", err)
	}
	peers, err := flags.buildPeerListener(nil)
	if err != nil {
		t.Fatalf("buildPeerListener() = %v", err)
	}
	if !peers.servesTLS() {
		t.Fatal("the peer listener does not serve TLS despite a keypair")
	}
	if _, err := flags.cacheReplication(nil, peers); err == nil || !strings.Contains(err.Error(), "Kubernetes pod") {
		t.Fatalf("cacheReplication() with a TLS peer listener = %v, want it past the plaintext and port checks", err)
	}
}

// pipeListener is a net.Listener over in-memory connections, so a test can drive
// a real TLS handshake and a real HTTP server without binding a port (which this
// repository's tests deliberately never do).
//
// The connections buffer rather than being a net.Pipe, and that is load-bearing:
// net.Pipe is synchronous and unbuffered, so any exchange where both sides write
// before either reads deadlocks — and a TLS 1.3 server sends its session tickets
// the moment the handshake completes. A socketpair would do too, but syscall.
// Socketpair does not exist on Windows, and this package's tests run there.
// //pkg/serve/gateway has the same helper for the same reason; it is not importable
// from here, since it lives in that package's own test sources.
type pipeListener struct {
	conns   chan net.Conn
	closing chan struct{}
	once    sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), closing: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closing:
		return nil, net.ErrClosed
	}
}

// dial opens a connection to the listener and hands the far end to Accept.
func (l *pipeListener) dial() (net.Conn, error) {
	toServer, toClient := newPipeBuffer(), newPipeBuffer()
	client := &pipeConn{read: toClient, write: toServer, local: pipeAddr("peer-client"), peer: pipeAddr("peer-listener")}
	server := &pipeConn{read: toServer, write: toClient, local: pipeAddr("peer-listener"), peer: pipeAddr("peer-client")}
	select {
	case l.conns <- server:
		return client, nil
	case <-l.closing:
		client.Close()
		server.Close()
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closing) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr("peer-listener") }

type pipeAddr string

func (pipeAddr) Network() string  { return "pipe" }
func (a pipeAddr) String() string { return string(a) }

// pipeBuffer is one direction of a pipeConn: an unbounded byte queue that blocks
// readers until data arrives, the writer closes, or the deadline passes.
type pipeBuffer struct {
	mu       sync.Mutex
	cond     *sync.Cond
	buf      bytes.Buffer
	closed   bool
	deadline time.Time
	timer    *time.Timer
}

func newPipeBuffer() *pipeBuffer {
	b := &pipeBuffer{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *pipeBuffer) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.buf.Len() == 0 {
		if b.closed {
			return 0, io.EOF
		}
		if b.expiredLocked() {
			return 0, os.ErrDeadlineExceeded
		}
		b.cond.Wait()
	}
	return b.buf.Read(p)
}

func (b *pipeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, net.ErrClosed
	}
	n, err := b.buf.Write(p)
	b.cond.Broadcast()
	return n, err
}

func (b *pipeBuffer) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.cond.Broadcast()
	return nil
}

// setDeadline wakes any blocked reader once t has passed.
func (b *pipeBuffer) setDeadline(t time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deadline = t
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	if t.IsZero() {
		return
	}
	if delay := time.Until(t); delay > 0 {
		b.timer = time.AfterFunc(delay, func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.cond.Broadcast()
		})
	}
	b.cond.Broadcast()
}

func (b *pipeBuffer) expiredLocked() bool {
	return !b.deadline.IsZero() && !time.Now().Before(b.deadline)
}

// pipeConn is one end of an in-memory connection.
type pipeConn struct {
	read  *pipeBuffer
	write *pipeBuffer
	local pipeAddr
	peer  pipeAddr
	once  sync.Once
}

func (c *pipeConn) Read(p []byte) (int, error)  { return c.read.Read(p) }
func (c *pipeConn) Write(p []byte) (int, error) { return c.write.Write(p) }
func (c *pipeConn) LocalAddr() net.Addr         { return c.local }
func (c *pipeConn) RemoteAddr() net.Addr        { return c.peer }

func (c *pipeConn) Close() error {
	c.once.Do(func() {
		// Closing both directions models a TCP close: the peer's reads end too.
		_ = c.write.Close()
		_ = c.read.Close()
	})
	return nil
}

func (c *pipeConn) SetDeadline(t time.Time) error {
	c.read.setDeadline(t)
	c.write.setDeadline(t)
	return nil
}

func (c *pipeConn) SetReadDeadline(t time.Time) error  { c.read.setDeadline(t); return nil }
func (c *pipeConn) SetWriteDeadline(t time.Time) error { c.write.setDeadline(t); return nil }
