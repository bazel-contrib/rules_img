package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/serve/gateway"
)

// This file implements the *peer listener*: a second socket carrying nothing but
// the blob existence cache replication the instances of a serving deployment do
// between themselves.
//
// One listener cannot serve two transports. TLS decides per listener whether a
// client certificate is requested at all, so a deployment whose build clients
// speak plaintext HTTP with no credential and whose instances authenticate each
// other with mTLS has no single socket that can be both. Splitting them is what
// makes that combination expressible: --address/--port stays exactly as it was,
// and --peer-address/--peer-port gets its own TLS material and its own client
// authentication.
//
// The split is not only about transports. The replication endpoints are a write
// path into this instance's cache, and a false "this blob is there" makes a push
// client skip an upload it still owes — so on a gateway whose clients are
// anonymous, those endpoints must not be on the clients' listener at all. Enabling
// the peer listener therefore *moves* them: the client listener answers
// /_rules_img/cache/ with 404 from then on (see
// [gateway.Handler.SeparateReplicationHandler]).

// peerListener is the resolved configuration of the second listener, or nil when
// it is not enabled.
type peerListener struct {
	address string
	port    int

	// tls is the listener's own TLS material. It is also what this instance
	// presents to its peers as a client, so a symmetric deployment needs one
	// keypair and one CA and nothing else.
	tls *gateway.PeerTLS
	// auth authenticates the peers, or nil when the operator declared the listener
	// safe without it.
	auth *gateway.PeerAuth
}

// enabled reports whether the peer listener is configured. It is written to
// tolerate a nil receiver so callers need no second code path.
func (p *peerListener) enabled() bool { return p != nil }

// servesTLS reports whether the peer listener serves TLS, which is also the
// scheme its peers are reached on.
func (p *peerListener) servesTLS() bool {
	return p != nil && p.tls != nil && p.tls.HasCertificate()
}

// peerListenerRequested reports whether any flag asks for a peer listener. It is
// separate from building one so that a flag combination that cannot work is an
// error rather than a listener that quietly does not appear.
func (f *serveFlags) peerListenerRequested() bool {
	return f.peerAddress != "" || f.peerPort != 0 ||
		f.peerTLSCertFile != "" || f.peerTLSKeyFile != "" || f.peerClientCAFile != "" ||
		f.allowUnauthenticatedPeerListener
}

// validatePeerListener checks the peer listener flags and fills in what they
// inherit from the client listener. The inheritance is the point: a symmetric
// deployment that only wants "clients plaintext, peers mTLS" sets --peer-port and
// --peer-client-ca-file and nothing else.
func (f *serveFlags) validatePeerListener() error {
	if !f.peerListenerRequested() {
		return nil
	}
	if f.peerPort == 0 {
		return errors.New("the peer listener needs an explicit --peer-port: it is the port a discovered peer is reached on, so every instance must serve it on the same one")
	}
	if (f.peerTLSCertFile == "") != (f.peerTLSKeyFile == "") {
		return errors.New("--peer-tls-cert-file and --peer-tls-key-file must be given together")
	}
	// Inherit from the client listener where nothing was said. A gateway that
	// serves TLS to its clients almost always serves the same certificate to its
	// peers; one that does not can name a different keypair.
	if f.peerTLSCertFile == "" {
		f.peerTLSCertFile, f.peerTLSKeyFile = f.tlsCertFile, f.tlsKeyFile
	}
	if f.peerClientCAFile == "" {
		f.peerClientCAFile = f.clientCAFile
	}
	if f.peerAddress == "" {
		f.peerAddress = f.address
	}
	if f.peerClientCAFile != "" && f.peerTLSCertFile == "" {
		return errors.New("--peer-client-ca-file requires a certificate for the peer listener (--peer-tls-cert-file, or --tls-cert-file to reuse the client listener's): a client certificate can only be presented over TLS")
	}
	// Two listeners on one address and one port is a bind error at best and, if
	// SO_REUSEPORT were ever in play, two sockets sharing the traffic at worst.
	if f.unixSocket == "" && f.peerPort == f.port && f.peerAddress == f.address {
		return fmt.Errorf("--peer-port %d is the port the client listener already serves on %s; give the peer listener a port of its own", f.peerPort, f.address)
	}
	return nil
}

