package registryopts

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/gateway"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// countingBody records whether it was closed so tests can assert connection
// cleanup on the cancellation path.
type countingBody struct {
	r      *strings.Reader
	closed bool
}

func (b *countingBody) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *countingBody) Close() error {
	b.closed = true
	return nil
}

func newRequest(t *testing.T, ctx context.Context) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.test/v2/foo/manifests/latest", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	return req
}

func TestDefaultOptionsAndOverrides(t *testing.T) {
	base := Default()
	if got := len(base.Remote()); got != 3 {
		t.Fatalf("Default() has %d options, want 3 (auth, retry, jobs)", got)
	}

	// With is immutable: branching does not mutate the receiver.
	branched := base.With(remote.WithUserAgent("a"), remote.WithUserAgent("b"))
	if got := len(base.Remote()); got != 3 {
		t.Fatalf("Default() mutated by With: len=%d, want 3", got)
	}
	if got := len(branched.Remote()); got != 5 {
		t.Fatalf("With(x,y) len=%d, want 5", got)
	}

	// WithJobs(<=0) is a no-op (go-cr rejects WithJobs(0)); positive appends one.
	if got := len(base.WithJobs(0).Remote()); got != 3 {
		t.Fatalf("WithJobs(0) changed length to %d, want 3", got)
	}
	if got := len(base.WithJobs(8).Remote()); got != 4 {
		t.Fatalf("WithJobs(8) length %d, want 4", got)
	}

	// Push/Pull add a transport on top of the defaults.
	push, err := Push()
	if err != nil {
		t.Fatalf("Push() error: %v", err)
	}
	if got := len(push.Remote()); got != 4 {
		t.Fatalf("Push() has %d options, want 4 (defaults + transport)", got)
	}
	pull, err := Pull()
	if err != nil {
		t.Fatalf("Pull() error: %v", err)
	}
	if got := len(pull.Remote()); got != 4 {
		t.Fatalf("Pull() has %d options, want 4", got)
	}
}

func TestTransportBuilds(t *testing.T) {
	// With no gateway configured, Transport returns a non-nil pacing transport.
	t.Setenv(gateway.EnvGateway, "")
	t.Setenv(gateway.EnvPushGateway, "")
	t.Setenv(gateway.EnvPullGateway, "")
	rt, err := Transport(gateway.ModePull)
	if err != nil {
		t.Fatalf("Transport error: %v", err)
	}
	if rt == nil {
		t.Fatalf("Transport returned nil")
	}
	if _, ok := rt.(*retryAfterTransport); !ok {
		t.Fatalf("Transport = %T, want *retryAfterTransport", rt)
	}
}

// setInsecureForTest turns insecure mode on (or off) for the duration of the
// test, restoring the previous process-wide value afterwards.
func setInsecureForTest(t *testing.T, enabled bool) {
	t.Helper()
	previous := Insecure()
	SetInsecure(enabled)
	t.Cleanup(func() { SetInsecure(previous) })
}

func TestInsecureFromEnv(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{value: "", want: false},
		{value: "0", want: false},
		{value: "false", want: false},
		{value: "FALSE", want: false},
		{value: " ", want: false},
		{value: "1", want: true},
		{value: "true", want: true},
		{value: "True", want: true},
		{value: "yes", want: true},
	}
	for _, tc := range tests {
		t.Run("value="+tc.value, func(t *testing.T) {
			t.Setenv(EnvInsecure, tc.value)
			if got := insecureFromEnv(); got != tc.want {
				t.Fatalf("insecureFromEnv() with %s=%q = %v, want %v", EnvInsecure, tc.value, got, tc.want)
			}
		})
	}
}

