package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// This file discovers a gateway's peers from the Kubernetes EndpointSlices of its
// own Service, so that a serving deployment replicates its blob existence cache
// without every replica being told about every other one.
//
// The Service the gateway is already behind is the list of its peers: exactly the
// pods a client could have reached instead. Watching it means the set follows a
// scale-up, a scale-down, a rolling update and a lost node with no configuration
// and no restart.
//
// It speaks the API directly, as [inClusterTokenReviewer] does, rather than
// through k8s.io/client-go: what is needed is one list and one watch of one
// resource type, against a handful of fields, and the gateway keeps its
// dependencies to the standard library plus go-containerregistry. What that
// costs is that the list/watch bookkeeping is written out here — the resource
// version, re-listing when the watch expires, and backing off when the API server
// is unreachable — which is the code below.
//
// Two details of EndpointSlice shape the result:
//
//   - A not-ready endpoint is still in the slice, with ready=false. Those are kept
//     as replication targets and excluded as donors: a replica that is still
//     warming up should be told what the fleet learns (so it has it by the time it
//     serves) but has nothing to give away. A *terminating* endpoint is dropped
//     from both, since it is on its way out.
//   - The port in a slice is the target port, i.e. the port the peer's container
//     listens on. Peers are replicas of this same process, so the address to reach
//     one is its endpoint address with the port this instance itself serves — no
//     port name to configure, and no risk of picking the metrics port.

const (
	// endpointSliceAPI is the collection this watches, per namespace.
	endpointSliceAPI = "/apis/discovery.k8s.io/v1/namespaces/%s/endpointslices"
	// serviceNameLabel is the label Kubernetes puts on every EndpointSlice naming
	// the Service it belongs to. It is the only way to find the slices of a
	// Service: their names are generated.
	serviceNameLabel = "kubernetes.io/service-name"
	// serviceAccountNamespacePath is where a pod's own namespace is mounted, which
	// is the default namespace of the watched Service.
	serviceAccountNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

	// watchTimeoutSeconds asks the API server to close an idle watch, so a
	// connection wedged by a middlebox is noticed and re-established rather than
	// silently delivering nothing. The API server adds its own jitter.
	watchTimeoutSeconds = 300
	// listTimeout bounds one list request.
	listTimeout = 30 * time.Second
	// watchBackoffMin and watchBackoffMax bound the retry delay after a failed
	// list or watch. An unreachable API server must not turn into a hot loop, and
	// peer discovery going stale is survivable: replication is best effort, and the
	// last known peer set stays in force.
	watchBackoffMin = time.Second
	watchBackoffMax = 30 * time.Second
	// minWatchCycle is the shortest a list-and-watch cycle may take. The API server
	// holds a watch open for minutes, so this only bites when something in the path
	// ends one immediately — which without it would spin the loop.
	minWatchCycle = time.Second
	// maxEndpointSliceBytes bounds a list response. A slice holds at most 1000
	// endpoints and a Service may have several; this leaves generous room.
	maxEndpointSliceBytes = 32 << 20
)

// KubernetesPeerOptions configures [NewKubernetesPeers].
type KubernetesPeerOptions struct {
	// Service names the Service whose endpoints are this gateway's peers, as
	// "<namespace>/<name>" or as "<name>" for a Service in this pod's own
	// namespace. Required.
	Service string
	// Scheme and Port form a peer's URL together with its endpoint address. Port
	// is the port this instance serves on, which is also what its replicas listen
	// on.
	Scheme string
	Port   int
	// SelfName is this pod's name, which is how an instance recognizes its own
	// endpoint in the list and leaves itself out. Defaults to the hostname, which
	// in a pod is the pod name.
	SelfName string
	// Logger records discovery. Defaults to the standard logger.
	Logger *log.Logger
	// APIServerURL and Client override the in-cluster API server connection. They
	// exist for tests; in a pod both are built from the pod's environment.
	APIServerURL string
	Client       *http.Client
	// TokenPath is the file holding the credential presented to the API server.
	// Defaults to the pod's projected ServiceAccount token, re-read per request
	// because kubelet rotates it.
	TokenPath string
}

