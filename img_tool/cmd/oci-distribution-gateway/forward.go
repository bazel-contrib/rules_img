package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/serve/gateway"
)

// forwardFlags holds the flags of the forwarding mode. Notably absent, on
// purpose: --policy-file, --dangerously-allow-all, --default-registry and
// --credential-helper. A forwarder authorizes nothing and holds no registry
// credentials — the serving gateway it relays to does both, which is the whole
// point of running one.
type forwardFlags struct {
	peer           string
	address        string
	port           int
	unixSocket     string
	unixSocketMode string
	forwarderID    string

	peerCAFile     string
	peerServerName string
	peerCertFile   string
	peerKeyFile    string
	peerTokenFile  string

	allowPlaintextPeer       bool
	allowAnonymousPeer       bool
	skipPeerVerification     bool
	allowUnauthenticatedText bool

	shutdownTimeout time.Duration
	metrics         metricsFlags
}

func (f *forwardFlags) register(flagSet *flag.FlagSet) {
	flagSet.StringVar(&f.peer, "peer", "", "Serving gateway to relay to: https://host[:port] (HTTP/2 over TLS) or http://host[:port] (plaintext h2c). Required.")
	flagSet.StringVar(&f.address, "address", "localhost", "Address to bind the forwarder to (ignored when --unix-socket is set)")
	flagSet.IntVar(&f.port, "port", 0, "Port to bind the forwarder to (0 picks a free port; ignored when --unix-socket is set)")
	flagSet.StringVar(&f.unixSocket, "unix-socket", "", "Path to a UNIX domain socket to listen on instead of TCP. This is how a sidecar exposes itself to the build actions in its pod.")
	flagSet.StringVar(&f.unixSocketMode, "unix-socket-mode", "", "Octal mode to chmod the UNIX socket to after binding, e.g. 0660. Empty leaves the mode the OS created it with, which is what a build action running as another user usually needs.")
	flagSet.StringVar(&f.forwarderID, "forwarder-id", "", "Identifies this forwarder in the peer's decision log. Defaults to the hostname (the pod name in Kubernetes).")

	flagSet.StringVar(&f.peerCAFile, "peer-ca-file", "", "PEM bundle of CAs used to verify the peer's certificate. Empty uses the system roots. Re-read when the file changes and on SIGHUP.")
	flagSet.StringVar(&f.peerServerName, "peer-server-name", "", "Name to verify in the peer's certificate, overriding the one in --peer. Needed when dialing a pod IP directly.")
	flagSet.StringVar(&f.peerCertFile, "peer-cert-file", "", "PEM client certificate to present to the peer (mTLS). Re-read when the file changes and on SIGHUP; pooled connections are recycled so a rotated certificate takes effect.")
	flagSet.StringVar(&f.peerKeyFile, "peer-key-file", "", "PEM private key matching --peer-cert-file. Both or neither.")
	flagSet.StringVar(&f.peerTokenFile, "peer-token-file", "", "File holding the bearer token to present to the peer. Re-read per request (with a short cache), so a projected Kubernetes ServiceAccount token — which kubelet rotates under the running pod — keeps working.")

	flagSet.BoolVar(&f.allowPlaintextPeer, "dangerously-allow-plaintext-peer", false, "Permit an http:// peer. DANGEROUS: a bearer token then crosses the network in the clear. The legitimate case is a service mesh providing mTLS underneath, in which case the peer also needs --dangerously-allow-plaintext-h2c.")
	flagSet.BoolVar(&f.allowAnonymousPeer, "dangerously-allow-anonymous-peer", false, "Relay to the peer with no credential of our own. DANGEROUS unless something else authenticates the hop, such as a service mesh.")
	flagSet.BoolVar(&f.skipPeerVerification, "dangerously-skip-peer-verification", false, "Do not verify the peer's TLS certificate. DANGEROUS: anything on the path can then impersonate the peer and read every request.")
	flagSet.BoolVar(&f.allowUnauthenticatedText, "dangerously-allow-unauthenticated-clients", false, "Serve a network-reachable address. DANGEROUS: a forwarder authenticates none of its own clients, so anything able to reach it can spend the peer's credential within the peer's policy.")

	flagSet.DurationVar(&f.shutdownTimeout, "shutdown-timeout", 30*time.Second, "How long in-flight requests may take to finish after a shutdown signal. Set this above your longest blob transfer.")
	f.metrics.register(flagSet)
}

