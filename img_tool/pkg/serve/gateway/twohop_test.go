package gateway

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"

	clientgateway "github.com/bazel-contrib/rules_img/img_tool/pkg/gateway"
)

// This file pins the claim the two-hop design is built on: adding a forwarding
// gateway in front of a serving one is *invisible* to the client. Every fidelity
// property is also asserted individually elsewhere, but only an end-to-end
// comparison catches a regression in something nobody thought to enumerate — which
// is exactly what happens when a proxy is inserted into a protocol as detailed as
// OCI distribution.
//
// The chain is built entirely in-process: client request -> ForwardHandler ->
// a RoundTripper that invokes the real Handler -> the same fake upstream registry
// the single-hop tests use. No listener, no port.

// handlerTransport turns an http.Handler into an http.RoundTripper, so one gateway
// can be chained behind another without a network in between.
type handlerTransport struct {
	handler http.Handler
}

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	t.handler.ServeHTTP(recorder, req)
	resp := recorder.Result()
	resp.Request = req
	return resp, nil
}

// twoHopChain builds a forwarder in front of a serving gateway, both talking to
// the given fake upstream.
func twoHopChain(t *testing.T, cp *CompiledPolicy, upstream http.RoundTripper, peerAuth *PeerAuth, credential string) *ForwardHandler {
	t.Helper()
	opts := []Option{
		WithAuthorizer(cp),
		WithKeychain(authn.NewMultiKeychain()),
		WithLogger(log.New(io.Discard, "", 0)),
		WithBaseTransport(upstream),
	}
	if peerAuth != nil {
		opts = append(opts, WithPeerAuth(peerAuth))
	}
	serving := New(opts...)

	cfg := ForwardConfig{
		Peer:        mustParseURL(t, "https://peer.test:8443"),
		Transport:   handlerTransport{handler: serving},
		ForwarderID: "test-forwarder",
		Logger:      log.New(io.Discard, "", 0),
	}
	if credential != "" {
		cfg.Credential = func(context.Context) (string, error) { return credential, nil }
	}
	forwarder, err := NewForward(cfg)
	if err != nil {
		t.Fatalf("NewForward: %v", err)
	}
	return forwarder
}

// oneHop sends a request straight to a serving gateway.
func oneHop(t *testing.T, cp *CompiledPolicy, upstream http.RoundTripper, method, host, target, body string) *http.Response {
	t.Helper()
	serving := New(
		WithAuthorizer(cp),
		WithKeychain(authn.NewMultiKeychain()),
		WithLogger(log.New(io.Discard, "", 0)),
		WithBaseTransport(upstream),
	)
	return handlerTransport{handler: serving}.mustRoundTrip(t, method, host, target, body)
}

func (t handlerTransport) mustRoundTrip(tb *testing.T, method, host, target, body string) *http.Response {
	tb.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	if host != "" {
		r.Header.Set(clientgateway.OriginalHostHeader, host)
	}
	resp, err := t.RoundTrip(r)
	if err != nil {
		tb.Fatalf("%s %s: %v", method, target, err)
	}
	return resp
}

// responseSnapshot is what a client can observe about a response.
type responseSnapshot struct {
	status  int
	headers map[string][]string
	body    string
}

func snapshot(t *testing.T, resp *http.Response) responseSnapshot {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response body: %v", err)
	}
	resp.Body.Close()
	headers := make(map[string][]string, len(resp.Header))
	for k, v := range resp.Header {
		// Content-Length is recomputed per hop and Date is a timestamp; neither says
		// anything about protocol fidelity.
		switch http.CanonicalHeaderKey(k) {
		case "Content-Length", "Date":
			continue
		}
		headers[http.CanonicalHeaderKey(k)] = v
	}
	return responseSnapshot{status: resp.StatusCode, headers: headers, body: string(body)}
}

func (s responseSnapshot) String() string {
	keys := make([]string, 0, len(s.headers))
	for k := range s.headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%d\n", s.status)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %v\n", k, s.headers[k])
	}
	fmt.Fprintf(&b, "\n%s", s.body)
	return b.String()
}