// KubernetesPeers is a [PeerSource] backed by a list and watch of the
// EndpointSlices of one Service. It is safe for concurrent use.
type KubernetesPeers struct {
	namespace  string
	service    string
	scheme     string
	port       int
	selfName   string
	collection string
	client     *http.Client
	tokenPath  string
	log        *log.Logger

	// peers is the current set, swapped as a whole so readers never lock.
	peers atomic.Pointer[[]Peer]
	// settled reports that at least one list has succeeded, which is how a warming
	// instance tells "not listed yet" from "no peers".
	settled atomic.Bool

	// mu guards slices, the per-slice endpoint sets the watch updates
	// incrementally.
	mu     sync.Mutex
	slices map[string][]Peer
}

// NewKubernetesPeers builds a peer source from the pod's environment: the API
// server address, its CA, and the pod's own ServiceAccount token.
//
// The gateway's ServiceAccount needs to get, list and watch endpointslices in the
// namespace of the Service, which no built-in role grants — see the Role in
// //cmd/oci-distribution-gateway/blob-existence-cache.md.
func NewKubernetesPeers(opts KubernetesPeerOptions) (*KubernetesPeers, error) {
	namespace, service, err := splitServiceRef(opts.Service)
	if err != nil {
		return nil, err
	}
	k := &KubernetesPeers{
		namespace: namespace,
		service:   service,
		scheme:    opts.Scheme,
		port:      opts.Port,
		selfName:  opts.SelfName,
		client:    opts.Client,
		tokenPath: opts.TokenPath,
		log:       opts.Logger,
		slices:    make(map[string][]Peer),
	}
	if k.log == nil {
		k.log = log.New(os.Stderr, "", log.LstdFlags)
	}
	if k.scheme == "" {
		k.scheme = "https"
	}
	if k.port <= 0 {
		return nil, errors.New("peer discovery needs the port this gateway serves on")
	}
	if k.selfName == "" {
		k.selfName, _ = os.Hostname()
	}
	if k.tokenPath == "" {
		k.tokenPath = serviceAccountTokenPath
	}
	base := opts.APIServerURL
	if base == "" {
		if base, err = inClusterAPIServerURL(); err != nil {
			return nil, fmt.Errorf("peer discovery: %w", err)
		}
	}
	if k.client == nil {
		// No client timeout: a watch is long-lived by design, and every request
		// here is bounded by its context instead.
		if k.client, err = inClusterAPIClient(0); err != nil {
			return nil, fmt.Errorf("peer discovery: %w", err)
		}
	}
	k.collection = strings.TrimSuffix(base, "/") + fmt.Sprintf(endpointSliceAPI, url.PathEscape(k.namespace))
	empty := []Peer{}
	k.peers.Store(&empty)
	return k, nil
}

// splitServiceRef parses a "<namespace>/<name>" or "<name>" Service reference,
// defaulting the namespace to the pod's own.
func splitServiceRef(ref string) (namespace, name string, err error) {
	if ref == "" {
		return "", "", errors.New("peer discovery needs a Service name")
	}
	if namespace, name, found := strings.Cut(ref, "/"); found {
		if namespace == "" || name == "" || strings.Contains(name, "/") {
			return "", "", fmt.Errorf("peer discovery Service %q must be <namespace>/<name> or <name>", ref)
		}
		return namespace, name, nil
	}
	own, err := readTrimmedFile(serviceAccountNamespacePath, 4<<10)
	if err != nil {
		return "", "", fmt.Errorf("peer discovery Service %q gives no namespace and this process is not in a pod: %w", ref, err)
	}
	return string(own), ref, nil
}

// inClusterAPIServer is defined in peerauth.go, alongside the TokenReview client
// that reads the same three pieces of pod environment.

// Peers implements [PeerSource].
func (k *KubernetesPeers) Peers() []Peer {
	if peers := k.peers.Load(); peers != nil {
		return *peers
	}
	return nil
}

// Settled implements [PeerSource]: true once a list has succeeded.
func (k *KubernetesPeers) Settled() bool { return k.settled.Load() }

