package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// This file proves the property the whole two-hop design rests on: several
// concurrent requests to the peer share ONE connection. It is worth a test of its
// own because the failure mode is silent. net/http only speaks unencrypted HTTP/2
// to an http:// URL when the transport's protocol set includes UnencryptedHTTP2
// and *excludes* HTTP1; with HTTP1 also set it quietly falls back to HTTP/1.1,
// which means one connection per concurrent request — no error, no log, no metric,
// just the requirement gone.
//
// Everything runs over the buffered in-memory connections in memconn_test.go, so
// no port is bound.
//
// The connection is established with one warm-up request before the concurrent
// ones. That is deliberate rather than a workaround: with a cold pool, net/http
// starts a dial per waiting request because it has no connection to share yet, so
// multiplexing is a steady-state property — which is also the state a build farm's
// sidecar is in.

// holdPath marks a request the test handler holds open until every other held
// request has arrived, so they are genuinely concurrent.
const holdPath = "/hold/"

// concurrentHandler reports the protocol it was reached over, and holds requests
// under holdPath until all of them have arrived.
type concurrentHandler struct {
	arrived *sync.WaitGroup
}

func (h *concurrentHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, holdPath) && h.arrived != nil {
		h.arrived.Done()
		h.arrived.Wait()
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, r.Proto)
}

// serveInMemory runs handler on an in-memory listener and returns a transport that
// dials it, plus the listener, whose dial count is the assertion.
func serveInMemory(t *testing.T, handler http.Handler, serverProtocols, clientProtocols *http.Protocols, serverTLS, clientTLS *tls.Config) (*http.Transport, *memListener) {
	t.Helper()
	listener := newMemListener("peer.test:8443")
	server := &http.Server{
		Handler:   handler,
		Protocols: serverProtocols,
		TLSConfig: serverTLS,
	}
	go func() {
		var err error
		if serverTLS != nil {
			err = server.ServeTLS(listener, "", "")
		} else {
			err = server.Serve(listener)
		}
		if !isListenerClosed(err) && !strings.Contains(err.Error(), "Server closed") {
			t.Logf("in-memory server stopped: %v", err)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})

	transport := &http.Transport{
		Protocols:       clientProtocols,
		TLSClientConfig: clientTLS,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return listener.Dial()
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	return transport, listener
}

// get performs one request and returns the protocol it was served over.
func get(t *testing.T, transport *http.Transport, url string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("%s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	return string(body)
}

// runConcurrently issues n requests that the handler holds until all have arrived,
// and returns the protocol each was served over.
func runConcurrently(t *testing.T, transport *http.Transport, scheme string, arrived *sync.WaitGroup, n int) []string {
	t.Helper()
	arrived.Add(n)
	protos := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			url := fmt.Sprintf("%s://peer.test:8443%s%d", scheme, holdPath, i)
			req, err := http.NewRequest(http.MethodGet, url, nil)
			if err != nil {
				errs[i] = err
				arrived.Done()
				return
			}
			resp, err := transport.RoundTrip(req)
			if err != nil {
				errs[i] = err
				arrived.Done()
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				errs[i] = err
				return
			}
			protos[i] = string(body)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	return protos
}

// h2cProtocols returns the protocol sets peerTransport builds for an http:// peer.
func h2cProtocols() (server, client *http.Protocols) {
	server = &http.Protocols{}
	server.SetHTTP1(true)
	server.SetUnencryptedHTTP2(true)
	client = &http.Protocols{}
	// HTTP1 deliberately left false: net/http only speaks h2c to an http:// URL
	// when the set excludes it.
	client.SetUnencryptedHTTP2(true)
	return server, client
}

func TestPeerConnectionMultiplexesConcurrentRequestsOverH2C(t *testing.T) {
	const concurrency = 8
	var arrived sync.WaitGroup
	serverProtocols, clientProtocols := h2cProtocols()
	transport, listener := serveInMemory(t, &concurrentHandler{arrived: &arrived}, serverProtocols, clientProtocols, nil, nil)

	if proto := get(t, transport, "http://peer.test:8443/v2/"); proto != "HTTP/2.0" {
		t.Fatalf("warm-up request served over %q, want HTTP/2.0", proto)
	}
	warm := listener.dials()

	for i, proto := range runConcurrently(t, transport, "http", &arrived, concurrency) {
		if proto != "HTTP/2.0" {
			t.Errorf("request %d served over %q, want HTTP/2.0", i, proto)
		}
	}

	// The assertion that matters: eight requests in flight at once, no new
	// connection.
	if got := listener.dials(); got != warm {
		t.Errorf("connections dialed = %d, want %d; %d concurrent requests were not multiplexed", got, warm, concurrency)
	}
}

func TestPeerConnectionMultiplexesConcurrentRequestsOverTLS(t *testing.T) {
	const concurrency = 8
	ca := newTestCA(t, "test CA")
	dir := t.TempDir()
	certPEM, keyPEM, _ := ca.issue(t, leafOptions{dnsNames: []string{"peer.test"}, server: true})
	material := newTestPeerTLS(t, PeerTLSOptions{
		CertFile: writeFile(t, dir, "tls.crt", certPEM),
		KeyFile:  writeFile(t, dir, "tls.key", keyPEM),
	})
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca.pem) {
		t.Fatal("building the client's root pool")
	}

	// ALPN offers exactly "h2", which is what an https:// peer transport does: a
	// middlebox that cannot speak HTTP/2 then fails the handshake loudly instead of
	// silently costing us multiplexing.
	serverProtocols := &http.Protocols{}
	serverProtocols.SetHTTP1(true)
	serverProtocols.SetHTTP2(true)
	clientProtocols := &http.Protocols{}
	clientProtocols.SetHTTP2(true)

	var arrived sync.WaitGroup
	transport, listener := serveInMemory(t, &concurrentHandler{arrived: &arrived}, serverProtocols, clientProtocols,
		material.ServerConfig(tls.NoClientCert, nil, nextProtosForTest(serverProtocols)),
		&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "peer.test"})

	if proto := get(t, transport, "https://peer.test:8443/v2/"); proto != "HTTP/2.0" {
		t.Fatalf("warm-up request served over %q, want HTTP/2.0", proto)
	}
	warm := listener.dials()

	for i, proto := range runConcurrently(t, transport, "https", &arrived, concurrency) {
		if proto != "HTTP/2.0" {
			t.Errorf("request %d served over %q, want HTTP/2.0", i, proto)
		}
	}
	if got := listener.dials(); got != warm {
		t.Errorf("connections dialed = %d, want %d; %d concurrent requests were not multiplexed", got, warm, concurrency)
	}
}

