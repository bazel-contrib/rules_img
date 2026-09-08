package gateway

import (
	"bytes"
	"io"
	"net/http"
	"sync/atomic"
)

// This file gives a forwarded request a body that can be sent a second time.
//
// net/http's HTTP/2 transport resends a request on a fresh connection when the
// far end answers with a graceful-shutdown GOAWAY that does not cover the
// stream — the frame a registry, a load balancer or a service mesh sends while
// draining a connection it is retiring, and which states in as many words that
// this request was never processed. It will only do so if it can rewind the
// body: with Request.GetBody unset it gives up with
//
//	cannot retry err [http2: Transport received Server's graceful shutdown
//	GOAWAY] after Request.Body was written; define Request.GetBody to avoid
//	this error
//
// and a manifest push fails on what is a routine event in any rolling restart.
// The same copy is what lets [Handler.forward] send a request again after the
// upstream rejected a stale token.
//
// An inbound body is a stream that can be read once, so the only way to hand out
// a second copy is to have kept one, and keeping one costs memory whose size the
// client chooses. Two bounds decide which bodies are kept: a limit per body, and
// a budget for all of them at once. A body that fits neither is forwarded
// exactly as it was before — streamed, with backpressure, and simply not
// retryable, which is what every body was until now.

// maxReplayableBody is the largest single body kept for a retry. 4 MiB is the
// manifest size the distribution spec requires every registry to accept, which
// makes it the natural bound for the request this exists for; the small blobs a
// push sends along the way (an image config, a tiny layer) fit under it too, and
// a real layer never will.
const maxReplayableBody = 4 << 20

// replayBudgetBytes is what all the copies held at one moment may occupy
// together. A gateway serves a whole build farm, so the per-body limit on its
// own would bound nothing: enough concurrent uploads would multiply it.
const replayBudgetBytes = 64 << 20

// replayBudget rations the memory spent on keeping request bodies resendable.
type replayBudget struct {
	limit int64
	inUse atomic.Int64
}

func newReplayBudget(limit int64) *replayBudget {
	return &replayBudget{limit: limit}
}

// acquire reserves n bytes and reports whether the budget had room for them. The
// returned release gives them back; it is never nil, so it can be deferred
// whether or not the reservation succeeded.
func (b *replayBudget) acquire(n int64) (release func(), ok bool) {
	if b == nil || n <= 0 || n > b.limit {
		return func() {}, false
	}
	if b.inUse.Add(n) > b.limit {
		b.inUse.Add(-n)
		return func() {}, false
	}
	var once atomic.Bool
	return func() {
		if once.CompareAndSwap(false, true) {
			b.inUse.Add(-n)
		}
	}, true
}

// replayableBody reads a request's body into memory so it can be sent more than
// once, and returns the copy together with the release that gives its share of
// the budget back. release is never nil.
//
// A nil copy is the ordinary answer and never an error: the request carries no
// body, does not declare its length, declares one above [maxReplayableBody], or
// arrives while the budget is spent. The caller forwards the streaming body
// unchanged.
//
// An error means the client's body ended before its declared length. Nothing is
// left to forward with, so the request cannot be served at all.
func replayableBody(r *http.Request, budget *replayBudget) ([]byte, func(), error) {
	nothing := func() {}
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength <= 0 || r.ContentLength > maxReplayableBody {
		return nil, nothing, nil
	}
	release, ok := budget.acquire(r.ContentLength)
	if !ok {
		return nil, nothing, nil
	}
	// The server's body reader is bounded by Content-Length already, so this
	// reads the whole body exactly, and reports a client that stopped short.
	buf := make([]byte, r.ContentLength)
	if _, err := io.ReadFull(r.Body, buf); err != nil {
		release()
		return nil, nothing, err
	}
	return buf, release, nil
}

// replayBody returns a fresh reader over a body kept by [replayableBody].
func replayBody(buf []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(buf))
}
