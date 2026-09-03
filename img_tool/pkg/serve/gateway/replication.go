package gateway

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// This file replicates the blob existence cache between the instances of a
// serving deployment.
//
// The cache memoizes one fact — this blob is in this repository — and an instance
// learns it the expensive way: a client asks, and an upstream round trip answers.
// With N replicas behind one Service, the same first-seen blob costs up to N such
// round trips, one per instance, because each learns it separately. Replication
// removes the multiplier: whichever instance pays for the answer tells the others,
// so a fleet pays once.
//
// What travels is only the identity of a blob — (registry, repository, digest) —
// and only for facts, never for misses. A miss is not a fact: a blob that is
// absent now can be pushed a second later. The receiving instance stores the
// entry exactly as if it had learned it itself, with its own clock, so nothing
// about access time or LRU order is synchronized. Each instance evicts its own
// least recently used entries, and the caches are deliberately allowed to
// diverge: the cache is a hint, and two instances holding different subsets of it
// is not a failure mode.
//
// Three things travel between peers:
//
//   - An insertion, broadcast when this instance admits an entry.
//   - A deletion, broadcast when a blob is actually deleted through this instance,
//     so the fleet stops claiming a blob that is gone rather than each instance
//     finding out for itself at the end of its TTL.
//   - A donation, which a starting instance asks a peer for: the hottest entries
//     of a running cache, so a new replica does not send the fleet's whole working
//     set upstream again while it fills its own (see [CacheReplication.warmUp]).
//
// Everything here is best effort, and three rules keep it that way:
//
//  1. Nothing on the request path does network I/O. An insertion is queued on a
//     buffered channel with a non-blocking send; a full queue drops the event
//     (counted) rather than making a client wait.
//  2. A send is fire-and-forget. Failures are logged and counted, never retried:
//     a lost message costs one peer one upstream probe, which is precisely the
//     cost of not having replicated at all.
//  3. A received event is applied to the local cache and *not* re-broadcast. That
//     is structural rather than a rule to remember: [CacheReplication.apply] writes to
//     the cache directly and has no path to the queue, which is the only way in.
//     Re-broadcasting would turn any two instances into a broadcast storm.

// Paths of the replication endpoints. They sit outside /v2/ so they cannot
// collide with a registry endpoint, and under the reserved rules_img namespace so
// no OCI client can reach one by accident — and so a forwarding gateway can refuse
// the whole namespace in one check rather than naming each endpoint.
const (
	// reservedPathPrefix is the gateway's own control surface: never a registry
	// endpoint, never relayed by a forwarder.
	reservedPathPrefix    = "/_rules_img/"
	replicationPathPrefix = reservedPathPrefix + "cache/"
	// replicationEventsPath accepts a batch of cache events from a peer.
	replicationEventsPath = replicationPathPrefix + "events"
	// replicationDonatePath answers with the hottest entries of this instance's
	// cache, or — with limit=0 — with just the headers describing it, which is how
	// a starting instance picks which peer to ask.
	replicationDonatePath = replicationPathPrefix + "donate"
)

// Headers of the replication protocol. They live in the reserved X-rules_img-
// namespace, which copyHeader strips wholesale, so none of them can reach an
// upstream registry.
const (
	// cacheOriginHeader names the instance a replication request came from. An
	// instance that sees its own id refuses the request: a peer list that (through
	// a stale Service endpoint, or a hand-written peer list) contains this
	// instance would otherwise have it talk to itself.
	cacheOriginHeader = "X-rules_img-Cache-Origin"
	// cacheStartedHeader is when the answering instance started serving, in
	// RFC3339. A warming instance prefers the oldest peer it can find: an instance
	// that has been up longer has had longer to fill its cache.
	cacheStartedHeader = "X-rules_img-Cache-Started"
	// cacheEntriesHeader is how many entries the answering instance holds, which
	// breaks a tie between two peers of the same age.
	cacheEntriesHeader = "X-rules_img-Cache-Entries"
)

