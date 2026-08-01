package gateway

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests cover the receiving end of cache replication: who is allowed to
// write to an instance's cache, what a donation carries, and the warm-up that
// keeps a starting instance out of the load balancer until it has been seeded.

// postEvents sends a batch to an instance's replication endpoint the way a peer
// would, with the given origin identity, and returns the response.
func postEvents(h *Handler, origin string, events ...cacheEvent) *http.Response {
	body, _ := json.Marshal(cacheEventBatch{Events: events})
	r, _ := http.NewRequest(http.MethodPost, "http://gateway"+replicationEventsPath, strings.NewReader(string(body)))
	if origin != "" {
		r.Header.Set(cacheOriginHeader, origin)
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, r)
	return recorder.Result()
}

// donate fetches a donation from an instance, as a warming peer would.
func donate(h *Handler, origin string, limit int) *http.Response {
	r, _ := http.NewRequest(http.MethodGet, "http://gateway"+replicationDonatePath+"?limit="+strconv.Itoa(limit), nil)
	if origin != "" {
		r.Header.Set(cacheOriginHeader, origin)
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, r)
	return recorder.Result()
}

// TestReceivedFactsAreStoredLocally covers the plain case: a batch a peer sends
// becomes cache entries that answer probes, with this instance's own clock.
func TestReceivedFactsAreStoredLocally(t *testing.T) {
	router := newPeerRouter()
	upstream := &blobUpstream{status: http.StatusNotFound}
	fleet := newFleet(t, router, replicaConfig{id: "a", peers: []string{"b"}, upstream: upstream})
	handler := fleet["a"].handler

	resp := postEvents(handler, "b", cacheEvent{Registry: testUpstreamHost, Repository: "app", Digest: testCacheDigest})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("posting a batch = %d, want 204", resp.StatusCode)
	}
	// The probe is now answered without the registry, which answers 404 here: a
	// forwarded probe would have been a miss.
	probe := head(handler, testUpstreamHost, testBlobPath)
	if probe.StatusCode != http.StatusOK {
		t.Fatalf("probe after replication = %d, want 200", probe.StatusCode)
	}
	if got := len(upstream.requests); got != 0 {
		t.Fatalf("the probe reached the registry %d times, want it answered from the replicated fact", got)
	}
	// No length travelled with the fact, so the answer states none rather than
	// inventing one.
	if got := probe.Header.Get("Content-Length"); got != "" {
		t.Fatalf("replayed Content-Length = %q, want it absent", got)
	}

	// A deletion takes it away again.
	resp = postEvents(handler, "b", cacheEvent{Registry: testUpstreamHost, Repository: "app", Digest: testCacheDigest, Deleted: true})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("posting a deletion = %d, want 204", resp.StatusCode)
	}
	if held(fleet["a"], testCacheDigest) {
		t.Fatal("a kept a blob a peer said was deleted")
	}
}

// TestReceivedFactsMustNameADigest holds a peer to the same rule the local paths
// follow: only a well-formed digest is ever a cache key, so a peer cannot fill an
// instance's cache with keys no registry could answer for.
func TestReceivedFactsMustNameADigest(t *testing.T) {
	router := newPeerRouter()
	fleet := newFleet(t, router, replicaConfig{id: "a", peers: []string{"b"}})
	handler := fleet["a"].handler

	postEvents(handler, "b",
		cacheEvent{Registry: testUpstreamHost, Repository: "app", Digest: "latest"},
		cacheEvent{Registry: testUpstreamHost, Repository: "app", Digest: ""},
		cacheEvent{Registry: "", Repository: "app", Digest: testCacheDigest},
		cacheEvent{Registry: testUpstreamHost, Repository: "", Digest: testCacheDigest},
	)
	if entries := handler.blobCache.stats().entries; entries != 0 {
		t.Fatalf("cache holds %d entries after a batch of unusable facts, want 0", entries)
	}
}

// TestReplicationRequiresAPeerIdentity is the gate that separates using the
// gateway from writing to its cache. Every client of this listener is
// authenticated, but only the identities named as peers may insert facts: a false
// one would make a push client skip an upload it still owes.
func TestReplicationRequiresAPeerIdentity(t *testing.T) {
	router := newPeerRouter()
	fleet := newFleet(t, router, replicaConfig{
		id:             "a",
		peers:          []string{"b"},
		allowedPeerIDs: []string{"spiffe://cluster.local/ns/img/sa/gateway"},
	})
	handler := fleet["a"].handler

	// An unauthenticated client (this listener requires nothing) is not a peer.
	resp := postEvents(handler, "b", cacheEvent{Registry: testUpstreamHost, Repository: "app", Digest: testCacheDigest})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("posting without a peer identity = %d, want 403", resp.StatusCode)
	}
	if entries := handler.blobCache.stats().entries; entries != 0 {
		t.Fatalf("a refused batch stored %d entries", entries)
	}

	// The same request from the allow-listed identity is accepted. The principal is
	// what PeerAuth would have set for a verified client certificate.
	if !handler.replication.allowsPeer("cert:spiffe://cluster.local/ns/img/sa/gateway") {
		t.Fatal("the allow-listed certificate identity was refused")
	}
	if handler.replication.allowsPeer("cert:spiffe://cluster.local/ns/other/sa/worker") {
		t.Fatal("an identity outside the allow-list was accepted")
	}
	// A static shared token authenticates no identity, so it can never match.
	if handler.replication.allowsPeer("token") {
		t.Fatal("a static token was accepted as a peer identity")
	}
}

