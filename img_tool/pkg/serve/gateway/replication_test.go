package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"

	clientgateway "github.com/bazel-contrib/rules_img/img_tool/pkg/gateway"
)

// These tests cover replication of the blob existence cache between gateway
// instances: what one instance teaches another, what it deliberately does not,
// and the two properties the whole design rests on — that a client request never
// waits for a peer, and that a fact learned from a peer is never sent back out.
//
// Instances are wired to each other in-process through peerRouter, so a "fleet"
// here is a set of Handlers and a map: no listener, no port.

// peerRouter routes a replication request to the instance that owns the host in
// its URL, which is how several gateways talk to each other without a network.
type peerRouter struct {
	mu       sync.Mutex
	handlers map[string]http.Handler
	// requests counts the replication requests each host received, by path.
	requests map[string]int
	// fail, when set for a host, makes every request to it fail as an unreachable
	// peer would.
	fail map[string]bool
}

func newPeerRouter() *peerRouter {
	return &peerRouter{
		handlers: map[string]http.Handler{},
		requests: map[string]int{},
		fail:     map[string]bool{},
	}
}

func (p *peerRouter) add(host string, handler http.Handler) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handlers[host] = handler
}

func (p *peerRouter) RoundTrip(req *http.Request) (*http.Response, error) {
	p.mu.Lock()
	handler, ok := p.handlers[req.URL.Host]
	p.requests[req.URL.Host+req.URL.Path]++
	failing := p.fail[req.URL.Host]
	p.mu.Unlock()
	if !ok || failing {
		return nil, &net.OpError{Op: "dial", Err: errPeerUnreachable}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	resp := recorder.Result()
	resp.Request = req
	return resp, nil
}

func (p *peerRouter) count(host, path string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[host+path]
}

// errPeerUnreachable stands in for a refused connection.
var errPeerUnreachable = errors.New("peer unreachable")

// replicaConfig is what a test varies about one instance of a fleet.
type replicaConfig struct {
	// id is the instance's identity, and the host it is reachable at is
	// "<id>.test".
	id string
	// peers are the ids of the instances it replicates to.
	peers []string
	// allowedPeerIDs restricts who may write to its cache.
	allowedPeerIDs []string
	// warmupTimeout and warmupEntries configure seeding from a peer; both zero
	// means no seeding.
	warmupTimeout time.Duration
	warmupEntries int
	// batchSize caps one message, defaulting to one event so that a test does not
	// depend on the flush timer to see the first batch.
	batchSize int
	// upstream is the fake registry behind it.
	upstream http.RoundTripper
}

// replica is one instance of a test fleet.
type replica struct {
	handler *Handler
	clock   *fakeClock
	// stop stops the instance's background work.
	stop func()
}

// newFleet builds a set of interconnected gateway instances. Every instance
// caches, replicates to the peers named in its config, and shares one router.
func newFleet(t *testing.T, router *peerRouter, configs ...replicaConfig) map[string]*replica {
	t.Helper()
	fleet := map[string]*replica{}
	for _, cfg := range configs {
		peers := make(StaticPeers, 0, len(cfg.peers))
		for _, peer := range cfg.peers {
			peers = append(peers, "https://"+peer+".test")
		}
		batchSize := cfg.batchSize
		if batchSize == 0 {
			batchSize = 1
		}
		replication, err := NewCacheReplication(ReplicationConfig{
			Peers:          peers,
			Client:         &http.Client{Transport: router},
			SelfID:         cfg.id,
			AllowedPeerIDs: cfg.allowedPeerIDs,
			BatchSize:      batchSize,
			WarmupTimeout:  cfg.warmupTimeout,
			WarmupEntries:  cfg.warmupEntries,
			Logger:         log.New(io.Discard, "", 0),
		})
		if err != nil {
			t.Fatalf("NewCacheReplication(%s): %v", cfg.id, err)
		}
		upstream := cfg.upstream
		if upstream == nil {
			upstream = &blobUpstream{status: http.StatusNotFound}
		}
		handler := New(
			WithAuthorizer(allowHostPolicy(t, testUpstreamHost, "blob:read", "blob:write")),
			WithKeychain(authn.NewMultiKeychain()),
			WithLogger(log.New(io.Discard, "", 0)),
			WithBaseTransport(upstream),
			WithBlobExistenceCache(time.Hour, 4096*entryCost),
			WithCacheReplication(replication),
		)
		if handler.replication == nil {
			t.Fatalf("replication was disabled for %s", cfg.id)
		}
		clock := &fakeClock{t: handler.blobCache.base}
		handler.blobCache.now = clock.now
		router.add(cfg.id+".test", handler)

		done := make(chan struct{})
		var once sync.Once
		stop := func() { once.Do(func() { close(done) }) }
		t.Cleanup(stop)
		go handler.RunCacheReplication(done)
		fleet[cfg.id] = &replica{handler: handler, clock: clock, stop: stop}
	}
	return fleet
}

// eventually waits for condition to hold, failing the test if it does not within
// a second. Replication is asynchronous by design, so every assertion about what
// a peer received is a wait.
func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// held reports whether an instance's cache holds a blob, without touching its LRU
// order in a way a test would have to account for.
func held(r *replica, digest string) bool {
	_, ok := r.handler.blobCache.lookup(testUpstreamHost, "app", digest)
	return ok
}

// TestReplicationSharesADiscoveredBlob is the point of the feature: the probe one
// instance paid an upstream round trip for is answered by another instance
// without one.
func TestReplicationSharesADiscoveredBlob(t *testing.T) {
	router := newPeerRouter()
	upstreamA := &blobUpstream{status: http.StatusOK, contentLength: "4096"}
	upstreamB := &blobUpstream{status: http.StatusOK, contentLength: "4096"}
	fleet := newFleet(t, router,
		replicaConfig{id: "a", peers: []string{"b"}, upstream: upstreamA},
		replicaConfig{id: "b", peers: []string{"a"}, upstream: upstreamB},
	)

	if resp := head(fleet["a"].handler, testUpstreamHost, testBlobPath); resp.StatusCode != http.StatusOK {
		t.Fatalf("probe on a = %d, want 200", resp.StatusCode)
	}
	eventually(t, "b to learn the blob from a", func() bool { return held(fleet["b"], testCacheDigest) })

	if resp := head(fleet["b"].handler, testUpstreamHost, testBlobPath); resp.StatusCode != http.StatusOK {
		t.Fatalf("probe on b = %d, want 200", resp.StatusCode)
	}
	if got := len(upstreamB.requests); got != 0 {
		t.Fatalf("b sent %d requests upstream, want the probe answered from what a replicated", got)
	}
}

// TestReplicationIgnoresMisses holds the line the cache itself holds: only facts
// travel. A blob that is absent now can be pushed a second later, so "not there"
// must not be replicated any more than it is cached.
func TestReplicationIgnoresMisses(t *testing.T) {
	router := newPeerRouter()
	upstreamA := &blobUpstream{status: http.StatusNotFound}
	fleet := newFleet(t, router,
		replicaConfig{id: "a", peers: []string{"b"}, upstream: upstreamA},
		replicaConfig{id: "b", peers: []string{"a"}},
	)

	if resp := head(fleet["a"].handler, testUpstreamHost, testBlobPath); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("probe on a = %d, want 404", resp.StatusCode)
	}
	// Nothing should ever arrive at b. Give the batcher a chance to prove it.
	time.Sleep(20 * replicationFlushDelay)
	if got := router.count("b.test", replicationEventsPath); got != 0 {
		t.Fatalf("a sent %d replication messages for a miss, want none", got)
	}
	if held(fleet["b"], testCacheDigest) {
		t.Fatal("b holds a blob whose only news was a 404")
	}
}