func TestTwoHopsAreIndistinguishableFromOne(t *testing.T) {
	// The same request table the single-hop tests use, driven through both paths and
	// compared byte for byte.
	requests := []struct {
		name           string
		method, target string
		body           string
	}{
		{"version check", http.MethodGet, "/v2/", ""},
		{"manifest read", http.MethodGet, "/v2/app/manifests/v1.0", ""},
		{"manifest head", http.MethodHead, "/v2/app/manifests/v1.0", ""},
		{"nested repository", http.MethodGet, "/v2/team/sub/app/manifests/v1.0", ""},
		{"blob read", http.MethodGet, "/v2/app/blobs/sha256:abc", ""},
		{"blob head", http.MethodHead, "/v2/app/blobs/sha256:abc", ""},
		{"upload start", http.MethodPost, "/v2/app/blobs/uploads/", ""},
		{"upload chunk", http.MethodPatch, "/v2/app/blobs/uploads/id?_state=abc", "chunk"},
		{"cross-repo mount", http.MethodPost, "/v2/app/blobs/uploads/?mount=sha256:abc&from=other/repo", ""},
		{"unknown endpoint", http.MethodGet, "/v2/app/nonsense", ""},
		{"missing endpoint", http.MethodGet, "/not-a-registry-path", ""},
	}
	cp := allowHostPolicy(t, testUpstreamHost, "*")

	for _, tc := range requests {
		t.Run(tc.name, func(t *testing.T) {
			single := snapshot(t, oneHop(t, cp, &fakeUpstreamRT{}, tc.method, testUpstreamHost, "http://gateway"+tc.target, tc.body))
			forwarder := twoHopChain(t, cp, &fakeUpstreamRT{}, nil, "")
			double := snapshot(t, handlerTransport{handler: forwarder}.mustRoundTrip(t, tc.method, testUpstreamHost, "http://gateway"+tc.target, tc.body))

			if single.String() != double.String() {
				t.Errorf("the second hop changed the response.\n--- one hop ---\n%s\n--- two hops ---\n%s", single, double)
			}
		})
	}
}

// registryRequests returns the upstream requests that are not the go-containerregistry
// authentication ping, which every first request to a repository triggers.
func registryRequests(upstream *fakeUpstreamRT) []*http.Request {
	var requests []*http.Request
	for _, r := range upstream.requests {
		if r.URL.Path == "/v2/" {
			continue
		}
		requests = append(requests, r)
	}
	return requests
}

// uploadAwareUpstream is a fake registry that also answers the PATCH and PUT of an
// upload session, which the shared fakeUpstreamRT does not.
type uploadAwareUpstream struct {
	requests []*http.Request
}

func (u *uploadAwareUpstream) RoundTrip(req *http.Request) (*http.Response, error) {
	u.requests = append(u.requests, req.Clone(req.Context()))
	respond := func(status int, header http.Header, body string) *http.Response {
		if header == nil {
			header = http.Header{}
		}
		return &http.Response{
			StatusCode: status, Status: http.StatusText(status), Header: header,
			Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: req,
		}
	}
	switch path := req.URL.Path; {
	case path == "/v2/":
		return respond(http.StatusOK, nil, ""), nil
	case strings.HasSuffix(path, "/blobs/uploads/"):
		h := http.Header{}
		// A relative Location, which the serving gateway makes absolute. The state
		// token deliberately contains a ';' and a percent-escape: both are what a
		// naive proxy mangles.
		h.Set("Location", "/v2/app/blobs/uploads/upload%2Fid?_state=semi;colon")
		return respond(http.StatusAccepted, h, ""), nil
	case strings.Contains(path, "/blobs/uploads/"):
		if req.URL.RawQuery != "_state=semi;colon" {
			return respond(http.StatusBadRequest, nil, "the upload session state was altered in transit"), nil
		}
		h := http.Header{}
		h.Set("Location", req.URL.RequestURI())
		return respond(http.StatusAccepted, h, ""), nil
	default:
		return respond(http.StatusNotFound, nil, ""), nil
	}
}

