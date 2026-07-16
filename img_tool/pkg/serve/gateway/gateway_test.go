package gateway

import (
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"

	clientgateway "github.com/bazel-contrib/rules_img/img_tool/pkg/gateway"
)

const testUpstreamHost = "registry.test"

// fakeUpstreamRT is an in-memory registry used as the gateway's base transport.
// Using a RoundTripper (rather than httptest.NewServer) keeps the tests
// hermetic and avoids binding a network port.
type fakeUpstreamRT struct {
	// requests records the upstream requests observed, for assertions.
	requests []*http.Request
}

func (f *fakeUpstreamRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.requests = append(f.requests, req)
	resp := func(status int, hdr http.Header, body string) *http.Response {
		if hdr == nil {
			hdr = http.Header{}
		}
		return &http.Response{
			StatusCode:    status,
			Status:        http.StatusText(status),
			Header:        hdr,
			Body:          io.NopCloser(strings.NewReader(body)),
			ContentLength: int64(len(body)),
			Request:       req,
		}
	}
	switch {
	case req.URL.Path == "/v2/":
		return resp(http.StatusOK, nil, ""), nil
	case strings.Contains(req.URL.Path, "/manifests/"):
		h := http.Header{}
		h.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		h.Set("Docker-Content-Digest", "sha256:deadbeef")
		return resp(http.StatusOK, h, `{"schemaVersion":2}`), nil
	case strings.HasSuffix(req.URL.Path, "/blobs/uploads/"):
		h := http.Header{}
		// Relative Location the gateway must rewrite to an absolute upstream URL.
		h.Set("Location", "/v2/app/blobs/uploads/upload-id?_state=xyz")
		return resp(http.StatusAccepted, h, ""), nil
	default:
		return resp(http.StatusNotFound, nil, ""), nil
	}
}

func newTestHandler(policy Policy, allowHost string, base http.RoundTripper) *Handler {
	return New(
		WithPolicy(policy),
		WithAllowedRegistries([]*regexp.Regexp{regexp.MustCompile("^(?:" + regexp.QuoteMeta(allowHost) + ")$")}),
		WithKeychain(authn.NewMultiKeychain()), // always anonymous, hermetic
		WithLogger(log.New(io.Discard, "", 0)),
		WithBaseTransport(base),
	)
}

