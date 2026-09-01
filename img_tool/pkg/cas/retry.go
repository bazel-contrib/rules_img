package cas

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Environment variables that tune how remote cache (REAPI) operations retry on
// transient failures. They are the counterpart of the IMG_REGISTRY_RETRY_*
// knobs that govern container registry traffic.
const (
	// EnvRetryMaxAttempts caps the number of attempts (initial try + retries)
	// for a single remote cache operation. Must be >= 1; values below 1 are
	// clamped to 1 (which disables retrying).
	EnvRetryMaxAttempts = "IMG_REAPI_RETRY_MAX_ATTEMPTS"
	// EnvRetryBaseDelay is the initial backoff delay (a Go duration string like
	// "250ms"). Subsequent delays grow exponentially.
	EnvRetryBaseDelay = "IMG_REAPI_RETRY_BASE_DELAY"
	// EnvRetryMaxDelay caps a single backoff wait (a Go duration string). It
	// bounds both the exponential backoff and how long we honor a delay the
	// server asked for in a RetryInfo error detail.
	EnvRetryMaxDelay = "IMG_REAPI_RETRY_MAX_DELAY"
	// EnvRPCTimeout bounds a single attempt of a unary call (FindMissingBlobs,
	// BatchReadBlobs, BatchUpdateBlobs, GetCapabilities). It is the equivalent
	// of Bazel's --remote_timeout. "0" disables it.
	EnvRPCTimeout = "IMG_REAPI_RPC_TIMEOUT"
	// EnvIdleTimeout bounds how long a ByteStream read may go without receiving
	// any data before it is torn down and resumed from the current offset. It is
	// the equivalent of Bazel's --remote_grpc_download_idle_timeout. "0"
	// disables it.
	//
	// A whole-call deadline cannot tell a slow-but-progressing transfer from a
	// hung one, so bulk transfers get an inactivity limit instead.
	EnvIdleTimeout = "IMG_REAPI_IDLE_TIMEOUT"
)

const (
	defaultRetryMaxAttempts = 6
	defaultRetryBaseDelay   = 250 * time.Millisecond
	defaultRetryMaxDelay    = 5 * time.Second
	defaultRPCTimeout       = 60 * time.Second
	defaultIdleTimeout      = 60 * time.Second

	// maxRetryWarnings bounds how many retry warnings a process prints. A
	// degraded cache would otherwise emit one line per retried blob, of which
	// there can be thousands.
	maxRetryWarnings = 20
)

// retryWarnings counts the retry warnings printed so far, process-wide.
var retryWarnings atomic.Int64

// envRetryPolicy is the environment-derived policy that [New] defaults to,
// parsed once: a deploy builds one client per pooled connection, and a warning
// about a malformed value belongs on stderr once, not once per connection.
var envRetryPolicy = sync.OnceValue(RetryPolicyFromEnv)

// RetryPolicy describes how remote cache operations retry transient failures.
//
// Every CAS read is idempotent and content-addressed, and a ByteStream read
// resumes at an offset, so retrying is always safe: the digest check validates
// whatever comes back. Writes are content-addressed too, and a ByteStream write
// resumes at the server's committed_size (see [CAS.WriteBlob] for the limits of
// that).
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts, including the first. 1
	// disables retrying.
	//
	// The budget counts *consecutive* failures: a transfer that makes forward
	// progress gets the full budget back, so a long download over a lossy link
	// is not killed by an attempt counter (cf. Bazel's ProgressiveBackoff).
	MaxAttempts int
	// BaseDelay is the backoff before the first retry. It doubles with every
	// consecutive failure, up to MaxDelay.
	BaseDelay time.Duration
	// MaxDelay caps a single wait, including one a server asked for.
	MaxDelay time.Duration
	// RPCTimeout bounds one attempt of a unary call. Zero means no deadline.
	RPCTimeout time.Duration
	// IdleTimeout bounds how long a ByteStream read may receive no data before
	// it is resumed from its current offset. Zero means no limit.
	IdleTimeout time.Duration
}

// DefaultRetryPolicy is the policy used when nothing configures one. Its shape
// follows Bazel's remote cache client: a handful of attempts with exponential
// backoff, a whole-call deadline for unary RPCs and an inactivity deadline for
// bulk transfers.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: defaultRetryMaxAttempts,
		BaseDelay:   defaultRetryBaseDelay,
		MaxDelay:    defaultRetryMaxDelay,
		RPCTimeout:  defaultRPCTimeout,
		IdleTimeout: defaultIdleTimeout,
	}
}

