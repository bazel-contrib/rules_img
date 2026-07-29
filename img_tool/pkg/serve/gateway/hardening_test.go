package gateway

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"

	clientgateway "github.com/bazel-contrib/rules_img/img_tool/pkg/gateway"
)

// This file covers the hardening that concentrating a build farm's registry
// credentials into one shared, network-reachable deployment made necessary. Each
// case was self-inflicted at worst when the gateway was a per-pod sidecar, and is
// cross-tenant once it is shared.

func TestCopyHeaderDropsTheReservedNamespaceAndForwardingHeaders(t *testing.T) {
	src := http.Header{}
	// The gateway's own control headers, plus one that does not exist yet: the
	// prefix is dropped wholesale so a header added later cannot leak by omission.
	src.Set(clientgateway.OriginalHostHeader, "registry.test")
	src.Set(forwardedByHeader, "some-forwarder")
	src.Set(gatewayErrorHeader, "peer_unauthenticated")
	src.Set(requestIDHeader, "abc")
	src.Set("X-rules_img-Something-New", "value")
	// A worker pod's address has no business reaching a registry.
	src.Set("X-Forwarded-For", "10.0.0.1")
	src.Set("X-Forwarded-Host", "evil.test")
	src.Set("X-Forwarded-Proto", "https")
	src.Set("Forwarded", "for=10.0.0.1")
	// The credential of the hop the request arrived on.
	src.Set("Authorization", "Bearer peer-credential")
	src.Set("Host", "gateway")
	// ...and what must survive.
	src.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
	src.Set("Range", "bytes=0-1023")

	dst := http.Header{}
	copyHeader(dst, src)

	for header := range dst {
		if strings.HasPrefix(http.CanonicalHeaderKey(header), reservedHeaderPrefix) {
			t.Errorf("header %s was forwarded upstream", header)
		}
	}
	for _, header := range []string{
		"Authorization", "Host", "X-Forwarded-For", "X-Forwarded-Host",
		"X-Forwarded-Proto", "Forwarded",
	} {
		if got := dst.Get(header); got != "" {
			t.Errorf("%s = %q, want it dropped", header, got)
		}
	}
	for _, header := range []string{"Accept", "Range"} {
		if dst.Get(header) == "" {
			t.Errorf("%s was dropped, but registries need it", header)
		}
	}
}

func TestClassifyUsesTheEscapedPath(t *testing.T) {
	// The gateway forwards r.URL.RequestURI(), which is the escaped path, so the
	// authorization decision has to be taken on the same bytes. Matching the decoded
	// path instead would authorize "/v2/a%2Fb/manifests/x" as repository "a/b" while
	// the registry receives "a%2Fb".
	r := httptest.NewRequest(http.MethodGet, "http://gateway/v2/a%2Fb/manifests/x", nil)
	if got := r.URL.Path; got != "/v2/a/b/manifests/x" {
		t.Fatalf("precondition: decoded path = %q, want the escape decoded", got)
	}
	if cls, ok := classify(r); ok {
		t.Errorf("classify accepted a percent-escaped repository as %q; the registry would see something else", cls.repo)
	}

	// An upload-session reference is opaque and may legitimately contain escapes, so
	// that group must still match.
	upload := httptest.NewRequest(http.MethodPatch, "http://gateway/v2/app/blobs/uploads/upload%2Fid?_state=x", nil)
	cls, ok := classify(upload)
	if !ok {
		t.Fatal("classify rejected an upload session whose reference contains an escape")
	}
	if cls.repo != "app" || cls.op != opNameBlobUpload {
		t.Errorf("classify = (repo %q, op %q), want (app, %s)", cls.repo, cls.op, opNameBlobUpload)
	}
}