func newForwardFlagSet() (*flag.FlagSet, *forwardFlags) {
	flagSet := flag.NewFlagSet("oci-distribution-gateway forward", flag.ContinueOnError)
	flags := &forwardFlags{}
	flags.register(flagSet)
	flagSet.Usage = func() {
		printUsage(flagSet,
			"Relay OCI distribution requests to another gateway.\n\n"+
				"A forwarding gateway holds no registry credentials and enforces no policy:\n"+
				"it passes requests unchanged to the serving gateway named by --peer over one\n"+
				"multiplexed HTTP/2 connection, adding only its own peer credential. Run it as\n"+
				"a sidecar on a UNIX socket so build actions reach it without credentials of\n"+
				"their own.",
			"oci-distribution-gateway forward --unix-socket <path> --peer <url> [OPTIONS]",
			[]string{
				"oci-distribution-gateway forward --unix-socket /worker/oci-gateway.sock \\\n      --peer https://oci-gateway.img-gateway.svc:8443 --peer-token-file /var/run/gw/token",
				"oci-distribution-gateway forward --unix-socket /run/gw.sock \\\n      --peer https://oci-gateway.img-gateway.svc:8443 --peer-ca-file /tls/ca.crt \\\n      --peer-cert-file /tls/tls.crt --peer-key-file /tls/tls.key",
			})
	}
	return flagSet, flags
}

func forwardProcess(ctx context.Context, args []string) {
	flagSet, flags := newForwardFlagSet()
	parseFlags(flagSet, args)

	peerURL, err := flags.validate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		flagSet.Usage()
		os.Exit(1)
	}
	socketMode, err := parseSocketMode(flags.unixSocketMode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if reachableFromNetwork(flags.unixSocket, flags.address) && !flags.allowUnauthenticatedText {
		fmt.Fprintf(os.Stderr, "Error: refusing to serve %s:%d, because a forwarding gateway authenticates none of its clients.\n", flags.address, flags.port)
		fmt.Fprintln(os.Stderr, "Listen on a UNIX socket (--unix-socket) or a loopback address, or pass")
		fmt.Fprintln(os.Stderr, "--dangerously-allow-unauthenticated-clients if the address really is private.")
		os.Exit(1)
	}

	metrics, metricsServer, flush := flags.metrics.setup(ctx, forwardServiceName)
	defer flush()

	var recordReload func(material string, err error)
	onReload := func(material string, err error) {
		if recordReload != nil {
			recordReload(material, err)
		}
	}

	peerTLS, err := gateway.NewPeerTLS(gateway.PeerTLSOptions{
		CertFile: flags.peerCertFile,
		KeyFile:  flags.peerKeyFile,
		CAFile:   flags.peerCAFile,
		OnReload: onReload,
	})
	if err != nil {
		log.Fatalf("Failed to load peer TLS material: %v", err)
	}

	transport := peerTransport(peerURL, peerTLS, flags.peerServerName, flags.skipPeerVerification)
	var credential func(context.Context) (string, error)
	if flags.peerTokenFile != "" {
		reader := newTokenReader(flags.peerTokenFile)
		credential = reader.token
	}

	handler, err := gateway.NewForward(gateway.ForwardConfig{
		Peer:          peerURL,
		Transport:     transport,
		Credential:    credential,
		ForwarderID:   flags.forwarderID,
		MeterProvider: metrics.MeterProvider,
	})
	if err != nil {
		log.Fatalf("Failed to configure the forwarder: %v", err)
	}
	recordReload = handler.RecordMaterialReload

	listener, cleanup, err := listen(flags.unixSocket, flags.address, flags.port, socketMode)
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}
	defer cleanup()
	if err := flags.rejectSelfPeer(peerURL, listener.Addr()); err != nil {
		log.Fatalf("Error: %v", err)
	}

	// Clients are build actions speaking plaintext HTTP/1.1 over a UNIX socket,
	// exactly as they do to a single-hop gateway. The bounds match the serving
	// mode's: patient with bodies, because blobs are large.
	protocols := &http.Protocols{}
	protocols.SetHTTP1(true)
	server := &http.Server{
		Handler:           handler,
		Protocols:         protocols,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       30 * time.Minute,
		WriteTimeout:      30 * time.Minute,
		IdleTimeout:       5 * time.Minute,
	}

	done := make(chan struct{})
	defer close(done)
	if peerTLS.HasCertificate() || peerTLS.HasCA() {
		go peerTLS.Watch(done, 0)
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if err := peerTLS.Reload(); err != nil {
				log.Printf("peer TLS material reload FAILED, keeping previous material: %v", err)
				continue
			}
			// GetClientCertificate runs per handshake, so a connection already
			// established keeps presenting the retired certificate. Drop the idle
			// ones so the next request re-handshakes with the new identity; in-flight
			// transfers are left alone.
			transport.CloseIdleConnections()
		}
	}()

	log.Printf("forwarding to peer %s (%s)", peerURL.Redacted(), flags.credentialSummary())

	(&gatewayServer{
		server:          server,
		serve:           func() error { return server.Serve(listener) },
		addr:            listener.Addr(),
		metrics:         metricsServer,
		shutdownTimeout: flags.shutdownTimeout,
		banner:          "oci-distribution-gateway forwarding, listening",
	}).run()
}

