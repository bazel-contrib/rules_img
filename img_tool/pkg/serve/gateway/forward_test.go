package gateway

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	clientgateway "github.com/bazel-contrib/rules_img/img_tool/pkg/gateway"
)

// fakePeer stands in for the serving gateway. Like fakeUpstreamRT it is a
// RoundTripper rather than a real server, which keeps the tests hermetic and
// binds no network port.
type fakePeer struct {
	// requests records what the forwarder sent, for assertions.
	requests []*http.Request
	// respond builds the reply. Defaults to an empty 200.
	respond func(*http.Request) (*http.Response, error)
}

func (p *fakePeer) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone the headers: ReverseProxy reuses the request after RoundTrip returns.
	recorded := req.Clone(req.Context())
	p.requests = append(p.requests, recorded)
	if p.respond != nil {
		return p.respond(req)
	}
	return peerResponse(req, http.StatusOK, nil, ""), nil
}

func (p *fakePeer) last(t *testing.T) *http.Request {
	t.Helper()
	if len(p.requests) == 0 {
		t.Fatal("the forwarder sent no request to the peer")
	}
	return p.requests[len(p.requests)-1]
}

func peerResponse(req *http.Request, status int, header http.Header, body string) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode:    status,
		Status:        http.StatusText(status),
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

// newTestForwarder wires a ForwardHandler onto a fake peer, plus a collect
// function for the metrics it recorded.
func newTestForwarder(t *testing.T, peer *fakePeer, credential string) (*ForwardHandler, func() *metricdata.ResourceMetrics) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutting down meter provider: %v", err)
		}
	})
	cfg := ForwardConfig{
		Peer:          mustParseURL(t, "https://peer.test:8443"),
		Transport:     peer,
		ForwarderID:   "test-forwarder",
		Logger:        log.New(io.Discard, "", 0),
		MeterProvider: provider,
	}
	if credential != "" {
		cfg.Credential = func(context.Context) (string, error) { return credential, nil }
	}
	f, err := NewForward(cfg)
	if err != nil {
		t.Fatalf("NewForward: %v", err)
	}
	collect := func() *metricdata.ResourceMetrics {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collecting metrics: %v", err)
		}
		return &rm
	}
	return f, collect
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing %q: %v", raw, err)
	}
	return u
}

// forward sends one request through the forwarder and returns the response.
func forward(f *ForwardHandler, method, host, target string, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	if host != "" {
		r.Header.Set(clientgateway.OriginalHostHeader, host)
	}
	w := httptest.NewRecorder()
	f.ServeHTTP(w, r)
	return w
}

// histogramCount reports how many observations a named float64 histogram made with
// all of want, which is the count series a rate is computed from.
func histogramCount(t *testing.T, rm *metricdata.ResourceMetrics, name string, want ...attribute.KeyValue) uint64 {
	t.Helper()
	hist, ok := findMetric(t, rm, name).(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("metric %q is not a float64 histogram", name)
	}
	var count uint64
	for _, dp := range hist.DataPoints {
		if matches(dp.Attributes, want) {
			count += dp.Count
		}
	}
	return count
}

func TestForwardPassesTheProtocolThrough(t *testing.T) {
	peer := &fakePeer{}
	f, _ := newTestForwarder(t, peer, "")

	forward(f, http.MethodGet, "registry.test", "http://gateway/v2/app/blobs/sha256:abc", "")

	out := peer.last(t)
	// The original-host header IS the protocol and must survive verbatim: the peer
	// has to resolve exactly what a single-hop gateway would have.
	if got := out.Header.Get(clientgateway.OriginalHostHeader); got != "registry.test" {
		t.Errorf("original host header = %q, want %q", got, "registry.test")
	}
	if got, want := out.URL.Scheme+"://"+out.URL.Host, "https://peer.test:8443"; got != want {
		t.Errorf("peer URL = %q, want %q", got, want)
	}
	if got, want := out.URL.EscapedPath(), "/v2/app/blobs/sha256:abc"; got != want {
		t.Errorf("peer path = %q, want %q", got, want)
	}
	// SetURL clears Host so SNI, certificate verification and :authority agree.
	if out.Host != "" {
		t.Errorf("outbound Host = %q, want it left to the peer URL", out.Host)
	}
	if got := out.Header.Get(forwardedByHeader); got != "test-forwarder" {
		t.Errorf("%s = %q, want %q", forwardedByHeader, got, "test-forwarder")
	}
	if out.Header.Get(requestIDHeader) == "" {
		t.Errorf("%s was not set, so the two hops cannot be correlated in the audit log", requestIDHeader)
	}
}