func do(h *Handler, method, host, path string) *http.Response {
	r, _ := http.NewRequest(method, "http://gateway"+path, nil)
	if host != "" {
		r.Header.Set(clientgateway.OriginalHostHeader, host)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Result()
}

func TestForwardManifestRead(t *testing.T) {
	up := &fakeUpstreamRT{}
	h := newTestHandler(Policy{AllowManifestRead: true}, testUpstreamHost, up)

	resp := do(h, http.MethodGet, testUpstreamHost, "/v2/app/manifests/latest")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "schemaVersion") {
		t.Fatalf("unexpected body: %s", body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "image.manifest") {
		t.Fatalf("content-type not forwarded: %q", ct)
	}
	// The upstream request should target the real registry over https.
	var forwarded *http.Request
	for _, r := range up.requests {
		if strings.Contains(r.URL.Path, "/manifests/") {
			forwarded = r
		}
	}
	if forwarded == nil {
		t.Fatal("manifest request was not forwarded upstream")
	}
	if forwarded.URL.Scheme != "https" || forwarded.URL.Host != testUpstreamHost {
		t.Fatalf("forwarded to %s://%s, want https://%s", forwarded.URL.Scheme, forwarded.URL.Host, testUpstreamHost)
	}
	if forwarded.Header.Get(clientgateway.OriginalHostHeader) != "" {
		t.Fatalf("gateway control header leaked upstream")
	}
}

func TestForwardDeniedByPolicy(t *testing.T) {
	h := newTestHandler(Policy{AllowManifestRead: false, AllowBlobRead: true}, testUpstreamHost, &fakeUpstreamRT{})
	resp := do(h, http.MethodGet, testUpstreamHost, "/v2/app/manifests/latest")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestForwardManifestWriteDeniedWhenOnlyReadAllowed(t *testing.T) {
	h := newTestHandler(Policy{AllowManifestRead: true}, testUpstreamHost, &fakeUpstreamRT{})
	resp := do(h, http.MethodPut, testUpstreamHost, "/v2/app/manifests/latest")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestHostNotAllowed(t *testing.T) {
	h := newTestHandler(Policy{AllowManifestRead: true}, testUpstreamHost, &fakeUpstreamRT{})
	resp := do(h, http.MethodGet, "evil.example.com", "/v2/app/manifests/latest")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestMissingHostHeader(t *testing.T) {
	h := newTestHandler(Policy{AllowManifestRead: true}, testUpstreamHost, &fakeUpstreamRT{})
	resp := do(h, http.MethodGet, "", "/v2/app/manifests/latest")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestDefaultRegistryFallback(t *testing.T) {
	// No header, but a default registry is configured and allow-listed.
	h := New(
		WithPolicy(Policy{AllowManifestRead: true}),
		WithAllowedRegistries([]*regexp.Regexp{regexp.MustCompile("^(?:" + regexp.QuoteMeta(testUpstreamHost) + ")$")}),
		WithKeychain(authn.NewMultiKeychain()),
		WithLogger(log.New(io.Discard, "", 0)),
		WithBaseTransport(&fakeUpstreamRT{}),
		WithDefaultRegistry(testUpstreamHost),
	)
	resp := do(h, http.MethodGet, "", "/v2/app/manifests/latest")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s; expected fallback to default registry", resp.StatusCode, body)
	}

	// An explicit header still overrides the default.
	resp = do(h, http.MethodGet, testUpstreamHost, "/v2/app/manifests/latest")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("explicit header status = %d, want 200", resp.StatusCode)
	}
}

func TestDefaultRegistryStillAllowListed(t *testing.T) {
	// A default registry that is not in the allow-list is still rejected.
	h := New(
		WithPolicy(Policy{AllowManifestRead: true}),
		WithAllowedRegistries([]*regexp.Regexp{regexp.MustCompile("^(?:ghcr\\.io)$")}),
		WithKeychain(authn.NewMultiKeychain()),
		WithLogger(log.New(io.Discard, "", 0)),
		WithBaseTransport(&fakeUpstreamRT{}),
		WithDefaultRegistry(testUpstreamHost),
	)
	resp := do(h, http.MethodGet, "", "/v2/app/manifests/latest")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (default registry not allow-listed)", resp.StatusCode)
	}
}

func TestVersionCheckAnonymous(t *testing.T) {
	h := newTestHandler(Policy{}, testUpstreamHost, &fakeUpstreamRT{})
	resp := do(h, http.MethodGet, testUpstreamHost, "/v2/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("Docker-Distribution-API-Version") != "registry/2.0" {
		t.Fatalf("missing api version header")
	}
	if resp.Header.Get("WWW-Authenticate") != "" {
		t.Fatalf("version check should not challenge for auth")
	}
}

func TestUnknownEndpoint(t *testing.T) {
	h := newTestHandler(Policy{AllowManifestRead: true, AllowBlobRead: true}, testUpstreamHost, &fakeUpstreamRT{})
	resp := do(h, http.MethodGet, testUpstreamHost, "/v2/app/whatever")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestForwardBlobUploadRewritesLocation(t *testing.T) {
	h := newTestHandler(Policy{AllowBlobWrite: true}, testUpstreamHost, &fakeUpstreamRT{})
	resp := do(h, http.MethodPost, testUpstreamHost, "/v2/app/blobs/uploads/")
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	// The relative Location must be rewritten to an absolute upstream URL so the
	// client re-routes it through the gateway with the correct original host.
	want := "https://" + testUpstreamHost + "/v2/app/blobs/uploads/upload-id"
	if !strings.HasPrefix(loc, want) {
		t.Fatalf("Location = %q, want prefix %q", loc, want)
	}
	if !strings.Contains(loc, "_state=xyz") {
		t.Fatalf("Location lost query: %q", loc)
	}
}

func TestRewriteLocation(t *testing.T) {
	repo, err := name.NewRepository("registry.example.com/app")
	if err != nil {
		t.Fatal(err)
	}
	got := rewriteLocation("/v2/app/blobs/uploads/id?_state=x", repo)
	if got != "https://registry.example.com/v2/app/blobs/uploads/id?_state=x" {
		t.Fatalf("relative rewrite = %q", got)
	}
	abs := "https://cdn.example.net/blob?sig=abc"
	if rewriteLocation(abs, repo) != abs {
		t.Fatalf("absolute location was rewritten")
	}
	if rewriteLocation("", repo) != "" {
		t.Fatalf("empty location changed")
	}
}

func TestCopyHeaderStripsControlAndAuth(t *testing.T) {
	src := http.Header{}
	src.Set("Accept", "application/json")
	src.Set("Authorization", "Bearer secret")
	src.Set(clientgateway.OriginalHostHeader, "registry.example.com")
	src.Set("Connection", "X-Custom")
	src.Set("X-Custom", "drop-me")
	src.Set("Keep-Alive", "timeout=5")

	dst := http.Header{}
	copyHeader(dst, src)

	if dst.Get("Accept") != "application/json" {
		t.Errorf("Accept not copied")
	}
	if dst.Get("Authorization") != "" {
		t.Errorf("Authorization must be stripped")
	}
	if dst.Get(clientgateway.OriginalHostHeader) != "" {
		t.Errorf("original-host header must be stripped")
	}
	if dst.Get("Keep-Alive") != "" {
		t.Errorf("hop-by-hop Keep-Alive must be stripped")
	}
	if dst.Get("X-Custom") != "" {
		t.Errorf("header named in Connection must be stripped")
	}
}

func TestValidateRedirectTarget(t *testing.T) {
	blocked := []string{
		"https://127.0.0.1/blob",
		"http://10.1.2.3/blob",
		"https://169.254.169.254/latest/meta-data",
		"https://192.168.0.5/blob",
		"https://[::1]/blob",
		"ftp://example.com/blob",
	}
	for _, raw := range blocked {
		u, _ := url.Parse(raw)
		if err := validateRedirectTarget(u); err == nil {
			t.Errorf("validateRedirectTarget(%q) = nil, want error", raw)
		}
	}
	allowed := []string{
		"https://production.cloudflare.docker.com/blob",
		"https://ghcr-blobs.example.net/blob?sig=abc",
		"https://8.8.8.8/blob", // public IP literal is fine
	}
	for _, raw := range allowed {
		u, _ := url.Parse(raw)
		if err := validateRedirectTarget(u); err != nil {
			t.Errorf("validateRedirectTarget(%q) = %v, want nil", raw, err)
		}
	}
}

func TestCheckRedirect(t *testing.T) {
	cdn, _ := http.NewRequest(http.MethodGet, "https://cdn.example.net/blob", nil)
	internal, _ := http.NewRequest(http.MethodGet, "https://169.254.169.254/", nil)

	// Reads may follow a redirect to a public host.
	if err := checkRedirect(http.MethodGet)(cdn, nil); err != nil {
		t.Errorf("GET redirect to public host should be followed: %v", err)
	}
	// Reads must not follow a redirect to a link-local address (SSRF guard).
	if err := checkRedirect(http.MethodGet)(internal, nil); err == nil {
		t.Errorf("GET redirect to link-local address should be refused")
	}
	// Writes never follow redirects; the 3xx is passed back to the client.
	if err := checkRedirect(http.MethodPut)(cdn, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("PUT redirect should return ErrUseLastResponse, got %v", err)
	}
	// Redirect loops are capped.
	via := make([]*http.Request, 10)
	if err := checkRedirect(http.MethodGet)(cdn, via); err == nil || errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("redirect chain of 10 should be stopped, got %v", err)
	}
}

func TestAllowlistUsesResolvedRegistry(t *testing.T) {
	// A header of "docker.io" resolves to index.docker.io; the allow-list must
	// be enforced against the resolved registry.
	allowIndex := newTestHandler(Policy{AllowManifestRead: true}, "index.docker.io", &fakeUpstreamRT{})
	if resp := do(allowIndex, http.MethodGet, "docker.io", "/v2/library/ubuntu/manifests/latest"); resp.StatusCode != http.StatusOK {
		t.Fatalf("docker.io should resolve to allowed index.docker.io, got %d", resp.StatusCode)
	}

	// A bare "myregistry" (no dot) resolves to Docker Hub, NOT to a host named
	// "myregistry"; allowing only "myregistry" must therefore deny it.
	allowMyReg := newTestHandler(Policy{AllowManifestRead: true}, "myregistry", &fakeUpstreamRT{})
	if resp := do(allowMyReg, http.MethodGet, "myregistry", "/v2/app/manifests/latest"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bare 'myregistry' header resolves to Docker Hub and must be denied, got %d", resp.StatusCode)
	}
}