const (
	// replicationFlushDelay is how long the first event of a batch waits for
	// company. It bounds the latency replication adds to a fact reaching the
	// fleet, and it is short because that is the whole point: a few milliseconds
	// is nothing against the upstream round trip it saves, while being long
	// enough that a push storm's insertions arrive as batches of hundreds rather
	// than one RPC each.
	//
	// The timer starts when an event lands in an empty batch and is never
	// restarted, so a continuous stream of events cannot postpone a flush
	// indefinitely.
	replicationFlushDelay = 5 * time.Millisecond

	// defaultReplicationBatchSize is the default cap on one batch.
	defaultReplicationBatchSize = 256

	// replicationQueueDepth is the queue between the request path and the sender.
	// It absorbs the bursts a fleet-wide push produces; past it, events are
	// dropped rather than slowing a client down.
	replicationQueueDepth = 8192

	// replicationMaxInFlight bounds the batches being sent at once, so a set of
	// unresponsive peers cannot pile up goroutines and bodies. A batch that finds
	// no slot is dropped, like one that fails to send.
	replicationMaxInFlight = 4

	// replicationSendTimeout bounds one batch's round trip to one peer. A peer
	// slower than this is treated as one that did not get the message.
	replicationSendTimeout = 5 * time.Second

	// maxReplicationBatchBytes bounds the body of an inbound batch, so a peer
	// cannot make this instance buffer an arbitrary amount. It leaves ample room
	// for the largest batch this implementation sends.
	maxReplicationBatchBytes = 4 << 20

	// maxConcurrentDonations bounds the donations one instance serves at a time.
	// A deployment scaling from 1 to 50 replicas has 49 instances asking at once,
	// and serving registry traffic matters more than seeding them all instantly:
	// past this, a donation request is refused and the asking instance moves on to
	// another peer (or gives up and warms itself the slow way).
	maxConcurrentDonations = 2

	// donationProbeTimeout and donationTimeout bound picking a donor and reading
	// its answer. Both run only while this instance is warming up, off the request
	// path, inside the overall warm-up budget.
	donationProbeTimeout = 2 * time.Second
	donationTimeout      = 30 * time.Second

	// maxDonorProbes is how many peers a starting instance asks about their age
	// before choosing one to seed from. It is a sample, not a survey: probing all
	// of a 200-replica deployment would cost more than the seeding is worth.
	maxDonorProbes = 8

	// peerPollInterval is how often the warm-up rechecks whether peers are known
	// yet, for the case where discovery has not returned its first answer.
	peerPollInterval = 100 * time.Millisecond
)

// Peer is one gateway instance that cache events are replicated to.
type Peer struct {
	// URL is the peer's base URL: scheme and host only, no trailing slash.
	URL string
	// ID is the peer's instance identity when the source knows it (the pod name,
	// for Kubernetes discovery), and empty otherwise. It is only used to keep this
	// instance from replicating to itself.
	ID string
	// Ready reports whether the peer is serving traffic. A peer that is not — one
	// still warming up, for instance — is still sent events, so that it has them
	// by the time it serves, but is never asked to donate its cache.
	Ready bool
}

// PeerSource supplies the set of peers to replicate to. It is consulted per
// batch, so a set that changes while the process runs — pods coming and going —
// needs no restart. Implementations must be safe for concurrent use.
type PeerSource interface {
	// Peers returns the peers known right now, excluding this instance.
	Peers() []Peer
	// Settled reports whether the source has produced an authoritative answer at
	// least once. It is what lets a starting instance tell "no peers yet" from
	// "no peers": the first replica of a deployment must not spend its whole
	// warm-up budget waiting for peers that do not exist.
	Settled() bool
}

// runnablePeerSource is a [PeerSource] with background work of its own, such as a
// watch against the Kubernetes API.
type runnablePeerSource interface {
	PeerSource
	Run(done <-chan struct{})
}

// StaticPeers is a [PeerSource] over a fixed list of peer base URLs. Every peer is
// assumed ready, and none is identified, so an instance's own URL in the list is
// only caught by the receiving side's origin check.
type StaticPeers []string

func (p StaticPeers) Peers() []Peer {
	peers := make([]Peer, 0, len(p))
	for _, url := range p {
		peers = append(peers, Peer{URL: strings.TrimSuffix(url, "/"), Ready: true})
	}
	return peers
}

func (p StaticPeers) Settled() bool { return true }

// ReplicationConfig configures replication of the blob existence cache. It is
// installed with [WithCacheReplication].
type ReplicationConfig struct {
	// Peers supplies the instances to replicate to. Replication is off when it is
	// nil.
	Peers PeerSource
	// Client sends replication requests to peers. It should be configured with the
	// TLS material and timeouts of the peers' listeners; a nil client disables
	// sending (but not receiving).
	Client *http.Client
	// Credential returns the bearer token to present to a peer, or "" for none. It
	// is called per request so a rotated token — a projected Kubernetes
	// ServiceAccount token, say — is picked up without a restart.
	Credential func(context.Context) (string, error)
	// SelfID identifies this instance to its peers, so that a peer list containing
	// this instance does not make it replicate to itself. Defaults to the hostname,
	// which in Kubernetes is the pod name that endpoint discovery reports.
	SelfID string
	// AllowedPeerIDs restricts which authenticated clients may write to this
	// instance's cache, matched against the client's identity exactly as
	// [PeerAuthOptions.AllowedClientIDs] is. Empty accepts any client the listener
	// authenticated, which is only appropriate when every client of this listener
	// is a peer: a client that can insert entries can make this gateway claim a
	// blob that is not there, and a push client that believes that claim skips an
	// upload it still owes.
	AllowedPeerIDs []string
	// BatchSize caps how many events one message carries. Defaults to
	// [defaultReplicationBatchSize].
	BatchSize int
	// WarmupTimeout bounds how long a starting instance seeds its cache from a
	// peer before reporting itself healthy. Zero disables seeding.
	WarmupTimeout time.Duration
	// WarmupEntries is how many entries it asks a peer for. Zero disables seeding.
	WarmupEntries int
	// Logger records replication. Defaults to the handler's logger.
	Logger *log.Logger
}