func TestForwardReplacesTheClientsAuthorization(t *testing.T) {
	peer := &fakePeer{}
	f, _ := newTestForwarder(t, peer, "peer-secret")

	r := httptest.NewRequest(http.MethodGet, "http://gateway/v2/app/manifests/v1", nil)
	r.Header.Set(clientgateway.OriginalHostHeader, "registry.test")
	// A build action must not be able to inject or shadow the peer credential.
	r.Header.Set("Authorization", "Bearer stolen-from-the-action")
	f.ServeHTTP(httptest.NewRecorder(), r)

	if got, want := peer.last(t).Header.Get("Authorization"), "Bearer peer-secret"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}

func TestForwardSendsNoAuthorizationWithoutACredential(t *testing.T) {
	peer := &fakePeer{}
	f, _ := newTestForwarder(t, peer, "")

	r := httptest.NewRequest(http.MethodGet, "http://gateway/v2/app/manifests/v1", nil)
	r.Header.Set(clientgateway.OriginalHostHeader, "registry.test")
	r.Header.Set("Authorization", "Bearer stolen-from-the-action")
	f.ServeHTTP(httptest.NewRecorder(), r)

	if got := peer.last(t).Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want it dropped entirely", got)
	}
}

func TestForwardStripsSpoofableHeaders(t *testing.T) {
	peer := &fakePeer{}
	f, _ := newTestForwarder(t, peer, "")

	r := httptest.NewRequest(http.MethodGet, "http://gateway/v2/app/manifests/v1", nil)
	r.Header.Set(clientgateway.OriginalHostHeader, "registry.test")
	// Everything a client might send to forge provenance or a diagnostic.
	r.Header.Set(forwardedByHeader, "someone-else")
	r.Header.Set(requestIDHeader, "forged-id")
	r.Header.Set(gatewayErrorHeader, "peer_unauthenticated")
	r.Header.Set("X-Forwarded-For", "10.0.0.1")
	r.Header.Set("X-Forwarded-Host", "evil.test")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("Forwarded", "for=10.0.0.1")
	r.Header.Set("Expect", "100-continue")
	f.ServeHTTP(httptest.NewRecorder(), r)

	out := peer.last(t)
	for _, header := range []string{
		"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded",
		gatewayErrorHeader, "Expect",
	} {
		if got := out.Header.Get(header); got != "" {
			t.Errorf("%s = %q, want it stripped", header, got)
		}
	}
	if got := out.Header.Get(forwardedByHeader); got != "test-forwarder" {
		t.Errorf("%s = %q, want this forwarder's own id", forwardedByHeader, got)
	}
	if got := out.Header.Get(requestIDHeader); got == "forged-id" {
		t.Error("the client's request id was forwarded, so it can forge audit correlation")
	}
}

func TestForwardPreservesTheRequestURI(t *testing.T) {
	// Every case is a request URI that must arrive at the peer byte-identical. The
	// upload-session cases are the ones that break real pushes: httputil re-encodes
	// RawQuery through url.Values.Encode before Rewrite is called, which sorts keys
	// and drops ';'-joined pairs, and an upload session is addressed by an opaque
	// state token the registry issued.
	for _, target := range []string{
		"/v2/",
		"/v2/app/blobs/sha256:abc",
		"/v2/app/blobs/uploads/",
		"/v2/team/sub/app/manifests/v1.0",
		"/v2/app/blobs/uploads/upload-id?_state=abc-DEF_123",
		"/v2/app/blobs/uploads/upload%2Fid?_state=has%2Bplus",
		"/v2/app/blobs/uploads/upload-id?_state=semi;colon",
		"/v2/app/blobs/uploads/?mount=sha256:abc&from=other/repo",
		"/v2/app/blobs/uploads/upload-id?digest=sha256:abc&_state=zzz",
	} {
		t.Run(target, func(t *testing.T) {
			peer := &fakePeer{}
			f, _ := newTestForwarder(t, peer, "")
			forward(f, http.MethodGet, "registry.test", "http://gateway"+target, "")
			if got := peer.last(t).URL.RequestURI(); got != target {
				t.Errorf("peer request URI = %q, want %q", got, target)
			}
		})
	}
}

