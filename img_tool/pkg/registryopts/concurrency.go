package registryopts

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/logs"
)

// Environment variables that expose and bound how many registry requests the
// process has in flight at once.
const (
	// EnvMaxConcurrentRequests overrides how many requests to the destination
	// registry may be in flight at once, process-wide. It takes precedence over
	// the --jobs value a command passes to [LimitConcurrencyToJobs]; <= 0 means
	// unlimited.
	EnvMaxConcurrentRequests = "IMG_REGISTRY_MAX_CONCURRENT_REQUESTS"
	// EnvLogConcurrency turns on concurrency logging to stderr:
	//
	//	requests  log every registry request as it is admitted, with the
	//	          current and peak in-flight counts ("1"/"true"/"on"/"all"
	//	          are accepted as synonyms)
	//	summary   only print the end-of-run summary line
	//	off       neither (the default; "0"/"false" are synonyms)
	//
	// `img --verbose` implies "requests" when this variable is unset.
	EnvLogConcurrency = "IMG_REGISTRY_LOG_CONCURRENCY"
)

// Role selects which pool of registry requests a transport's traffic belongs to.
type Role int

const (
	// RoleDestination is traffic to the registry we are writing to: blob and
	// manifest uploads plus the existence checks that precede them. This is the
	// pool --jobs bounds.
	RoleDestination Role = iota
	// RoleSource is traffic to registries we read from (base images, referrer
	// lookups). It is counted but never throttled — throttling it would only
	// slow data on its way into the img tool.
	RoleSource
	// RoleAuto classifies each request by method: writes are destination
	// traffic, reads are source traffic. It is for transports that do both (the
	// BES syncer streams source blobs into destination uploads over one
	// transport).
	RoleAuto
)

func (r Role) pool(method string) poolID {
	switch r {
	case RoleSource:
		return poolSource
	case RoleAuto:
		switch method {
		case http.MethodGet, http.MethodHead, "":
			return poolSource
		default:
			return poolDestination
		}
	default:
		return poolDestination
	}
}

type poolID int

const (
	poolDestination poolID = iota
	poolSource
	numPools
)

func (p poolID) String() string {
	if p == poolSource {
		return "source"
	}
	return "destination"
}

// logMode selects how much the concurrency tracker reports.
type logMode int

const (
	logOff logMode = iota
	logSummary
	logRequests
)

// slotPool admits requests belonging to one pool, optionally bounded to a fixed
// number of slots, and records how concurrent they got. The limit can be changed
// at any time — commands learn it from --jobs after transports are already
// built — so slots are handed out through a FIFO queue of waiters rather than a
// fixed-capacity channel.
type slotPool struct {
	id poolID

	mu      sync.Mutex
	limit   int // 0 means unlimited
	held    int
	waiters []chan int

	peak      int
	total     int64
	blocked   int64
	waitNanos int64
}

func newSlotPool(id poolID, limit int) *slotPool {
	if limit < 0 {
		limit = 0
	}
	return &slotPool{id: id, limit: limit}
}