// WithCacheReplication replicates what this instance learns about blobs to its
// peers, so that a serving deployment pays for one upstream existence probe per
// blob rather than one per replica.
//
// It has effect only when the blob existence cache is enabled
// ([WithBlobExistenceCache]); with the cache off there is nothing to replicate and
// New says so. Everything it does is best effort: no client request ever waits for
// a peer, and a failed or dropped message costs another instance one upstream
// probe, which is what it would have paid anyway.
//
// The peers must be able to authenticate this instance and vice versa — the
// endpoints sit behind the same client authentication as the registry protocol,
// and [ReplicationConfig.AllowedPeerIDs] is what keeps an ordinary client from
// writing to this instance's cache.
//
// [Handler.RunCacheReplication] must be called for any of it to happen.
func WithCacheReplication(r *CacheReplication) Option {
	return func(h *Handler) { h.replication = r }
}

// CacheReplication is the replication of one instance's blob existence cache to
// its peers: the queue events wait in, the peer set they go to, and the receiving
// end of the protocol. It is created by [NewCacheReplication] and installed with
// [WithCacheReplication].
//
// A nil *CacheReplication is a disabled one: every method tolerates it, so the
// gateway needs no second code path for replication being off.
type CacheReplication struct {
	cache   *blobExistenceCache
	metrics *metrics
	peers   PeerSource
	client  *http.Client

	credential func(context.Context) (string, error)
	selfID     string
	// started is when this instance came up, which it reports to a peer that is
	// choosing whom to seed from.
	started time.Time
	// allowed compiles ReplicationConfig.AllowedPeerIDs. Empty means every
	// authenticated client may replicate.
	allowed []identityPattern

	batchSize     int
	warmupTimeout time.Duration
	warmupEntries int
	log           *log.Logger

	// events carries insertions and deletions from the request path to the sender.
	// It is buffered and never blocks a request: an event that finds it full is
	// dropped.
	events chan cacheEvent
	// inflight is the semaphore bounding concurrent flushes.
	inflight chan struct{}
	// donating bounds the donations served at once, so seeding a fleet of starting
	// replicas cannot crowd out registry traffic.
	donating chan struct{}

	// dropped counts events the queue had no room for, reported so a fleet that
	// has outgrown the queue is visible rather than silently under-replicating.
	dropped atomic.Int64
	// running guards against a second start.
	running atomic.Bool
}

// cacheEvent is one replicated fact about one blob: it is in this repository, or
// it has been deleted from it. Access times and content lengths are deliberately
// absent — the first is local to an instance, and the second is something the
// receiver may already know better (see [blobExistenceCache.insert]).
type cacheEvent struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	Digest     string `json:"digest"`
	// Deleted marks a blob a client deleted through the sending gateway, rather
	// than one it discovered.
	Deleted bool `json:"deleted,omitempty"`
}

// cacheEventBatch is the body of a POST to [replicationEventsPath].
type cacheEventBatch struct {
	Events []cacheEvent `json:"events"`
}

// donatedEntry is one entry of a donation. Unlike a broadcast event it carries
// the size and the remaining lifetime: a donation copies a page of an existing
// cache rather than reporting something just observed, so re-basing the deadlines
// on receipt would let a fact outlive the TTL that bounds it.
type donatedEntry struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	Digest     string `json:"digest"`
	// Size is the Content-Length the donor holds for the blob, or -1 when it has
	// none.
	Size int64 `json:"size"`
	// TTLMillis is what is left of the donor's deadline for the entry.
	TTLMillis int64 `json:"ttlMs"`
}

// NewCacheReplication validates cfg and returns the replication to install with
// [WithCacheReplication]. A peer allow-list entry that is not a valid identity
// pattern is a startup error, like the client allow-list it mirrors.
func NewCacheReplication(cfg ReplicationConfig) (*CacheReplication, error) {
	if cfg.Peers == nil {
		return nil, errors.New("cache replication needs a source of peers")
	}
	r := &CacheReplication{
		peers:         cfg.Peers,
		client:        cfg.Client,
		credential:    cfg.Credential,
		selfID:        cfg.SelfID,
		started:       time.Now(),
		batchSize:     cfg.BatchSize,
		warmupTimeout: cfg.WarmupTimeout,
		warmupEntries: cfg.WarmupEntries,
		log:           cfg.Logger,
		events:        make(chan cacheEvent, replicationQueueDepth),
		inflight:      make(chan struct{}, replicationMaxInFlight),
		donating:      make(chan struct{}, maxConcurrentDonations),
	}
	if r.log == nil {
		r.log = log.Default()
	}
	if r.batchSize <= 0 {
		r.batchSize = defaultReplicationBatchSize
	}
	if r.selfID == "" {
		r.selfID, _ = os.Hostname()
	}
	if r.selfID == "" {
		return nil, errors.New("cache replication needs an id for this instance and the hostname is unreadable")
	}
	for _, id := range cfg.AllowedPeerIDs {
		pattern, err := compileIdentityPattern(id)
		if err != nil {
			return nil, err
		}
		r.allowed = append(r.allowed, pattern)
	}
	return r, nil
}

