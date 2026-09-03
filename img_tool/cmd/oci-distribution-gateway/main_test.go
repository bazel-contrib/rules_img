package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseSubcommand(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantMode string
		wantRest []string
		wantErr  bool
	}{{
		// The legacy form: no verb at all. Every deployment that predates the
		// subcommands, and both Kubernetes manifests in the docs, invoke it this way.
		name:     "bare flags mean serving mode",
		args:     []string{"oci-distribution-gateway", "--unix-socket=/run/gw.sock", "--policy-file=/etc/policy.json"},
		wantMode: "serve",
		wantRest: []string{"--unix-socket=/run/gw.sock", "--policy-file=/etc/policy.json"},
	}, {
		name:     "no arguments at all",
		args:     []string{"oci-distribution-gateway"},
		wantMode: "serve",
		wantRest: []string{},
	}, {
		name:     "single-dash flags too",
		args:     []string{"oci-distribution-gateway", "-policy-file=/etc/policy.json"},
		wantMode: "serve",
		wantRest: []string{"-policy-file=/etc/policy.json"},
	}, {
		name:     "explicit serve",
		args:     []string{"oci-distribution-gateway", "serve", "--port=8080"},
		wantMode: "serve",
		wantRest: []string{"--port=8080"},
	}, {
		name:     "forward",
		args:     []string{"oci-distribution-gateway", "forward", "--peer=https://gw:8443"},
		wantMode: "forward",
		wantRest: []string{"--peer=https://gw:8443"},
	}, {
		name:    "unknown verb",
		args:    []string{"oci-distribution-gateway", "relay"},
		wantErr: true,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			mode, rest, err := parseSubcommand(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseSubcommand(%v) = (%q, %v, nil), want an error", tc.args, mode, rest)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSubcommand(%v): %v", tc.args, err)
			}
			if mode != tc.wantMode {
				t.Errorf("mode = %q, want %q", mode, tc.wantMode)
			}
			if !slices.Equal(rest, tc.wantRest) {
				t.Errorf("rest = %v, want %v", rest, tc.wantRest)
			}
		})
	}
}

func TestPeerTransportProtocols(t *testing.T) {
	// This is the guard for the single highest-risk line in the two-hop feature.
	// net/http only speaks unencrypted HTTP/2 to an http:// URL when the transport's
	// protocol set includes UnencryptedHTTP2 and *excludes* HTTP1. With HTTP1 also
	// set it silently falls back to HTTP/1.1 — one connection per concurrent request,
	// with no error, no log and no metric to show the requirement was lost.
	material := newEmptyPeerTLS(t)

	https := peerTransport(mustURL(t, "https://gw.test:8443"), material, "", false)
	if !https.Protocols.HTTP2() {
		t.Error("an https:// peer transport does not enable HTTP/2")
	}
	if https.Protocols.HTTP1() {
		t.Error("an https:// peer transport enables HTTP/1, so ALPN would offer a silent downgrade")
	}
	if https.TLSClientConfig == nil {
		t.Error("an https:// peer transport has no TLS configuration")
	}

	plaintext := peerTransport(mustURL(t, "http://gw.test:8080"), material, "", false)
	if !plaintext.Protocols.UnencryptedHTTP2() {
		t.Error("an http:// peer transport does not enable h2c")
	}
	if plaintext.Protocols.HTTP1() {
		t.Error("an http:// peer transport enables HTTP/1, which makes net/http skip h2c entirely")
	}
	if plaintext.TLSClientConfig != nil {
		t.Error("an http:// peer transport has a TLS configuration")
	}
}

func TestPeerTransportSetsNoClientTimeoutButKeepsPings(t *testing.T) {
	// A multi-gigabyte blob legitimately takes minutes, so liveness has to come from
	// HTTP/2 pings rather than a request timeout. And the server's receive-window
	// settings must never be applied here: Go's HTTP/2 *client* already advertises
	// 1 GiB per connection, and lowering it would throttle every blob download.
	transport := peerTransport(mustURL(t, "https://gw.test:8443"), newEmptyPeerTLS(t), "", false)
	if transport.HTTP2 == nil || transport.HTTP2.SendPingTimeout == 0 || transport.HTTP2.PingTimeout == 0 {
		t.Error("the peer transport has no HTTP/2 ping timeouts, so a vanished peer hangs a stream for minutes")
	}
	if transport.HTTP2.MaxReceiveBufferPerConnection != 0 || transport.HTTP2.MaxReceiveBufferPerStream != 0 {
		t.Error("the peer transport lowers the client's HTTP/2 receive windows, which throttles blob downloads")
	}
}