func TestForwardCopiesTheResponseThrough(t *testing.T) {
	peer := &fakePeer{respond: func(req *http.Request) (*http.Response, error) {
		h := http.Header{}
		h.Set("Docker-Content-Digest", "sha256:deadbeef")
		// The serving gateway has already made this absolute; the forwarder must
		// leave it alone so the client's transport routes the next request at the
		// real registry rather than back at the peer.
		h.Set("Location", "https://registry.test/v2/app/blobs/uploads/id?_state=xyz")
		return peerResponse(req, http.StatusAccepted, h, "body"), nil
	}}
	f, _ := newTestForwarder(t, peer, "")

	w := forward(f, http.MethodPost, "registry.test", "http://gateway/v2/app/blobs/uploads/", "")

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d", w.Code, http.StatusAccepted)
	}
	if got, want := w.Header().Get("Location"), "https://registry.test/v2/app/blobs/uploads/id?_state=xyz"; got != want {
		t.Errorf("Location = %q, want it passed through as %q", got, want)
	}
	if got := w.Header().Get("Docker-Content-Digest"); got != "sha256:deadbeef" {
		t.Errorf("Docker-Content-Digest = %q, want it passed through", got)
	}
	if got := w.Body.String(); got != "body" {
		t.Errorf("body = %q, want %q", got, "body")
	}
}

func TestForwardTranslatesPeerAuthFailures(t *testing.T) {
	// A peer that rejected *us* must not be reported to the client as a registry
	// 401: go-containerregistry would start a token exchange against the gateway
	// and send an operator hunting for registry credentials.
	for _, tc := range []struct {
		name         string
		gatewayError string
		peerStatus   int
		wantStatus   int
		wantErrType  string
	}{
		{"no credential", errPeerUnauthenticated, http.StatusUnauthorized, http.StatusBadGateway, errPeerUnauthorized},
		{"bad credential", errPeerBadCredential, http.StatusUnauthorized, http.StatusBadGateway, errPeerUnauthorized},
		{"identity denied", errPeerIdentityDenied, http.StatusForbidden, http.StatusForbidden, errPeerForbidden},
		{"validation unavailable", errPeerAuthFailed, http.StatusServiceUnavailable, http.StatusServiceUnavailable, errPeerUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := &fakePeer{respond: func(req *http.Request) (*http.Response, error) {
				h := http.Header{}
				h.Set(gatewayErrorHeader, tc.gatewayError)
				return peerResponse(req, tc.peerStatus, h, `{"errors":[]}`), nil
			}}
			f, collect := newTestForwarder(t, peer, "tok")

			w := forward(f, http.MethodGet, "registry.test", "http://gateway/v2/app/manifests/v1", "")

			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			if got := counterValue(t, collect(), "oci.gateway.forward.errors", semconv.ErrorTypeKey.String(tc.wantErrType)); got != 1 {
				t.Errorf("forward.errors{error.type=%s} = %d, want 1", tc.wantErrType, got)
			}
		})
	}
}

func TestForwardPassesRegistryAuthFailuresThrough(t *testing.T) {
	// A 401 *without* the gateway header is the registry's own answer relayed by
	// the peer, and the client needs to see it unchanged.
	peer := &fakePeer{respond: func(req *http.Request) (*http.Response, error) {
		h := http.Header{}
		h.Set("WWW-Authenticate", `Bearer realm="https://registry.test/token"`)
		return peerResponse(req, http.StatusUnauthorized, h, `{"errors":[{"code":"UNAUTHORIZED"}]}`), nil
	}}
	f, _ := newTestForwarder(t, peer, "tok")

	w := forward(f, http.MethodGet, "registry.test", "http://gateway/v2/app/manifests/v1", "")

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want the registry's %d passed through", w.Code, http.StatusUnauthorized)
	}
	if got := w.Header().Get("WWW-Authenticate"); got == "" {
		t.Error("WWW-Authenticate was dropped, so the client cannot complete a token exchange")
	}
}