// bind hands the replication the cache it replicates and the instruments it
// reports through, both of which [New] builds. It is the last step of
// construction, before any goroutine exists.
func (r *CacheReplication) bind(cache *blobExistenceCache, metrics *metrics) {
	r.cache = cache
	r.metrics = metrics
}

// record queues one fact for replication. It is called from the request path and
// must never block: the event is offered to the queue and dropped if there is no
// room, because a client waiting on a peer is a worse outcome than a peer paying
// for one probe.
func (r *CacheReplication) record(ctx context.Context, registry, repository, digest string, deleted bool) {
	if r == nil {
		return
	}
	select {
	case r.events <- cacheEvent{Registry: registry, Repository: repository, Digest: digest, Deleted: deleted}:
	default:
		r.dropped.Add(1)
		r.metrics.recordReplicationDropped(ctx, 1)
	}
}

// run starts replication and blocks until done is closed: the peer source's own
// background work, the warm-up, and the batching loop that is this goroutine.
//
// warmedUp is called when the warm-up finishes, however it finishes, which is what
// lets this instance report itself healthy.
func (r *CacheReplication) run(done <-chan struct{}, warmedUp func()) {
	if r == nil {
		return
	}
	if !r.running.CompareAndSwap(false, true) {
		// Two batchers would each take a share of the queue and send half the facts
		// each, which is not wrong but is certainly not intended.
		r.log.Printf("cache replication is already running; ignoring a second start")
		return
	}
	if source, ok := r.peers.(runnablePeerSource); ok {
		go source.Run(done)
	}
	go func() {
		defer warmedUp()
		r.warmUp(done)
	}()
	r.batch(done)
}

// batch accumulates events into messages and hands each finished one to the
// sender.
//
// The first event of a batch starts the flush timer, later events join the batch,
// and it goes out when the timer expires or the batch is full — whichever comes
// first. The timer is deliberately not restarted by a later event: a sustained
// stream of insertions would otherwise keep postponing the flush, and the latency
// the bound is about is that of the *first* event in the batch.
//
// Events still queued when done is closed are not sent. A shutdown that raced a
// handful of insertions costs the peers the probes they would have paid without
// replication at all, and a flush that outlived the process would have nowhere to
// report its failure.
func (r *CacheReplication) batch(done <-chan struct{}) {
	for {
		pending := make([]cacheEvent, 0, r.batchSize)
		select {
		case <-done:
			return
		case event := <-r.events:
			pending = append(pending, event)
		}
		deadline := time.After(replicationFlushDelay)
	fill:
		for len(pending) < r.batchSize {
			select {
			case <-done:
				return
			case event := <-r.events:
				pending = append(pending, event)
			case <-deadline:
				break fill
			}
		}
		r.flush(pending)
	}
}

// flush sends one batch to every peer, concurrently, without waiting for any of
// them. The batch is encoded once and shared by the sends.
func (r *CacheReplication) flush(events []cacheEvent) {
	events = dedupeEvents(events)
	if len(events) == 0 {
		return
	}
	peers := r.peers.Peers()
	if len(peers) == 0 || r.client == nil {
		return
	}
	body, err := json.Marshal(cacheEventBatch{Events: events})
	if err != nil {
		// Only reachable for a value JSON cannot represent, which these are not.
		r.log.Printf("cache replication: encoding a batch of %d events failed: %v", len(events), err)
		return
	}
	select {
	case r.inflight <- struct{}{}:
	default:
		// Every send slot is busy, which means the peers are not keeping up. Drop
		// this batch instead of queueing work behind them.
		r.dropped.Add(int64(len(events)))
		r.metrics.recordReplicationDropped(context.Background(), len(events))
		return
	}
	go func() {
		defer func() { <-r.inflight }()
		var wg sync.WaitGroup
		for _, peer := range peers {
			wg.Add(1)
			go func(peer Peer) {
				defer wg.Done()
				r.send(peer, body, events)
			}(peer)
		}
		wg.Wait()
	}()
}

// eventKey identifies the blob an event is about, which is everything about it
// except whether it was found or deleted.
type eventKey struct{ registry, repository, digest string }

// dedupeEvents keeps only the last event for each blob in a batch.
//
// The last is the one that is true: an insertion followed by a deletion of the
// same blob must reach a peer as a deletion. Everything before it is redundant —
// two identical insertions teach a peer exactly what one does — and a fleet
// reading the same layer hundreds of times inside one flush window produces
// exactly that.
func dedupeEvents(events []cacheEvent) []cacheEvent {
	if len(events) < 2 {
		return events
	}
	lastAt := make(map[eventKey]int, len(events))
	for i, event := range events {
		lastAt[eventKey{event.Registry, event.Repository, event.Digest}] = i
	}
	if len(lastAt) == len(events) {
		return events
	}
	deduped := make([]cacheEvent, 0, len(lastAt))
	for i, event := range events {
		if lastAt[eventKey{event.Registry, event.Repository, event.Digest}] == i {
			deduped = append(deduped, event)
		}
	}
	return deduped
}