// validate applies the fail-closed startup checks and returns the parsed peer.
func (f *forwardFlags) validate() (*url.URL, error) {
	if f.peer == "" {
		return nil, errors.New("--peer is required in forwarding mode")
	}
	peerURL, err := url.Parse(f.peer)
	if err != nil {
		return nil, fmt.Errorf("--peer %q is not a URL: %w", f.peer, err)
	}
	switch peerURL.Scheme {
	case "https":
	case "http":
		if !f.allowPlaintextPeer {
			return nil, fmt.Errorf("--peer %q is plaintext; use https:// or pass --dangerously-allow-plaintext-peer", f.peer)
		}
	default:
		return nil, fmt.Errorf("--peer %q must use https:// or http://", f.peer)
	}
	if peerURL.Host == "" {
		return nil, fmt.Errorf("--peer %q is missing a host", f.peer)
	}
	// A path or query would be silently ignored: the request URI comes from the
	// client, and only the scheme and host of the peer are used.
	if peerURL.Path != "" || peerURL.RawQuery != "" || peerURL.User != nil {
		return nil, fmt.Errorf("--peer %q must be a scheme and host only", f.peer)
	}
	if (f.peerCertFile == "") != (f.peerKeyFile == "") {
		return nil, errors.New("--peer-cert-file and --peer-key-file must be given together")
	}
	if f.peerCertFile == "" && f.peerTokenFile == "" && !f.allowAnonymousPeer {
		return nil, errors.New("no peer credential configured; set --peer-token-file or --peer-cert-file, or pass --dangerously-allow-anonymous-peer when a service mesh authenticates the hop")
	}
	return peerURL, nil
}

