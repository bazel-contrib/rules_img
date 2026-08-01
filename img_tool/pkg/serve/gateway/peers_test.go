package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests cover discovering peers from Kubernetes EndpointSlices. The API
// server is a plain http.Handler reached through handlerTransport, so a list and a
// watch are exercised without a cluster and without a port.
//
// A watch here delivers its events and ends, rather than staying open: that is
// exactly what the API server does at its own timeout, and it is the path this
// implementation is built around — deliver, end, list again.

// fakeAPIServer answers EndpointSlice lists and watches from canned responses.
type fakeAPIServer struct {
	// list is the response to a non-watch request.
	list endpointSliceList
	// watchBody is written verbatim to a watch request, as a stream of events.
	watchBody string
	// status, when non-zero, is answered to everything.
	status int
	// requests records the query strings received, so a test can tell a list from a
	// watch and check what was asked for. requestsMu is set only by the tests that
	// read them from another goroutine.
	requests   []string
	requestsMu *sync.Mutex
}

func (f *fakeAPIServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.requestsMu != nil {
		f.requestsMu.Lock()
	}
	f.requests = append(f.requests, r.URL.RawQuery)
	if f.requestsMu != nil {
		f.requestsMu.Unlock()
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	if r.URL.Query().Get("watch") == "1" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, f.watchBody)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(f.list)
}

func (f *fakeAPIServer) requestCount() int {
	f.requestsMu.Lock()
	defer f.requestsMu.Unlock()
	return len(f.requests)
}

// newTestPeers builds a KubernetesPeers against a fake API server, with a token
// file on disk so the credential is read the way it is in a pod.
func newTestPeers(t *testing.T, api http.Handler, service string) *KubernetesPeers {
	t.Helper()
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("a-service-account-token\n"), 0o600); err != nil {
		t.Fatalf("writing the token file: %v", err)
	}
	peers, err := NewKubernetesPeers(KubernetesPeerOptions{
		Service:      service,
		Scheme:       "https",
		Port:         8443,
		SelfName:     "gateway-0",
		Logger:       log.New(io.Discard, "", 0),
		APIServerURL: "https://api.test",
		Client:       &http.Client{Transport: handlerTransport{handler: api}},
		TokenPath:    tokenPath,
	})
	if err != nil {
		t.Fatalf("NewKubernetesPeers: %v", err)
	}
	return peers
}

// slice builds an EndpointSlice as the API server would report one. Each endpoint
// is written "<pod>@<address>", optionally suffixed ":notready" or ":terminating".
func slice(name string, endpoints ...string) endpointSlice {
	var s endpointSlice
	s.Metadata.Name = name
	s.Metadata.ResourceVersion = "1"
	for _, endpoint := range endpoints {
		spec, condition, _ := strings.Cut(endpoint, ":")
		pod, address, _ := strings.Cut(spec, "@")
		entry := sliceEndpoint{
			Addresses: []string{address},
			TargetRef: &objectRef{Kind: "Pod", Name: pod},
		}
		yes := true
		no := false
		switch condition {
		case "notready":
			entry.Conditions.Ready = &no
		case "terminating":
			entry.Conditions.Terminating = &yes
		}
		s.Endpoints = append(s.Endpoints, entry)
	}
	return s
}