// TestNameOptionsInsecure covers the half of insecure mode that made pushes to a
// plain-HTTP registry fail with "server gave HTTP response to HTTPS client": the
// reference's registry must resolve to http.
func TestNameOptionsInsecure(t *testing.T) {
	setInsecureForTest(t, false)
	if got := len(NameOptions()); got != 0 {
		t.Fatalf("NameOptions() with insecure off returned %d options, want 0", got)
	}
	secureRef, err := name.NewTag("registry.example.com/app:latest", NameOptions()...)
	if err != nil {
		t.Fatalf("NewTag: %v", err)
	}
	if got := secureRef.Context().Registry.Scheme(); got != "https" {
		t.Fatalf("scheme with insecure off = %q, want https", got)
	}

	SetInsecure(true)
	insecureRef, err := name.NewTag("registry.example.com/app:latest", NameOptions()...)
	if err != nil {
		t.Fatalf("NewTag: %v", err)
	}
	if got := insecureRef.Context().Registry.Scheme(); got != "http" {
		t.Fatalf("scheme with insecure on = %q, want http", got)
	}

	// Caller-supplied options are kept, and name.Insecure is appended to them.
	ref, err := name.NewRegistry("", NameOptions(name.WithDefaultRegistry("registry.example.com"))...)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if ref.RegistryStr() != "registry.example.com" {
		t.Fatalf("RegistryStr = %q, want the default registry passed in", ref.RegistryStr())
	}
	if got := ref.Scheme(); got != "http" {
		t.Fatalf("scheme = %q, want http", got)
	}
}

// TestBaseTransportInsecure covers the other half: untrusted TLS certificates are
// accepted, without mutating go-cr's shared default transport.
func TestBaseTransportInsecure(t *testing.T) {
	setInsecureForTest(t, false)
	if got := BaseTransport(); got != remote.DefaultTransport {
		t.Fatalf("BaseTransport() with insecure off = %v, want remote.DefaultTransport", got)
	}

	SetInsecure(true)
	rt := BaseTransport()
	transport, ok := rt.(*http.Transport)
	if !ok {
		t.Fatalf("BaseTransport() = %T, want *http.Transport", rt)
	}
	if transport == remote.DefaultTransport {
		t.Fatalf("BaseTransport() returned the shared remote.DefaultTransport; it must be a clone")
	}
	if transport.TLSClientConfig == nil || !transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("BaseTransport() TLSClientConfig = %+v, want InsecureSkipVerify", transport.TLSClientConfig)
	}
	if defaultTransport, ok := remote.DefaultTransport.(*http.Transport); ok {
		if defaultTransport.TLSClientConfig != nil && defaultTransport.TLSClientConfig.InsecureSkipVerify {
			t.Fatalf("remote.DefaultTransport was mutated to skip TLS verification")
		}
	}
}