// rejectSelfPeer refuses a --peer pointing at this process, which would relay
// every request back to itself until the pod runs out of resources.
func (f *forwardFlags) rejectSelfPeer(peerURL *url.URL, own net.Addr) error {
	addr, ok := own.(*net.TCPAddr)
	if !ok {
		return nil
	}
	peerHost, peerPort, err := net.SplitHostPort(peerURL.Host)
	if err != nil {
		// No explicit port, so it cannot collide with our own numeric one.
		return nil
	}
	if peerPort != fmt.Sprint(addr.Port) {
		return nil
	}
	ip := net.ParseIP(strings.Trim(peerHost, "[]"))
	if ip == nil {
		return nil
	}
	if ip.Equal(addr.IP) || (ip.IsLoopback() && addr.IP.IsLoopback()) {
		return fmt.Errorf("--peer %s points at this gateway's own listener %s", peerURL.Host, own)
	}
	return nil
}

// credentialSummary describes the configured peer credential for the startup
// banner, without revealing any of it.
func (f *forwardFlags) credentialSummary() string {
	var parts []string
	if f.peerCertFile != "" {
		parts = append(parts, "client certificate")
	}
	if f.peerTokenFile != "" {
		parts = append(parts, "bearer token")
	}
	if len(parts) == 0 {
		return "no credential"
	}
	return strings.Join(parts, " + ")
}

// peerTransport builds the transport used for the hop to the peer.
//
// The protocol set is the load-bearing part. net/http only speaks unencrypted
// HTTP/2 to an http:// URL when the transport's protocol set includes
// UnencryptedHTTP2 and *excludes* HTTP1; with HTTP1 also set it silently falls
// back to HTTP/1.1, which means one connection per concurrent request — the
// requirement this whole hop exists to satisfy, quietly lost with no error, no log
// and no metric. For an https:// peer, leaving HTTP1 out makes ALPN offer exactly
// "h2", so a middlebox that cannot speak HTTP/2 fails the handshake loudly instead
// of degrading in silence.
//
// No HTTP2Config receive windows are set here: Go's HTTP/2 *client* already
// advertises 1 GiB per connection and 4 MiB per stream, and lowering those to the
// server-side values would throttle every blob download.
func peerTransport(peerURL *url.URL, peerTLS *gateway.PeerTLS, serverName string, skipVerify bool) *http.Transport {
	protocols := &http.Protocols{}
	transport := &http.Transport{
		Protocols:           protocols,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConns:        16,
		TLSHandshakeTimeout: 10 * time.Second,
		// Detect a peer that vanished (a rolling update, a lost node) in seconds
		// rather than waiting for the kernel to give up minutes later.
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: 30 * time.Second,
			PingTimeout:     15 * time.Second,
		},
	}
	if peerURL.Scheme == "https" {
		protocols.SetHTTP2(true)
		// Not shared with anything else: http.Transport mutates TLSClientConfig in
		// place (unlike http.Server, which clones it).
		transport.TLSClientConfig = peerTLS.ClientConfig(serverName, skipVerify)
	} else {
		protocols.SetUnencryptedHTTP2(true)
	}
	return transport
}

// peerTokenReader reads the peer credential from disk, caching it briefly.
//
// It is deliberately re-read rather than loaded once: the file may be a projected
// Kubernetes ServiceAccount token, which kubelet replaces at 80% of a lifetime
// whose floor is ten minutes, so a copy taken at startup begins failing within the
// hour. A short cache keeps that off the per-request path without letting the copy
// go stale.
type peerTokenReader struct {
	path string
	ttl  time.Duration

	mu      sync.Mutex
	cached  string
	expires time.Time
}

func newTokenReader(path string) *peerTokenReader {
	return &peerTokenReader{path: path, ttl: 10 * time.Second}
}

func (r *peerTokenReader) token(context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Now().Before(r.expires) {
		return r.cached, nil
	}
	data, err := os.ReadFile(r.path)
	if err != nil {
		return "", fmt.Errorf("reading peer token %q: %w", r.path, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("peer token %q is empty", r.path)
	}
	r.cached = token
	r.expires = time.Now().Add(r.ttl)
	return token, nil
}