// Run keeps the peer set current until done is closed. It lists, then watches
// from that list's resource version, and re-lists whenever the watch cannot be
// resumed — the loop every Kubernetes informer runs, in the small.
//
// A watch always ends: the API server closes it at its own timeout, the resource
// version ages out, a proxy drops the connection. That is the healthy cycle and
// costs one list per five minutes. Anything else backs off, so an unreachable or
// unauthorized API server cannot turn into a hot loop; the last known peer set
// stays in force while it is out of reach, since replication is best effort and a
// stale peer list is much better than none.
func (k *KubernetesPeers) Run(done <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-done
		cancel()
	}()

	backoff := watchBackoffMin
	retry := func(what string, err error) bool {
		k.log.Printf("peer discovery: %s the EndpointSlices of %s/%s failed, keeping %d known peer(s): %v",
			what, k.namespace, k.service, len(k.Peers()), err)
		if !sleepFor(ctx, backoff) {
			return false
		}
		backoff = min(backoff*2, watchBackoffMax)
		return true
	}
	for ctx.Err() == nil {
		version, err := k.list(ctx)
		if err != nil {
			if ctx.Err() != nil || !retry("listing", err) {
				return
			}
			continue
		}
		started := time.Now()
		err = k.watch(ctx, version)
		switch {
		case ctx.Err() != nil:
			return
		case err == nil || errors.Is(err, errWatchClosed):
			// The expected end of a watch. List again at once — unless the watch ended
			// immediately, which is not the API server's timeout but something in the
			// path refusing to stream, and would otherwise spin this loop.
			backoff = watchBackoffMin
			if time.Since(started) < minWatchCycle && !sleepFor(ctx, minWatchCycle) {
				return
			}
		default:
			if !retry("watching", err) {
				return
			}
		}
	}
}

// errWatchClosed is the expected end of a watch: the API server closed the stream
// because its timeout elapsed. It is not a failure and needs no backoff.
var errWatchClosed = errors.New("watch closed by the API server")

// sleepFor waits for d, reporting false if ctx was cancelled first.
func sleepFor(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// endpointSliceList is the subset of a discovery.k8s.io/v1 EndpointSliceList this
// reads.
type endpointSliceList struct {
	Metadata struct {
		ResourceVersion string `json:"resourceVersion"`
		Continue        string `json:"continue"`
	} `json:"metadata"`
	Items []endpointSlice `json:"items"`
}

// endpointSlice is the subset of an EndpointSlice this reads.
type endpointSlice struct {
	Metadata struct {
		Name            string `json:"name"`
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
	Endpoints []sliceEndpoint `json:"endpoints"`
}

// sliceEndpoint is one endpoint of an EndpointSlice: an address, whether it is
// serving, and what it belongs to.
type sliceEndpoint struct {
	Addresses  []string `json:"addresses"`
	Conditions struct {
		// Both are pointers because absent means something: an absent ready is
		// ready, and an absent terminating is not terminating.
		Ready       *bool `json:"ready"`
		Terminating *bool `json:"terminating"`
	} `json:"conditions"`
	Hostname  *string    `json:"hostname"`
	TargetRef *objectRef `json:"targetRef"`
}

// objectRef is the reference an endpoint carries to the object behind it, which
// for a gateway's peers is a Pod and whose name is that pod's — the same name the
// pod reports as its hostname.
type objectRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// watchEvent is one line of a watch stream.
type watchEvent struct {
	Type   string          `json:"type"`
	Object json.RawMessage `json:"object"`
}

// list replaces the peer set from a fresh list and returns the resource version
// to watch from.
func (k *KubernetesPeers) list(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	query := url.Values{"labelSelector": {serviceNameLabel + "=" + k.service}}
	resp, err := k.request(ctx, k.collection+"?"+query.Encode())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var list endpointSliceList
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxEndpointSliceBytes)).Decode(&list); err != nil {
		return "", fmt.Errorf("decoding the EndpointSlice list: %w", err)
	}
	if list.Metadata.Continue != "" {
		// Only reachable for a Service with an implausible number of slices, and
		// what it costs is peers in the pages not read: say so rather than quietly
		// replicating to a subset.
		k.log.Printf("peer discovery: %s/%s has more EndpointSlices than one page; replicating only to the first page's endpoints",
			k.namespace, k.service)
	}

	k.mu.Lock()
	k.slices = make(map[string][]Peer, len(list.Items))
	for _, slice := range list.Items {
		k.slices[slice.Metadata.Name] = k.peersOf(slice)
	}
	k.mu.Unlock()
	k.publish()
	k.settled.Store(true)
	return list.Metadata.ResourceVersion, nil
}

