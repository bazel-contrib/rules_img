package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests cover splitting cache replication onto a listener of its own, which
// is what lets a deployment authenticate its peers differently from its clients:
// anonymous plaintext for build clients, mTLS between the instances. The two
// properties that matter are that the endpoints *move* rather than being
// duplicated, and that the separate listener exposes nothing but them.

// TestSeparateReplicationMovesTheEndpointsOffTheClientListener is the security
// property of the split. A gateway whose clients are anonymous must not leave the
// write path into its blob existence cache on the clients' listener: a client that
// can insert a fact about a blob that is not there makes push clients skip an
// upload they still owe.
func TestSeparateReplicationMovesTheEndpointsOffTheClientListener(t *testing.T) {
	router := newPeerRouter()
	fleet := newFleet(t, router, replicaConfig{id: "a", peers: []string{"b"}})
	handler := fleet["a"].handler

	// Before the split the client listener serves them, which is the single-listener
	// deployment every existing gateway is.
	if resp := postEvents(handler, "b", cacheEvent{Registry: testUpstreamHost, Repository: "app", Digest: testCacheDigest}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("posting to the client listener before the split = %d, want 204", resp.StatusCode)
	}

	peerHandler := handler.SeparateReplicationHandler(nil)
	if peerHandler == nil {
		t.Fatal("SeparateReplicationHandler returned nil for a replicating gateway")
	}

	// Afterwards the client listener refuses them...
	resp := postEvents(handler, "b", cacheEvent{Registry: testUpstreamHost, Repository: "app", Digest: testOtherDigest})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("posting to the client listener after the split = %d, want 404", resp.StatusCode)
	}
	if held(fleet["a"], testOtherDigest) {
		t.Fatal("the client listener wrote to the cache after replication moved to its own listener")
	}
	// ...and the peer listener serves them.
	if resp := postEventsTo(peerHandler, "b", []cacheEvent{{Registry: testUpstreamHost, Repository: "app", Digest: testOtherDigest}}, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("posting to the peer listener = %d, want 204", resp.StatusCode)
	}
	if !held(fleet["a"], testOtherDigest) {
		t.Fatal("the peer listener did not write the fact to the cache")
	}
}

// TestSeparateReplicationListenerServesNothingElse holds the peer listener to a
// closed surface. It carries a credential the peers trust, so anything of the
// registry protocol reachable there would be a way to spend that credential on an
// upstream registry.
func TestSeparateReplicationListenerServesNothingElse(t *testing.T) {
	router := newPeerRouter()
	fleet := newFleet(t, router, replicaConfig{id: "a", peers: []string{"b"}})
	peerHandler := fleet["a"].handler.SeparateReplicationHandler(nil)

	for _, path := range []string{
		"/v2/",
		"/v2/app/manifests/latest",
		"/v2/app/blobs/" + testCacheDigest,
		"/",
	} {
		r, _ := http.NewRequest(http.MethodGet, "http://gateway"+path, nil)
		r.Header.Set("X-rules_img-Original-Host", testUpstreamHost)
		recorder := httptest.NewRecorder()
		peerHandler.ServeHTTP(recorder, r)
		if got := recorder.Code; got != http.StatusNotFound {
			t.Errorf("GET %s on the peer listener = %d, want 404", path, got)
		}
	}
}

// TestSeparateReplicationListenerAnswersHealth keeps the readiness probe working
// against the peer listener. A Kubernetes probe names a port, so a peer listener
// that could not answer would have to be probed through the client one — which
// reports the readiness of a different socket.
func TestSeparateReplicationListenerAnswersHealth(t *testing.T) {
	router := newPeerRouter()
	fleet := newFleet(t, router, replicaConfig{id: "a", peers: []string{"b"}})
	peerHandler := fleet["a"].handler.SeparateReplicationHandler(nil)

	r, _ := http.NewRequest(http.MethodGet, "http://gateway"+healthPath, nil)
	recorder := httptest.NewRecorder()
	peerHandler.ServeHTTP(recorder, r)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s on the peer listener = %d, want 200", healthPath, recorder.Code)
	}
	if got := recorder.Body.String(); !strings.Contains(got, "ok") {
		t.Errorf("health body = %q, want it to say ok", got)
	}
}

// TestSeparateReplicationRequiresReplication covers the flag combination that asks
// for a peer listener on a gateway that does not replicate. Returning nil is what
// lets the command refuse to start rather than opening a listener with nothing on
// it.
func TestSeparateReplicationRequiresReplication(t *testing.T) {
	handler := newTestHandler(allowHostPolicy(t, testUpstreamHost, "blob:read"), &fakeUpstreamRT{})
	if peerHandler := handler.SeparateReplicationHandler(nil); peerHandler != nil {
		t.Fatalf("SeparateReplicationHandler() = %v for a gateway that does not replicate, want nil", peerHandler)
	}
}

// TestSeparateReplicationListenerAuthenticatesPeers checks that the peer
// listener's own authentication is enforced — it is a different PeerAuth from the
// client listener's, which is the entire point of the split.
func TestSeparateReplicationListenerAuthenticatesPeers(t *testing.T) {
	dir := t.TempDir()
	tokens := writeFile(t, dir, "tokens", []byte("0123456789abcdef0123456789abcdef\n"))
	auth, err := NewPeerAuth(PeerAuthOptions{TokenFiles: []string{tokens}})
	if err != nil {
		t.Fatalf("NewPeerAuth: %v", err)
	}

	router := newPeerRouter()
	fleet := newFleet(t, router, replicaConfig{id: "a", peers: []string{"b"}})
	peerHandler := fleet["a"].handler.SeparateReplicationHandler(auth)

	// No credential: refused before the endpoint is reached.
	if resp := postEventsTo(peerHandler, "b", []cacheEvent{{Registry: testUpstreamHost, Repository: "app", Digest: testCacheDigest}}, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("posting with no credential = %d, want 401", resp.StatusCode)
	}
	if held(fleet["a"], testCacheDigest) {
		t.Fatal("an unauthenticated peer wrote to the cache")
	}
	// The health probe stays reachable without one, so a readiness probe works.
	r, _ := http.NewRequest(http.MethodGet, "http://gateway"+healthPath, nil)
	recorder := httptest.NewRecorder()
	peerHandler.ServeHTTP(recorder, r)
	if recorder.Code != http.StatusOK {
		t.Fatalf("health with no credential = %d, want 200", recorder.Code)
	}
	// With the token it goes through.
	resp := postEventsTo(peerHandler, "b", []cacheEvent{{Registry: testUpstreamHost, Repository: "app", Digest: testCacheDigest}},
		withBearer("0123456789abcdef0123456789abcdef"))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("posting with the token = %d, want 204", resp.StatusCode)
	}
	if !held(fleet["a"], testCacheDigest) {
		t.Fatal("an authenticated peer's fact did not reach the cache")
	}
}