// setLimit changes the number of slots. Requests already in flight are never
// interrupted, so held may exceed a freshly lowered limit until they drain.
func (p *slotPool) setLimit(limit int) {
	if limit < 0 {
		limit = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.limit = limit
	p.grantLocked()
}

// acquire takes a slot, blocking until one is free when the pool is bounded. It
// reports how many requests are now in flight (including this one) and how long
// it waited, plus a release function that is safe to call more than once.
func (p *slotPool) acquire(ctx context.Context) (inFlight int, waited time.Duration, release func(), err error) {
	p.mu.Lock()
	if p.hasCapacityLocked() {
		inFlight = p.admitLocked()
		p.mu.Unlock()
		return inFlight, 0, p.releaser(), nil
	}
	ch := make(chan int, 1)
	p.waiters = append(p.waiters, ch)
	p.blocked++
	p.mu.Unlock()

	start := time.Now()
	select {
	case inFlight = <-ch:
		waited = time.Since(start)
		p.mu.Lock()
		p.waitNanos += int64(waited)
		p.mu.Unlock()
		return inFlight, waited, p.releaser(), nil
	case <-ctx.Done():
		p.mu.Lock()
		if !p.dequeueLocked(ch) {
			// The slot was granted while we were giving up on it; hand it back.
			p.releaseLocked()
		}
		p.mu.Unlock()
		return 0, 0, nil, ctx.Err()
	}
}

func (p *slotPool) hasCapacityLocked() bool {
	return p.limit == 0 || p.held < p.limit
}

func (p *slotPool) admitLocked() int {
	p.held++
	p.total++
	if p.held > p.peak {
		p.peak = p.held
	}
	return p.held
}

// grantLocked hands free slots to queued requests, oldest first.
func (p *slotPool) grantLocked() {
	for len(p.waiters) > 0 && p.hasCapacityLocked() {
		ch := p.waiters[0]
		p.waiters = p.waiters[1:]
		ch <- p.admitLocked()
	}
}

// dequeueLocked removes ch from the queue, reporting whether it was still
// waiting. A false return means the slot was already granted to it.
func (p *slotPool) dequeueLocked(ch chan int) bool {
	for i, queued := range p.waiters {
		if queued == ch {
			p.waiters = append(p.waiters[:i], p.waiters[i+1:]...)
			return true
		}
	}
	return false
}

func (p *slotPool) releaser() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			p.releaseLocked()
			p.mu.Unlock()
		})
	}
}

func (p *slotPool) releaseLocked() {
	p.held--
	p.grantLocked()
}

func (p *slotPool) stats() PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return PoolStats{
		Limit:    p.limit,
		InFlight: p.held,
		Peak:     p.peak,
		Total:    p.total,
		Blocked:  p.blocked,
		Wait:     time.Duration(p.waitNanos),
	}
}

// PoolStats is a snapshot of one pool of registry requests.
type PoolStats struct {
	// Limit is the configured cap, or 0 when unlimited.
	Limit int
	// InFlight is how many requests are in flight right now.
	InFlight int
	// Peak is the highest InFlight value observed so far.
	Peak int
	// Total is how many requests were admitted so far.
	Total int64
	// Blocked is how many requests had to wait for a free slot.
	Blocked int64
	// Wait is the total time requests spent waiting for a free slot.
	Wait time.Duration
}

// ConcurrencyStats is a snapshot of process-wide registry request concurrency.
type ConcurrencyStats struct {
	// Destination covers requests to the registry being written to (bounded by
	// --jobs).
	Destination PoolStats
	// Source covers requests to registries being read from (counted only).
	Source PoolStats
}

// concurrencyTracker holds the pools and the reporting settings.
type concurrencyTracker struct {
	pools [numPools]*slotPool
	// envLimit records that EnvMaxConcurrentRequests set the destination limit
	// explicitly, which makes LimitConcurrencyToJobs a no-op.
	envLimit bool
	mode     logMode
	logger   *log.Logger
}

func newConcurrencyTracker(destinationLimit int, envLimit bool, mode logMode, out io.Writer) *concurrencyTracker {
	t := &concurrencyTracker{envLimit: envLimit, mode: mode}
	t.pools[poolDestination] = newSlotPool(poolDestination, destinationLimit)
	t.pools[poolSource] = newSlotPool(poolSource, 0)
	if mode != logOff {
		t.logger = log.New(out, "img registry: ", log.Ltime|log.Lmicroseconds)
	}
	return t
}

func (t *concurrencyTracker) limitToJobs(jobs int) {
	if t.envLimit || jobs <= 0 {
		return
	}
	t.pools[poolDestination].setLimit(jobs)
}

func (t *concurrencyTracker) stats() ConcurrencyStats {
	return ConcurrencyStats{
		Destination: t.pools[poolDestination].stats(),
		Source:      t.pools[poolSource].stats(),
	}
}