// buildPeerListener loads the peer listener's TLS material and client
// authentication. It returns nil when no peer listener was asked for.
//
// The peers' allow-list is deliberately the same --allowed-cache-peer-id that
// gates writing to the cache: with a listener of their own there is no second
// population to distinguish, and giving it a separate identity flag would only
// make two places to keep in sync.
func (f *serveFlags) buildPeerListener(onReload func(material string, err error)) (*peerListener, error) {
	if !f.peerListenerRequested() {
		return nil, nil
	}
	listener := &peerListener{address: f.peerAddress, port: f.peerPort}

	var err error
	if f.peerTLSCertFile != "" || f.peerClientCAFile != "" {
		listener.tls, err = gateway.NewPeerTLS(gateway.PeerTLSOptions{
			CertFile: f.peerTLSCertFile,
			KeyFile:  f.peerTLSKeyFile,
			CAFile:   f.peerClientCAFile,
			OnReload: onReload,
		})
		if err != nil {
			return nil, fmt.Errorf("loading the peer listener's TLS material: %w", err)
		}
	}
	if f.peerClientCAFile != "" {
		listener.auth, err = gateway.NewPeerAuth(gateway.PeerAuthOptions{
			TLS: listener.tls,
			// The peers are the only clients of this listener, so who may talk to it
			// and who may write to the cache are one and the same question.
			AllowedClientIDs: f.allowedCachePeerIDs,
			OnReload:         onReload,
		})
		if err != nil {
			return nil, fmt.Errorf("configuring authentication on the peer listener: %w", err)
		}
	}
	// The peer listener is a write path into this instance's cache. Refuse to open
	// an unauthenticated one on a network-reachable address unless the operator
	// says so: a client that can insert a fact about a blob that is not there makes
	// push clients skip an upload they still owe.
	if listener.auth == nil && reachableFromNetwork("", f.peerAddress) && !f.allowUnauthenticatedPeerListener {
		return nil, fmt.Errorf("refusing to serve the peer listener on %s:%d without authenticating its peers.\n"+
			"Configure --peer-client-ca-file (with --allowed-cache-peer-id), bind a loopback address,\n"+
			"or pass --dangerously-allow-unauthenticated-peer-listener if a service mesh authenticates the hop",
			f.peerAddress, f.peerPort)
	}
	return listener, nil
}

// serve starts the peer listener and returns it, so the caller can shut it down
// with the rest of the process. handler is what
// [gateway.Handler.SeparateReplicationHandler] returned.
func (p *peerListener) serve(handler http.Handler) (*gatewayListener, error) {
	socket, _, err := listen("", p.address, p.port, 0)
	if err != nil {
		return nil, fmt.Errorf("listening for peers on %s:%d: %w", p.address, p.port, err)
	}
	server, serveOn := p.server(handler)
	return &gatewayListener{server: server, serve: func() error { return serveOn(socket) }, addr: socket.Addr()}, nil
}

// server assembles the peer listener's HTTP server and the call that serves a
// socket with it — ServeTLS when this listener speaks TLS, Serve when it does not.
// It is separate from [peerListener.serve] so the assembly can be checked without
// binding anything.
func (p *peerListener) server(handler http.Handler) (*http.Server, func(net.Listener) error) {
	// Replication is a handful of small JSON requests, not blob traffic, so the
	// bounds here are ordinary. HTTP/2 is offered over ALPN because a donation of
	// tens of thousands of entries is one long stream and multiplexing it with the
	// event batches costs nothing; plaintext h2c is deliberately not offered, since
	// nothing on this listener needs it.
	server := &http.Server{
		Handler:           handler,
		Protocols:         serveProtocols(false, false),
		HTTP2:             serveHTTP2Config(),
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       5 * time.Minute,
	}
	if !p.servesTLS() {
		return server, server.Serve
	}
	clientAuth := tls.NoClientCert
	var verify func(tls.ConnectionState) error
	if p.auth != nil {
		clientAuth = p.auth.ClientAuthType()
		verify = p.auth.VerifyConnection
	}
	server.TLSConfig = p.tls.ServerConfig(clientAuth, verify, nextProtosFor(server.Protocols))
	// The empty paths are correct: ServeTLS accepts them when the config can
	// produce a certificate, which ours does through GetCertificate.
	return server, func(socket net.Listener) error { return server.ServeTLS(socket, "", "") }
}

// watch keeps the peer listener's on-disk material fresh, mirroring what the
// client listener does with its own.
func (p *peerListener) watch(done <-chan struct{}) {
	if p == nil {
		return
	}
	if p.tls != nil {
		go p.tls.Watch(done, 0)
	}
	if p.auth != nil {
		go p.auth.Watch(done, 0)
	}
}

// reload re-reads the peer listener's material on SIGHUP. A failed reload keeps
// what is in force, like every other reload here.
func (p *peerListener) reload() {
	if p == nil {
		return
	}
	if p.tls != nil {
		if err := p.tls.Reload(); err != nil {
			log.Printf("peer listener TLS material reload FAILED, keeping previous material: %v", err)
		}
	}
	if p.auth != nil {
		if err := p.auth.Reload(); err != nil {
			log.Printf("peer listener token reload FAILED, keeping previous tokens: %v", err)
		}
	}
}

// summary describes the peer listener for the startup banner.
func (p *peerListener) summary() string {
	transport := "plaintext"
	if p.servesTLS() {
		transport = "TLS"
	}
	auth := "no client authentication"
	if p.auth != nil {
		auth = "mtls"
		if methods := p.auth.Methods(); len(methods) > 0 {
			auth = methods[0]
		}
	}
	return fmt.Sprintf("%s, %s", transport, auth)
}
