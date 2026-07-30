package registryopts

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingTransport answers requests only once release is closed, and records the
// highest number of round trips that were inside it at the same time.
type blockingTransport struct {
	entered chan struct{}
	release chan struct{}
	status  int

	mu      sync.Mutex
	current int
	max     int
}

func newBlockingTransport(status int) *blockingTransport {
	return &blockingTransport{
		entered: make(chan struct{}, 256),
		release: make(chan struct{}),
		status:  status,
	}
}

func (b *blockingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	b.mu.Lock()
	b.current++
	if b.current > b.max {
		b.max = b.current
	}
	b.mu.Unlock()
	b.entered <- struct{}{}
	<-b.release
	b.mu.Lock()
	b.current--
	b.mu.Unlock()
	return &http.Response{
		StatusCode: b.status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("payload")),
		Request:    req,
	}, nil
}

func (b *blockingTransport) maxConcurrent() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.max
}

func respondingTransport(status int, body string) roundTripFunc {
	return func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	}
}

func testTracker(t *testing.T, destinationLimit int) *concurrencyTracker {
	t.Helper()
	return newConcurrencyTracker(destinationLimit, destinationLimit > 0, logOff, io.Discard)
}

func request(t *testing.T, ctx context.Context, method, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	return req
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// drain runs n concurrent round trips through rt and returns once they are all
// done, releasing the blocking transport after everyone that can be admitted has
// entered it.
func drain(t *testing.T, rt http.RoundTripper, method, url string, n int) {
	t.Helper()
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := rt.RoundTrip(request(t, context.Background(), method, url))
			if err != nil {
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()
}

func TestDestinationRequestsAreBoundedByTheLimit(t *testing.T) {
	const limit = 3
	inner := newBlockingTransport(http.StatusCreated)
	tracker := testTracker(t, limit)
	rt := wrapConcurrency(inner, RoleDestination, tracker)

	go drain(t, rt, http.MethodPut, "https://registry.test/v2/foo/blobs/uploads/1", 12)

	// Once `limit` requests are inside the inner transport, no more may enter.
	for range limit {
		<-inner.entered
	}
	select {
	case <-inner.entered:
		t.Fatalf("a %dth request entered the transport while %d slots were held", limit+1, limit)
	case <-time.After(50 * time.Millisecond):
	}
	close(inner.release)

	waitFor(t, "all destination requests to finish", func() bool {
		return tracker.stats().Destination.Total == 12 && tracker.stats().Destination.InFlight == 0
	})
	if got := inner.maxConcurrent(); got != limit {
		t.Fatalf("max concurrent round trips = %d, want %d", got, limit)
	}
	stats := tracker.stats()
	if stats.Destination.Peak != limit {
		t.Fatalf("destination peak = %d, want %d", stats.Destination.Peak, limit)
	}
	if stats.Destination.Blocked == 0 {
		t.Fatalf("destination blocked = 0, want the queued requests to be counted")
	}
	if stats.Source.Total != 0 {
		t.Fatalf("source total = %d, want 0 (destination traffic must not touch the source pool)", stats.Source.Total)
	}
}

// A HEAD to the destination registry is a connection to the destination registry,
// so it draws from the same pool as the uploads.
func TestDestinationReadsShareTheLimitWithWrites(t *testing.T) {
	inner := newBlockingTransport(http.StatusOK)
	tracker := testTracker(t, 2)
	rt := wrapConcurrency(inner, RoleDestination, tracker)

	go drain(t, rt, http.MethodHead, "https://registry.test/v2/foo/manifests/latest", 6)
	for range 2 {
		<-inner.entered
	}
	select {
	case <-inner.entered:
		t.Fatalf("a third HEAD entered while both destination slots were held")
	case <-time.After(50 * time.Millisecond):
	}
	close(inner.release)
	waitFor(t, "the HEADs to finish", func() bool { return tracker.stats().Destination.Total == 6 })
	if got := tracker.stats().Destination.Peak; got != 2 {
		t.Fatalf("destination peak = %d, want 2", got)
	}
}

// Downloads carry data towards the img tool, so they are counted but never
// throttled.
func TestSourceRequestsAreCountedButNotBounded(t *testing.T) {
	inner := newBlockingTransport(http.StatusOK)
	tracker := testTracker(t, 2)
	rt := wrapConcurrency(inner, RoleSource, tracker)

	const n = 8
	go drain(t, rt, http.MethodGet, "https://base.test/v2/foo/blobs/sha256:aa", n)
	for range n {
		<-inner.entered
	}
	close(inner.release)

	waitFor(t, "the downloads to finish", func() bool { return tracker.stats().Source.InFlight == 0 })
	stats := tracker.stats()
	if stats.Source.Peak != n {
		t.Fatalf("source peak = %d, want %d (downloads must not be throttled)", stats.Source.Peak, n)
	}
	if stats.Source.Blocked != 0 {
		t.Fatalf("source blocked = %d, want 0", stats.Source.Blocked)
	}
	if stats.Source.Limit != 0 {
		t.Fatalf("source limit = %d, want 0", stats.Source.Limit)
	}
}

// The BES syncer streams source blobs into destination uploads over one
// transport, so RoleAuto has to attribute each request by method.
func TestRoleAutoAttributesByMethod(t *testing.T) {
	tracker := testTracker(t, 4)
	rt := wrapConcurrency(respondingTransport(http.StatusOK, "body"), RoleAuto, tracker)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		resp, err := rt.RoundTrip(request(t, context.Background(), method, "https://base.test/v2/foo/blobs/sha256:aa"))
		if err != nil {
			t.Fatalf("%s error: %v", method, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodPut} {
		resp, err := rt.RoundTrip(request(t, context.Background(), method, "https://registry.test/v2/foo/blobs/uploads/1"))
		if err != nil {
			t.Fatalf("%s error: %v", method, err)
		}
		resp.Body.Close()
	}

	stats := tracker.stats()
	if stats.Source.Total != 2 {
		t.Fatalf("source total = %d, want 2 (GET+HEAD)", stats.Source.Total)
	}
	if stats.Destination.Total != 3 {
		t.Fatalf("destination total = %d, want 3 (POST+PATCH+PUT)", stats.Destination.Total)
	}
}

// A layer streamed out of a source registry holds its download open for the whole
// duration of the upload that consumes it. The upload must not be able to block
// behind it.
func TestStreamingCopyCannotBlockItself(t *testing.T) {
	tracker := testTracker(t, 1)
	source := wrapConcurrency(respondingTransport(http.StatusOK, "layer bytes"), RoleSource, tracker)
	destination := wrapConcurrency(respondingTransport(http.StatusCreated, ""), RoleDestination, tracker)

	// Two copies in flight, both holding their source download open, with a
	// destination limit of one.
	var held []io.ReadCloser
	for range 2 {
		resp, err := source.RoundTrip(request(t, context.Background(), http.MethodGet, "https://base.test/v2/base/blobs/sha256:aa"))
		if err != nil {
			t.Fatalf("source GET error: %v", err)
		}
		held = append(held, resp.Body)
	}
	defer func() {
		for _, body := range held {
			body.Close()
		}
	}()
	if got := tracker.stats().Source.InFlight; got != 2 {
		t.Fatalf("source in-flight = %d, want 2", got)
	}

	done := make(chan error, 1)
	go func() {
		resp, err := destination.RoundTrip(request(t, context.Background(), http.MethodPut, "https://registry.test/v2/app/blobs/uploads/1"))
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("destination PUT error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("destination PUT blocked behind held source downloads")
	}
}

func TestSourceDownloadHoldsItsSlotUntilBodyClose(t *testing.T) {
	tracker := testTracker(t, 2)
	rt := wrapConcurrency(respondingTransport(http.StatusOK, "blob bytes"), RoleSource, tracker)

	resp, err := rt.RoundTrip(request(t, context.Background(), http.MethodGet, "https://base.test/v2/foo/blobs/sha256:aa"))
	if err != nil {
		t.Fatalf("RoundTrip error: %v", err)
	}
	if got := tracker.stats().Source.InFlight; got != 1 {
		t.Fatalf("in-flight while streaming the body = %d, want 1", got)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if got := tracker.stats().Source.InFlight; got != 1 {
		t.Fatalf("in-flight before Close = %d, want 1", got)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("closing body: %v", err)
	}
	if got := tracker.stats().Source.InFlight; got != 0 {
		t.Fatalf("in-flight after Close = %d, want 0", got)
	}
	// Double-closing must not release the slot twice.
	resp.Body.Close()
	if got := tracker.stats().Source.InFlight; got != 0 {
		t.Fatalf("in-flight after double Close = %d, want 0", got)
	}
}

func TestSlotIsReleasedWhenTheBodyIsNotStreamed(t *testing.T) {
	cases := []struct {
		name   string
		role   Role
		method string
		status int
	}{
		// A 401 must not hold its slot: go-cr fetches a fresh token right after
		// it, over the same transport.
		{name: "source unauthorized get", role: RoleSource, method: http.MethodGet, status: http.StatusUnauthorized},
		{name: "source redirected get", role: RoleSource, method: http.MethodGet, status: http.StatusTemporaryRedirect},
		{name: "source rate limited get", role: RoleSource, method: http.MethodGet, status: http.StatusTooManyRequests},
		{name: "source head", role: RoleSource, method: http.MethodHead, status: http.StatusOK},
		// Destination traffic never holds a slot past the round trip: an
		// upload's request body has already been sent by then.
		{name: "destination get", role: RoleDestination, method: http.MethodGet, status: http.StatusOK},
		{name: "destination put", role: RoleDestination, method: http.MethodPut, status: http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tracker := testTracker(t, 1)
			rt := wrapConcurrency(respondingTransport(tc.status, "body"), tc.role, tracker)

			resp, err := rt.RoundTrip(request(t, context.Background(), tc.method, "https://registry.test/v2/foo/blobs/sha256:aa"))
			if err != nil {
				t.Fatalf("RoundTrip error: %v", err)
			}
			stats := tracker.stats()
			if stats.Destination.InFlight != 0 || stats.Source.InFlight != 0 {
				t.Fatalf("in-flight = destination %d / source %d before the body was closed, want 0/0",
					stats.Destination.InFlight, stats.Source.InFlight)
			}
			// The body is still readable after the slot was released.
			if _, err := io.ReadAll(resp.Body); err != nil {
				t.Fatalf("reading body: %v", err)
			}
			resp.Body.Close()
		})
	}
}

// Commands learn their --jobs value at different times, so the limit has to apply
// to transports that already exist.
func TestLimitToJobsAppliesToExistingTransports(t *testing.T) {
	inner := newBlockingTransport(http.StatusCreated)
	tracker := newConcurrencyTracker(0, false, logOff, io.Discard)
	rt := wrapConcurrency(inner, RoleDestination, tracker)

	tracker.limitToJobs(2)
	if got := tracker.stats().Destination.Limit; got != 2 {
		t.Fatalf("destination limit = %d, want 2", got)
	}

	go drain(t, rt, http.MethodPut, "https://registry.test/v2/foo/blobs/uploads/1", 6)
	for range 2 {
		<-inner.entered
	}
	select {
	case <-inner.entered:
		t.Fatalf("a third request entered after the limit was lowered to 2")
	case <-time.After(50 * time.Millisecond):
	}

	// Raising the limit admits queued requests immediately.
	tracker.limitToJobs(4)
	for range 2 {
		select {
		case <-inner.entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("raising the limit did not admit the queued requests")
		}
	}
	close(inner.release)
	waitFor(t, "the uploads to finish", func() bool { return tracker.stats().Destination.Total == 6 })
	if got := tracker.stats().Destination.Peak; got != 4 {
		t.Fatalf("destination peak = %d, want 4", got)
	}
}

func TestLimitToJobsIgnoredWhenTheEnvironmentSetTheLimit(t *testing.T) {
	tracker := newConcurrencyTracker(3, true, logOff, io.Discard)
	tracker.limitToJobs(64)
	if got := tracker.stats().Destination.Limit; got != 3 {
		t.Fatalf("destination limit = %d, want the environment's 3", got)
	}

	// Non-positive jobs values leave the limit alone.
	unset := newConcurrencyTracker(0, false, logOff, io.Discard)
	unset.limitToJobs(0)
	unset.limitToJobs(-1)
	if got := unset.stats().Destination.Limit; got != 0 {
		t.Fatalf("destination limit = %d, want 0", got)
	}
}

func TestQueuedRequestRespectsContextCancellation(t *testing.T) {
	inner := newBlockingTransport(http.StatusCreated)
	defer close(inner.release)
	tracker := testTracker(t, 1)
	rt := wrapConcurrency(inner, RoleDestination, tracker)

	// Occupy the only slot.
	go func() {
		resp, err := rt.RoundTrip(request(t, context.Background(), http.MethodPut, "https://registry.test/v2/foo/blobs/uploads/1"))
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-inner.entered

	ctx, cancel := context.WithCancel(context.Background())
	var queued atomic.Bool
	errCh := make(chan error, 1)
	go func() {
		queued.Store(true)
		_, err := rt.RoundTrip(request(t, ctx, http.MethodPut, "https://registry.test/v2/foo/blobs/uploads/2"))
		errCh <- err
	}()
	waitFor(t, "the second request to queue", func() bool {
		return queued.Load() && tracker.stats().Destination.Blocked > 0
	})
	cancel()

	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("queued request error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("cancelling the context did not unblock the queued request")
	}
	stats := tracker.stats()
	if stats.Destination.Total != 1 {
		t.Fatalf("destination total = %d, want 1 (a cancelled request never went out)", stats.Destination.Total)
	}
	if stats.Destination.InFlight != 1 {
		t.Fatalf("destination in-flight = %d, want 1 (the cancelled request must not leak a slot)", stats.Destination.InFlight)
	}
}

func TestSlotIsReleasedOnTransportError(t *testing.T) {
	tracker := testTracker(t, 1)
	rt := wrapConcurrency(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	}), RoleDestination, tracker)

	if _, err := rt.RoundTrip(request(t, context.Background(), http.MethodPut, "https://registry.test/v2/")); err == nil {
		t.Fatalf("expected the inner error to be returned")
	}
	if got := tracker.stats().Destination.InFlight; got != 0 {
		t.Fatalf("in-flight after a failed round trip = %d, want 0", got)
	}
}

func TestRolePoolSelection(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, "", http.MethodPut} {
		if got := RoleDestination.pool(method); got != poolDestination {
			t.Errorf("RoleDestination.pool(%q) = %v, want destination", method, got)
		}
		if got := RoleSource.pool(method); got != poolSource {
			t.Errorf("RoleSource.pool(%q) = %v, want source", method, got)
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodHead, ""} {
		if got := RoleAuto.pool(method); got != poolSource {
			t.Errorf("RoleAuto.pool(%q) = %v, want source", method, got)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if got := RoleAuto.pool(method); got != poolDestination {
			t.Errorf("RoleAuto.pool(%q) = %v, want destination", method, got)
		}
	}
}

func TestRequestTargetDropsQueryString(t *testing.T) {
	req := request(t, context.Background(), http.MethodGet, "https://registry.test/v2/foo/blobs/sha256:aa?X-Amz-Signature=secret")
	if got, want := requestTarget(req.URL), "https://registry.test/v2/foo/blobs/sha256:aa"; got != want {
		t.Fatalf("requestTarget = %q, want %q", got, want)
	}
	if got := requestTarget(nil); got != "" {
		t.Fatalf("requestTarget(nil) = %q, want empty", got)
	}
}

func TestMaxConcurrentRequestsEnv(t *testing.T) {
	tests := []struct {
		value    string
		want     int
		wantFrom bool
	}{
		{value: "", want: 0, wantFrom: false},
		{value: "16", want: 16, wantFrom: true},
		{value: " 8 ", want: 8, wantFrom: true},
		{value: "0", want: 0, wantFrom: true},
		{value: "-4", want: 0, wantFrom: true},
		{value: "lots", want: 0, wantFrom: false},
	}
	for _, tc := range tests {
		t.Setenv(EnvMaxConcurrentRequests, tc.value)
		got, gotFrom := maxConcurrentRequestsEnv()
		if got != tc.want || gotFrom != tc.wantFrom {
			t.Errorf("maxConcurrentRequestsEnv(%q) = %d, %v; want %d, %v", tc.value, got, gotFrom, tc.want, tc.wantFrom)
		}
	}
}

func TestConcurrencyLogModeEnv(t *testing.T) {
	tests := []struct {
		value string
		want  logMode
	}{
		{value: "", want: logOff},
		{value: "off", want: logOff},
		{value: "0", want: logOff},
		{value: "false", want: logOff},
		{value: "summary", want: logSummary},
		{value: "SUMMARY", want: logSummary},
		{value: "1", want: logRequests},
		{value: "true", want: logRequests},
		{value: "requests", want: logRequests},
		{value: "typo", want: logRequests},
	}
	for _, tc := range tests {
		t.Setenv(EnvLogConcurrency, tc.value)
		if got := concurrencyLogMode(); got != tc.want {
			t.Errorf("concurrencyLogMode(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestSummary(t *testing.T) {
	if got := newConcurrencyTracker(0, false, logOff, io.Discard).summary(); got != "" {
		t.Fatalf("summary with logging off = %q, want empty", got)
	}

	tracker := newConcurrencyTracker(2, true, logSummary, io.Discard)
	if got := tracker.summary(); got != "" {
		t.Fatalf("summary without any request = %q, want empty", got)
	}

	destination := wrapConcurrency(respondingTransport(http.StatusCreated, ""), RoleDestination, tracker)
	drain(t, destination, http.MethodPut, "https://registry.test/v2/foo/manifests/latest", 3)
	source := wrapConcurrency(respondingTransport(http.StatusOK, "blob"), RoleSource, tracker)
	drain(t, source, http.MethodGet, "https://base.test/v2/foo/blobs/sha256:aa", 1)

	got := tracker.summary()
	for _, want := range []string{"registry requests:", "3 to destination (peak", "/2 concurrent", "1 from sources (peak 1 concurrent)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary = %q, want it to contain %q", got, want)
		}
	}
}

func TestLoggingEveryRequest(t *testing.T) {
	var out strings.Builder
	tracker := newConcurrencyTracker(4, true, logRequests, &out)
	rt := wrapConcurrency(respondingTransport(http.StatusCreated, ""), RoleDestination, tracker)

	resp, err := rt.RoundTrip(request(t, context.Background(), http.MethodPut, "https://registry.test/v2/foo/blobs/uploads/1?state=secret"))
	if err != nil {
		t.Fatalf("RoundTrip error: %v", err)
	}
	resp.Body.Close()

	line := out.String()
	for _, want := range []string{"img registry:", "destination in-flight=1/4", "peak=1", "total=1", "PUT https://registry.test/v2/foo/blobs/uploads/1"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line = %q, want it to contain %q", line, want)
		}
	}
	if strings.Contains(line, "secret") {
		t.Fatalf("log line leaks the query string: %q", line)
	}
}