// send posts one batch to one peer. A failure is logged and counted; there is no
// retry, because the cost of a lost message is that the peer pays for one probe
// it could have skipped.
func (r *CacheReplication) send(peer Peer, body []byte, events []cacheEvent) {
	if peer.ID != "" && peer.ID == r.selfID {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), replicationSendTimeout)
	defer cancel()

	err := r.post(ctx, peer, body)
	r.metrics.recordReplicationSent(ctx, events, err)
	if err != nil {
		// One line per failed batch, at the rate batches are produced. A peer that
		// is down stays visible in the metric.
		r.log.Printf("cache replication: sending %d events to %s failed: %v", len(events), peer.URL, err)
	}
}

func (r *CacheReplication) post(ctx context.Context, peer Peer, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, peer.URL+replicationEventsPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := r.authorize(ctx, req); err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Read the (empty) body so the connection can be reused for the next batch.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peer answered %d", resp.StatusCode)
	}
	return nil
}

// authorize adds this instance's identity and credential to a request to a peer.
func (r *CacheReplication) authorize(ctx context.Context, req *http.Request) error {
	req.Header.Set(cacheOriginHeader, r.selfID)
	if r.credential == nil {
		return nil
	}
	token, err := r.credential(ctx)
	if err != nil {
		return fmt.Errorf("reading the peer credential: %w", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}

// apply writes received events to the local cache. It is the receiving end of the
// protocol and, by construction, has no path back to the outbound queue: an event
// this instance was told about is never re-broadcast, which is what keeps two
// instances from feeding each other the same fact forever.
func (r *CacheReplication) apply(events []cacheEvent) (inserts, deletes int) {
	if r.cache == nil {
		return 0, 0
	}
	for _, event := range events {
		if event.Registry == "" || event.Repository == "" || event.Digest == "" {
			continue
		}
		if !digestRe.MatchString(event.Digest) {
			// The local paths only ever cache a well-formed digest; hold a peer to
			// the same rule rather than letting it put anything in the key space.
			continue
		}
		if event.Deleted {
			r.cache.forget(event.Registry, event.Repository, event.Digest)
			deletes++
			continue
		}
		// Stored exactly as a local insertion would be: this instance's clock, this
		// instance's TTL, and the head of its own LRU list. No size is claimed —
		// the sender's proof of existence says nothing about what this instance may
		// already know about the blob's length.
		r.cache.insert(event.Registry, event.Repository, event.Digest, -1, r.cache.ttl)
		inserts++
	}
	return inserts, deletes
}

// allowsPeer reports whether a client authenticated as principal may write to
// this instance's cache.
//
// With no allow-list configured, every client the listener authenticated is
// accepted — including, on an anonymous listener, an unauthenticated one. That is
// deliberate but load-bearing: a false "this blob exists" makes a push client skip
// an upload it still owes, so a listener whose clients are not all peers wants
// [ReplicationConfig.AllowedPeerIDs] set. The startup banner says so.
func (r *CacheReplication) allowsPeer(principal string) bool {
	if len(r.allowed) == 0 {
		return true
	}
	identity, ok := principalIdentity(principal)
	if !ok {
		return false
	}
	for _, pattern := range r.allowed {
		if pattern.matches(identity) {
			return true
		}
	}
	return false
}

// principalIdentity extracts the identity part of an [observation.principal] —
// the certificate identity or the ServiceAccount username — so that a peer
// allow-list is written in the same terms as --allowed-client-id and
// --allowed-serviceaccount. A static shared token authenticates no identity at
// all, and so never matches an allow-list.
func principalIdentity(principal string) (string, bool) {
	for _, prefix := range []string{"cert:", "serviceaccount:"} {
		if identity, ok := strings.CutPrefix(principal, prefix); ok && identity != "" {
			return identity, true
		}
	}
	return "", false
}

// SeparateReplicationHandler splits cache replication off onto a listener of its
// own and returns the [http.Handler] for it, or nil when this gateway does not
// replicate.
//
// It exists for the deployment whose two audiences want two different transports:
// build clients reaching the gateway over plaintext HTTP with no credential of
// their own, while the instances of the deployment authenticate each other with
// mTLS. Those cannot be the same socket — a listener either requires a client
// certificate or it does not — so the peer traffic gets a second one, with its own
// TLS material, its own client authentication, and nothing on it but the
// replication endpoints and the health probe.
//
// Once it is called the registry-protocol listener answers /_rules_img/cache/ with
// 404, so the write path into this instance's blob existence cache exists only on
// the peer listener. That is the whole point of separating them: an anonymous
// client that could insert facts could make a push client skip an upload it still
// owes.
//
// peerAuth authenticates the peers. It may be nil, which is only appropriate when
// the peer listener is unreachable except through something that authenticates it
// (a service mesh, or a loopback address); the caller is expected to say so out
// loud.
func (h *Handler) SeparateReplicationHandler(peerAuth *PeerAuth) http.Handler {
	if h.replication == nil {
		return nil
	}
	h.replicationSeparate.Store(true)
	return &replicationHandler{gateway: h, peerAuth: peerAuth}
}

// replicationHandler serves the replication endpoints (and the health probe) on a
// listener of their own. It is what [Handler.SeparateReplicationHandler] returns.
type replicationHandler struct {
	gateway  *Handler
	peerAuth *PeerAuth
}

// ServeHTTP implements [http.Handler]. It is deliberately a closed surface: the
// health probe, the two replication endpoints, and 404 for everything else. None
// of the registry protocol is reachable here, so a peer credential cannot be spent
// on an upstream registry.
func (rh *replicationHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := rh.gateway
	obs, w, r := h.metrics.begin(w, r)
	defer obs.finish(r.Context())

	path := r.URL.EscapedPath()
	if path == healthPath {
		h.serveHealth(w)
		return
	}
	if rh.peerAuth != nil {
		principal, err := rh.peerAuth.Authenticate(r)
		if err != nil {
			h.writePeerAuthError(obs, w, r, err)
			return
		}
		obs.principal = principal
	}
	if !strings.HasPrefix(path, replicationPathPrefix) {
		h.writeError(obs, w, r, http.StatusNotFound, "UNSUPPORTED", errUnsupportedEndpoint,
			"this listener serves blob existence cache replication only")
		return
	}
	h.serveCacheReplication(obs, w, r, path)
}

// serveCacheReplication answers the two replication endpoints, or explains why it
// will not.
//
// Three gates come before anything is written to the cache, in this order:
//
//  1. Replication must be configured. Without it the endpoints do not exist, so a
//     gateway that does not replicate cannot be talked into holding facts it never
//     verified.
//  2. The client must be a peer. The listener has already authenticated it — this
//     is the same surface the registry protocol is served on — but authenticating
//     a client says only that it may *use* the gateway. Writing to the existence
//     cache is a different power: a false "this blob is here" makes a push client
//     skip an upload it still owes, so an allow-list of peer identities is what
//     separates the two (see [CacheReplication.allowsPeer]).
//  3. The request must not be this instance's own. A peer list containing this
//     instance — a stale endpoint, a hand-written flag — would otherwise have it
//     round-trip its own events; answering 409 makes that visible in the sender's
//     log and error metric rather than wasting quiet effort.
//
// The policy is deliberately not consulted: a replication request names no
// repository to authorize, and the facts it carries were authorized when the peer
// that observed them served the request they came from.
func (h *Handler) serveCacheReplication(obs *observation, w http.ResponseWriter, r *http.Request, path string) {
	op, route := replicationEndpoint(path)
	obs.setUpstream("", request{op: op, route: route})

	rep := h.replication
	if rep == nil || route == "" {
		h.writeError(obs, w, r, http.StatusNotFound, "UNSUPPORTED", errUnsupportedEndpoint,
			"this gateway does not replicate its blob existence cache")
		return
	}
	if !rep.allowsPeer(obs.principal) {
		h.writeError(obs, w, r, http.StatusForbidden, "DENIED", errPeerIdentityDenied,
			"this gateway does not accept cache replication from the presented client identity")
		return
	}
	if r.Header.Get(cacheOriginHeader) == rep.selfID {
		h.writeError(obs, w, r, http.StatusConflict, "UNSUPPORTED", errCacheSelfReplication,
			"this gateway is the sender of that request: remove it from its own peer list")
		return
	}
	switch path {
	case replicationEventsPath:
		rep.serveEvents(w, r)
	case replicationDonatePath:
		rep.serveDonation(w, r)
	}
	h.log.Printf("%s %q (%s%s) -> %d", r.Method, path, op, obs.logContext(), obs.w.statusCode())
}

// replicationEndpoint maps a replication path to its metric operation and route,
// or to empty strings when the path is not one of the endpoints.
func replicationEndpoint(path string) (op, route string) {
	switch path {
	case replicationEventsPath:
		return opNameCacheEvents, replicationEventsPath
	case replicationDonatePath:
		return opNameCacheDonate, replicationDonatePath
	default:
		return opNameUnknown, ""
	}
}

// serveEvents applies a batch a peer sent.
//
// It answers 204 as soon as the events are in the cache. Nothing about the answer
// is load-bearing for the sender — it does not retry — so this stays a plain
// status rather than a report of what was applied.
func (r *CacheReplication) serveEvents(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeOCIError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "cache events are posted")
		return
	}
	var batch cacheEventBatch
	if err := json.NewDecoder(io.LimitReader(req.Body, maxReplicationBatchBytes)).Decode(&batch); err != nil {
		writeOCIError(w, http.StatusBadRequest, "UNSUPPORTED", "malformed cache event batch")
		return
	}
	inserts, deletes := r.apply(batch.Events)
	r.metrics.recordReplicationReceived(req.Context(), inserts, deletes)
	w.WriteHeader(http.StatusNoContent)
}

