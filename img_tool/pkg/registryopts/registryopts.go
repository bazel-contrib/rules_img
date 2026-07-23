// Package registryopts centralizes the go-containerregistry remote.Options that
// the img tool enforces for every registry operation: multi-keychain
// authentication, a patient retry backoff, a transport routed through the
// oci-distribution-gateway (when one is configured) that honors a registry's
// Retry-After header, and a default concurrency.
//
// Callers assemble options through a small builder so the enforced defaults live
// in one place while still allowing per-call additions and overrides:
//
//	pushOpts, err := registryopts.Push()          // auth + retry + gateway/Retry-After transport + jobs
//	if err != nil { ... }
//	pusher, err := remote.NewPusher(pushOpts.WithJobs(n).Remote()...)
//
// Because go-cr applies options in order (last wins), any option added via
// With/WithJobs/WithTransport overrides the corresponding default.
package registryopts

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/registry"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/gateway"
)

// Environment variables that tune how registry pushes and pulls retry on
// transient failures (rate limits, 5xx, network flakes). They apply to every
// go-cr registry operation the img tool performs.
const (
	// EnvRetryMaxAttempts caps the number of attempts (initial try + retries)
	// for a single registry request. Must be >= 1; values below 1 are clamped
	// to 1 (which disables retrying).
	EnvRetryMaxAttempts = "IMG_REGISTRY_RETRY_MAX_ATTEMPTS"
	// EnvRetryBaseDelay is the initial backoff delay (a Go duration string like
	// "1s" or "500ms"). Subsequent delays grow exponentially.
	EnvRetryBaseDelay = "IMG_REGISTRY_RETRY_BASE_DELAY"
	// EnvRetryMaxDelay caps a single backoff/Retry-After wait (a Go duration
	// string). It bounds both the exponential backoff and how long we honor a
	// server's Retry-After header.
	EnvRetryMaxDelay = "IMG_REGISTRY_RETRY_MAX_DELAY"
)

// DefaultJobs is the default registry concurrency (concurrent manifest pushes,
// and the per-pusher concurrent blob transfers via remote.WithJobs). It matches
// go-containerregistry's remote/crane library default of 4, keeping the number
// of concurrent registry requests modest so operations are less likely to trip
// server-side rate limits (HTTP 429). Commands may override it (for example
// `img deploy` defaults to GOMAXPROCS); users override it with --jobs.
const DefaultJobs = 4

const (
	defaultRetryMaxAttempts = 6
	defaultRetryBaseDelay   = 1 * time.Second
	defaultRetryMaxDelay    = 60 * time.Second
	retryBackoffFactor      = 2.0
	retryBackoffJitter      = 0.2
	// retryAfterMaxBodyDrain bounds how much of a rate-limit response body we
	// buffer before sleeping, matching go-cr's own 64 KiB error-body limit.
	retryAfterMaxBodyDrain = 64 * 1024
)

// Options is a builder over the go-cr remote.Options that the img tool enforces
// for registry operations. It is immutable: With/WithJobs/WithTransport return a
// new Options, so a value returned by Default/Push/Pull can be safely branched.
type Options struct {
	opts []remote.Option
}

// Default returns the transport-independent enforced defaults: multi-keychain
// auth, retry backoff, and default concurrency. Callers routing through the
// gateway should use [Push]/[Pull]; callers with a bespoke transport (caching,
// redirect, ...) start here and add [Options.WithTransport].
func Default() *Options {
	return &Options{opts: []remote.Option{
		registry.WithAuthFromMultiKeychain(),
		RetryBackoffOption(),
		remote.WithJobs(DefaultJobs),
	}}
}

// Push returns the enforced defaults for push (write) operations, including a
// transport routed through the push gateway (when configured) that honors
// Retry-After.
func Push() (*Options, error) {
	return withGatewayTransport(gateway.ModePush)
}

// Pull returns the enforced defaults for pull (read) operations, including a
// transport routed through the pull gateway (when configured) that honors
// Retry-After.
func Pull() (*Options, error) {
	return withGatewayTransport(gateway.ModePull)
}

func withGatewayTransport(mode gateway.Mode) (*Options, error) {
	t, err := Transport(mode)
	if err != nil {
		return nil, err
	}
	return Default().WithTransport(t), nil
}

// With returns a copy of o with the given options appended. Because go-cr
// applies options in order, later options override earlier defaults of the same
// kind.
func (o *Options) With(opts ...remote.Option) *Options {
	if len(opts) == 0 {
		return o
	}
	merged := make([]remote.Option, 0, len(o.opts)+len(opts))
	merged = append(merged, o.opts...)
	merged = append(merged, opts...)
	return &Options{opts: merged}
}

// WithJobs overrides the default concurrency. Non-positive values are ignored
// (go-cr rejects WithJobs(<=0)), leaving the existing value in place.
func (o *Options) WithJobs(jobs int) *Options {
	if jobs <= 0 {
		return o
	}
	return o.With(remote.WithJobs(jobs))
}

// WithTransport overrides the default transport, e.g. for a caller that wraps
// its own caching or redirect transport. Wrap the replacement with
// [WrapRetryAfter] if it should also honor Retry-After.
func (o *Options) WithTransport(rt http.RoundTripper) *Options {
	return o.With(remote.WithTransport(rt))
}