// TestDefaultInsecureInstallsTransport verifies that callers which only take the
// enforced defaults (no explicit transport) still get the insecure transport.
func TestDefaultInsecureInstallsTransport(t *testing.T) {
	setInsecureForTest(t, true)
	if got := len(Default().Remote()); got != 4 {
		t.Fatalf("Default() with insecure on has %d options, want 4 (auth, retry, jobs, transport)", got)
	}
	SetInsecure(false)
	if got := len(Default().Remote()); got != 3 {
		t.Fatalf("Default() with insecure off has %d options, want 3", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    time.Duration
		wantOK  bool
		exactly bool
	}{
		{name: "empty", value: "", wantOK: false, exactly: true},
		{name: "seconds", value: "10", want: 10 * time.Second, wantOK: true, exactly: true},
		{name: "zero", value: "0", want: 0, wantOK: true, exactly: true},
		{name: "whitespace", value: "  5 ", want: 5 * time.Second, wantOK: true, exactly: true},
		{name: "negative", value: "-3", wantOK: false, exactly: true},
		{name: "garbage", value: "soon", wantOK: false, exactly: true},
		{name: "past date", value: "Mon, 02 Jan 2006 15:04:05 GMT", want: 0, wantOK: true, exactly: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseRetryAfter(tc.value)
			if ok != tc.wantOK {
				t.Fatalf("parseRetryAfter(%q) ok = %v, want %v", tc.value, ok, tc.wantOK)
			}
			if tc.exactly && got != tc.want {
				t.Fatalf("parseRetryAfter(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}

	future := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	got, ok := parseRetryAfter(future)
	if !ok {
		t.Fatalf("parseRetryAfter(future date) ok = false, want true")
	}
	if got <= 0 || got > 120*time.Second {
		t.Fatalf("parseRetryAfter(future date) = %v, want (0, 120s]", got)
	}
}

func TestRetryBackoffDefaultsAndEnv(t *testing.T) {
	t.Setenv(EnvRetryMaxAttempts, "")
	t.Setenv(EnvRetryBaseDelay, "")
	t.Setenv(EnvRetryMaxDelay, "")
	b := RetryBackoff()
	if b.Steps != defaultRetryMaxAttempts || b.Duration != defaultRetryBaseDelay || b.Cap != defaultRetryMaxDelay {
		t.Fatalf("default backoff = %+v, want steps=%d dur=%v cap=%v", b, defaultRetryMaxAttempts, defaultRetryBaseDelay, defaultRetryMaxDelay)
	}
	if b.Factor != retryBackoffFactor || b.Jitter != retryBackoffJitter {
		t.Fatalf("default backoff factor/jitter = %v/%v, want %v/%v", b.Factor, b.Jitter, retryBackoffFactor, retryBackoffJitter)
	}

	t.Setenv(EnvRetryMaxAttempts, "9")
	t.Setenv(EnvRetryBaseDelay, "250ms")
	t.Setenv(EnvRetryMaxDelay, "5s")
	b = RetryBackoff()
	if b.Steps != 9 || b.Duration != 250*time.Millisecond || b.Cap != 5*time.Second {
		t.Fatalf("env backoff = %+v, want steps=9 dur=250ms cap=5s", b)
	}

	t.Setenv(EnvRetryMaxAttempts, "0")
	if got := RetryBackoff().Steps; got != 1 {
		t.Fatalf("attempts=0 clamps to %d, want 1", got)
	}
	t.Setenv(EnvRetryMaxAttempts, "-5")
	if got := RetryBackoff().Steps; got != 1 {
		t.Fatalf("attempts=-5 clamps to %d, want 1", got)
	}

	t.Setenv(EnvRetryMaxAttempts, "")
	t.Setenv(EnvRetryBaseDelay, "nonsense")
	t.Setenv(EnvRetryMaxDelay, "-1s")
	b = RetryBackoff()
	if b.Duration != defaultRetryBaseDelay || b.Cap != defaultRetryMaxDelay {
		t.Fatalf("invalid durations = dur %v cap %v, want defaults", b.Duration, b.Cap)
	}
}

func TestWrapRetryAfterPacesAndRestoresBody(t *testing.T) {
	t.Setenv(EnvRetryMaxDelay, "20ms")

	const bodyText = `{"errors":[{"code":"TOOMANYREQUESTS"}]}`
	var attempts int
	rt := WrapRetryAfter(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": []string{"30"}},
			Body:       io.NopCloser(strings.NewReader(bodyText)),
			Request:    r,
		}, nil
	}))

	start := time.Now()
	resp, err := rt.RoundTrip(newRequest(t, context.Background()))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("RoundTrip returned error: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("inner RoundTrip called %d times, want 1 (wrapper must not retry itself)", attempts)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if elapsed < 10*time.Millisecond {
		t.Fatalf("elapsed = %v, expected pacing >= ~10ms", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("elapsed = %v, expected the wait to be capped well under 30s", elapsed)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading restored body: %v", err)
	}
	if string(got) != bodyText {
		t.Fatalf("restored body = %q, want %q", got, bodyText)
	}
}

func TestWrapRetryAfterSkipsWhenNotApplicable(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header http.Header
	}{
		{name: "success", status: http.StatusOK, header: http.Header{"Retry-After": []string{"30"}}},
		{name: "429 without header", status: http.StatusTooManyRequests, header: http.Header{}},
		{name: "500 without header", status: http.StatusInternalServerError, header: http.Header{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := WrapRetryAfter(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: tc.status,
					Header:     tc.header,
					Body:       io.NopCloser(strings.NewReader("body")),
					Request:    r,
				}, nil
			}))
			start := time.Now()
			resp, err := rt.RoundTrip(newRequest(t, context.Background()))
			if err != nil {
				t.Fatalf("RoundTrip error: %v", err)
			}
			if time.Since(start) > 500*time.Millisecond {
				t.Fatalf("unexpected pacing delay for %s", tc.name)
			}
			if _, err := io.ReadAll(resp.Body); err != nil {
				t.Fatalf("reading body: %v", err)
			}
		})
	}
}