// TestReplicationSharesACommittedUpload covers the other side the cache learns
// from: the instance that wins the race to push a layer answers every other
// instance's probe for it.
func TestReplicationSharesACommittedUpload(t *testing.T) {
	router := newPeerRouter()
	upstreamA := &blobUpstream{status: http.StatusCreated}
	fleet := newFleet(t, router,
		replicaConfig{id: "a", peers: []string{"b"}, upstream: upstreamA},
		replicaConfig{id: "b", peers: []string{"a"}},
	)

	put, _ := http.NewRequest(http.MethodPut, "http://gateway/v2/app/blobs/uploads/session-1?digest="+testCacheDigest, strings.NewReader(""))
	put.Header.Set(clientgateway.OriginalHostHeader, testUpstreamHost)
	recorder := httptest.NewRecorder()
	fleet["a"].handler.ServeHTTP(recorder, put)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("upload commit = %d, want 201", recorder.Code)
	}
	eventually(t, "b to learn the pushed blob", func() bool { return held(fleet["b"], testCacheDigest) })
}

// TestReplicationForwardsADelete is the counterpart: a blob a client really
// deleted stops being claimed by the whole fleet at once, rather than by each
// instance at the end of its own TTL.
func TestReplicationForwardsADelete(t *testing.T) {
	router := newPeerRouter()
	fleet := newFleet(t, router,
		replicaConfig{id: "a", peers: []string{"b"}, upstream: &blobUpstream{status: http.StatusAccepted}},
		replicaConfig{id: "b", peers: []string{"a"}},
	)
	// Both instances believe the blob is there.
	fleet["a"].handler.blobCache.store(testUpstreamHost, "app", testCacheDigest, 1)
	fleet["b"].handler.blobCache.store(testUpstreamHost, "app", testCacheDigest, 1)

	if resp := do(fleet["a"].handler, http.MethodDelete, testUpstreamHost, testBlobPath); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete through a = %d, want 202", resp.StatusCode)
	}
	if held(fleet["a"], testCacheDigest) {
		t.Fatal("a still claims a blob it just deleted")
	}
	eventually(t, "b to forget the deleted blob", func() bool { return !held(fleet["b"], testCacheDigest) })
}