// Remote returns the assembled options to pass to go-cr (remote.Get, NewPusher,
// Write, Layer, ...).
func (o *Options) Remote() []remote.Option {
	return o.opts
}

// Transport builds the base transport for the given gateway mode: gateway
// routing (when configured) wrapped to honor Retry-After. Commands that share
// one transport across several pushers (e.g. `img deploy`) can build it once
// here and pass it to [Options.WithTransport].
func Transport(mode gateway.Mode) (http.RoundTripper, error) {
	base, err := gateway.WrapTransport(remote.DefaultTransport, mode)
	if err != nil {
		return nil, err
	}
	return WrapRetryAfter(base), nil
}

// RetryBackoff returns the exponential backoff policy used for registry
// operations. It deliberately replaces go-cr's short default (3 attempts over
// ~4 seconds) with a more patient policy so that transient rate limits
// (HTTP 429 TOOMANYREQUESTS) and 5xx/network blips are ridden out instead of
// failing the build. The policy is configurable via the IMG_REGISTRY_RETRY_*
// environment variables.
//
// go-cr applies this to BOTH the writer-level retry it uses for manifest/blob
// uploads (remote.Write*/Pusher) AND the transport-level retry it uses for
// reads (remote.Get/Layer/...), so a single option covers pushes and pulls.
//
// Note: Cap (from EnvRetryMaxDelay) clamps each individual wait; if Cap is set
// smaller than base*factor^(attempts-1) the effective number of attempts is
// reduced, because go-cr's backoff ends the loop once a wait would exceed Cap.
func RetryBackoff() remote.Backoff {
	return remote.Backoff{
		Duration: retryBaseDelay(),
		Factor:   retryBackoffFactor,
		Jitter:   retryBackoffJitter,
		Steps:    retryMaxAttempts(),
		Cap:      retryMaxDelay(),
	}
}

// RetryBackoffOption returns the go-cr remote.Option that installs
// [RetryBackoff]. It is part of the [Default] option set; it must be supplied
// when a Pusher/Puller is first constructed, since options attached to a reused
// Pusher/Puller (remote.Reuse) are ignored.
func RetryBackoffOption() remote.Option {
	return remote.WithRetryBackoff(RetryBackoff())
}

// WrapRetryAfter wraps base so that a rate-limit response carrying a
// Retry-After header pauses for the server-directed duration before the
// response is returned. go-cr retries 429/503 responses but ignores
// Retry-After and uses a fixed exponential backoff; without this wrapper a
// registry that asks us to "try again in 10 seconds" is retried too early and
// the request fails. This wrapper only paces — it never retries on its own;
// go-cr's retry loop re-issues the request (rebuilding its body) after we
// return. It is meant to sit below go-cr's auth/retry wrapping (i.e. passed via
// remote.WithTransport), so it never interferes with the 401 token refresh.
//
// Because it cannot observe go-cr's remaining retry budget, it also paces before
// the final (already-exhausted) attempt's response is returned. That is a
// bounded extra wait (at most EnvRetryMaxDelay) on an operation that is about to
// fail anyway, which we accept in exchange for honoring Retry-After on every
// non-terminal attempt.
func WrapRetryAfter(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &retryAfterTransport{inner: base, maxDelay: retryMaxDelay()}
}

type retryAfterTransport struct {
	inner    http.RoundTripper
	maxDelay time.Duration
}

func (t *retryAfterTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	// Only 429 and 503 carry a meaningful Retry-After for our purposes.
	if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable {
		return resp, nil
	}
	delay, ok := parseRetryAfter(resp.Header.Get("Retry-After"))
	if !ok || delay <= 0 {
		return resp, nil
	}
	if t.maxDelay > 0 && delay > t.maxDelay {
		delay = t.maxDelay
	}
	// Buffer and restore the (small) error body so the connection returns to the
	// pool while we sleep and go-cr can still parse the structured error after
	// its retries are exhausted.
	resp.Body = drainAndReplace(resp.Body)

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return resp, nil
	case <-req.Context().Done():
		resp.Body.Close()
		return nil, req.Context().Err()
	}
}

// drainAndReplace reads up to retryAfterMaxBodyDrain of body, closes it, and
// returns a fresh reader over the buffered bytes so the response stays
// re-readable after the underlying connection is released.
func drainAndReplace(body io.ReadCloser) io.ReadCloser {
	if body == nil {
		return http.NoBody
	}
	buf, _ := io.ReadAll(io.LimitReader(body, retryAfterMaxBodyDrain))
	body.Close()
	return io.NopCloser(bytes.NewReader(buf))
}

// parseRetryAfter parses an HTTP Retry-After header value, which may be either
// a non-negative number of seconds or an HTTP-date. It returns ok=false when
// the value is absent or unparseable.
func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if when, err := http.ParseTime(v); err == nil {
		d := time.Until(when)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

func retryMaxAttempts() int {
	if v := os.Getenv(EnvRetryMaxAttempts); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n < 1 {
				return 1
			}
			return n
		}
	}
	return defaultRetryMaxAttempts
}

func retryBaseDelay() time.Duration {
	if v := os.Getenv(EnvRetryBaseDelay); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultRetryBaseDelay
}

func retryMaxDelay() time.Duration {
	if v := os.Getenv(EnvRetryMaxDelay); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultRetryMaxDelay
}