func TestForwardReportsAnUnreachablePeerAsRetryable(t *testing.T) {
	peer := &fakePeer{respond: func(*http.Request) (*http.Response, error) {
		return nil, syscall.ECONNREFUSED
	}}
	f, collect := newTestForwarder(t, peer, "tok")

	w := forward(f, http.MethodGet, "registry.test", "http://gateway/v2/app/manifests/v1", "")

	// 503 with Retry-After is what go-containerregistry retries *and* what the img
	// tool's own Retry-After pacer honours.
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	if got := w.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want %q", got, "1")
	}
	rm := collect()
	if got := counterValue(t, rm, "oci.gateway.forward.errors", semconv.ErrorTypeKey.String(errConnectionRefused)); got != 1 {
		t.Errorf("forward.errors{error.type=connection_refused} = %d, want 1", got)
	}
	// There was no peer leg, so there is nothing to time: recording a zero-second
	// peer latency would quietly drag the histogram down on every outage.
	if hasMetric(rm, "oci.gateway.forward.peer.duration") {
		t.Error("a peer latency was recorded for a request the peer never answered")
	}
}

func TestForwardRefusesAProtocolUpgrade(t *testing.T) {
	peer := &fakePeer{respond: func(req *http.Request) (*http.Response, error) {
		return peerResponse(req, http.StatusSwitchingProtocols, nil, ""), nil
	}}
	f, _ := newTestForwarder(t, peer, "tok")

	w := forward(f, http.MethodGet, "registry.test", "http://gateway/v2/app/manifests/v1", "")

	// The byte-counting ResponseWriter is not an http.Hijacker, so an attempted
	// upgrade would fail far more confusingly than a clean gateway error.
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestForwardFailsWhenTheCredentialCannotBeRead(t *testing.T) {
	peer := &fakePeer{}
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	f, err := NewForward(ForwardConfig{
		Peer:          mustParseURL(t, "https://peer.test:8443"),
		Transport:     peer,
		Credential:    func(context.Context) (string, error) { return "", syscall.ENOENT },
		Logger:        log.New(io.Discard, "", 0),
		MeterProvider: provider,
	})
	if err != nil {
		t.Fatalf("NewForward: %v", err)
	}

	w := forward(f, http.MethodGet, "registry.test", "http://gateway/v2/app/manifests/v1", "")

	// Better a retryable gateway error than an anonymous request the peer rejects
	// with a much less useful message.
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	if len(peer.requests) != 0 {
		t.Errorf("the peer was contacted %d time(s), want 0", len(peer.requests))
	}
}

func TestForwardRelaysUnclassifiablePathsAnyway(t *testing.T) {
	// A sidecar must never reject what its (possibly newer) peer would accept:
	// worker pods outlive the serving deployment's version.
	peer := &fakePeer{}
	f, collect := newTestForwarder(t, peer, "")

	w := forward(f, http.MethodGet, "registry.test", "http://gateway/v2/app/some-new-endpoint", "")

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want the peer's %d", w.Code, http.StatusOK)
	}
	if len(peer.requests) != 1 {
		t.Fatalf("the peer was contacted %d time(s), want 1", len(peer.requests))
	}
	if got := durationCount(t, collect(), attrOperation.String(opNameUnknown)); got != 1 {
		t.Errorf("requests with oci.operation=unknown = %d, want 1", got)
	}
}

