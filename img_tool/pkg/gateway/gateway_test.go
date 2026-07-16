package gateway

import (
	"net/http"
	"testing"
)

// recordingRT records the last request it received and returns a canned 200.
type recordingRT struct {
	last *http.Request
}

func (r *recordingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	r.last = req
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
}

func TestEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     Mode
		env      map[string]string
		expected string
	}{
		{
			name:     "pull specific wins",
			mode:     ModePull,
			env:      map[string]string{EnvPullGateway: "http://pull", EnvGateway: "http://fallback"},
			expected: "http://pull",
		},
		{
			name:     "push specific wins",
			mode:     ModePush,
			env:      map[string]string{EnvPushGateway: "http://push", EnvGateway: "http://fallback"},
			expected: "http://push",
		},
		{
			name:     "pull falls back",
			mode:     ModePull,
			env:      map[string]string{EnvGateway: "http://fallback"},
			expected: "http://fallback",
		},
		{
			name:     "push falls back",
			mode:     ModePush,
			env:      map[string]string{EnvGateway: "http://fallback"},
			expected: "http://fallback",
		},
		{
			name:     "push does not use pull",
			mode:     ModePush,
			env:      map[string]string{EnvPullGateway: "http://pull"},
			expected: "",
		},
		{
			name:     "none configured",
			mode:     ModePull,
			env:      map[string]string{},
			expected: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvGateway, "")
			t.Setenv(EnvPushGateway, "")
			t.Setenv(EnvPullGateway, "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := Endpoint(tc.mode); got != tc.expected {
				t.Fatalf("Endpoint(%v) = %q, want %q", tc.mode, got, tc.expected)
			}
		})
	}
}

func TestWrapTransportNoGateway(t *testing.T) {
	t.Setenv(EnvGateway, "")
	t.Setenv(EnvPushGateway, "")
	t.Setenv(EnvPullGateway, "")

	base := &recordingRT{}
	got, err := WrapTransport(base, ModePull)
	if err != nil {
		t.Fatalf("WrapTransport: %v", err)
	}
	if got != http.RoundTripper(base) {
		t.Fatalf("WrapTransport returned a wrapper when no gateway is configured")
	}
}

func TestNewTransportRewritesHTTP(t *testing.T) {
	base := &recordingRT{}
	rt, err := NewTransport("https://gw.example.com:9000", base)
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, "https://index.docker.io/v2/library/ubuntu/manifests/latest", nil)
	req.Header.Set("Authorization", "Bearer secret")
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	got := base.last
	if got.URL.Scheme != "https" || got.URL.Host != "gw.example.com:9000" {
		t.Fatalf("URL not rewritten to gateway: got %s://%s", got.URL.Scheme, got.URL.Host)
	}
	if got.URL.Path != "/v2/library/ubuntu/manifests/latest" {
		t.Fatalf("path not preserved: %s", got.URL.Path)
	}
	if h := got.Header.Get(OriginalHostHeader); h != "index.docker.io" {
		t.Fatalf("original host header = %q, want index.docker.io", h)
	}
	if h := got.Header.Get("Authorization"); h != "" {
		t.Fatalf("Authorization header should be stripped, got %q", h)
	}
	// The caller's request must be untouched.
	if req.URL.Host != "index.docker.io" {
		t.Fatalf("caller request was mutated: host is %s", req.URL.Host)
	}
	if req.Header.Get(OriginalHostHeader) != "" {
		t.Fatalf("caller request gained the original-host header")
	}
}

func TestNewTransportHostHeaderDerivedFromGateway(t *testing.T) {
	base := &recordingRT{}
	rt, err := NewTransport("http://gw.internal", base)
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://ghcr.io/v2/", nil)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if base.last.Host != "" {
		t.Fatalf("Host should be cleared so net/http derives it from the gateway URL, got %q", base.last.Host)
	}
	if base.last.URL.Host != "gw.internal" {
		t.Fatalf("gateway host = %q, want gw.internal", base.last.URL.Host)
	}
}

func TestNewTransportUnixKeepsHost(t *testing.T) {
	base := &recordingRT{}
	rt, err := NewTransport("unix:/run/gw.sock", base)
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	tr, ok := rt.(*transport)
	if !ok || !tr.unix {
		t.Fatalf("expected unix transport, got %T (unix=%v)", rt, ok && tr.unix)
	}
	// Swap in the recorder so we can observe the rewritten request without
	// actually dialing a socket.
	tr.base = base

	req, _ := http.NewRequest(http.MethodGet, "https://quay.io/v2/foo/bar/blobs/sha256:deadbeef", nil)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if base.last.URL.Scheme != "http" {
		t.Fatalf("unix transport should force http scheme, got %q", base.last.URL.Scheme)
	}
	if base.last.URL.Host != "quay.io" {
		t.Fatalf("unix transport should keep the original host in the URL, got %q", base.last.URL.Host)
	}
	if h := base.last.Header.Get(OriginalHostHeader); h != "quay.io" {
		t.Fatalf("original host header = %q, want quay.io", h)
	}
}

func TestNewTransportErrors(t *testing.T) {
	for _, endpoint := range []string{
		"ftp://nope",
		"unix:",
		"://missing-scheme",
		"http://",
	} {
		if _, err := NewTransport(endpoint, nil); err == nil {
			t.Errorf("NewTransport(%q) expected error, got nil", endpoint)
		}
	}
}