func TestWrapRetryAfterRespectsContextCancellation(t *testing.T) {
	t.Setenv(EnvRetryMaxDelay, "60s")

	body := &countingBody{r: strings.NewReader("rate limited")}
	rt := WrapRetryAfter(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     http.Header{"Retry-After": []string{"30"}},
			Body:       body,
			Request:    r,
		}, nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	resp, err := rt.RoundTrip(newRequest(t, ctx))
	if err == nil {
		t.Fatalf("expected context cancellation error, got nil (resp=%v)", resp)
	}
	if !isContextError(err) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("cancellation did not interrupt the pacing wait")
	}
	if !body.closed {
		t.Fatalf("response body was not closed on cancellation")
	}
}

func isContextError(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}

// plainHTTPRegistry returns a RoundTripper that behaves like a registry which only
// speaks plain HTTP: an https request fails the way a TLS handshake against a
// plaintext server does, an http request is served by an in-memory OCI registry.
// It also records the scheme of every request, so a test can tell which one
// go-containerregistry ended up using. No socket is bound.
func plainHTTPRegistry() (http.RoundTripper, *[]string) {
	handler := registry.New()
	var mu sync.Mutex
	schemes := &[]string{}
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		*schemes = append(*schemes, req.URL.Scheme)
		mu.Unlock()
		if req.URL.Scheme != "http" {
			return nil, errors.New("http: server gave HTTP response to HTTPS client")
		}
		// The handler expects a server-side request, which always has a body.
		serverReq := req.Clone(req.Context())
		if serverReq.Body == nil {
			serverReq.Body = http.NoBody
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, serverReq)
		resp := recorder.Result()
		resp.Request = req
		return resp, nil
	}), schemes
}

// TestInsecurePushToPlainHTTPRegistry is the end-to-end shape of the problem
// insecure mode solves: pushing to a registry that only speaks plain HTTP under a
// host name go-containerregistry does not recognize as local (so not localhost,
// *.localhost, a loopback address or an RFC1918 address) -- the shape of a k3d
// registry or an /etc/hosts-mapped development registry.
func TestInsecurePushToPlainHTTPRegistry(t *testing.T) {
	const host = "registry.example.com:5000"

	// A failing request must not burn the patient production backoff.
	t.Setenv(EnvRetryMaxAttempts, "1")

	image, err := random.Image(64, 1)
	if err != nil {
		t.Fatalf("building test image: %v", err)
	}

	// With insecure mode on, the push and the read-back fall back to http and
	// succeed.
	setInsecureForTest(t, true)
	transport, schemes := plainHTTPRegistry()
	opts := Default().WithTransport(transport).Remote()
	ref, err := name.NewTag(host+"/app:latest", NameOptions()...)
	if err != nil {
		t.Fatalf("NewTag: %v", err)
	}
	if err := remote.Write(ref, image, opts...); err != nil {
		t.Fatalf("pushing to a plain-HTTP registry with insecure mode on: %v", err)
	}
	if _, err := remote.Get(ref, opts...); err != nil {
		t.Fatalf("reading back from a plain-HTTP registry with insecure mode on: %v", err)
	}
	if !slices.Contains(*schemes, "http") {
		t.Fatalf("no request was made over http (schemes: %v)", *schemes)
	}

	// Without it, go-cr only ever tries https and the push fails -- the error
	// reported in the issue this flag fixes.
	SetInsecure(false)
	secureTransport, secureSchemes := plainHTTPRegistry()
	secureRef, err := name.NewTag(host+"/app:latest", NameOptions()...)
	if err != nil {
		t.Fatalf("NewTag: %v", err)
	}
	err = remote.Write(secureRef, image, Default().WithTransport(secureTransport).Remote()...)
	if err == nil {
		t.Fatal("pushing over https to a plain-HTTP registry succeeded, want a failure")
	}
	if !strings.Contains(err.Error(), "gave HTTP response to HTTPS client") {
		t.Fatalf("error = %v, want the plain-HTTP-vs-HTTPS failure", err)
	}
	if slices.Contains(*secureSchemes, "http") {
		t.Fatalf("an http request was made with insecure mode off (schemes: %v)", *secureSchemes)
	}
}