// TestReplicationKeepsARefusedDeleteLocal is the asymmetry that keeps a client
// from flushing the fleet's cache: this instance drops its entry whatever the
// registry answers (that costs one probe), but the peers are only told when the
// registry says the blob really went.
func TestReplicationKeepsARefusedDeleteLocal(t *testing.T) {
	router := newPeerRouter()
	fleet := newFleet(t, router,
		replicaConfig{id: "a", peers: []string{"b"}, upstream: &blobUpstream{status: http.StatusMethodNotAllowed}},
		replicaConfig{id: "b", peers: []string{"a"}},
	)
	fleet["a"].handler.blobCache.store(testUpstreamHost, "app", testCacheDigest, 1)
	fleet["b"].handler.blobCache.store(testUpstreamHost, "app", testCacheDigest, 1)

	if resp := do(fleet["a"].handler, http.MethodDelete, testUpstreamHost, testBlobPath); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("delete through a = %d, want 405", resp.StatusCode)
	}
	if held(fleet["a"], testCacheDigest) {
		t.Fatal("a kept an entry after forwarding a delete for it")
	}
	time.Sleep(20 * replicationFlushDelay)
	if !held(fleet["b"], testCacheDigest) {
		t.Fatal("b dropped an entry for a delete the registry refused")
	}
}