// serveDonation hands a starting peer the hottest entries of this instance's
// cache.
//
// The snapshot is taken first and streamed afterwards, so a slow reader holds no
// lock (see [blobExistenceCache.hottest]), and the number of donations in flight
// is bounded: an instance donating is an instance serving, and registry traffic
// comes first. A limit of zero asks for no entries at all, which is how a peer
// reads the age and occupancy headers to decide whom to ask.
func (r *CacheReplication) serveDonation(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeOCIError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "a cache donation is fetched")
		return
	}
	limit, err := donationLimit(req.URL.Query().Get("limit"))
	if err != nil {
		writeOCIError(w, http.StatusBadRequest, "UNSUPPORTED", err.Error())
		return
	}
	w.Header().Set(cacheStartedHeader, r.started.UTC().Format(time.RFC3339))
	w.Header().Set(cacheEntriesHeader, strconv.FormatInt(r.cache.stats().entries, 10))
	if limit == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}
	select {
	case r.donating <- struct{}{}:
		defer func() { <-r.donating }()
	default:
		// Already donating to as many peers as this instance will at once. The
		// asker moves on to another peer, or warms itself up the slow way.
		w.Header().Set("Retry-After", "1")
		writeOCIError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "already donating cache entries to other peers")
		return
	}
	entries := r.cache.hottest(limit)
	w.Header().Set("Content-Type", "application/jsonl")
	w.WriteHeader(http.StatusOK)
	// One JSON object per entry, streamed: a donation of tens of thousands of
	// entries is written and read incrementally rather than buffered whole on
	// either side.
	encoder := json.NewEncoder(w)
	for _, entry := range entries {
		if err := encoder.Encode(donatedEntry{
			Registry:   entry.registry,
			Repository: entry.repository,
			Digest:     entry.digest,
			Size:       entry.contentLength,
			TTLMillis:  entry.lifetime.Milliseconds(),
		}); err != nil {
			// The asker went away mid-stream, which costs it nothing but a slower
			// start.
			return
		}
	}
}

