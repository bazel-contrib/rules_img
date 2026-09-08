package gateway

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	clientgateway "github.com/bazel-contrib/rules_img/img_tool/pkg/gateway"
)

// testManifest is a body small enough for the gateway to keep a copy of.
const testManifest = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`

// recordingUpstream keeps every request the gateway sent it, along with the body
// that arrived, so a test can ask both what went on the wire and whether it
// could have been put there a second time.
type recordingUpstream struct {
	requests []*http.Request
	bodies   []string
}

func (u *recordingUpstream) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
		return upstreamResponse(http.StatusOK, nil, ""), nil
	}
	u.requests = append(u.requests, r)
	u.bodies = append(u.bodies, readAll(r.Body))
	return upstreamResponse(http.StatusCreated, nil, ""), nil
}

func (u *recordingUpstream) last(t *testing.T) *http.Request {
	t.Helper()
	if len(u.requests) == 0 {
		t.Fatal("the gateway sent no request upstream")
	}
	return u.requests[len(u.requests)-1]
}

func readAll(body io.ReadCloser) string {
	if body == nil {
		return ""
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return ""
	}
	return string(b)
}

// putManifest pushes a manifest through the gateway. The request is shaped like
// one a server produced — a body that is a stream, and no GetBody — so the test
// cannot be passed by http.NewRequest's special case for in-memory readers.
func putManifest(h *Handler, body string) *http.Response {
	r := httptest.NewRequest(http.MethodPut, "/v2/app/manifests/latest", strings.NewReader(body))
	r.Header.Set(clientgateway.OriginalHostHeader, testUpstreamHost)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Result()
}

func manifestWriteHandler(t *testing.T, base http.RoundTripper) *Handler {
	t.Helper()
	return newTestHandler(allowHostPolicy(t, testUpstreamHost, "manifest:read", "manifest:write"), base)
}

func TestForwardGivesTheUpstreamRequestARewindableBody(t *testing.T) {
	upstream := &recordingUpstream{}
	h := manifestWriteHandler(t, upstream)

	if resp := putManifest(h, testManifest); resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if got := upstream.bodies[0]; got != testManifest {
		t.Fatalf("upstream body = %q, want %q", got, testManifest)
	}

	// The assertion that matters: the transport underneath can send this request
	// a second time, which is what it needs to survive a graceful GOAWAY.
	sent := upstream.last(t)
	if sent.GetBody == nil {
		t.Fatal("the upstream request has no GetBody, so the HTTP/2 transport cannot retry it after a GOAWAY")
	}
	rewound, err := sent.GetBody()
	if err != nil {
		t.Fatalf("GetBody: %v", err)
	}
	if got := readAll(rewound); got != testManifest {
		t.Errorf("rewound body = %q, want %q", got, testManifest)
	}
	if sent.ContentLength != int64(len(testManifest)) {
		t.Errorf("upstream Content-Length = %d, want %d", sent.ContentLength, len(testManifest))
	}
}

func TestForwardStreamsABodyTooLargeToKeep(t *testing.T) {
	oversized := strings.Repeat("x", maxReplayableBody+1)
	upstream := &recordingUpstream{}
	h := manifestWriteHandler(t, upstream)

	if resp := putManifest(h, oversized); resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	// Not retryable — nothing can be done about that without holding an unbounded
	// upload in memory — but it must still arrive whole.
	if got := upstream.bodies[0]; got != oversized {
		t.Errorf("upstream body is %d bytes, want %d", len(got), len(oversized))
	}
	if sent := upstream.last(t); sent.GetBody != nil {
		t.Error("a body too large to keep was buffered anyway")
	}
}

func TestForwardStreamsWhenTheReplayBudgetIsSpent(t *testing.T) {
	upstream := &recordingUpstream{}
	h := manifestWriteHandler(t, upstream)
	release, ok := h.replay.acquire(replayBudgetBytes)
	if !ok {
		t.Fatal("could not spend the whole replay budget")
	}
	defer release()

	if resp := putManifest(h, testManifest); resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if got := upstream.bodies[0]; got != testManifest {
		t.Errorf("upstream body = %q, want %q", got, testManifest)
	}
	if sent := upstream.last(t); sent.GetBody != nil {
		t.Error("a body was buffered although the budget was spent")
	}

	// And the budget is given back, so the next request is retryable again.
	release()
	if resp := putManifest(h, testManifest); resp.StatusCode != http.StatusCreated {
		t.Fatalf("status after releasing the budget = %d, want 201", resp.StatusCode)
	}
	if sent := upstream.last(t); sent.GetBody == nil {
		t.Error("the released budget was not reusable")
	}
}

// goAwayUpstream models the one net/http behaviour this whole mechanism exists
// for: x/net/http2's shouldRetryRequest, which resends a request the peer says
// it never processed — a graceful-shutdown GOAWAY that does not cover the stream
// — but only when Request.GetBody can rewind a body that has already been
// written, and otherwise gives up with the error spelled out below.
type goAwayUpstream struct {
	drained bool
	body    string
}

func (u *goAwayUpstream) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
		return upstreamResponse(http.StatusOK, nil, ""), nil
	}
	if !u.drained {
		// The connection is draining: the request went out, the body with it, and
		// the peer's GOAWAY says the stream was never processed.
		u.drained = true
		_, _ = io.Copy(io.Discard, r.Body)
		if r.GetBody == nil {
			return nil, errors.New("http2: Transport: cannot retry err [http2: Transport received Server's " +
				"graceful shutdown GOAWAY] after Request.Body was written; define Request.GetBody to avoid this error")
		}
		rewound, err := r.GetBody()
		if err != nil {
			return nil, err
		}
		r.Body = rewound
	}
	u.body = readAll(r.Body)
	return upstreamResponse(http.StatusCreated, nil, ""), nil
}

func TestForwardSurvivesAGracefulGoAwayMidRequest(t *testing.T) {
	upstream := &goAwayUpstream{}
	h := manifestWriteHandler(t, upstream)

	resp := putManifest(h, testManifest)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: a drained upstream connection failed the push", resp.StatusCode)
	}
	if upstream.body != testManifest {
		t.Errorf("upstream body after the retry = %q, want %q", upstream.body, testManifest)
	}
}

func TestForwardReportsAGracefulGoAwayItCannotRetry(t *testing.T) {
	// The bound is real: a body too large to keep still cannot be resent, and the
	// gateway reports that as a bad gateway rather than pretending otherwise.
	upstream := &goAwayUpstream{}
	h := manifestWriteHandler(t, upstream)

	resp := putManifest(h, strings.Repeat("x", maxReplayableBody+1))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestForwardHandlerGivesThePeerRequestARewindableBody(t *testing.T) {
	peer := &fakePeer{}
	f, _ := newTestForwarder(t, peer, "")

	w := forward(f, http.MethodPut, testUpstreamHost, "/v2/app/manifests/latest", testManifest)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	sent := peer.last(t)
	if got := readAll(sent.Body); got != testManifest {
		t.Fatalf("peer body = %q, want %q", got, testManifest)
	}
	if sent.ContentLength != int64(len(testManifest)) {
		t.Errorf("peer Content-Length = %d, want %d", sent.ContentLength, len(testManifest))
	}
	if sent.GetBody == nil {
		t.Fatal("the peer request has no GetBody, so a rolling update of the serving gateway fails the request")
	}
	rewound, err := sent.GetBody()
	if err != nil {
		t.Fatalf("GetBody: %v", err)
	}
	if got := readAll(rewound); got != testManifest {
		t.Errorf("rewound body = %q, want %q", got, testManifest)
	}
}

func TestForwardHandlerStreamsABodyTooLargeToKeep(t *testing.T) {
	oversized := strings.Repeat("x", maxReplayableBody+1)
	var arrived string
	peer := &fakePeer{respond: func(r *http.Request) (*http.Response, error) {
		arrived = readAll(r.Body)
		return peerResponse(r, http.StatusCreated, nil, ""), nil
	}}
	f, _ := newTestForwarder(t, peer, "")

	w := forward(f, http.MethodPut, testUpstreamHost, "/v2/app/blobs/uploads/1?digest=sha256:x", oversized)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	if arrived != oversized {
		t.Errorf("peer received %d bytes, want %d", len(arrived), len(oversized))
	}
	if sent := peer.last(t); sent.GetBody != nil {
		t.Error("a body too large to keep was buffered anyway")
	}
}

func TestReplayableBodyRejectsATruncatedBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/v2/app/manifests/latest", strings.NewReader(testManifest))
	// A client that declared more than it sent: there is nothing to forward with.
	r.Body = io.NopCloser(strings.NewReader(testManifest[:10]))

	buf, release, err := replayableBody(r, newReplayBudget(replayBudgetBytes))
	defer release()
	if err == nil {
		t.Fatalf("replayableBody accepted a truncated body, returning %d bytes", len(buf))
	}
}

func TestReplayBudget(t *testing.T) {
	budget := newReplayBudget(100)

	if _, ok := budget.acquire(101); ok {
		t.Error("a reservation larger than the whole budget was granted")
	}
	if _, ok := budget.acquire(0); ok {
		t.Error("an empty reservation was granted")
	}
	release, ok := budget.acquire(60)
	if !ok {
		t.Fatal("the first reservation was refused")
	}
	if _, ok := budget.acquire(60); ok {
		t.Error("the budget was overdrawn")
	}
	second, ok := budget.acquire(40)
	if !ok {
		t.Error("a reservation that exactly fits was refused")
	}
	release()
	release() // Releasing twice must not hand the same bytes out again.
	if _, ok := budget.acquire(70); ok {
		t.Error("a double release gave back more than it took")
	}
	second()
	if third, ok := budget.acquire(100); !ok {
		t.Error("the budget was not fully restored")
	} else {
		third()
	}
	if inUse := budget.inUse.Load(); inUse != 0 {
		t.Errorf("budget in use = %d, want 0", inUse)
	}
}