// TestDiscoveryListsPeers covers the first list: every endpoint of the Service
// becomes a peer, addressed on the port this instance serves, and this instance is
// not one of them.
func TestDiscoveryListsPeers(t *testing.T) {
	api := &fakeAPIServer{}
	api.list.Metadata.ResourceVersion = "42"
	api.list.Items = []endpointSlice{slice("gw-abc",
		"gateway-0@10.0.0.1", // this instance
		"gateway-1@10.0.0.2",
		"gateway-2@10.0.0.3",
	)}
	peers := newTestPeers(t, api, "img-gateway/oci-distribution-gateway")

	if peers.Settled() {
		t.Fatal("a source that has not listed yet reports itself settled")
	}
	version, err := peers.list(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if version != "42" {
		t.Fatalf("resource version = %q, want 42", version)
	}
	if !peers.Settled() {
		t.Fatal("a source that has listed does not report itself settled")
	}
	got := peers.Peers()
	if len(got) != 2 {
		t.Fatalf("peers = %+v, want the two endpoints that are not this instance", got)
	}
	if got[0].URL != "https://10.0.0.2:8443" || got[1].URL != "https://10.0.0.3:8443" {
		t.Fatalf("peer URLs = %q and %q, want the endpoint addresses on this gateway's port", got[0].URL, got[1].URL)
	}
	if got[0].ID != "gateway-1" || !got[0].Ready {
		t.Fatalf("peer = %+v, want the pod name and ready", got[0])
	}
	// The label selector is what finds the slices of a Service, whose names are
	// generated: without it the list would be every slice in the namespace.
	if !strings.Contains(api.requests[0], "kubernetes.io%2Fservice-name%3Doci-distribution-gateway") {
		t.Fatalf("list query = %q, want it selected by service name", api.requests[0])
	}
}

// TestDiscoveryKeepsNotReadyPeersAndDropsTerminatingOnes is the distinction that
// makes warm-up work: an instance that is still seeding is told what the fleet
// learns (so it has it by the time it serves) but is never asked to donate, while
// one that is shutting down is no longer anything.
func TestDiscoveryKeepsNotReadyPeersAndDropsTerminatingOnes(t *testing.T) {
	api := &fakeAPIServer{}
	api.list.Items = []endpointSlice{slice("gw-abc",
		"gateway-1@10.0.0.2:notready",
		"gateway-2@10.0.0.3:terminating",
		"gateway-3@10.0.0.4",
	)}
	peers := newTestPeers(t, api, "img-gateway/gw")
	if _, err := peers.list(context.Background()); err != nil {
		t.Fatalf("list: %v", err)
	}

	got := peers.Peers()
	if len(got) != 2 {
		t.Fatalf("peers = %+v, want the not-ready and the ready one", got)
	}
	byURL := map[string]Peer{}
	for _, peer := range got {
		byURL[peer.URL] = peer
	}
	if peer, ok := byURL["https://10.0.0.2:8443"]; !ok || peer.Ready {
		t.Fatalf("the not-ready peer = %+v, want it kept and marked not ready", peer)
	}
	if peer, ok := byURL["https://10.0.0.4:8443"]; !ok || !peer.Ready {
		t.Fatalf("the ready peer = %+v, want it kept and marked ready", peer)
	}
	if _, ok := byURL["https://10.0.0.3:8443"]; ok {
		t.Fatal("a terminating endpoint is still a peer")
	}
}

// TestDiscoveryFollowsTheWatch is the point of watching rather than polling: pods
// coming and going change the peer set without a restart and without a re-list.
func TestDiscoveryFollowsTheWatch(t *testing.T) {
	api := &fakeAPIServer{}
	api.list.Items = []endpointSlice{slice("gw-abc", "gateway-1@10.0.0.2")}
	peers := newTestPeers(t, api, "img-gateway/gw")
	if _, err := peers.list(context.Background()); err != nil {
		t.Fatalf("list: %v", err)
	}

	// A scale-up (the slice now has two endpoints), then the slice's deletion.
	grown, _ := json.Marshal(slice("gw-abc", "gateway-1@10.0.0.2", "gateway-2@10.0.0.3"))
	second, _ := json.Marshal(slice("gw-def", "gateway-3@10.0.0.4"))
	api.watchBody = `{"type":"MODIFIED","object":` + string(grown) + "}\n" +
		`{"type":"ADDED","object":` + string(second) + "}\n" +
		`{"type":"BOOKMARK","object":{"metadata":{"resourceVersion":"99"}}}` + "\n"

	// The stream ends after those events, which is what the API server does at its
	// own timeout, and is reported as the expected end of a watch.
	err := peers.watch(context.Background(), "42")
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("watch ended with %v, want the expected close", err)
	}
	if got := len(peers.Peers()); got != 3 {
		t.Fatalf("peers = %+v, want the three endpoints the watch reported", peers.Peers())
	}

	// A deleted slice takes its endpoints with it.
	api.watchBody = `{"type":"DELETED","object":` + string(second) + "}\n"
	_ = peers.watch(context.Background(), "99")
	got := peers.Peers()
	if len(got) != 2 || got[0].URL != "https://10.0.0.2:8443" || got[1].URL != "https://10.0.0.3:8443" {
		t.Fatalf("peers after the slice was deleted = %+v, want only the first slice's endpoints", got)
	}
	if len(api.requests) < 2 || api.requests[1] == api.requests[len(api.requests)-1] {
		t.Fatalf("watch requests = %q, want each to resume from a resource version", api.requests)
	}
}