func (t *concurrencyTracker) logAdmitted(p *slotPool, req *http.Request, inFlight int, waited time.Duration) {
	if t.mode < logRequests {
		return
	}
	s := p.stats()
	var limit string
	if s.Limit > 0 {
		limit = "/" + strconv.Itoa(s.Limit)
	}
	var wait string
	if waited > 0 {
		wait = " waited=" + waited.Round(time.Millisecond).String()
	}
	t.logger.Printf("%s in-flight=%d%s peak=%d total=%d%s %s %s",
		p.id, inFlight, limit, s.Peak, s.Total, wait, req.Method, requestTarget(req.URL))
}

// summary renders one line describing every registry request made so far. It
// returns "" when reporting is disabled or nothing was requested.
func (t *concurrencyTracker) summary() string {
	if t.mode == logOff {
		return ""
	}
	s := t.stats()
	if s.Destination.Total == 0 && s.Source.Total == 0 {
		return ""
	}
	return fmt.Sprintf("registry requests: %s, %s",
		formatPoolStats("to destination", s.Destination),
		formatPoolStats("from sources", s.Source))
}

func formatPoolStats(name string, s PoolStats) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s (peak %d", s.Total, name, s.Peak)
	if s.Limit > 0 {
		fmt.Fprintf(&b, "/%d", s.Limit)
	}
	b.WriteString(" concurrent")
	if s.Blocked > 0 {
		fmt.Fprintf(&b, ", %d throttled, %s waiting", s.Blocked, s.Wait.Round(time.Millisecond))
	}
	b.WriteString(")")
	return b.String()
}

// globalTracker is the process-wide tracker. Every transport built by this
// package shares it, so the counts and the destination limit cover all registry
// traffic regardless of how many pushers, pullers and errgroups are layered
// above it.
var globalTracker = sync.OnceValue(func() *concurrencyTracker {
	limit, fromEnv := maxConcurrentRequestsEnv()
	return newConcurrencyTracker(limit, fromEnv, concurrencyLogMode(), os.Stderr)
})

// LimitConcurrencyToJobs bounds requests to the destination registry to the
// command's --jobs value: at most jobs blob/manifest uploads and existence
// checks are in flight at once, process-wide. Reads from source registries are
// left unbounded — they carry data towards the img tool, not towards the
// registry being written to.
//
// This is what makes --jobs mean "connections to the destination registry".
// Without it, jobs is only a per-fan-out limit that rules_img and
// go-containerregistry each apply again at every level (push operations, then a
// manifest's children, then an image's layers), so the requests actually in
// flight are the product of the nested limits.
//
// Commands call it once, as early as they know their --jobs value; it may be
// called after transports have been built, since the pool is resized in place.
// [EnvMaxConcurrentRequests] takes precedence when set.
func LimitConcurrencyToJobs(jobs int) {
	globalTracker().limitToJobs(jobs)
}

// Concurrency reports process-wide registry request concurrency: how many
// requests are in flight, how concurrent they got, and how much they were
// throttled. Counting is always on.
func Concurrency() ConcurrencyStats {
	return globalTracker().stats()
}

// ConcurrencySummary renders [Concurrency] as a single human-readable line, or
// "" when concurrency reporting is disabled (see [EnvLogConcurrency]) or no
// registry request was made.
func ConcurrencySummary() string {
	return globalTracker().summary()
}

// LogConcurrencySummary writes [ConcurrencySummary] to w when it is non-empty.
// Commands call it once they are done talking to registries.
func LogConcurrencySummary(w io.Writer) {
	if summary := ConcurrencySummary(); summary != "" {
		fmt.Fprintf(w, "    %s\n", summary)
	}
}