func TestLogLinesQuoteTheRequestPath(t *testing.T) {
	// url.URL.Path is percent-decoded, so a request for /v2/x%0A... would otherwise
	// put a real newline in the log and let a client forge audit lines. The shared
	// audit trail is what makes this matter.
	var logs bytes.Buffer
	h := New(
		WithAuthorizer(allowHostPolicy(t, testUpstreamHost, "*")),
		WithKeychain(authn.NewMultiKeychain()),
		WithLogger(log.New(&logs, "", 0)),
		WithBaseTransport(&fakeUpstreamRT{}),
	)
	// A path that decodes to two lines, and is not a valid repository, so it is
	// rejected and logged.
	r := httptest.NewRequest(http.MethodGet, "http://gateway/v2/evil%0A2026-01-01%20forged%20log%20line/manifests/x", nil)
	r.Header.Set(clientgateway.OriginalHostHeader, testUpstreamHost)
	h.ServeHTTP(httptest.NewRecorder(), r)

	if logs.Len() == 0 {
		t.Fatal("the rejected request was not logged")
	}
	if strings.Contains(logs.String(), "\n2026-01-01") {
		t.Errorf("a decoded newline reached the log, so audit lines can be forged:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), `%0A`) {
		t.Errorf("the log does not show the escaped path:\n%s", logs.String())
	}
}

func TestRewriteLocationPreservesEscapes(t *testing.T) {
	// An upload-session reference is opaque. Rebuilding the URL from the decoded
	// path alone would hand the client back a reference the registry never issued.
	repo, err := name.NewRepository(testUpstreamHost + "/app")
	if err != nil {
		t.Fatalf("building the test repository: %v", err)
	}
	for _, tc := range []struct{ in, want string }{
		{"/v2/app/blobs/uploads/plain?_state=x", "https://registry.test/v2/app/blobs/uploads/plain?_state=x"},
		{"/v2/app/blobs/uploads/a%2Fb?_state=x", "https://registry.test/v2/app/blobs/uploads/a%2Fb?_state=x"},
		{"/v2/app/blobs/uploads/id?_state=semi;colon", "https://registry.test/v2/app/blobs/uploads/id?_state=semi;colon"},
		// An absolute Location is the registry's own and is left alone: the client's
		// gateway transport re-routes it using the host it names.
		{"https://cdn.test/blob/abc", "https://cdn.test/blob/abc"},
	} {
		if got := rewriteLocation(tc.in, repo); got != tc.want {
			t.Errorf("rewriteLocation(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// truncatingUpstream returns a response whose body fails part-way through, with no
// Content-Length — the case net/http cannot catch for us.
type truncatingUpstream struct{}

func (truncatingUpstream) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/v2/" {
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader("")), Request: req,
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		// -1 means "unknown length", i.e. a chunked or streaming registry response.
		ContentLength: -1,
		Body:          io.NopCloser(&failingReader{prefix: "partial-blob-"}),
		Request:       req,
	}, nil
}

// failingReader yields a prefix and then fails, like a connection dropping
// mid-blob.
type failingReader struct {
	prefix string
	read   bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if !r.read {
		r.read = true
		return copy(p, r.prefix), nil
	}
	return 0, syscall.ECONNRESET
}

func TestForwardAbortsATruncatedResponse(t *testing.T) {
	// Returning normally after a failed copy delivers a clean, short 200. net/http
	// catches that when the upstream declared a Content-Length, but when it did not,
	// the client silently accepts a truncated blob — or a truncated manifest whose
	// digest is then quietly wrong. Aborting is the only signal left once the status
	// is on the wire, and go-containerregistry retries the unexpected EOF it sees.
	h := New(
		WithAuthorizer(allowHostPolicy(t, testUpstreamHost, "*")),
		WithKeychain(authn.NewMultiKeychain()),
		WithLogger(log.New(io.Discard, "", 0)),
		WithBaseTransport(truncatingUpstream{}),
	)

	var panicked any
	func() {
		defer func() { panicked = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "http://gateway/v2/app/blobs/sha256:abc", nil)
			r.Header.Set(clientgateway.OriginalHostHeader, testUpstreamHost)
			return r
		}())
	}()

	if panicked != http.ErrAbortHandler { //nolint:errorlint // the sentinel is compared, not wrapped
		t.Errorf("recovered %v, want http.ErrAbortHandler so net/http aborts the response", panicked)
	}
}

func TestDenyPrivateAddresses(t *testing.T) {
	// The upstream host comes from a client-supplied header, and
	// go-containerregistry resolves a private or loopback host to a *plaintext*
	// endpoint — so without this guard a policy whose registry pattern is "*" lets a
	// client use the gateway as an internal HTTP proxy. The check runs at dial time,
	// against the address actually resolved, so DNS names and redirects are covered
	// by the same guard.
	guarded, err := DenyPrivateAddresses(http.DefaultTransport)
	if err != nil {
		t.Fatalf("DenyPrivateAddresses: %v", err)
	}
	transport, ok := guarded.(*http.Transport)
	if !ok {
		t.Fatalf("DenyPrivateAddresses returned %T, want *http.Transport", guarded)
	}
	if transport.DialContext == nil {
		t.Fatal("DenyPrivateAddresses did not install a dial guard")
	}

	// Exercise the guard itself rather than dialing: no socket is bound here.
	for _, address := range []string{
		"127.0.0.1:8080", "[::1]:8080", "10.0.0.5:8080", "192.168.1.1:443",
		"172.16.0.1:443", "169.254.169.254:80", "0.0.0.0:80",
	} {
		err := checkDialAddress(address)
		if err == nil {
			t.Errorf("dialing %s was allowed, want it refused", address)
			continue
		}
		if got := transportErrorType(err); got != errPrivateUpstream {
			t.Errorf("dialing %s classified as %q, want %q", address, got, errPrivateUpstream)
		}
	}
	for _, address := range []string{"93.184.216.34:443", "[2606:2800:220:1:248:1893:25c8:1946]:443"} {
		if err := checkDialAddress(address); err != nil {
			t.Errorf("dialing the public address %s was refused: %v", address, err)
		}
	}
}

func TestDenyPrivateAddressesRejectsAnUnsupportedTransport(t *testing.T) {
	if _, err := DenyPrivateAddresses(&fakeUpstreamRT{}); err == nil {
		t.Error("DenyPrivateAddresses accepted a RoundTripper it cannot wrap")
	}
}

func TestPolicyReachesAnyHost(t *testing.T) {
	// What the startup warning keys off.
	if !AllowAll().ReachesAnyHost() {
		t.Error("--dangerously-allow-all does not report an unbounded host reach")
	}
	wildcard := allowHostPolicy(t, "*", "blob:read")
	if !wildcard.ReachesAnyHost() {
		t.Error(`a registry: "*" rule does not report an unbounded host reach`)
	}
	named := allowHostPolicy(t, "registry.test", "blob:read")
	if named.ReachesAnyHost() {
		t.Error("a policy naming its registries reports an unbounded host reach")
	}
}

func TestHealthEndpointIsUnauthenticatedAndSaysNothing(t *testing.T) {
	h := New(
		WithAuthorizer(&CompiledPolicy{}), // deny everything
		WithKeychain(authn.NewMultiKeychain()),
		WithLogger(log.New(io.Discard, "", 0)),
		WithBaseTransport(&fakeUpstreamRT{}),
	)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://gateway"+healthPath, nil))

	if w.Code != http.StatusOK {
		t.Errorf("%s status = %d, want %d", healthPath, w.Code, http.StatusOK)
	}
	if got := strings.TrimSpace(w.Body.String()); got != "ok" {
		t.Errorf("%s body = %q, want %q; it must reveal nothing", healthPath, got, "ok")
	}
}