// TestReplicatedFactIsNotRebroadcast is the property that keeps two instances
// from feeding each other forever: what arrives from a peer is applied and stops
// there. b replicates to c, so a fact b learned from a would show up at c if
// receiving re-broadcast.
func TestReplicatedFactIsNotRebroadcast(t *testing.T) {
	router := newPeerRouter()
	fleet := newFleet(t, router,
		replicaConfig{id: "a", peers: []string{"b"}, upstream: &blobUpstream{status: http.StatusOK, contentLength: "7"}},
		replicaConfig{id: "b", peers: []string{"c"}},
		replicaConfig{id: "c", peers: []string{"b"}},
	)

	if resp := head(fleet["a"].handler, testUpstreamHost, testBlobPath); resp.StatusCode != http.StatusOK {
		t.Fatalf("probe on a = %d, want 200", resp.StatusCode)
	}
	eventually(t, "b to learn the blob from a", func() bool { return held(fleet["b"], testCacheDigest) })

	// b now knows something c does not. If it passed it on, c would hold it.
	time.Sleep(20 * replicationFlushDelay)
	if got := router.count("c.test", replicationEventsPath); got != 0 {
		t.Fatalf("b sent %d messages to c for a fact it received, want none", got)
	}
	if held(fleet["c"], testCacheDigest) {
		t.Fatal("c holds a fact that reached it only by being re-broadcast")
	}
}

// TestReplicationSurvivesAnUnreachablePeer pins the promise that matters most:
// replication is best effort, so a peer that is down changes nothing a client
// sees.
func TestReplicationSurvivesAnUnreachablePeer(t *testing.T) {
	router := newPeerRouter()
	upstreamA := &blobUpstream{status: http.StatusOK, contentLength: "4096"}
	fleet := newFleet(t, router,
		// "gone" is never added to the router, so every send to it fails.
		replicaConfig{id: "a", peers: []string{"gone"}, upstream: upstreamA},
	)

	for range 3 {
		if resp := head(fleet["a"].handler, testUpstreamHost, testBlobPath); resp.StatusCode != http.StatusOK {
			t.Fatalf("probe on a = %d, want 200", resp.StatusCode)
		}
	}
	// The first probe was answered upstream and the rest from the local cache: the
	// unreachable peer neither broke nor slowed any of them.
	if got := len(upstreamA.requests); got != 1 {
		t.Fatalf("a sent %d probes upstream, want 1", got)
	}
	eventually(t, "the failed sends to be attempted", func() bool {
		return router.count("gone.test", replicationEventsPath) > 0
	})
}

// TestRecordNeverBlocks is the request path's guarantee, exercised at the point it
// could be violated: with the queue full and nothing draining it, recording a fact
// still returns, and the facts that did not fit are counted rather than waited on.
func TestRecordNeverBlocks(t *testing.T) {
	replication, err := NewCacheReplication(ReplicationConfig{
		Peers:  StaticPeers{"https://peer.test"},
		SelfID: "self",
		Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("NewCacheReplication: %v", err)
	}
	replication.bind(newBlobExistenceCache(time.Hour, 64*entryCost), nil)

	// Nothing runs the batcher, so the queue can only fill up.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range replicationQueueDepth + 100 {
			replication.record(context.Background(), testCacheRegistry, testCacheRepository, blobDigest(i), false)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recording facts blocked when the replication queue was full")
	}
	if dropped := replication.dropped.Load(); dropped < 100 {
		t.Fatalf("dropped %d facts, want at least the 100 that did not fit", dropped)
	}
}

// TestBatchingFillsToTheLimit checks the size half of the batching rule: with more
// facts queued than one message may carry, the messages that go out are full ones
// rather than one per fact.
func TestBatchingFillsToTheLimit(t *testing.T) {
	batches := newBatchRecorder()
	replication := newSendingReplication(t, batches, 8)
	done := make(chan struct{})
	defer close(done)
	go replication.batch(done)

	for i := range 32 {
		replication.record(context.Background(), testCacheRegistry, testCacheRepository, blobDigest(i), false)
	}
	eventually(t, "all 32 facts to be sent", func() bool { return batches.events() == 32 })
	sizes := batches.sizes()
	for _, size := range sizes {
		if size > 8 {
			t.Fatalf("a message carried %d facts, above the batch size of 8 (sizes %v)", size, sizes)
		}
	}
	if len(sizes) > 16 {
		t.Fatalf("32 facts went out in %d messages, want them batched (sizes %v)", len(sizes), sizes)
	}
}