func TestForwardReportsOnlyItsOwnHop(t *testing.T) {
	// The forwarder relays traffic the serving gateway reports in full, so it must
	// not re-export the registry-shaped instruments: a fleet-wide sum of any of them
	// would otherwise count every blob, byte and existence check twice.
	peer := &fakePeer{respond: func(req *http.Request) (*http.Response, error) {
		return peerResponse(req, http.StatusOK, nil, strings.Repeat("x", 4096)), nil
	}}
	f, collect := newTestForwarder(t, peer, "")

	forward(f, http.MethodGet, "registry.test", "http://gateway/v2/app/blobs/sha256:abc", "")
	forward(f, http.MethodHead, "registry.test", "http://gateway/v2/app/blobs/sha256:abc", "")
	rm := collect()

	// What a forwarder reports: its request rate and latency (semantic convention,
	// so read with a service.name filter), its hop, and its own failures.
	for _, name := range []string{
		"http.server.request.duration",
		"http.server.active_requests",
		"oci.gateway.forward.peer.duration",
	} {
		if !hasMetric(rm, name) {
			t.Errorf("%s was not recorded, so the hop cannot be observed", name)
		}
	}
	// What it must leave to the serving tier.
	for _, name := range []string{
		"oci.gateway.io",
		"oci.gateway.blob.downloads",
		"oci.gateway.blob.download.size",
		"oci.gateway.blob.uploads",
		"oci.gateway.blob.upload.size",
		"oci.gateway.existence_checks",
		"oci.gateway.errors",
		"oci.gateway.upstream.duration",
		"oci.gateway.upstream.auth_handshakes",
		"oci.gateway.policy.decisions",
		"oci.gateway.policy.reloads",
		"oci.gateway.policy.rules",
	} {
		if hasMetric(rm, name) {
			t.Errorf("%s was recorded by a forwarder; a fleet-wide sum would double it", name)
		}
	}

	// The peer-leg count is the relayed request rate, keyed so it cannot be
	// confused with the serving tier's own upstream leg.
	if got := histogramCount(t, rm, "oci.gateway.forward.peer.duration", attrRegistry.String("registry.test")); got != 2 {
		t.Errorf("peer duration count = %d, want 2", got)
	}
	if got := durationCount(t, rm, attrRegistry.String("registry.test"), attrOperation.String(opNameBlobRead)); got != 1 {
		t.Errorf("request duration count for blob.read = %d, want 1", got)
	}
}

func TestForwardReportsTheProtocolAndConnectionCount(t *testing.T) {
	// Requests-per-connection is the one number that says whether the hop is
	// actually multiplexed, and network.protocol.version says whether HTTP/2 is in
	// use at all. Neither is visible from the serving tier per sidecar.
	peer := &fakePeer{respond: func(req *http.Request) (*http.Response, error) {
		resp := peerResponse(req, http.StatusOK, nil, "")
		resp.Proto = "HTTP/2.0"
		return resp, nil
	}}
	f, collect := newTestForwarder(t, peer, "")

	forward(f, http.MethodGet, "registry.test", "http://gateway/v2/app/manifests/v1", "")
	rm := collect()

	if got := histogramCount(t, rm, "oci.gateway.forward.peer.duration", semconv.NetworkProtocolVersion("2")); got != 1 {
		t.Errorf("peer duration count with network.protocol.version=2 = %d, want 1", got)
	}
	// The fake transport opens no real connection, so nothing is counted here; the
	// instrument exists and stays at zero rather than reporting a phantom dial.
	if hasMetric(rm, "oci.gateway.forward.peer.connections") {
		if got := counterValue(t, rm, "oci.gateway.forward.peer.connections"); got != 0 {
			t.Errorf("peer connections = %d, want 0 for a transport that never dialled", got)
		}
	}
}

func TestProtocolVersion(t *testing.T) {
	for _, tc := range []struct{ proto, want string }{
		{"HTTP/2.0", "2"},
		{"HTTP/1.1", "1.1"},
		{"HTTP/3.0", "3.0"},
		{"", "unknown"},
	} {
		if got := protocolVersion(tc.proto); got != tc.want {
			t.Errorf("protocolVersion(%q) = %q, want %q", tc.proto, got, tc.want)
		}
	}
}

func TestForwardBoundsTheRegistryAttribute(t *testing.T) {
	peer := &fakePeer{}
	f, collect := newTestForwarder(t, peer, "")
	// The host comes from a client header, so the number of reported values is
	// capped rather than left to a misbehaving client.
	f.metrics.registries = newBoundedValues(2)

	for _, host := range []string{"a.test", "b.test", "c.test", "d.test"} {
		forward(f, http.MethodGet, host, "http://gateway/v2/app/manifests/v1", "")
	}

	rm := collect()
	if got := durationCount(t, rm, attrRegistry.String(registryOverflow)); got != 2 {
		t.Errorf("requests attributed to %q = %d, want 2", registryOverflow, got)
	}
}