func TestForwardFlagValidation(t *testing.T) {
	tokenFile := writeTempFile(t, "token", "a-token")
	certFile := writeTempFile(t, "tls.crt", "cert")
	keyFile := writeTempFile(t, "tls.key", "key")

	for _, tc := range []struct {
		name  string
		flags forwardFlags
		want  string // a substring of the expected error; empty means it must pass
	}{{
		name:  "no peer",
		flags: forwardFlags{},
		want:  "--peer is required",
	}, {
		name:  "plaintext peer without the escape hatch",
		flags: forwardFlags{peer: "http://gw:8080", peerTokenFile: tokenFile},
		want:  "is plaintext",
	}, {
		name:  "plaintext peer with the escape hatch",
		flags: forwardFlags{peer: "http://gw:8080", peerTokenFile: tokenFile, allowPlaintextPeer: true},
	}, {
		name:  "unsupported scheme",
		flags: forwardFlags{peer: "grpc://gw:8080", peerTokenFile: tokenFile},
		want:  "must use https:// or http://",
	}, {
		name:  "peer with no host",
		flags: forwardFlags{peer: "https://", peerTokenFile: tokenFile},
		want:  "missing a host",
	}, {
		// A path or query would be silently ignored, since the request URI comes from
		// the client and only the peer's scheme and host are used.
		name:  "peer with a path",
		flags: forwardFlags{peer: "https://gw:8443/v2/", peerTokenFile: tokenFile},
		want:  "scheme and host only",
	}, {
		name:  "peer with userinfo",
		flags: forwardFlags{peer: "https://user:pass@gw:8443", peerTokenFile: tokenFile},
		want:  "scheme and host only",
	}, {
		name:  "no credential",
		flags: forwardFlags{peer: "https://gw:8443"},
		want:  "no peer credential configured",
	}, {
		name:  "anonymous peer with the escape hatch",
		flags: forwardFlags{peer: "https://gw:8443", allowAnonymousPeer: true},
	}, {
		name:  "half a keypair",
		flags: forwardFlags{peer: "https://gw:8443", peerCertFile: certFile},
		want:  "must be given together",
	}, {
		name:  "a full keypair is a credential",
		flags: forwardFlags{peer: "https://gw:8443", peerCertFile: certFile, peerKeyFile: keyFile},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.flags.validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("validate() = %v, want it to pass", err)
			case tc.want == "":
				return
			case err == nil:
				t.Fatalf("validate() passed, want an error containing %q", tc.want)
			case !strings.Contains(err.Error(), tc.want):
				t.Errorf("validate() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// TestCacheReplicationFlagValidation covers the startup checks of cache
// replication. They are all fail-closed in the same way: a flag combination that
// could only half-work refuses to start, rather than serving with a control that
// looks configured but is not.
func TestCacheReplicationFlagValidation(t *testing.T) {
	// The cache itself has to be on for there to be anything to replicate.
	enabled := serveFlags{blobCacheTTL: time.Hour, blobCacheMaxMemory: defaultBlobCacheMemory, port: 8443}

	withCache := func(mutate func(*serveFlags)) serveFlags {
		flags := enabled
		mutate(&flags)
		return flags
	}

	for _, tc := range []struct {
		name  string
		flags serveFlags
		want  string // a substring of the expected error; empty means it must pass
		// off means replication must end up disabled rather than configured.
		off bool
	}{{
		name:  "no peers configured",
		flags: enabled,
		off:   true,
	}, {
		// Silently ignoring this one would look like a control that is in force.
		name:  "an allow-list with no peers",
		flags: withCache(func(f *serveFlags) { f.allowedCachePeerIDs = repeatedFlag{"spiffe://cluster.local/ns/img/sa/gw"} }),
		want:  "needs --blob-existence-cache-peer",
	}, {
		name:  "a peer token file with no peers",
		flags: withCache(func(f *serveFlags) { f.cachePeerTokenFile = "/run/token" }),
		want:  "needs --blob-existence-cache-peer",
	}, {
		name: "both ways of naming peers",
		flags: withCache(func(f *serveFlags) {
			f.cachePeers = repeatedFlag{"https://gw-1:8443"}
			f.cachePeerService = "gw"
		}),
		want: "use one of them",
	}, {
		name: "peers without the cache",
		flags: withCache(func(f *serveFlags) {
			f.cachePeers = repeatedFlag{"https://gw-1:8443"}
			f.blobCacheTTL = 0
		}),
		want: "needs the blob existence cache",
	}, {
		name: "peers cannot reach a UNIX socket",
		flags: withCache(func(f *serveFlags) {
			f.cachePeers = repeatedFlag{"https://gw-1:8443"}
			f.unixSocket = "/run/gw.sock"
		}),
		want: "instead of --unix-socket",
	}, {
		name:  "a plaintext peer without the escape hatch",
		flags: withCache(func(f *serveFlags) { f.cachePeers = repeatedFlag{"http://gw-1:8443"} }),
		want:  "is plaintext",
	}, {
		name: "a plaintext peer with the escape hatch",
		flags: withCache(func(f *serveFlags) {
			f.cachePeers = repeatedFlag{"http://gw-1:8443"}
			f.allowPlaintextCachePeer = true
		}),
	}, {
		name:  "a peer with a path",
		flags: withCache(func(f *serveFlags) { f.cachePeers = repeatedFlag{"https://gw-1:8443/v2/"} }),
		want:  "scheme and host only",
	}, {
		name:  "a peer with an unsupported scheme",
		flags: withCache(func(f *serveFlags) { f.cachePeers = repeatedFlag{"grpc://gw-1:8443"} }),
		want:  "must use https:// or http://",
	}, {
		// A discovered peer is reached on the port this instance serves, which a
		// listener that took whatever port was free does not have.
		name: "discovery without a fixed port",
		flags: withCache(func(f *serveFlags) {
			f.cachePeerService = "gw"
			f.port = 0
			f.allowPlaintextCachePeer = true
		}),
		want: "needs an explicit --port",
	}, {
		// Past validation, discovery needs a pod's environment, which a test is not.
		name: "discovery outside a pod",
		flags: withCache(func(f *serveFlags) {
			f.cachePeerService = "img-gateway/gw"
			f.allowPlaintextCachePeer = true
		}),
		want: "Kubernetes pod",
	}, {
		name: "a plaintext gateway serving discovered peers needs the escape hatch",
		flags: withCache(func(f *serveFlags) {
			f.cachePeerService = "img-gateway/gw"
		}),
		want: "serves replication over plaintext",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			// serverTLS is nil throughout: these are the checks that do not depend on
			// the TLS material, and a nil one is a gateway serving plaintext. So is
			// the peer listener, which these cases do not use.
			replication, err := tc.flags.cacheReplication(nil, nil)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("cacheReplication() = %v, want it to pass", err)
			case tc.want == "" && tc.off != (replication == nil):
				t.Fatalf("cacheReplication() returned %v, want replication off = %v", replication, tc.off)
			case tc.want == "":
				return
			case err == nil:
				t.Fatalf("cacheReplication() passed, want an error containing %q", tc.want)
			case !strings.Contains(err.Error(), tc.want):
				t.Errorf("cacheReplication() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestReachableFromNetwork(t *testing.T) {
	// This is what decides whether a listener needs client authentication. It fails
	// closed: anything not recognizably local is treated as reachable.
	for _, tc := range []struct {
		unixSocket, address string
		want                bool
	}{
		{unixSocket: "/run/gw.sock", address: "localhost", want: false},
		{address: "localhost", want: false},
		{address: "localhost.localhost", want: false},
		{address: "127.0.0.1", want: false},
		{address: "::1", want: false},
		{address: "[::1]", want: false},
		{address: "0.0.0.0", want: true},
		{address: "", want: true},
		{address: "::", want: true},
		{address: "10.0.0.5", want: true},
		{address: "gateway.svc", want: true},
	} {
		if got := reachableFromNetwork(tc.unixSocket, tc.address); got != tc.want {
			t.Errorf("reachableFromNetwork(%q, %q) = %v, want %v", tc.unixSocket, tc.address, got, tc.want)
		}
	}
}

func TestParseSocketMode(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    uint32
		wantErr bool
	}{
		{in: "", want: 0},
		{in: "0", want: 0},
		{in: "0660", want: 0o660},
		{in: "660", want: 0o660},
		{in: "777", want: 0o777},
		{in: "1777", wantErr: true},
		{in: "0999", wantErr: true},
		{in: "rw-rw----", wantErr: true},
	} {
		got, err := parseSocketMode(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseSocketMode(%q) = %o, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSocketMode(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseSocketMode(%q) = %o, want %o", tc.in, got, tc.want)
		}
	}
}

func TestByteSizeFlag(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    byteSizeFlag
		wantErr bool
	}{
		{in: "0", want: 0},
		{in: "67108864", want: 64 << 20},
		{in: "64MiB", want: 64 << 20},
		{in: "64Mi", want: 64 << 20},
		{in: "512KiB", want: 512 << 10},
		{in: "2GiB", want: 2 << 30},
		{in: "1024B", want: 1024},
		{in: "64MB", want: 64_000_000},
		{in: "2GB", want: 2_000_000_000},
		{in: " 64MiB ", want: 64 << 20},
		{in: "64 MiB", want: 64 << 20},
		// A bare K/M/G is binary to Docker and decimal to Kubernetes, so it is
		// refused rather than guessed at.
		{in: "64M", wantErr: true},
		{in: "64G", wantErr: true},
		{in: "64mib", wantErr: true},
		{in: "-1", wantErr: true},
		{in: "1.5MiB", wantErr: true},
		{in: "", wantErr: true},
		{in: "MiB", wantErr: true},
		{in: "9223372036854775807GiB", wantErr: true},
	} {
		var got byteSizeFlag
		err := got.Set(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("byteSizeFlag.Set(%q) = %d, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("byteSizeFlag.Set(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("byteSizeFlag.Set(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestByteSizeFlagString covers the flag defaults the usage message prints, which
// is what an operator copies into a manifest.
func TestByteSizeFlagString(t *testing.T) {
	for _, tc := range []struct {
		in   byteSizeFlag
		want string
	}{
		{in: 0, want: "0"},
		{in: 64 << 20, want: "64MiB"},
		{in: 512 << 10, want: "512KiB"},
		{in: 3 << 30, want: "3GiB"},
		{in: 1000, want: "1000"},
		{in: defaultBlobCacheMemory, want: "64MiB"},
	} {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("byteSizeFlag(%d).String() = %q, want %q", int64(tc.in), got, tc.want)
		}
	}
}

// TestByteSizeFlagRoundTrips checks that what the usage message prints parses
// back to the same number, so a default copied out of --help means what it said.
func TestByteSizeFlagRoundTrips(t *testing.T) {
	for _, size := range []byteSizeFlag{0, 1, 1000, 1 << 10, defaultBlobCacheMemory, 3 << 30} {
		var back byteSizeFlag
		if err := back.Set(size.String()); err != nil {
			t.Errorf("byteSizeFlag(%d).String() = %q, which does not parse: %v", int64(size), size, err)
			continue
		}
		if back != size {
			t.Errorf("byteSizeFlag(%d) printed as %q, which parses back as %d", int64(size), size, int64(back))
		}
	}
}

func TestServeProtocols(t *testing.T) {
	// A UNIX socket stays HTTP/1.1 only: the registry protocol is request/response
	// and the img tool's unix transport does not attempt h2 anyway.
	unix := serveProtocols(true, false)
	if !unix.HTTP1() || unix.HTTP2() || unix.UnencryptedHTTP2() {
		t.Errorf("a unix-socket listener speaks %v, want HTTP/1.1 only", unix)
	}

	// A TCP listener adds ALPN h2, which can only be selected inside a TLS
	// handshake, so a plaintext single-hop deployment is unaffected.
	tcp := serveProtocols(false, false)
	if !tcp.HTTP1() || !tcp.HTTP2() {
		t.Errorf("a TCP listener speaks %v, want HTTP/1.1 and HTTP/2", tcp)
	}
	if tcp.UnencryptedHTTP2() {
		t.Error("h2c is enabled by default; sniffing its preface happens before any read deadline is set")
	}

	if h2c := serveProtocols(false, true); !h2c.UnencryptedHTTP2() {
		t.Error("--dangerously-allow-plaintext-h2c did not enable h2c")
	}
}

func TestNextProtosFor(t *testing.T) {
	// The ALPN list has to be computed rather than left to net/http: ServeTLS only
	// fixes up the config it clones, while the config handed out from
	// GetConfigForClient (which is how a rotated client CA takes effect) is used
	// as-is.
	both := &http.Protocols{}
	both.SetHTTP1(true)
	both.SetHTTP2(true)
	if got := nextProtosFor(both); !slices.Equal(got, []string{"h2", "http/1.1"}) {
		t.Errorf("nextProtosFor(h1+h2) = %v, want [h2 http/1.1]", got)
	}

	h1 := &http.Protocols{}
	h1.SetHTTP1(true)
	if got := nextProtosFor(h1); !slices.Equal(got, []string{"http/1.1"}) {
		t.Errorf("nextProtosFor(h1) = %v, want [http/1.1]", got)
	}
}

func TestServeHTTP2ConfigRaisesTheReceiveWindows(t *testing.T) {
	// Go defaults to a 1 MiB flow control window for a whole connection, shared by
	// every stream on it, so without raising it a forwarding gateway multiplexing
	// several blob uploads starves itself and one slow stream holds up the rest.
	cfg := serveHTTP2Config()
	const oneMiB = 1 << 20
	if cfg.MaxReceiveBufferPerConnection <= oneMiB {
		t.Errorf("MaxReceiveBufferPerConnection = %d, want more than Go's %d default", cfg.MaxReceiveBufferPerConnection, oneMiB)
	}
	// The API documents the connection window as "less than 4MiB".
	if cfg.MaxReceiveBufferPerConnection >= 4<<20 {
		t.Errorf("MaxReceiveBufferPerConnection = %d, which is outside the documented range", cfg.MaxReceiveBufferPerConnection)
	}
	if cfg.MaxReceiveBufferPerStream <= 0 || cfg.MaxReceiveBufferPerStream > cfg.MaxReceiveBufferPerConnection {
		t.Errorf("MaxReceiveBufferPerStream = %d, want a positive value no larger than the connection window", cfg.MaxReceiveBufferPerStream)
	}
	// Bounded so a stalled upstream registry cannot realise an unbounded number of
	// per-stream windows.
	if cfg.MaxConcurrentStreams <= 0 {
		t.Error("MaxConcurrentStreams is unbounded, so worst-case memory cannot be budgeted")
	}
	if cfg.SendPingTimeout == 0 || cfg.PingTimeout == 0 {
		t.Error("the HTTP/2 server has no ping timeouts, so a vanished peer hangs a stream for minutes")
	}
}

func TestPeerTokenReaderRereadsARotatedToken(t *testing.T) {
	// The file may be a projected Kubernetes ServiceAccount token, which kubelet
	// replaces at 80% of a lifetime whose floor is ten minutes — so a copy taken at
	// startup starts failing within the hour.
	path := writeTempFile(t, "token", "first-token\n")
	reader := newTokenReader(path)
	reader.ttl = 0 // read every time, so the test does not have to wait out the cache

	got, err := reader.token(t.Context())
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if got != "first-token" {
		t.Errorf("token = %q, want %q (surrounding whitespace trimmed)", got, "first-token")
	}

	if err := os.WriteFile(path, []byte("rotated-token\n"), 0o600); err != nil {
		t.Fatalf("rotating the token: %v", err)
	}
	if got, err := reader.token(t.Context()); err != nil || got != "rotated-token" {
		t.Errorf("token after rotation = (%q, %v), want (%q, nil)", got, err, "rotated-token")
	}
}

func TestPeerTokenReaderRejectsAnEmptyFile(t *testing.T) {
	reader := newTokenReader(writeTempFile(t, "token", "   \n"))
	if _, err := reader.token(t.Context()); err == nil {
		t.Error("token accepted an empty file")
	}
	missing := newTokenReader(filepath.Join(t.TempDir(), "absent"))
	if _, err := missing.token(t.Context()); err == nil {
		t.Error("token accepted a missing file")
	}
}

func TestRejectSelfPeer(t *testing.T) {
	// A --peer pointing at this process would relay every request back to itself
	// until the pod ran out of resources.
	flags := &forwardFlags{}
	own := &testTCPAddr{ip: "127.0.0.1", port: 8443}
	for _, tc := range []struct {
		peer    string
		wantErr bool
	}{
		{peer: "https://127.0.0.1:8443", wantErr: true},
		{peer: "https://localhost:8443", wantErr: false}, // a name, not an address
		{peer: "https://127.0.0.1:9999", wantErr: false},
		{peer: "https://10.0.0.5:8443", wantErr: false},
		{peer: "https://gw.svc:8443", wantErr: false},
	} {
		err := flags.rejectSelfPeer(mustURL(t, tc.peer), own.addr())
		if (err != nil) != tc.wantErr {
			t.Errorf("rejectSelfPeer(%s) = %v, want error: %v", tc.peer, err, tc.wantErr)
		}
	}
}

func TestLogFileCapturesEverythingTheGatewayLogs(t *testing.T) {
	// The point of the flag: a sidecar's log goes to a file of its own instead of
	// into the output of whatever it shares a terminal with. Every part of the
	// gateway logs through the standard logger, so this one redirection is the
	// whole mechanism.
	path := filepath.Join(t.TempDir(), "gateway.log")
	if err := os.WriteFile(path, []byte("an earlier run\n"), 0o600); err != nil {
		t.Fatalf("seeding the log file: %v", err)
	}
	sink := (&logFlags{file: path}).setup()
	t.Cleanup(sink.close)

	log.Print("a decision")
	if got := readFile(t, path); !strings.Contains(got, "an earlier run") || !strings.Contains(got, "a decision") {
		t.Errorf("log file = %q, want the earlier run appended to, not truncated", got)
	}

	// A reopen with the file still where it was appends to it, rather than
	// truncating what is already there.
	sink.reopen()
	log.Print("after the reopen")
	if got := readFile(t, path); !strings.Contains(got, "a decision") || !strings.Contains(got, "after the reopen") {
		t.Errorf("log file after reopen = %q, want it appended to, not truncated", got)
	}
}

func TestLogFileReopensAfterRotation(t *testing.T) {
	// Renaming a file a process holds open is a POSIX move, and it is the one a
	// log rotator makes. Windows refuses it outright, and never delivers the
	// SIGHUP that would trigger the reopen either.
	if runtime.GOOS == "windows" {
		t.Skip("a rotator cannot rename a file the gateway holds open on Windows")
	}
	path := filepath.Join(t.TempDir(), "gateway.log")
	sink := (&logFlags{file: path}).setup()
	t.Cleanup(sink.close)

	// The rotator renames the file away, and until the gateway opens the path
	// again it is writing to something nobody can find.
	rotated := path + ".1"
	log.Print("before the rotation")
	if err := os.Rename(path, rotated); err != nil {
		t.Fatalf("rotating the log file: %v", err)
	}
	sink.reopen()
	log.Print("after the rotation")

	if got := readFile(t, path); !strings.Contains(got, "after the rotation") {
		t.Errorf("log file after reopen = %q, want the line logged after the rotation", got)
	}
	if got := readFile(t, rotated); strings.Contains(got, "after the rotation") {
		t.Errorf("rotated file = %q, want nothing written to it after the reopen", got)
	}
}

func TestLogFileUnsetLeavesLoggingOnStderr(t *testing.T) {
	// The default has to stay exactly what it was: no file, no redirection, and a
	// nil sink that the deferred close and the SIGHUP reopen both tolerate.
	sink := (&logFlags{}).setup()
	if sink != nil {
		t.Fatalf("setup with no --log-file returned %v, want nil", sink)
	}
	sink.reopen()
	sink.close()
}