// TestReplicationRefusesItself catches the misconfiguration that would otherwise
// be silent: an instance in its own peer list, replicating to itself.
func TestReplicationRefusesItself(t *testing.T) {
	router := newPeerRouter()
	fleet := newFleet(t, router, replicaConfig{id: "a", peers: []string{"a"}})
	handler := fleet["a"].handler

	resp := postEvents(handler, "a", cacheEvent{Registry: testUpstreamHost, Repository: "app", Digest: testCacheDigest})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("posting a batch to ourselves = %d, want 409", resp.StatusCode)
	}
	if resp := donate(handler, "a", 10); resp.StatusCode != http.StatusConflict {
		t.Fatalf("asking ourselves for a donation = %d, want 409", resp.StatusCode)
	}
}

// TestReplicationEndpointsAbsentWithoutPeers keeps the surface from existing at all
// on a gateway that does not replicate: an instance with no peers cannot be talked
// into holding facts it never verified.
func TestReplicationEndpointsAbsentWithoutPeers(t *testing.T) {
	handler, _ := newCachingHandler(t, allowHostPolicy(t, testUpstreamHost, "blob:read"), &blobUpstream{status: http.StatusOK}, time.Hour)
	if resp := postEvents(handler, "b", cacheEvent{Registry: testUpstreamHost, Repository: "app", Digest: testCacheDigest}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("posting to a gateway that does not replicate = %d, want 404", resp.StatusCode)
	}
	if resp := donate(handler, "b", 10); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("asking a gateway that does not replicate for a donation = %d, want 404", resp.StatusCode)
	}
}

// TestDonationReportsAgeAndOccupancy covers the limit=0 probe a starting instance
// uses to choose a donor: it reads the headers and costs the donor no snapshot.
func TestDonationReportsAgeAndOccupancy(t *testing.T) {
	router := newPeerRouter()
	fleet := newFleet(t, router, replicaConfig{id: "a", peers: []string{"b"}})
	handler := fleet["a"].handler
	for i := range 3 {
		handler.blobCache.store(testUpstreamHost, "app", blobDigest(i), int64(i))
	}

	resp := donate(handler, "b", 0)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("probing a donor = %d, want 200", resp.StatusCode)
	}
	if _, err := time.Parse(time.RFC3339, resp.Header.Get(cacheStartedHeader)); err != nil {
		t.Fatalf("%s = %q, want an RFC3339 time: %v", cacheStartedHeader, resp.Header.Get(cacheStartedHeader), err)
	}
	if got := resp.Header.Get(cacheEntriesHeader); got != "3" {
		t.Fatalf("%s = %q, want 3", cacheEntriesHeader, got)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Fatalf("a limit=0 probe returned %d bytes of entries, want none", len(body))
	}
}

// TestDonationCarriesSizeAndRemainingLifetime is the property that keeps a copied
// fact from outliving the TTL that bounds it: the donor sends what is left of its
// deadline, not a fresh one, and the size it knows travels with it.
func TestDonationCarriesSizeAndRemainingLifetime(t *testing.T) {
	router := newPeerRouter()
	fleet := newFleet(t, router, replicaConfig{id: "a", peers: []string{"b"}})
	donor := fleet["a"]
	donor.handler.blobCache.store(testUpstreamHost, "app", testCacheDigest, 4096)
	// Two thirds of the way through the entry's life.
	donor.clock.advance(40 * time.Minute)

	resp := donate(donor.handler, "b", 10)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("donation = %d, want 200", resp.StatusCode)
	}
	var entry donatedEntry
	if err := json.NewDecoder(resp.Body).Decode(&entry); err != nil {
		t.Fatalf("decoding the donation: %v", err)
	}
	if entry.Digest != testCacheDigest || entry.Repository != "app" || entry.Registry != testUpstreamHost {
		t.Fatalf("donated entry = %+v, want the stored key", entry)
	}
	if entry.Size != 4096 {
		t.Fatalf("donated size = %d, want 4096", entry.Size)
	}
	if want := (20 * time.Minute).Milliseconds(); entry.TTLMillis != want {
		t.Fatalf("donated lifetime = %dms, want the %dms left of the donor's deadline", entry.TTLMillis, want)
	}
}