// donationLimit parses the limit query parameter of a donation request. A limit
// above what one snapshot may carry is not an error: the cache applies that
// ceiling itself (see [maxSnapshotEntries]), so a peer asking for more simply gets
// what there is.
func donationLimit(value string) (int, error) {
	if value == "" {
		return maxSnapshotEntries, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 0 {
		return 0, errors.New("limit must be a non-negative number of entries")
	}
	return min(limit, maxSnapshotEntries), nil
}

// warmUp seeds this instance's cache from a peer before it reports itself
// healthy.
//
// A replica that joins with an empty cache sends the fleet's whole working set
// upstream again: every probe it serves is a miss until it has seen the blob
// itself. Asking a running peer for its hottest entries turns that into one
// request. It is bounded on both sides — the warm-up gives up at
// [ReplicationConfig.WarmupTimeout] and reports healthy anyway, and a donor
// refuses when it is already donating to others — because a slow start is a much
// smaller problem than a replica that never becomes ready.
//
// Events broadcast by peers are already being applied while this runs: the
// listener is open before warm-up begins, so a fact learned by the fleet during
// the warm-up is not lost.
func (r *CacheReplication) warmUp(done <-chan struct{}) {
	if r.warmupTimeout <= 0 || r.warmupEntries <= 0 || r.client == nil || r.cache == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.warmupTimeout)
	defer cancel()
	go func() {
		select {
		case <-done:
			cancel()
		case <-ctx.Done():
		}
	}()

	started := time.Now()
	candidates := r.waitForDonors(ctx)
	if len(candidates) == 0 {
		r.log.Printf("blob existence cache: no peer to warm up from, serving with an empty cache")
		return
	}
	for _, peer := range r.rankDonors(ctx, candidates) {
		entries, err := r.requestDonation(ctx, peer)
		if err != nil {
			r.log.Printf("blob existence cache: warming up from %s failed: %v", peer.URL, err)
			if ctx.Err() != nil {
				return
			}
			continue
		}
		r.metrics.recordWarmup(ctx, entries)
		r.log.Printf("blob existence cache: warmed up with %d entries from %s in %v", entries, peer.URL, time.Since(started).Round(time.Millisecond))
		return
	}
	r.log.Printf("blob existence cache: no peer donated entries, serving with an empty cache")
}