// RetryPolicyFromEnv is [DefaultRetryPolicy] with the IMG_REAPI_RETRY_*,
// IMG_REAPI_RPC_TIMEOUT and IMG_REAPI_IDLE_TIMEOUT overrides applied. Values
// that cannot be parsed are ignored with a warning.
func RetryPolicyFromEnv() RetryPolicy {
	policy := DefaultRetryPolicy()
	if raw := strings.TrimSpace(os.Getenv(EnvRetryMaxAttempts)); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			policy.MaxAttempts = n
		} else {
			warnInvalidEnv(EnvRetryMaxAttempts, raw, policy.MaxAttempts)
		}
	}
	policy.BaseDelay = durationFromEnv(EnvRetryBaseDelay, policy.BaseDelay, false)
	policy.MaxDelay = durationFromEnv(EnvRetryMaxDelay, policy.MaxDelay, false)
	policy.RPCTimeout = durationFromEnv(EnvRPCTimeout, policy.RPCTimeout, true)
	policy.IdleTimeout = durationFromEnv(EnvIdleTimeout, policy.IdleTimeout, true)
	return policy.normalized()
}

// durationFromEnv reads a Go duration from an environment variable. Negative
// values are rejected; zero is accepted only when zeroOK (where it means "no
// deadline").
func durationFromEnv(name string, fallback time.Duration, zeroOK bool) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 || (d == 0 && !zeroOK) {
		warnInvalidEnv(name, raw, fallback)
		return fallback
	}
	return d
}

func warnInvalidEnv(name, raw string, fallback any) {
	fmt.Fprintf(os.Stderr, "WARNING: ignoring invalid %s=%q, using %v\n", name, raw, fallback)
}

// normalized clamps a policy into a usable range.
func (p RetryPolicy) normalized() RetryPolicy {
	if p.MaxAttempts < 1 {
		p.MaxAttempts = 1
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = defaultRetryBaseDelay
	}
	if p.MaxDelay < p.BaseDelay {
		p.MaxDelay = p.BaseDelay
	}
	if p.RPCTimeout < 0 {
		p.RPCTimeout = 0
	}
	if p.IdleTimeout < 0 {
		p.IdleTimeout = 0
	}
	return p
}

// retryCounters records what the retry loops did, for reporting.
type retryCounters struct {
	retries atomic.Uint64
	gaveUp  atomic.Uint64
	// byCode is indexed by gRPC status code. Codes outside the known range are
	// counted as codes.Unknown.
	byCode [17]atomic.Uint64
}

// RetryStats is a snapshot of what the retry loops did.
type RetryStats struct {
	// Retries is the number of retried attempts (i.e. failures that were not
	// reported to the caller).
	Retries uint64
	// GaveUp is the number of operations that failed after exhausting their
	// attempt budget.
	GaveUp uint64
	// ByCode counts retried attempts per gRPC status code.
	ByCode map[codes.Code]uint64
}

// Empty reports whether nothing was ever retried.
func (s RetryStats) Empty() bool { return s.Retries == 0 && s.GaveUp == 0 }

// String renders the stats as one line, e.g.
// "12 retried (Internal 1, Unavailable 11), 1 gave up".
func (s RetryStats) String() string {
	parts := make([]string, 0, len(s.ByCode))
	for code, n := range s.ByCode {
		parts = append(parts, fmt.Sprintf("%s %d", code, n))
	}
	slices.Sort(parts)
	line := fmt.Sprintf("%d retried", s.Retries)
	if len(parts) > 0 {
		line += " (" + strings.Join(parts, ", ") + ")"
	}
	if s.GaveUp > 0 {
		line += fmt.Sprintf(", %d gave up", s.GaveUp)
	}
	return line
}

func (c *retryCounters) snapshot() RetryStats {
	stats := RetryStats{
		Retries: c.retries.Load(),
		GaveUp:  c.gaveUp.Load(),
		ByCode:  make(map[codes.Code]uint64),
	}
	for i := range c.byCode {
		if n := c.byCode[i].Load(); n > 0 {
			stats.ByCode[codes.Code(i)] = n
		}
	}
	return stats
}

// retryConfig is the shared retry state: the policy and the counters every
// retry loop of one client reports into.
type retryConfig struct {
	policy   RetryPolicy
	counters *retryCounters
}

func newRetryConfig(policy RetryPolicy) retryConfig {
	return retryConfig{policy: policy.normalized(), counters: &retryCounters{}}
}

// start begins a retry loop for one logical operation. what names the operation
// in warnings and in the final error, e.g. "reading blob sha256:abc… (12 MiB)".
func (rc retryConfig) start(what string) *retrier {
	return &retrier{config: rc, what: what, started: time.Now()}
}