// watch applies EndpointSlice changes as they happen, until the stream ends.
func (k *KubernetesPeers) watch(ctx context.Context, version string) error {
	// The API server closes the stream at timeoutSeconds, which ends this cleanly.
	// The context deadline is the backstop for a connection something else has
	// wedged: without it a silently dead watch would deliver nothing forever.
	ctx, cancel := context.WithTimeout(ctx, (watchTimeoutSeconds+60)*time.Second)
	defer cancel()
	query := url.Values{
		"labelSelector":       {serviceNameLabel + "=" + k.service},
		"watch":               {"1"},
		"allowWatchBookmarks": {"true"},
		"resourceVersion":     {version},
		"timeoutSeconds":      {fmt.Sprint(watchTimeoutSeconds)},
	}
	resp, err := k.request(ctx, k.collection+"?"+query.Encode())
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// A watch is a stream of JSON objects, which json.Decoder reads one at a time
	// as they arrive.
	decoder := json.NewDecoder(resp.Body)
	for {
		var event watchEvent
		if err := decoder.Decode(&event); err != nil {
			if errors.Is(err, io.EOF) {
				return errWatchClosed
			}
			return err
		}
		switch event.Type {
		case "ADDED", "MODIFIED", "DELETED":
			var slice endpointSlice
			if err := json.Unmarshal(event.Object, &slice); err != nil {
				return fmt.Errorf("decoding a watched EndpointSlice: %w", err)
			}
			k.mu.Lock()
			if event.Type == "DELETED" {
				delete(k.slices, slice.Metadata.Name)
			} else {
				k.slices[slice.Metadata.Name] = k.peersOf(slice)
			}
			k.mu.Unlock()
			k.publish()
		case "BOOKMARK":
			// Carries only a resource version, to keep a resumable one at hand. This
			// implementation re-lists rather than resuming, so there is nothing to do.
		case "ERROR":
			// Almost always "resource version too old": the answer is to list again,
			// which is what returning does.
			return fmt.Errorf("the API server ended the watch: %s", strings.TrimSpace(string(event.Object)))
		}
	}
}

// peersOf turns one EndpointSlice into the peers it names, leaving out this
// instance and any endpoint that is shutting down.
func (k *KubernetesPeers) peersOf(slice endpointSlice) []Peer {
	peers := make([]Peer, 0, len(slice.Endpoints))
	for _, endpoint := range slice.Endpoints {
		if endpoint.Conditions.Terminating != nil && *endpoint.Conditions.Terminating {
			continue
		}
		name := ""
		if endpoint.TargetRef != nil && endpoint.TargetRef.Kind == "Pod" {
			name = endpoint.TargetRef.Name
		} else if endpoint.Hostname != nil {
			name = *endpoint.Hostname
		}
		if name != "" && name == k.selfName {
			// This instance. Replicating to itself would cost a round trip to learn
			// what it already knows.
			continue
		}
		if len(endpoint.Addresses) == 0 {
			continue
		}
		// A ready condition that is absent means ready, per the API's own contract.
		ready := endpoint.Conditions.Ready == nil || *endpoint.Conditions.Ready
		peers = append(peers, Peer{
			URL:   k.scheme + "://" + net.JoinHostPort(endpoint.Addresses[0], fmt.Sprint(k.port)),
			ID:    name,
			Ready: ready,
		})
	}
	return peers
}

// publish recomputes the flat peer set from the per-slice ones and swaps it in.
// The result is sorted and deduplicated so that the set is stable across
// re-lists, which is what keeps the log from reporting churn that did not happen.
func (k *KubernetesPeers) publish() {
	k.mu.Lock()
	var peers []Peer
	for _, slice := range k.slices {
		peers = append(peers, slice...)
	}
	k.mu.Unlock()

	slices.SortFunc(peers, func(a, b Peer) int { return strings.Compare(a.URL, b.URL) })
	peers = slices.CompactFunc(peers, func(a, b Peer) bool { return a.URL == b.URL })
	previous := k.Peers()
	k.peers.Store(&peers)
	if len(previous) != len(peers) {
		k.log.Printf("peer discovery: %s/%s has %d peer(s) for cache replication (was %d)",
			k.namespace, k.service, len(peers), len(previous))
	}
}

// request performs one authenticated GET against the API server. The
// ServiceAccount token is read per request, because kubelet rotates it at 80% of
// a lifetime whose floor is ten minutes.
func (k *KubernetesPeers) request(ctx context.Context, url string) (*http.Response, error) {
	token, err := readTrimmedFile(k.tokenPath, maxTokenFileSize)
	if err != nil {
		return nil, fmt.Errorf("reading our ServiceAccount token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+string(token))
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("the API server answered %d: the gateway's ServiceAccount needs get, list and watch on endpointslices in namespace %s", resp.StatusCode, k.namespace)
		}
		return nil, fmt.Errorf("the API server answered %d", resp.StatusCode)
	}
	return resp, nil
}