// TestDonationSkipsExpiredEntries keeps a donation from teaching a peer something
// the donor itself would no longer answer.
func TestDonationSkipsExpiredEntries(t *testing.T) {
	router := newPeerRouter()
	fleet := newFleet(t, router, replicaConfig{id: "a", peers: []string{"b"}})
	donor := fleet["a"]
	donor.handler.blobCache.store(testUpstreamHost, "app", testCacheDigest, 1)
	donor.clock.advance(2 * time.Hour)
	donor.handler.blobCache.store(testUpstreamHost, "app", blobDigest(7), 2)

	resp := donate(donor.handler, "b", 10)
	decoder := json.NewDecoder(resp.Body)
	var donated []donatedEntry
	for {
		var entry donatedEntry
		if err := decoder.Decode(&entry); err != nil {
			break
		}
		donated = append(donated, entry)
	}
	if len(donated) != 1 || donated[0].Digest != blobDigest(7) {
		t.Fatalf("donated %+v, want only the entry that is still live", donated)
	}
}

// TestDonationIsRefusedWhenBusy pins the promise a donor makes to its own clients:
// seeding peers never crowds out registry traffic, so past a couple of donations at
// once the answer is "ask someone else".
func TestDonationIsRefusedWhenBusy(t *testing.T) {
	router := newPeerRouter()
	fleet := newFleet(t, router, replicaConfig{id: "a", peers: []string{"b"}})
	handler := fleet["a"].handler
	for range maxConcurrentDonations {
		handler.replication.donating <- struct{}{}
	}
	resp := donate(handler, "b", 10)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("donation while busy = %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Fatal("a refused donation carries no Retry-After")
	}
	// A probe is still answered: it costs the donor nothing to describe itself.
	if resp := donate(handler, "b", 0); resp.StatusCode != http.StatusOK {
		t.Fatalf("probing a busy donor = %d, want 200", resp.StatusCode)
	}
}

// TestWarmUpSeedsFromAPeer is the joining instance's story end to end: it reports
// itself unhealthy, takes a peer's hottest entries, and only then serves — so the
// probes it answers first are hits rather than a second copy of the fleet's
// upstream traffic.
func TestWarmUpSeedsFromAPeer(t *testing.T) {
	router := newPeerRouter()
	// The donor is built first and filled before the joiner starts.
	fleet := newFleet(t, router, replicaConfig{id: "donor", peers: []string{"joiner"}})
	donor := fleet["donor"]
	for i := range 5 {
		donor.handler.blobCache.store(testUpstreamHost, "app", blobDigest(i), int64(1000+i))
	}

	joinerUpstream := &blobUpstream{status: http.StatusNotFound}
	joining := newFleet(t, router, replicaConfig{
		id:            "joiner",
		peers:         []string{"donor"},
		warmupTimeout: 5 * time.Second,
		warmupEntries: 100,
		upstream:      joinerUpstream,
	})["joiner"]

	eventually(t, "the joiner to be seeded", func() bool {
		return joining.handler.blobCache.stats().entries == 5
	})
	eventually(t, "the joiner to report itself healthy", func() bool {
		return !joining.handler.warmingUp()
	})
	// The seeded entries answer probes, sizes and all, without the registry.
	probe := head(joining.handler, testUpstreamHost, "/v2/app/blobs/"+blobDigest(3))
	if probe.StatusCode != http.StatusOK {
		t.Fatalf("probe for a seeded blob = %d, want 200", probe.StatusCode)
	}
	if got := probe.Header.Get("Content-Length"); got != "1003" {
		t.Fatalf("replayed Content-Length = %q, want 1003 to have travelled with the donation", got)
	}
	if got := len(joinerUpstream.requests); got != 0 {
		t.Fatalf("the joiner sent %d requests upstream after being seeded, want none", got)
	}
}

// TestWarmUpKeepsTheInstanceOutOfService covers the health gate on its own: while
// an instance is seeding, /healthz refuses, and it recovers when the warm-up
// finishes — or, whatever happens, when its budget is spent.
func TestWarmUpKeepsTheInstanceOutOfService(t *testing.T) {
	router := newPeerRouter()
	// The only peer is unreachable, so this instance can never be seeded.
	replication, err := NewCacheReplication(ReplicationConfig{
		Peers:         StaticPeers{"https://gone.test"},
		Client:        &http.Client{Transport: router},
		SelfID:        "joiner",
		WarmupTimeout: time.Hour,
		WarmupEntries: 100,
		Logger:        log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("NewCacheReplication: %v", err)
	}
	handler := New(
		WithAuthorizer(allowHostPolicy(t, testUpstreamHost, "blob:read")),
		WithLogger(log.New(io.Discard, "", 0)),
		WithBlobExistenceCache(time.Hour, 64*entryCost),
		WithCacheReplication(replication),
	)
	clock := &fakeClock{t: time.Now()}
	handler.now = clock.now

	if resp := do(handler, http.MethodGet, "", healthPath); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("health while warming up = %d, want 503", resp.StatusCode)
	}
	// Past the budget the instance serves, seeded or not: a replica that never
	// becomes ready is a far worse outcome than one with a cold cache.
	clock.advance(time.Hour + time.Second)
	resp := do(handler, http.MethodGet, "", healthPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health after the warm-up budget = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) != "ok" {
		t.Fatalf("health body = %q, want ok", body)
	}
}