func TestTwoHopsForwardTheSameUpstreamRequest(t *testing.T) {
	// Not just the response: the request the *registry* sees must be identical too,
	// or the policy decided one thing while the registry acted on another.
	cp := allowHostPolicy(t, testUpstreamHost, "*")
	target := "/v2/app/blobs/uploads/id?_state=opaque;token&digest=sha256:abc"

	singleUpstream := &fakeUpstreamRT{}
	oneHop(t, cp, singleUpstream, http.MethodPut, testUpstreamHost, "http://gateway"+target, "")
	doubleUpstream := &fakeUpstreamRT{}
	forwarder := twoHopChain(t, cp, doubleUpstream, nil, "")
	handlerTransport{handler: forwarder}.mustRoundTrip(t, http.MethodPut, testUpstreamHost, "http://gateway"+target, "")

	singleRequests, doubleRequests := registryRequests(singleUpstream), registryRequests(doubleUpstream)
	if len(singleRequests) != 1 || len(doubleRequests) != 1 {
		t.Fatalf("registry requests: one hop = %d, two hops = %d, want 1 each", len(singleRequests), len(doubleRequests))
	}
	single, double := singleRequests[0], doubleRequests[0]
	if single.URL.String() != double.URL.String() {
		t.Errorf("upstream URL differs:\n one hop  = %s\n two hops = %s", single.URL, double.URL)
	}
	// The reserved namespace must not reach the registry, however many gateways the
	// request passed through.
	for header := range double.Header {
		if strings.HasPrefix(http.CanonicalHeaderKey(header), reservedHeaderPrefix) {
			t.Errorf("header %s reached the registry", header)
		}
	}
	for _, header := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded", "Authorization"} {
		if got := double.Header.Get(header); got != "" {
			t.Errorf("header %s = %q reached the registry", header, got)
		}
	}
}

func TestTwoHopsCarryAnUploadSession(t *testing.T) {
	// The full push shape. The Location the registry issues is made absolute by the
	// serving gateway, must pass through the forwarder untouched, and is what the
	// client's transport re-routes — so this is the one flow that breaks if any hop
	// rewrites it. The state token here contains a ';' and a percent-escape, the two
	// things a proxy that re-encodes URLs silently destroys.
	cp := allowHostPolicy(t, testUpstreamHost, "*")
	upstream := &uploadAwareUpstream{}
	forwarder := twoHopChain(t, cp, upstream, nil, "")
	chain := handlerTransport{handler: forwarder}

	start := chain.mustRoundTrip(t, http.MethodPost, testUpstreamHost, "http://gateway/v2/app/blobs/uploads/", "")
	if start.StatusCode != http.StatusAccepted {
		t.Fatalf("upload start status = %d, want %d", start.StatusCode, http.StatusAccepted)
	}
	location := start.Header.Get("Location")
	want := "https://" + testUpstreamHost + "/v2/app/blobs/uploads/upload%2Fid?_state=semi;colon"
	if location != want {
		t.Fatalf("Location = %q, want %q", location, want)
	}

	// The client's gateway transport turns that absolute URL back into a request to
	// the gateway, carrying the host it names. Replay that step here.
	chunk := chain.mustRoundTrip(t, http.MethodPatch, testUpstreamHost,
		"http://gateway/v2/app/blobs/uploads/upload%2Fid?_state=semi;colon", "chunk-bytes")
	body, _ := io.ReadAll(chunk.Body)
	if chunk.StatusCode != http.StatusAccepted {
		t.Fatalf("upload chunk status = %d (%s), want %d", chunk.StatusCode, body, http.StatusAccepted)
	}
	last := upstream.requests[len(upstream.requests)-1]
	if got, wantURI := last.URL.RequestURI(), "/v2/app/blobs/uploads/upload%2Fid?_state=semi;colon"; got != wantURI {
		t.Errorf("upstream request URI = %q, want %q", got, wantURI)
	}
}