// waitForDonors waits until the peer source can say who this instance's peers
// are, and returns the ones that are serving.
//
// The wait exists for discovery that has not answered yet — a Kubernetes watch
// takes a moment to list. Once the source has answered, an empty result is an
// answer: the first replica of a deployment reports healthy immediately rather
// than spending its whole budget waiting for peers that do not exist.
func (r *CacheReplication) waitForDonors(ctx context.Context) []Peer {
	ticker := time.NewTicker(peerPollInterval)
	defer ticker.Stop()
	for {
		var ready []Peer
		for _, peer := range r.peers.Peers() {
			if peer.Ready && (peer.ID == "" || peer.ID != r.selfID) {
				ready = append(ready, peer)
			}
		}
		if len(ready) > 0 || r.peers.Settled() {
			return ready
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// rankDonors picks the order in which peers are asked to donate: oldest first.
//
// An instance that has been up longer has had longer to fill its cache, so it is
// the better seed — and the age is data this instance has to ask for, which it
// does with a limit=0 request that returns headers only. The candidates are
// shuffled first, so that when the ages tie (a whole deployment started together)
// the load of donating is spread rather than always landing on the same peer.
// Peers that do not answer the probe are kept, at the back: an unanswered probe is
// not proof that the donation would fail.
//
// At most [maxDonorProbes] peers are considered at all. Probing a 200-replica
// deployment to seed one instance would cost more than the seeding is worth, and
// one donor is all that is needed.
func (r *CacheReplication) rankDonors(ctx context.Context, candidates []Peer) []Peer {
	rand.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	probed := candidates
	if len(probed) > maxDonorProbes {
		probed = probed[:maxDonorProbes]
	}

	type ranked struct {
		peer    Peer
		started time.Time
		entries int64
		ok      bool
	}
	results := make([]ranked, len(probed))
	var wg sync.WaitGroup
	for i, peer := range probed {
		wg.Add(1)
		go func(i int, peer Peer) {
			defer wg.Done()
			results[i] = ranked{peer: peer}
			started, entries, err := r.probeDonor(ctx, peer)
			if err != nil {
				return
			}
			results[i].started, results[i].entries, results[i].ok = started, entries, true
		}(i, peer)
	}
	wg.Wait()

	slices.SortStableFunc(results, func(a, b ranked) int {
		switch {
		case a.ok != b.ok:
			// An answered probe beats an unanswered one.
			if a.ok {
				return -1
			}
			return 1
		case !a.ok:
			return 0
		case !a.started.Equal(b.started):
			// Older first.
			return a.started.Compare(b.started)
		default:
			// Same age: the fuller cache is the better seed.
			return cmp.Compare(b.entries, a.entries)
		}
	})
	order := make([]Peer, 0, len(results))
	for _, result := range results {
		order = append(order, result.peer)
	}
	return order
}

// probeDonor asks a peer how long it has been up and how much it holds, without
// asking it for any entries.
func (r *CacheReplication) probeDonor(ctx context.Context, peer Peer) (started time.Time, entries int64, err error) {
	ctx, cancel := context.WithTimeout(ctx, donationProbeTimeout)
	defer cancel()
	resp, err := r.donationRequest(ctx, peer, 0)
	if err != nil {
		return time.Time{}, 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	started, err = time.Parse(time.RFC3339, resp.Header.Get(cacheStartedHeader))
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("peer reported no usable start time: %w", err)
	}
	entries, _ = strconv.ParseInt(resp.Header.Get(cacheEntriesHeader), 10, 64)
	return started, entries, nil
}

// requestDonation asks one peer for its hottest entries and inserts them into the
// local cache as they arrive, so that this instance is already answering probes
// from the first part of the donation while the rest is still streaming.
func (r *CacheReplication) requestDonation(ctx context.Context, peer Peer) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, donationTimeout)
	defer cancel()
	resp, err := r.donationRequest(ctx, peer, r.warmupEntries)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	inserted := 0
	// The body is a stream of JSON objects, bounded so a peer cannot make a
	// starting instance read forever.
	decoder := json.NewDecoder(io.LimitReader(resp.Body, int64(r.warmupEntries)*maxDonatedEntryBytes))
	for {
		var entry donatedEntry
		if err := decoder.Decode(&entry); err != nil {
			if errors.Is(err, io.EOF) {
				return inserted, nil
			}
			// A truncated donation is still a useful one: keep what arrived.
			return inserted, fmt.Errorf("reading donated entries after %d: %w", inserted, err)
		}
		if entry.Registry == "" || entry.Repository == "" || !digestRe.MatchString(entry.Digest) {
			continue
		}
		lifetime := time.Duration(entry.TTLMillis) * time.Millisecond
		if lifetime <= 0 {
			continue
		}
		// The donor's remaining lifetime, capped by this instance's own TTL: a
		// deadline set by a peer configured to remember blobs for longer must not
		// override what this instance was told to believe.
		r.cache.insert(entry.Registry, entry.Repository, entry.Digest, entry.Size, min(lifetime, r.cache.ttl))
		inserted++
	}
}

// maxDonatedEntryBytes bounds the encoded size of one donated entry, so that the
// body of a donation is bounded by the number of entries that were asked for.
// entryBytes bounds the key, and the rest of the line is fixed-width JSON.
const maxDonatedEntryBytes = entryBytes + 256

// donationRequest performs one GET against a peer's donation endpoint.
func (r *CacheReplication) donationRequest(ctx context.Context, peer Peer, limit int) (*http.Response, error) {
	url := peer.URL + replicationDonatePath + "?limit=" + strconv.Itoa(limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if err := r.authorize(ctx, req); err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		return nil, fmt.Errorf("peer answered %d", resp.StatusCode)
	}
	return resp, nil
}

// peerCount reports how many peers this instance would replicate to, for the
// metric callback.
func (r *CacheReplication) peerCount() int64 {
	if r == nil {
		return 0
	}
	return int64(len(r.peers.Peers()))
}

// summary describes the configured replication for the startup banner.
func (r *CacheReplication) summary() string {
	peers := "peers discovered dynamically"
	if static, ok := r.peers.(StaticPeers); ok {
		peers = fmt.Sprintf("%d static peer(s)", len(static))
	}
	gate := fmt.Sprintf("%d allowed peer identity(ies)", len(r.allowed))
	if len(r.allowed) == 0 {
		gate = "any authenticated client may write to this cache"
	}
	return fmt.Sprintf("%s, batches of up to %d every %v, %s", peers, r.batchSize, replicationFlushDelay, gate)
}