// retrier drives one retry loop.
//
// The loop shape is
//
//	r := rc.start("…")
//	for {
//		err := attempt()
//		if err == nil { return nil }
//		if giveUp := r.next(ctx, err); giveUp != nil { return giveUp }
//	}
//
// where next reports whether another attempt is worth making: it returns nil
// after waiting out the backoff, or the error to report to the caller.
type retrier struct {
	config  retryConfig
	what    string
	started time.Time

	// attempt is the number of failed attempts so far. It is reset by progress.
	attempt int
	// total is the number of failed attempts over the whole operation, which
	// progress does not reset. It is only used in the final error message.
	total int
}

// next decides whether the operation gets another attempt. It returns nil to
// retry (after waiting out the backoff), or the error the caller should report.
func (r *retrier) next(ctx context.Context, err error) error {
	r.attempt++
	r.total++
	if !retriable(ctx, err) {
		return err
	}
	if r.attempt >= r.config.policy.MaxAttempts {
		r.config.counters.gaveUp.Add(1)
		return fmt.Errorf("giving up %s after %d attempts over %s: %w",
			r.what, r.total, time.Since(r.started).Round(time.Millisecond), err)
	}
	r.config.counters.retries.Add(1)
	r.config.counters.countCode(status.Code(err))
	delay := r.delay(err)
	r.warn(err, delay)
	return sleepContext(ctx, delay)
}

// progress restores the full attempt budget after the operation made forward
// progress, so a long transfer over a lossy link is bounded by consecutive
// failures rather than by a total attempt count.
func (r *retrier) progress() { r.attempt = 0 }

// delay returns how long to wait before the next attempt: the server's own
// RetryInfo if it sent one, otherwise an exponential backoff with jitter.
func (r *retrier) delay(err error) time.Duration {
	policy := r.config.policy
	if requested, ok := serverRequestedDelay(err); ok {
		return min(requested, policy.MaxDelay)
	}
	interval := policy.BaseDelay << min(r.attempt-1, 32)
	if interval > policy.MaxDelay || interval <= 0 {
		interval = policy.MaxDelay
	}
	// Equal jitter: half the interval is fixed, so the server always gets a
	// break, and half is random, so a pool of connections that all failed on the
	// same server blip does not come back in lockstep.
	return interval/2 + time.Duration(rand.Int63n(int64(interval/2)+1))
}

// warn reports a retry on stderr, up to maxRetryWarnings times per process.
func (r *retrier) warn(err error, delay time.Duration) {
	printed := retryWarnings.Add(1)
	switch {
	case printed > maxRetryWarnings:
		return
	case printed == maxRetryWarnings:
		fmt.Fprintf(os.Stderr, "WARNING: remote cache: %s failed (%v); retrying in %v. Further retry warnings are suppressed.\n", r.what, err, delay)
	default:
		fmt.Fprintf(os.Stderr, "WARNING: remote cache: %s failed (%v); retrying in %v (attempt %d/%d)\n", r.what, err, delay, r.attempt+1, r.config.policy.MaxAttempts)
	}
}

func (c *retryCounters) countCode(code codes.Code) {
	if int(code) < 0 || int(code) >= len(c.byCode) {
		code = codes.Unknown
	}
	c.byCode[code].Add(1)
}

// retriable reports whether err is a transient remote cache failure that
// another attempt could get past.
//
// The classification follows Bazel's remote retrier: transport-level and
// server-side transient codes are retried, NOT_FOUND is a cache miss the
// caller's fallback chain handles, and anything that is not a gRPC status —
// a local read error, a digest mismatch — is permanent, because retrying it
// would not change the outcome.
func retriable(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	// Our own cancellation or deadline is not a server failure.
	if ctx.Err() != nil {
		return false
	}
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch st.Code() {
	case codes.Unavailable, codes.Internal, codes.Aborted, codes.Unknown,
		codes.DeadlineExceeded, codes.ResourceExhausted:
		// grpc-go reports transient name-resolution failures as Unavailable
		// (for example, "name resolver error: produced zero addresses"), so
		// they use the same bounded retry budget as other transport failures.
		return true
	case codes.Canceled:
		// Not our cancellation (checked above), so something between us and the
		// server cancelled the call.
		return true
	default:
		return false
	}
}

// serverRequestedDelay returns the delay the server asked for in a RetryInfo
// error detail. The REAPI spec says clients SHOULD respect it.
func serverRequestedDelay(err error) (time.Duration, bool) {
	st, ok := status.FromError(err)
	if !ok {
		return 0, false
	}
	for _, detail := range st.Details() {
		info, ok := detail.(*errdetails.RetryInfo)
		if !ok {
			continue
		}
		delay := info.GetRetryDelay().AsDuration()
		if delay <= 0 {
			continue
		}
		return delay, true
	}
	return 0, false
}

// sleepContext waits for d, or until ctx is done.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