func TestEnablingHTTP1SilentlyLosesMultiplexing(t *testing.T) {
	// The footgun, pinned so the reason for the "HTTP1 must stay false" comment in
	// peerTransport cannot be lost: with HTTP1 also enabled, net/http speaks
	// HTTP/1.1 to an http:// peer and every concurrent request gets its own
	// connection.
	const concurrency = 8
	serverProtocols, _ := h2cProtocols()
	clientProtocols := &http.Protocols{}
	clientProtocols.SetHTTP1(true)
	clientProtocols.SetUnencryptedHTTP2(true)

	var arrived sync.WaitGroup
	transport, listener := serveInMemory(t, &concurrentHandler{arrived: &arrived}, serverProtocols, clientProtocols, nil, nil)

	if proto := get(t, transport, "http://peer.test:8443/v2/"); proto != "HTTP/1.1" {
		t.Fatalf("warm-up request served over %q; with HTTP1 enabled net/http should have used HTTP/1.1", proto)
	}
	warm := listener.dials()

	runConcurrently(t, transport, "http", &arrived, concurrency)

	if got := listener.dials(); got <= warm {
		t.Error("HTTP/1 was enabled alongside h2c yet the requests were multiplexed; the guard in peerTransport may no longer be needed")
	} else {
		t.Logf("HTTP/1 opened %d connections for %d concurrent requests, as expected", got, concurrency)
	}
}

// nextProtosForTest mirrors the cmd package's nextProtosFor, which cannot be
// imported from here.
func nextProtosForTest(protocols *http.Protocols) []string {
	var protos []string
	if protocols.HTTP2() {
		protos = append(protos, "h2")
	}
	if protocols.HTTP1() {
		protos = append(protos, "http/1.1")
	}
	return protos
}

func TestForwardCountsPeerConnectionsOverARealHop(t *testing.T) {
	// The requests-per-connection ratio is the signal that says whether the hop is
	// multiplexed, so it has to be measured against a real HTTP/2 connection rather
	// than a fake RoundTripper. This wires a ForwardHandler in front of an in-memory
	// h2c server and compares what the forwarder counted with what the listener
	// actually accepted.
	const concurrency = 8
	var arrived sync.WaitGroup
	serverProtocols, clientProtocols := h2cProtocols()
	transport, listener := serveInMemory(t, &concurrentHandler{arrived: &arrived}, serverProtocols, clientProtocols, nil, nil)

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	forwarder, err := NewForward(ForwardConfig{
		Peer:          mustParseURL(t, "http://peer.test:8443"),
		Transport:     transport,
		Logger:        log.New(io.Discard, "", 0),
		MeterProvider: provider,
	})
	if err != nil {
		t.Fatalf("NewForward: %v", err)
	}

	// Warm the connection, then relay a burst that the peer holds open at once.
	forward(forwarder, http.MethodGet, "registry.test", "http://gateway/v2/", "")
	arrived.Add(concurrency)
	var wg sync.WaitGroup
	for i := range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := forward(forwarder, http.MethodGet, "registry.test",
				fmt.Sprintf("http://gateway%sapp/blobs/sha256:%02d", holdPath, i), "")
			if w.Code != http.StatusOK {
				t.Errorf("relayed request %d: status %d, want %d", i, w.Code, http.StatusOK)
			}
		}()
	}
	wg.Wait()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collecting metrics: %v", err)
	}
	relayed := histogramCount(t, &rm, "oci.gateway.forward.peer.duration")
	if relayed != concurrency+1 {
		t.Errorf("relayed requests = %d, want %d", relayed, concurrency+1)
	}
	// Every relayed request reported HTTP/2, which is what the operator's
	// network.protocol.version panel shows.
	if got := histogramCount(t, &rm, "oci.gateway.forward.peer.duration", semconv.NetworkProtocolVersion("2")); got != relayed {
		t.Errorf("relayed requests over HTTP/2 = %d, want all %d", got, relayed)
	}
	// The forwarder's own count of connections has to match reality, or the ratio
	// an operator alerts on is fiction.
	counted := counterValue(t, &rm, "oci.gateway.forward.peer.connections")
	if counted != int64(listener.dials()) {
		t.Errorf("counted %d peer connections, but the listener accepted %d", counted, listener.dials())
	}
	if counted != 1 {
		t.Errorf("peer connections = %d, want 1: %d concurrent requests should share one", counted, concurrency)
	}
}