// TestWarmUpEndsWithoutPeers is the first replica of a deployment: discovery
// answers "there is nobody", and it must serve at once rather than spend its whole
// budget waiting for peers that do not exist.
func TestWarmUpEndsWithoutPeers(t *testing.T) {
	replication, err := NewCacheReplication(ReplicationConfig{
		Peers:         StaticPeers{},
		Client:        &http.Client{},
		SelfID:        "only",
		WarmupTimeout: time.Hour,
		WarmupEntries: 100,
		Logger:        log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("NewCacheReplication: %v", err)
	}
	handler := New(
		WithAuthorizer(allowHostPolicy(t, testUpstreamHost, "blob:read")),
		WithLogger(log.New(io.Discard, "", 0)),
		WithBlobExistenceCache(time.Hour, 64*entryCost),
		WithCacheReplication(replication),
	)
	done := make(chan struct{})
	defer close(done)
	go handler.RunCacheReplication(done)

	eventually(t, "the only instance of a deployment to serve", func() bool {
		return do(handler, http.MethodGet, "", healthPath).StatusCode == http.StatusOK
	})
}

// TestWarmUpPrefersTheOldestPeer covers the choice of donor: an instance that has
// been up longer has had longer to fill its cache, so it is the one asked.
func TestWarmUpPrefersTheOldestPeer(t *testing.T) {
	router := newPeerRouter()
	fleet := newFleet(t, router,
		replicaConfig{id: "young", peers: []string{"joiner"}},
		replicaConfig{id: "old", peers: []string{"joiner"}},
	)
	// Both hold entries, so the choice is not decided by who has something to give.
	for i := range 4 {
		fleet["young"].handler.blobCache.store(testUpstreamHost, "app", blobDigest(i), 1)
		fleet["old"].handler.blobCache.store(testUpstreamHost, "app", blobDigest(i), 1)
	}
	fleet["old"].handler.replication.started = time.Now().Add(-time.Hour)

	joining := newFleet(t, router, replicaConfig{
		id:            "joiner",
		peers:         []string{"young", "old"},
		warmupTimeout: 5 * time.Second,
		warmupEntries: 100,
	})["joiner"]

	eventually(t, "the joiner to be seeded", func() bool {
		return joining.handler.blobCache.stats().entries == 4
	})
	if got := router.count("old.test", replicationDonatePath); got < 2 {
		t.Fatalf("the older peer was asked %d times (probe plus donation expected)", got)
	}
	// The younger peer is probed like the other, but not asked for entries: only
	// one donation is needed and the older instance answered it.
	if young, old := router.count("young.test", replicationDonatePath), router.count("old.test", replicationDonatePath); young >= old {
		t.Fatalf("the younger peer was asked %d times and the older %d; want the older preferred", young, old)
	}
}

// TestForwarderRefusesTheControlSurface is the hole a pass-through forwarder would
// otherwise open: a build action's request to a replication endpoint would reach the
// shared serving gateway carrying the *forwarder's* identity, which is a peer
// identity there. It stops at the sidecar.
func TestForwarderRefusesTheControlSurface(t *testing.T) {
	serving := &countingHandler{}
	forwarder, err := NewForward(ForwardConfig{
		Peer:      mustParseURL(t, "https://peer.test:8443"),
		Transport: handlerTransport{handler: serving},
		Logger:    log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("NewForward: %v", err)
	}
	for _, path := range []string{replicationEventsPath, replicationDonatePath, reservedPathPrefix + "anything"} {
		r, _ := http.NewRequest(http.MethodPost, "http://sidecar"+path, strings.NewReader("{}"))
		recorder := httptest.NewRecorder()
		forwarder.ServeHTTP(recorder, r)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("forwarding %s = %d, want 404", path, recorder.Code)
		}
	}
	if serving.requests != 0 {
		t.Fatalf("the serving gateway saw %d control requests through the forwarder, want none", serving.requests)
	}
}

// countingHandler counts the requests that reach it.
type countingHandler struct{ requests int }

func (c *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.requests++
	w.WriteHeader(http.StatusOK)
}