func TestTwoHopsAuthenticateTheHop(t *testing.T) {
	cp := allowHostPolicy(t, testUpstreamHost, "*")
	peerAuth := newTestPeerAuth(t, PeerAuthOptions{TokenFiles: []string{writeTokens(t, testToken)}})

	t.Run("with the right credential", func(t *testing.T) {
		upstream := &fakeUpstreamRT{}
		forwarder := twoHopChain(t, cp, upstream, peerAuth, testToken)
		resp := handlerTransport{handler: forwarder}.mustRoundTrip(t, http.MethodGet, testUpstreamHost, "http://gateway/v2/app/manifests/v1", "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
		}
		if got := len(registryRequests(upstream)); got != 1 {
			t.Errorf("registry requests = %d, want 1", got)
		}
	})

	t.Run("with the wrong credential", func(t *testing.T) {
		upstream := &fakeUpstreamRT{}
		forwarder := twoHopChain(t, cp, upstream, peerAuth, "wrong-token-of-the-right-length--")
		resp := handlerTransport{handler: forwarder}.mustRoundTrip(t, http.MethodGet, testUpstreamHost, "http://gateway/v2/app/manifests/v1", "")
		// 502, not 401: a 401 would send go-containerregistry off to negotiate a
		// registry token against the gateway.
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
		}
		if got := len(upstream.requests); got != 0 {
			t.Errorf("the registry was contacted %d time(s) despite a rejected hop", got)
		}
	})

	t.Run("with no credential", func(t *testing.T) {
		upstream := &fakeUpstreamRT{}
		forwarder := twoHopChain(t, cp, upstream, peerAuth, "")
		resp := handlerTransport{handler: forwarder}.mustRoundTrip(t, http.MethodGet, testUpstreamHost, "http://gateway/v2/app/manifests/v1", "")
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
		}
	})
}

func TestTwoHopsRelayAPolicyDenial(t *testing.T) {
	// A policy denial is the serving gateway's answer, not a hop failure, so it must
	// reach the client exactly as it would in a single-hop deployment.
	cp := allowHostPolicy(t, testUpstreamHost, "blob:read", "manifest:read")
	upstream := &fakeUpstreamRT{}
	forwarder := twoHopChain(t, cp, upstream, nil, "")

	resp := handlerTransport{handler: forwarder}.mustRoundTrip(t, http.MethodPut, testUpstreamHost, "http://gateway/v2/app/manifests/v1", "{}")

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "DENIED") {
		t.Errorf("body = %q, want the gateway's DENIED error", body)
	}
	if got := len(upstream.requests); got != 0 {
		t.Errorf("the registry was contacted %d time(s) despite the denial", got)
	}
}

func TestTwoHopsHealthEndpointNeedsNoCredential(t *testing.T) {
	// A Kubernetes readiness probe has to reach a listener that otherwise requires
	// authentication.
	cp := allowHostPolicy(t, testUpstreamHost, "*")
	peerAuth := newTestPeerAuth(t, PeerAuthOptions{TokenFiles: []string{writeTokens(t, testToken)}})
	serving := New(
		WithAuthorizer(cp),
		WithKeychain(authn.NewMultiKeychain()),
		WithLogger(log.New(io.Discard, "", 0)),
		WithBaseTransport(&fakeUpstreamRT{}),
		WithPeerAuth(peerAuth),
	)

	resp := handlerTransport{handler: serving}.mustRoundTrip(t, http.MethodGet, "", "http://gateway"+healthPath, "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("%s status = %d, want %d", healthPath, resp.StatusCode, http.StatusOK)
	}

	// Everything else on the same listener still needs a credential — including the
	// version check, so an unauthenticated client cannot even probe which registries
	// are allowed.
	for _, target := range []string{"http://gateway/v2/", "http://gateway/v2/app/manifests/v1"} {
		resp := handlerTransport{handler: serving}.mustRoundTrip(t, http.MethodGet, testUpstreamHost, target, "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s status = %d, want %d", target, resp.StatusCode, http.StatusUnauthorized)
		}
		if got := resp.Header.Get("WWW-Authenticate"); got != "" {
			t.Errorf("%s sent WWW-Authenticate %q; go-containerregistry would try a token exchange against the gateway", target, got)
		}
		if got := resp.Header.Get(gatewayErrorHeader); got != errPeerUnauthenticated {
			t.Errorf("%s %s = %q, want %q", target, gatewayErrorHeader, got, errPeerUnauthenticated)
		}
	}
}