// WrapConcurrency wraps base so that every registry request it carries is
// counted against the process-wide tracker and, for destination traffic, waits
// for a free slot before it is sent (see [LimitConcurrencyToJobs]).
//
// This is the only place where the number of concurrent registry requests can be
// observed or bounded, because it is the only place all of them pass through
// exactly once.
//
// The destination pool releases its slot when the round trip returns. That is
// the whole upload: an upload's request body is streamed during the round trip.
// It also means a destination request never holds a slot while another one has to
// finish, so the limit cannot deadlock the push — in particular a layer streamed
// out of a source registry (a shallow base image) opens its read before the
// upload that consumes it, and reads are never throttled.
//
// Source blob downloads do hold their slot until the response body is closed, so
// the reported concurrency reflects connections that stay busy while a body
// streams. That pool is counted, not bounded.
//
// Wrap the base transport, not go-cr's authenticated client: this must sit below
// go-cr's auth and retry wrapping so that token fetches are counted too and each
// retry attempt takes its own slot.
func WrapConcurrency(base http.RoundTripper, role Role) http.RoundTripper {
	return wrapConcurrency(base, role, globalTracker())
}

func wrapConcurrency(base http.RoundTripper, role Role, tracker *concurrencyTracker) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &concurrencyTransport{inner: base, role: role, tracker: tracker}
}

type concurrencyTransport struct {
	inner   http.RoundTripper
	role    Role
	tracker *concurrencyTracker
}

func (t *concurrencyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	pool := t.tracker.pools[t.role.pool(req.Method)]
	inFlight, waited, release, err := pool.acquire(req.Context())
	if err != nil {
		return nil, err
	}
	t.tracker.logAdmitted(pool, req, inFlight, waited)

	resp, err := t.inner.RoundTrip(req)
	if err != nil || resp == nil {
		release()
		return resp, err
	}
	if !holdsSlotUntilBodyClose(pool.id, req, resp) {
		release()
		return resp, nil
	}
	resp.Body = &releaseOnClose{ReadCloser: resp.Body, release: release}
	return resp, nil
}

// holdsSlotUntilBodyClose reports whether resp is a body the caller streams off
// the connection (a blob or manifest download), which keeps the request alive
// well past the round trip.
//
// Only source reads qualify. Error and redirect responses are excluded even
// there: their bodies are small, and go-cr issues follow-up requests (token
// refresh, redirect hops) while it still holds them.
func holdsSlotUntilBodyClose(pool poolID, req *http.Request, resp *http.Response) bool {
	if pool != poolSource || req.Method != http.MethodGet {
		return false
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return false
	}
	return resp.Body != nil && resp.Body != http.NoBody
}

// releaseOnClose frees a slot once the response body is closed. A caller that
// leaks the body leaks the slot; go-cr always closes what it opens.
type releaseOnClose struct {
	io.ReadCloser
	release func()
}

func (r *releaseOnClose) Close() error {
	err := r.ReadCloser.Close()
	r.release()
	return err
}

// requestTarget renders a request URL for logging without its query string,
// which can carry credentials on pre-signed blob URLs.
func requestTarget(u *url.URL) string {
	if u == nil {
		return ""
	}
	trimmed := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}
	return trimmed.String()
}

// maxConcurrentRequestsEnv reads the destination limit from the environment,
// reporting whether it was set explicitly.
func maxConcurrentRequestsEnv() (int, bool) {
	raw := os.Getenv(EnvMaxConcurrentRequests)
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: ignoring invalid %s=%q, falling back to --jobs\n", EnvMaxConcurrentRequests, raw)
		return 0, false
	}
	if n < 0 {
		n = 0
	}
	return n, true
}

func concurrencyLogMode() logMode {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(EnvLogConcurrency)))
	switch raw {
	case "":
		// `img --verbose` points go-cr's debug logger at stderr; follow it.
		if logs.Enabled(logs.Debug) {
			return logRequests
		}
		return logOff
	case "0", "false", "off", "no", "none":
		return logOff
	case "summary":
		return logSummary
	case "1", "true", "on", "yes", "all", "requests":
		return logRequests
	default:
		fmt.Fprintf(os.Stderr, "WARNING: unknown %s=%q, logging every request (accepted values: requests, summary, off)\n", EnvLogConcurrency, raw)
		return logRequests
	}
}