// TestDiscoveryReportsAWatchError makes the "list again" path explicit: an ERROR
// event (in practice "resource version too old") ends the watch with a failure, and
// the caller re-lists.
func TestDiscoveryReportsAWatchError(t *testing.T) {
	api := &fakeAPIServer{}
	peers := newTestPeers(t, api, "img-gateway/gw")
	api.watchBody = `{"type":"ERROR","object":{"kind":"Status","code":410,"reason":"Expired"}}` + "\n"

	err := peers.watch(context.Background(), "1")
	if err == nil {
		t.Fatal("an ERROR event ended the watch without an error")
	}
	if strings.Contains(err.Error(), "closed") {
		t.Fatalf("an ERROR event was reported as the expected close: %v", err)
	}
}

// TestDiscoveryExplainsMissingRBAC turns the one misconfiguration an operator will
// actually hit into an error that names the fix.
func TestDiscoveryExplainsMissingRBAC(t *testing.T) {
	api := &fakeAPIServer{status: http.StatusForbidden}
	peers := newTestPeers(t, api, "img-gateway/gw")
	_, err := peers.list(context.Background())
	if err == nil {
		t.Fatal("a 403 from the API server was not an error")
	}
	if !strings.Contains(err.Error(), "endpointslices") {
		t.Fatalf("error = %v, want it to name the permission that is missing", err)
	}
}

// TestServiceReferenceNamespace covers the two spellings of the flag, including the
// one that only works in a pod.
func TestServiceReferenceNamespace(t *testing.T) {
	namespace, name, err := splitServiceRef("img-gateway/oci-gateway")
	if err != nil || namespace != "img-gateway" || name != "oci-gateway" {
		t.Fatalf("splitServiceRef(ns/name) = (%q, %q, %v)", namespace, name, err)
	}
	for _, invalid := range []string{"", "/name", "ns/", "ns/name/extra"} {
		if _, _, err := splitServiceRef(invalid); err == nil {
			t.Fatalf("splitServiceRef(%q) was accepted", invalid)
		}
	}
	// A bare name needs the pod's own namespace, which is not mounted here.
	if _, _, err := splitServiceRef("oci-gateway"); err == nil {
		t.Skip("this machine has a Kubernetes ServiceAccount namespace mounted")
	}
}

// TestDiscoveryRunKeepsWatching covers the loop itself: it lists, watches, and
// picks the peer set up — and when the watch ends at once (something in the path
// refusing to stream, rather than the API server's timeout) it paces itself instead
// of spinning.
func TestDiscoveryRunKeepsWatching(t *testing.T) {
	api := &fakeAPIServer{requestsMu: &sync.Mutex{}}
	api.list.Items = []endpointSlice{slice("gw-abc", "gateway-1@10.0.0.2")}
	// Every watch ends immediately, which is the case that could spin.
	api.watchBody = ""
	peers := newTestPeers(t, api, "img-gateway/gw")

	done := make(chan struct{})
	go peers.Run(done)
	defer close(done)

	deadline := time.Now().Add(2 * time.Second)
	for len(peers.Peers()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := peers.Peers(); len(got) != 1 {
		t.Fatalf("peers = %+v, want the one endpoint the list reported", got)
	}
	// A cycle is a list and a watch, and cycles are paced at minWatchCycle, so a
	// couple of hundred milliseconds must not have produced many of them.
	if got := api.requestCount(); got > 4 {
		t.Fatalf("the discovery loop made %d API requests before its first pause, want it paced", got)
	}
}

// TestDiscoveryStaticPeers pins the other source: a fixed list, every entry ready
// and unidentified, with any trailing slash removed so a URL is built by
// concatenation.
func TestDiscoveryStaticPeers(t *testing.T) {
	peers := StaticPeers{"https://a.test:8443", "https://b.test:8443/"}
	if !peers.Settled() {
		t.Fatal("a static list is not settled")
	}
	got := peers.Peers()
	if len(got) != 2 || got[1].URL != "https://b.test:8443" {
		t.Fatalf("peers = %+v, want both, without a trailing slash", got)
	}
	for _, peer := range got {
		if !peer.Ready || peer.ID != "" {
			t.Fatalf("peer = %+v, want it ready and unidentified", peer)
		}
	}
}