// TestBatchingFlushesOnTheTimer checks the other half: a single fact does not wait
// for company that may never come, and the timer that sends it is not restarted by
// later facts — so a steady trickle is delivered continuously rather than held back.
func TestBatchingFlushesOnTheTimer(t *testing.T) {
	batches := newBatchRecorder()
	// A batch size far above what the test produces, so only the timer can flush.
	replication := newSendingReplication(t, batches, 1000)
	done := make(chan struct{})
	defer close(done)
	go replication.batch(done)

	replication.record(context.Background(), testCacheRegistry, testCacheRepository, blobDigest(1), false)
	eventually(t, "one fact to be sent on the timer alone", func() bool { return batches.events() == 1 })

	// A trickle of facts, each arriving after the previous batch's timer started.
	// If the timer were restarted by a later fact, none of these would be sent
	// until the trickle stopped.
	for i := range 5 {
		replication.record(context.Background(), testCacheRegistry, testCacheRepository, blobDigest(100+i), false)
		time.Sleep(2 * replicationFlushDelay)
	}
	eventually(t, "the trickle to be delivered as it arrives", func() bool { return batches.events() == 6 })
	if got := len(batches.sizes()); got < 2 {
		t.Fatalf("a trickle of 6 facts went out in %d message(s), want the timer to have flushed several", got)
	}
}

// TestBatchingDropsRedundantFacts covers the one thing a batch is condensed by:
// the same blob learned several times inside one flush window is one fact, and a
// deletion at the end of it is the one that must arrive.
func TestBatchingDropsRedundantFacts(t *testing.T) {
	events := []cacheEvent{
		{Registry: "reg", Repository: "app", Digest: "a"},
		{Registry: "reg", Repository: "app", Digest: "b"},
		{Registry: "reg", Repository: "app", Digest: "a"},
		{Registry: "reg", Repository: "app", Digest: "b", Deleted: true},
		{Registry: "reg", Repository: "other", Digest: "a"},
	}
	got := dedupeEvents(events)
	want := []cacheEvent{
		// The repeated insertion of a is kept once, at its last position.
		{Registry: "reg", Repository: "app", Digest: "a"},
		// b was deleted after it was inserted, so the deletion is what travels.
		{Registry: "reg", Repository: "app", Digest: "b", Deleted: true},
		// The same digest in another repository is another blob.
		{Registry: "reg", Repository: "other", Digest: "a"},
	}
	if len(got) != len(want) {
		t.Fatalf("dedupeEvents = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dedupeEvents[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	// A batch with nothing to condense is passed through as it is.
	if condensed := dedupeEvents(want); len(condensed) != len(want) {
		t.Fatalf("dedupeEvents of an already-distinct batch = %+v", condensed)
	}
}

// batchRecorder is a peer that records the messages it is sent.
type batchRecorder struct {
	mu      sync.Mutex
	batches [][]cacheEvent
}

func newBatchRecorder() *batchRecorder { return &batchRecorder{} }

func (b *batchRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var batch cacheEventBatch
	if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	b.batches = append(b.batches, batch.Events)
	b.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (b *batchRecorder) sizes() []int {
	b.mu.Lock()
	defer b.mu.Unlock()
	sizes := make([]int, 0, len(b.batches))
	for _, batch := range b.batches {
		sizes = append(sizes, len(batch))
	}
	return sizes
}

func (b *batchRecorder) events() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	total := 0
	for _, batch := range b.batches {
		total += len(batch)
	}
	return total
}

// newSendingReplication builds a replication whose only peer is the given
// recorder, for the tests that are about batching rather than about a fleet.
func newSendingReplication(t *testing.T, peer http.Handler, batchSize int) *CacheReplication {
	t.Helper()
	router := newPeerRouter()
	router.add("peer.test", peer)
	replication, err := NewCacheReplication(ReplicationConfig{
		Peers:     StaticPeers{"https://peer.test"},
		Client:    &http.Client{Transport: router},
		SelfID:    "self",
		BatchSize: batchSize,
		Logger:    log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("NewCacheReplication: %v", err)
	}
	replication.bind(newBlobExistenceCache(time.Hour, 64*entryCost), nil)
	return replication
}
