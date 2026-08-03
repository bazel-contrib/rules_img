// Package gateway implements an OCI distribution gateway: an HTTP handler that
// speaks the container registry (Docker Registry v2 / OCI Distribution) protocol
// but only forwards requests to real upstream registries.
//
// Clients connect anonymously and must set the X-rules_img-Original-Host header
// (see [github.com/bazel-contrib/rules_img/img_tool/pkg/gateway.OriginalHostHeader])
// to tell the gateway which upstream registry they want to reach. The gateway
// authenticates to that upstream itself using the crane keychain + token
// exchange flow, and authorizes every request against a [CompiledPolicy]: an
// ordered list of allow/deny rules matched on the resolved registry host,
// repository path, and operation. The policy is reloadable at runtime.
//
// Requests, transferred bytes, blob transfers, existence-check hit rates, and
// errors are reported as OpenTelemetry metrics through the [metric.MeterProvider]
// installed with [WithMeterProvider] (see metrics.go for the instruments).
//
// Successful blob existence checks can be memoized with
// [WithBlobExistenceCache], which is what keeps a build farm's repeated "is this
// layer already pushed?" probes from each costing an upstream round trip. Every
// other request that settles whether a blob is in a repository keeps that memo
// current: a read that serves the blob and an upload that commits it admit an entry,
// and a delete takes one away (see existencecache.go). With
// [WithCacheReplication], what one instance learns is broadcast to its peers, so a
// deployment of several replicas pays for one upstream probe per blob rather than
// one per replica (see replication.go).
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"go.opentelemetry.io/otel/metric"

	clientgateway "github.com/bazel-contrib/rules_img/img_tool/pkg/gateway"
)

// authHandshakeTimeout bounds the initial per-upstream ping + token-exchange
// handshake, which is performed once per repository+scope and cached.
const authHandshakeTimeout = 2 * time.Minute

// Headers the gateway sets on the hop between two gateways. They live in the
// reserved X-rules_img- namespace, which [copyHeader] strips wholesale, so none
// of them can reach an upstream registry and a client cannot forge one.
const (
	// forwardedByHeader names the forwarding gateway a request came through.
	forwardedByHeader = "X-rules_img-Forwarded-By"
	// gatewayErrorHeader distinguishes a gateway's own authentication failure
	// from the identical status code an upstream registry would return, so the
	// forwarding side can report the real cause instead of sending its client
	// hunting for registry credentials.
	gatewayErrorHeader = "X-rules_img-Gateway-Error"
	// requestIDHeader correlates the two hops in the decision log. Without it
	// there is no way to tie a build action's request to the shared gateway's
	// authorization decision.
	requestIDHeader = "X-rules_img-Request-Id"
	// reservedHeaderPrefix is the canonical form of the namespace above.
	reservedHeaderPrefix = "X-Rules_img-"
)

// healthPath is answered before authentication with a bare 200, so a Kubernetes
// readiness probe works against a listener that requires a credential. It is
// deliberately unauthenticated and reveals nothing beyond "a gateway is here":
// the /v2/ endpoint cannot serve this purpose, since it needs both the upstream
// host header and a credential.
const healthPath = "/healthz"

// Handler is an [http.Handler] that forwards registry requests to upstream
// registries, subject to a [CompiledPolicy].
type Handler struct {
	// policy holds the active policy, swapped atomically by Reload.
	policy          atomic.Pointer[CompiledPolicy]
	keychain        authn.Keychain
	base            http.RoundTripper
	defaultRegistry string
	log             *log.Logger

	// peerAuth authenticates the gateway's clients. Nil means clients connect
	// anonymously, which is only safe on a UNIX socket or loopback.
	peerAuth *PeerAuth

	// explicitPolicy is set by WithAuthorizer while options are applied and
	// installed by New; when nil, New falls back to a fail-closed deny-all
	// policy.
	explicitPolicy *CompiledPolicy

	// meterProvider is set by WithMeterProvider; nil means the global one.
	meterProvider metric.MeterProvider
	metrics       *metrics

	// blobCacheTTL and blobCacheMaxBytes are set by WithBlobExistenceCache and
	// consumed by New, which builds blobCache from them. blobCache is nil when
	// the cache is disabled, which every one of its methods tolerates.
	blobCacheTTL      time.Duration
	blobCacheMaxBytes int64
	blobCache         *blobExistenceCache

	// replication replicates what this instance learns to its peers. It is nil
	// when replication is off, which every one of its methods tolerates.
	replication *CacheReplication

	// replicationSeparate reports that replication is served on a listener of its
	// own, so this handler refuses the replication endpoints outright. It is set
	// by [Handler.SeparateReplicationHandler] during startup, and read on the
	// request path.
	replicationSeparate atomic.Bool

	// warming reports that this instance is still seeding its cache from a peer
	// and should be kept out of the load balancer until it is not — see
	// [Handler.warmingUp]. warmingUntil is the deadline past which it reports
	// itself healthy regardless.
	warming      atomic.Bool
	warmingUntil time.Time
	// now is the clock, replaced in tests.
	now func() time.Time

	cache authCache
}

// Option configures a [Handler].
type Option func(*Handler)

// WithAuthorizer installs the policy the gateway enforces (for example a
// [CompiledPolicy] loaded from a file with [LoadPolicyFile], or [AllowAll]). If
// no authorizer is supplied, the gateway denies every request.
func WithAuthorizer(p *CompiledPolicy) Option {
	return func(h *Handler) { h.explicitPolicy = p }
}

// WithKeychain sets the keychain used to resolve upstream credentials.
func WithKeychain(kc authn.Keychain) Option {
	return func(h *Handler) { h.keychain = kc }
}

// WithBaseTransport sets the transport used for outgoing upstream requests
// (before auth wrapping). Defaults to a clone of [http.DefaultTransport].
func WithBaseTransport(rt http.RoundTripper) Option {
	return func(h *Handler) { h.base = rt }
}

// WithLogger sets the logger used to record forwarded requests.
func WithLogger(l *log.Logger) Option {
	return func(h *Handler) { h.log = l }
}

// WithDefaultRegistry sets a fallback upstream registry used when a request does
// not carry the X-rules_img-Original-Host header. The default registry is still
// subject to the policy like any other upstream. An empty value keeps the header
// mandatory.
func WithDefaultRegistry(registry string) Option {
	return func(h *Handler) { h.defaultRegistry = registry }
}

// WithMeterProvider sets the OpenTelemetry meter provider the gateway records
// its metrics with. It defaults to the global provider, which discards
// everything until a binary installs an SDK, so instrumentation is inert unless
// an exporter is configured.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(h *Handler) { h.meterProvider = mp }
}

// WithPeerAuth installs client authentication. Every request on the listener,
// including the /v2/ version check, must then present an accepted credential.
// Without it clients connect anonymously, which is only safe on a UNIX socket or
// a loopback address — a gateway reachable over the network holds credentials
// that any pod able to route to it would otherwise be able to spend.
func WithPeerAuth(auth *PeerAuth) Option {
	return func(h *Handler) { h.peerAuth = auth }
}

// WithBlobExistenceCache memoizes the successful answers to blob existence
// checks (HEAD /v2/<name>/blobs/<digest>), so that a fleet asking the same
// question about the same layer pays for one upstream round trip instead of
// thousands. Entries are keyed by upstream registry, repository, and digest, and
// held for ttl.
//
// A blob read that the registry serves under the digest asked for is admitted too,
// as is an upload that the gateway forwards to a successful commit: the registry has
// just hashed that content against the digest, so a probe a moment later would answer
// 200 and the client that pushed spares every other client the first probe. Only the
// response that completes an upload counts, never the session it is the end of. A
// blob delete forwarded the other way drops the entry again.
//
// maxBytes bounds the cache's memory, which is allocated in full at startup and
// never grows; once it is full, the least recently used entry makes room for a
// new one. A ttl or maxBytes that is not positive disables the cache, as does a
// maxBytes below [MinBlobExistenceCacheBytes].
//
// ttl is what stands between a client and the one stale answer the gateway cannot
// see coming: a blob cannot change, but a registry can garbage-collect one behind
// the gateway's back, and a push client that believes a collected layer is still
// there will skip re-uploading it and commit a manifest referring to nothing. Keep
// ttl well inside the window in which the registry could collect a blob. A blob a
// client deletes through the gateway needs no such window — that entry goes at once.
//
// Manifests and tags are never cached: both are mutable, and a HEAD on one is
// exactly how a client finds out that it changed.
func WithBlobExistenceCache(ttl time.Duration, maxBytes int64) Option {
	return func(h *Handler) {
		h.blobCacheTTL = ttl
		h.blobCacheMaxBytes = maxBytes
	}
}

// New constructs a gateway [Handler].
func New(opts ...Option) *Handler {
	h := &Handler{
		keychain: authn.DefaultKeychain,
		base:     defaultBaseTransport(),
		log:      log.New(os.Stderr, "", log.LstdFlags),
		now:      time.Now,
	}
	for _, o := range opts {
		o(h)
	}
	policy := h.explicitPolicy
	if policy == nil {
		// Fail closed: an unconfigured gateway denies everything.
		policy = &CompiledPolicy{}
	}
	h.policy.Store(policy)
	h.cache.inner = make(map[string]*authEntry)
	h.blobCache = newBlobExistenceCache(h.blobCacheTTL, h.blobCacheMaxBytes)
	sources := gaugeSources{
		policyRules: func() int64 {
			if p := h.policy.Load(); p != nil {
				return int64(p.RuleCount())
			}
			return 0
		},
	}
	if h.blobCache != nil {
		sources.blobCache = h.blobCache.stats
		h.log.Printf("blob existence cache enabled: up to %d blobs over %d shards, each assumed present for %v",
			h.blobCache.capacity, len(h.blobCache.shards), h.blobCacheTTL)
	}
	if h.replication != nil && h.blobCache == nil {
		// Nothing to fill or to hand out. Say so rather than starting a replication
		// that could only ever send an empty batch.
		h.log.Printf("warning: cache replication is configured but the blob existence cache is disabled; not replicating")
		h.replication = nil
	}
	if h.replication != nil {
		sources.cachePeers = h.replication.peerCount
		// Report this instance as warming up from the very first probe, so a
		// readiness check cannot see it as healthy before it has had its chance to
		// seed. The deadline is authoritative: the flag being stuck for any reason
		// still cannot keep the instance out of service past the warm-up budget.
		if h.replication.warmupTimeout > 0 && h.replication.warmupEntries > 0 {
			h.warming.Store(true)
			h.warmingUntil = h.now().Add(h.replication.warmupTimeout)
		}
	}
	m, err := newMetrics(h.meterProvider, sources)
	if err != nil {
		// Instruments are still usable (no-ops at worst); serving matters more
		// than measuring it.
		h.log.Printf("warning: creating metric instruments: %v", err)
	}
	h.metrics = m
	if h.replication != nil {
		h.replication.bind(h.blobCache, m)
		h.log.Printf("blob existence cache replication enabled: %s", h.replication.summary())
	}
	return h
}

// RunCacheReplication runs the background half of cache replication until done is
// closed: peer discovery, the warm-up that seeds this instance's cache from a
// peer, and the batching of outbound events. It blocks, so call it in a goroutine,
// and it is a no-op when replication is not configured.
//
// Call it once the listener is open. Until it runs, events queue (and, past the
// queue's depth, are dropped) and — if warming up is configured — [Handler.serve]
// answers the health endpoint with 503 until the warm-up deadline passes.
func (h *Handler) RunCacheReplication(done <-chan struct{}) {
	h.replication.run(done, func() { h.warming.Store(false) })
}

// warmingUp reports whether this instance is still seeding its cache from a peer,
// and so should be kept out of the load balancer.
//
// The deadline is what makes this safe to answer a readiness probe with: a peer
// that never answers, a peer set that never appears, or replication that is never
// started all end the same way — the instance reports healthy once the warm-up
// budget is spent, having lost nothing but the seeding.
func (h *Handler) warmingUp() bool {
	return h.warming.Load() && h.now().Before(h.warmingUntil)
}

// Reload swaps in a policy freshly loaded from path and returns it. If the file
// cannot be read, parsed, or compiled, the previous policy is kept and the error
// is returned, so a bad edit never opens the gateway up. It is safe to call
// concurrently with in-flight requests.
func (h *Handler) Reload(path string) (*CompiledPolicy, error) {
	cp, err := LoadPolicyFile(path)
	h.metrics.recordReload(context.Background(), err)
	if err != nil {
		return nil, err
	}
	h.policy.Store(cp)
	return cp, nil
}

// RecordMaterialReload counts a reload of on-disk material (a TLS keypair, a CA
// bundle, or a token file) by outcome. A failed reload keeps the previous
// material in force, which is the right behaviour but also makes a persistently
// broken file invisible — so it is counted, not only logged.
func (h *Handler) RecordMaterialReload(material string, err error) {
	h.metrics.recordMaterialReload(context.Background(), material, err)
}

func defaultBaseTransport() http.RoundTripper {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return t.Clone()
	}
	return http.DefaultTransport
}

// ServeHTTP implements [http.Handler].
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// begin returns wrappers that count the bytes crossing the client
	// connection and capture the response status.
	obs, w, r := h.metrics.begin(w, r)
	defer obs.finish(r.Context())
	h.serve(obs, w, r)
}

// serve is the request logic of [Handler.ServeHTTP], with obs collecting the
// metrics for this request.
func (h *Handler) serve(obs *observation, w http.ResponseWriter, r *http.Request) {
	// Every path comparison below uses the escaped form, so the endpoint a request
	// reaches is decided by the bytes it actually sent (see classify).
	path := r.URL.EscapedPath()

	// The health endpoint is answered before authentication so a Kubernetes probe
	// can reach a listener that requires a credential.
	if path == healthPath {
		h.serveHealth(w)
		return
	}

	// Authenticate before anything else: an unauthenticated client must not even
	// be able to probe which upstream registries this gateway allows.
	if h.peerAuth != nil {
		principal, err := h.peerAuth.Authenticate(r)
		if err != nil {
			h.writePeerAuthError(obs, w, r, err)
			return
		}
		obs.principal = principal
	}

	// Cache replication between the instances of a serving deployment. It is
	// answered before anything registry-shaped: it names no upstream registry, and
	// what it may do to this instance's cache is gated by the identity of the peer
	// rather than by the policy (see replication.go).
	if strings.HasPrefix(path, replicationPathPrefix) {
		if h.replicationSeparate.Load() {
			// Replication has a listener of its own, which is the only place these
			// endpoints exist. Answering them here too would put the write path to
			// this instance's cache back on the surface the separate listener was
			// configured to take it off.
			h.writeError(obs, w, r, http.StatusNotFound, "UNSUPPORTED", errUnsupportedEndpoint,
				"this gateway replicates its blob existence cache on a separate listener")
			return
		}
		h.serveCacheReplication(obs, w, r, path)
		return
	}

	// Snapshot the policy once so a concurrent reload cannot split a single
	// request's decisions across two policies.
	authz := h.policy.Load()

	host := r.Header.Get(clientgateway.OriginalHostHeader)
	if host == "" {
		// Fall back to the configured default registry when the client did not
		// name a target registry. If no default is configured the header is
		// required.
		host = h.defaultRegistry
	}
	if host == "" {
		h.writeError(obs, w, r, http.StatusBadRequest, "UNSUPPORTED", errMissingHost,
			fmt.Sprintf("missing required %s header and no default registry configured", clientgateway.OriginalHostHeader))
		return
	}
	// The API version check has no repository; resolve just the registry so the
	// allow-list is enforced against the *resolved* upstream.
	if path == "/v2" || path == "/v2/" {
		versionCheck := request{op: opNameVersionCheck, route: routeVersion}
		reg, err := name.NewRegistry(host)
		if err != nil {
			obs.setUpstream("", versionCheck)
			h.writeError(obs, w, r, http.StatusBadRequest, "NAME_INVALID", errInvalidRegistry,
				fmt.Sprintf("invalid registry %q: %v", host, err))
			return
		}
		obs.setUpstream(hostname(reg.RegistryStr()), versionCheck)
		if !authz.RegistryAllowed(hostname(reg.RegistryStr())) {
			h.writeError(obs, w, r, http.StatusForbidden, "DENIED", errRegistryDenied,
				fmt.Sprintf("upstream registry %q is not allowed by this gateway", reg.RegistryStr()))
			return
		}
		// Answer anonymously so clients treat the gateway as an unauthenticated
		// registry and send us no credentials.
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
		h.log.Printf("%s %q (host=%s%s) -> 200 (version check)", r.Method, r.URL.EscapedPath(), reg.RegistryStr(), obs.logContext())
		return
	}

	cls, ok := classify(r)
	if !ok {
		h.writeError(obs, w, r, http.StatusNotFound, "UNSUPPORTED", errUnsupportedEndpoint,
			"unsupported registry endpoint")
		return
	}
	if cls.malformedQuery {
		// An upload query the gateway cannot parse the same way the upstream
		// would (e.g. a ';' separator, or duplicate mount/from values) could let
		// the client have us authorize a different mount source than the one the
		// upstream acts on. Refuse it rather than forward it.
		obs.setUpstream("", cls)
		h.writeError(obs, w, r, http.StatusBadRequest, "UNSUPPORTED", errMalformedQuery,
			"malformed or ambiguous upload query")
		return
	}

	repo, err := name.NewRepository(host + "/" + cls.repo)
	if err != nil {
		obs.setUpstream("", cls)
		h.writeError(obs, w, r, http.StatusBadRequest, "NAME_INVALID", errInvalidRepository,
			fmt.Sprintf("invalid repository %q: %v", cls.repo, err))
		return
	}
	// Enforce the allow-list and policy against the *resolved* registry and
	// repository, not the raw header/path. name.NewRepository routes a header
	// like "myregistry" (no dot) to Docker Hub and prepends library/ to a
	// single-segment Docker Hub repo, so matching the header/path alone would
	// not constrain the real upstream the gateway connects to.
	regHost := hostname(repo.RegistryStr())
	obs.setUpstream(regHost, cls)
	if !authz.RegistryAllowed(regHost) {
		h.writeError(obs, w, r, http.StatusForbidden, "DENIED", errRegistryDenied,
			fmt.Sprintf("upstream registry %q is not allowed by this gateway", repo.RegistryStr()))
		return
	}
	allow, idx, desc := authz.Decide(regHost, repo.RepositoryStr(), cls.req)
	obs.policyDecision(r.Context(), allow)
	if !allow {
		h.log.Printf("%s %q (host=%s repo=%q%s) denied by policy (rule=%d %q)", r.Method, r.URL.EscapedPath(), regHost, repo.RepositoryStr(), obs.logContext(), idx, desc)
		h.writeError(obs, w, r, http.StatusForbidden, "DENIED", errPolicyDenied,
			fmt.Sprintf("%s is not permitted by this gateway's policy", cls.kind))
		return
	}

	// A cross-repo blob mount additionally reads the source repository, so it
	// must be readable under the policy too. Resolve it against the same host
	// (OCI mounts are same-registry) and fail closed on any problem.
	if cls.mountFrom != "" {
		if !h.mountSourceReadable(authz, host, cls.mountFrom) {
			h.log.Printf("%s %q (host=%s%s) denied: mount source %q not readable by policy", r.Method, r.URL.EscapedPath(), regHost, obs.logContext(), cls.mountFrom)
			h.writeError(obs, w, r, http.StatusForbidden, "DENIED", errMountDenied,
				fmt.Sprintf("mounting from %q is not permitted by this gateway's policy", cls.mountFrom))
			return
		}
	}

	// A blob existence check may already be answerable without the upstream. The
	// cache is deliberately consulted *after* the policy decision above: whether
	// this client may ask the question is decided every time, and only the answer
	// is memoized.
	if h.serveCachedBlobHead(obs, w, r, repo, cls) {
		return
	}

	h.forward(obs, w, r, repo, cls)
}

// serveHealth answers the unauthenticated health endpoint. Both of a gateway's
// listeners answer it: a Kubernetes probe names a port, and a peer listener that
// could not say whether it is ready would have to be probed through the client
// one, which reports the readiness of a different socket.
func (h *Handler) serveHealth(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if h.warmingUp() {
		// Still seeding the blob existence cache from a peer. Serving now would
		// send this instance's share of the fleet's probes upstream for nothing,
		// so stay out of the Service until the seeding is done or its budget is
		// spent (see Handler.warmingUp).
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "warming up\n")
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

// serveCachedBlobHead answers a blob existence check from the blob existence
// cache, and reports whether it did.
//
// The replayed response carries the two headers that mean something on a blob
// HEAD: Content-Length, which is how a client sizes a layer without downloading
// it, and Docker-Content-Digest, which for a content-addressed blob is the digest
// the client just asked about and so needs no storing. Everything else a registry
// might have sent (its Date, ETag, Accept-Ranges, or any header of its own) is
// dropped rather than replayed to a different client minutes or hours later.
func (h *Handler) serveCachedBlobHead(obs *observation, w http.ResponseWriter, r *http.Request, repo name.Repository, cls request) bool {
	if !h.cacheableBlobHead(r, cls) {
		return false
	}
	contentLength, ok := h.blobCache.lookup(repo.RegistryStr(), repo.RepositoryStr(), cls.digest)
	obs.blobCacheLookup(r.Context(), ok)
	if !ok {
		return false
	}
	if contentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))
	}
	w.Header().Set("Docker-Content-Digest", cls.digest)
	w.WriteHeader(http.StatusOK)
	h.log.Printf("%s %q (host=%s%s) -> 200 (cached)", r.Method, r.URL.EscapedPath(), repo.RegistryStr(), obs.logContext())
	return true
}

// cacheableBlobHead reports whether a request may be answered from, or admitted
// to, the blob existence cache. Only a plain HEAD of a digest-referenced blob
// qualifies.
func (h *Handler) cacheableBlobHead(r *http.Request, cls request) bool {
	if h.blobCache == nil || cls.op != opNameBlobHead || cls.digest == "" {
		return false
	}
	for _, header := range uncacheableHeaders {
		if _, ok := r.Header[header]; ok {
			return false
		}
	}
	return true
}

// cacheableBlobRead reports whether a request is a read of a digest-referenced
// blob, the answer to which can say the blob is there.
func (h *Handler) cacheableBlobRead(cls request) bool {
	return h.blobCache != nil && cls.op == opNameBlobRead && cls.digest != ""
}

// cacheableBlobUpload reports whether a request is one whose success puts a blob in
// the repository under a digest the cache can be keyed on. [classify] carries the
// digest for exactly those requests and no others (see [committedDigest]).
func (h *Handler) cacheableBlobUpload(cls request) bool {
	return h.blobCache != nil && cls.op == opNameBlobUpload && cls.digest != ""
}

// rememberBlob admits a blob to the existence cache when the response the gateway
// just received is proof that the blob is in the repository. Three responses are:
//
//   - 200 to an existence check: the answer the cache exists to replay.
//   - 200 to a blob read that the registry says is the blob asked for. Handing over
//     the content is a stronger statement than answering a probe, and a fleet that
//     pulls a layer today is one that will ask whether it needs to push it tomorrow.
//   - 201 Created to the request that finishes an upload, which is the registry
//     stating that it hashed the content it received, matched it against the
//     digest, and stored it. A HEAD sent a moment later would answer 200, so the
//     client that did the pushing warms the cache for everyone else instead of the
//     next fleet member paying for a probe of its own.
//
// Nothing earlier in an upload counts. A 202 means only that a session was opened
// or a chunk was accepted; until the commit succeeds there is no blob, and a client
// told otherwise would skip an upload it still owes. Any status other than the one
// each case names leaves the cache untouched — including the 206 that answers a
// ranged read, whose Content-Length is a slice of the blob rather than its size.
//
// [Handler.forgetDeletedBlob] is the other direction.
func (h *Handler) rememberBlob(r *http.Request, repo name.Repository, cls request, resp *http.Response) {
	var length int64
	switch {
	case h.cacheableBlobHead(r, cls) && resp.StatusCode == http.StatusOK:
		length = blobLength(resp)
	case h.cacheableBlobRead(cls) && resp.StatusCode == http.StatusOK && servedDigest(resp, cls.digest):
		length = blobLength(resp)
	case h.cacheableBlobUpload(cls) && resp.StatusCode == http.StatusCreated && confirmsDigest(resp, cls.digest):
		length = uploadedLength(r, cls)
	default:
		return
	}
	h.blobCache.store(repo.RegistryStr(), repo.RepositoryStr(), cls.digest, length)
	// Tell the other instances of this deployment, so that the round trip this
	// fact cost is paid once by the fleet rather than once by each of them. The
	// event is queued, never sent from here: the client is waiting on this
	// goroutine (see CacheReplication.record).
	h.replication.record(r.Context(), repo.RegistryStr(), repo.RepositoryStr(), cls.digest, false)
}

// confirmsDigest reports whether the registry's own account of what it created
// agrees with the digest the request asked it to create.
//
// Docker-Content-Digest is not required on a 201 and, when a registry does send
// one, it is all but always the digest already in the request. A registry that
// names a different blob is one whose reading of the request is not the gateway's,
// which is the one case where remembering either answer would be a guess.
func confirmsDigest(resp *http.Response, digest string) bool {
	reported := resp.Header.Get("Docker-Content-Digest")
	return reported == "" || reported == digest
}

// servedDigest reports whether the registry said, in so many words, that the body it
// is answering a blob read with is the blob that was asked for.
//
// Here the header is required, not merely respected — the asymmetry with
// [confirmsDigest] is the point. A commit is proof on its own, because the registry
// hashed the content before answering; a read is only proof if the registry names
// what it served, since the gateway streams the body through without hashing it and
// a blob read may be redirected to storage that serves whatever it is pointed at. An
// answer without the header is forwarded to the client and forgotten.
func servedDigest(resp *http.Response, digest string) bool {
	return resp.Header.Get("Docker-Content-Digest") == digest
}

// uploadedLength is the size of a blob whose upload just committed, when this one
// request is proof of the size, and -1 (unknown) otherwise.
//
// The request whose body is the whole blob is a monolithic POST upload: the
// registry's 201 says it hashed exactly those bytes into the digest that was asked
// for. A session commit proves nothing about the size — the PUT that closes a
// session usually carries no body at all, the content having gone upstream in PATCH
// requests the gateway does not correlate with the commit — and a cross-repo mount
// transfers no bytes in the first place.
//
// The *response's* Content-Length is not the blob's and must not be read as it: a
// registry sets it to 0 on the 201, whose body is empty.
//
// An entry whose length is unknown is still worth keeping, since the existence
// answer is what a push client is blocked on, and a HEAD answered from one simply
// omits Content-Length — the same reply the cache already gives for a registry that
// reports no length.
func uploadedLength(r *http.Request, cls request) int64 {
	if r.Method != http.MethodPost || cls.mountFrom != "" || r.ContentLength < 0 {
		return -1
	}
	return r.ContentLength
}

// forgetDeletedBlob drops the cache entry for a blob a client asked the upstream to
// delete. It is the counterpart of [Handler.rememberBlob], and the only thing that
// unmakes an entry before its TTL runs out.
//
// The upstream's answer is deliberately not consulted, and this is called even when
// no answer arrived. The 202 that a delete usually earns (some registries answer 200
// or 204) means the entry is now false; a 5xx, a timeout, or a client that hung up
// means the gateway does not know whether it is; a refusal means the blob is still
// there. Only the last of those makes dropping the entry unnecessary, and dropping it
// costs one existence check the next time somebody asks. Keeping a false one costs a
// push client the layer it decided not to re-upload, and then a manifest that
// references a blob which is gone — so a delete that passed through here is read
// pessimistically.
//
// Dropping after the round trip rather than before it is what keeps a probe arriving
// mid-delete from writing the entry straight back: the upstream answers such a probe
// from the state before the delete.
//
// resp is the upstream's answer, or nil when none arrived. It decides only whether
// the peers are told: dropping this instance's own entry is free, while asking the
// whole fleet to forget is not, and the registry's answer is the only evidence that
// a blob really left. A delete the registry refused (405 from a registry with
// deletions disabled, 404, a 5xx) therefore costs this instance one probe and the
// others nothing — and a client cannot flush the fleet's cache by sending deletes
// the registry rejects.
func (h *Handler) forgetDeletedBlob(ctx context.Context, repo name.Repository, cls request, resp *http.Response) {
	// classify carries a digest on a blob write for a DELETE and nothing else.
	if h.blobCache == nil || cls.op != opNameBlobWrite || cls.digest == "" {
		return
	}
	h.blobCache.forget(repo.RegistryStr(), repo.RepositoryStr(), cls.digest)
	if resp != nil && deleteSucceeded(resp.StatusCode) {
		h.replication.record(ctx, repo.RegistryStr(), repo.RepositoryStr(), cls.digest, true)
	}
}

// deleteSucceeded reports whether a registry's answer to a blob delete says the
// blob is gone. The spec calls for 202 Accepted; registries answer 200 or 204 too.
func deleteSucceeded(status int) bool {
	switch status {
	case http.StatusOK, http.StatusAccepted, http.StatusNoContent:
		return true
	default:
		return false
	}
}

// uncacheableHeaders make a response depend on more than whether the blob exists,
// so a request carrying one is passed through untouched and its answer is not
// stored: the upstream would answer it with a 206 or a 304, and the cache only
// ever holds a plain 200. No OCI client sends one of these on a blob HEAD; the
// check is here so that one which does gets the registry's real answer.
var uncacheableHeaders = []string{
	"Range",
	"If-Match",
	"If-None-Match",
	"If-Modified-Since",
	"If-Unmodified-Since",
	"If-Range",
}

// blobLength is the Content-Length a registry reported for a blob, or -1 when it
// reported none or an unusable value.
//
// The header is parsed rather than read from [http.Response.ContentLength] so
// that a hand-rolled [http.RoundTripper] installed with [WithBaseTransport]
// behaves like an [http.Transport], which fills that field in from this header
// specifically for a HEAD response.
func blobLength(resp *http.Response) int64 {
	value := resp.Header.Get("Content-Length")
	if value == "" {
		return -1
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// mountSourceReadable reports whether the cross-repo mount source repository is
// readable under the policy. It resolves the source against the request's host
// (mounts are always same-registry per the OCI spec) and fails closed on a parse
// error or a disallowed registry.
func (h *Handler) mountSourceReadable(authz *CompiledPolicy, host, from string) bool {
	fromRepo, err := name.NewRepository(host + "/" + from)
	if err != nil {
		return false
	}
	fromHost := hostname(fromRepo.RegistryStr())
	if !authz.RegistryAllowed(fromHost) {
		return false
	}
	allow, _, _ := authz.Decide(fromHost, fromRepo.RepositoryStr(), reqBlobRead)
	return allow
}

// hostname strips the port, if any, from a resolved registry string so patterns
// match on the bare hostname.
func hostname(registryStr string) string {
	if hn, _, err := net.SplitHostPort(registryStr); err == nil {
		return hn
	}
	return registryStr
}

// forward proxies the request to the upstream registry using an authenticated
// transport and streams the response back to the client.
func (h *Handler) forward(obs *observation, w http.ResponseWriter, r *http.Request, repo name.Repository, cls request) {
	action := transport.PullScope
	if cls.write {
		action = transport.PushScope
	}

	rt, err := h.authTransport(r.Context(), obs, repo, action)
	if err != nil {
		h.writeError(obs, w, r, http.StatusBadGateway, "UNAUTHORIZED", authErrorType(err),
			fmt.Sprintf("authenticating to upstream %s: %v", repo.RegistryStr(), err))
		return
	}

	// Preserve the exact request URI (path + query) as received.
	upstreamURL := repo.Scheme() + "://" + repo.RegistryStr() + r.URL.RequestURI()
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, r.Body)
	if err != nil {
		h.writeError(obs, w, r, http.StatusBadGateway, "UNKNOWN", errBadUpstreamRequest,
			fmt.Sprintf("building upstream request: %v", err))
		return
	}
	copyHeader(outReq.Header, r.Header)
	outReq.ContentLength = r.ContentLength

	// Use an http.Client so redirects (e.g. a blob GET pointing at CDN/blob
	// storage) are followed transparently. checkRedirect only follows for safe
	// read methods and refuses redirects to private/link-local addresses.
	client := &http.Client{Transport: rt, CheckRedirect: checkRedirect(r.Method)}
	started := time.Now()
	resp, err := client.Do(outReq)
	if err != nil {
		// A delete whose answer never came back may have taken effect all the same,
		// and an entry for a blob that is gone is the one wrong answer this cache can
		// give. The peers are not told: with no answer there is no evidence a blob
		// left, and each of them will hear it from their own traffic or its TTL.
		h.forgetDeletedBlob(r.Context(), repo, cls, nil)
		h.writeError(obs, w, r, http.StatusBadGateway, "UNKNOWN", transportErrorType(err),
			fmt.Sprintf("forwarding to upstream %s: %v", repo.RegistryStr(), err))
		return
	}
	defer resp.Body.Close()
	obs.upstreamResponse(r.Context(), cls, resp.StatusCode, time.Since(started))

	// Bring the blob existence cache in line with what the registry just did: the
	// 200 that says a blob is there and the 201 that says it was just put there
	// admit an entry, and a delete takes one away. Both tell the peers.
	h.rememberBlob(r, repo, cls, resp)
	h.forgetDeletedBlob(r.Context(), repo, cls, resp)

	copyResponseHeader(w.Header(), resp.Header, repo)
	w.WriteHeader(resp.StatusCode)
	var copyErr error
	if r.Method != http.MethodHead {
		if _, copyErr = io.Copy(w, resp.Body); copyErr != nil {
			// The status and headers are already on the wire, so the only way to
			// tell the client the body is incomplete is to abort the response.
			// Returning normally would deliver a clean, short 200: net/http catches
			// that when the upstream declared a Content-Length, but when it did not
			// (a chunked or streaming registry response) the client would silently
			// accept a truncated blob or manifest. http.ErrAbortHandler is the
			// sentinel net/http understands for "kill this response without another
			// log line"; go-containerregistry retries the unexpected EOF it sees.
			obs.fail(r.Context(), transferErrorType(copyErr))
			obs.recordTransfer(r.Context(), r, cls, resp.StatusCode, copyErr)
			h.log.Printf("%s %q (host=%s%s): aborting response after %v", r.Method, r.URL.EscapedPath(), repo.RegistryStr(), obs.logContext(), copyErr)
			panic(http.ErrAbortHandler)
		}
	}
	obs.recordTransfer(r.Context(), r, cls, resp.StatusCode, copyErr)
	h.log.Printf("%s %q (host=%s%s) -> %d", r.Method, r.URL.EscapedPath(), repo.RegistryStr(), obs.logContext(), resp.StatusCode)
}

// authTransport returns a cached authenticated RoundTripper for the given
// repository and scope action ("pull" or "push,pull"). It resolves credentials
// from the keychain and performs the crane ping + token-exchange handshake.
func (h *Handler) authTransport(reqCtx context.Context, obs *observation, repo name.Repository, action string) (http.RoundTripper, error) {
	key := repo.String() + "|" + action
	return h.cache.get(key, func() (http.RoundTripper, error) {
		// The resulting transport is cached and shared across requests, so the
		// initial handshake must not be tied to the first caller's request
		// context: a cancellation there would otherwise poison every concurrent
		// waiter on the same sync.Once. Bound it with an independent timeout
		// instead. Per-request token refreshes still use the request's context.
		ctx, cancel := context.WithTimeout(context.Background(), authHandshakeTimeout)
		defer cancel()
		auth, err := authn.Resolve(ctx, h.keychain, repo)
		if err != nil {
			obs.authHandshake(reqCtx, err)
			return nil, fmt.Errorf("resolving credentials: %w", err)
		}
		rt, err := transport.NewWithContext(ctx, repo.Registry, auth, h.base, []string{repo.Scope(action)})
		obs.authHandshake(reqCtx, err)
		if err != nil {
			return nil, err
		}
		return rt, nil
	})
}

// checkRedirect is the http.Client redirect policy used when forwarding to
// upstream. It follows registry redirects (e.g. blob GETs pointing at CDN/blob
// storage) only for safe read methods, and refuses to follow a redirect to a
// private/loopback/link-local IP literal (an allow-listed but compromised
// upstream could otherwise steer the gateway at internal services such as the
// cloud metadata endpoint). For write methods it returns ErrUseLastResponse so
// the redirect is passed back to the client rather than followed with a dropped
// body or changed method.
func checkRedirect(originalMethod string) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errRedirectsStopped
		}
		if originalMethod != http.MethodGet && originalMethod != http.MethodHead {
			return http.ErrUseLastResponse
		}
		return validateRedirectTarget(req.URL)
	}
}

// validateRedirectTarget rejects redirect URLs that use a non-http(s) scheme or
// resolve to a private / loopback / link-local IP literal. DNS-based SSRF is out
// of scope (mirroring go-containerregistry's realm validation); operators should
// apply network-level controls if needed, or run with private upstreams denied
// (see [DenyPrivateAddresses], which does check the resolved address).
func validateRedirectTarget(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w to non-http(s) URL %q", errRefusedRedirect, u.Redacted())
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		if isPrivateAddress(ip) {
			return fmt.Errorf("%w to private or link-local address %q", errRefusedRedirect, u.Hostname())
		}
	}
	return nil
}

// isPrivateAddress reports whether ip is one the gateway must never be steered
// at: an allow-listed but compromised upstream (or a client naming an internal
// host in the original-host header) could otherwise reach internal services such
// as a cloud metadata endpoint.
func isPrivateAddress(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() || ip.IsUnspecified()
}

// DenyPrivateAddresses returns a copy of rt that refuses to connect to loopback,
// link-local, private, or unspecified addresses.
//
// The upstream registry is named by a client-supplied header, and
// go-containerregistry resolves a private or loopback host to a *plaintext*
// endpoint, so a policy whose registry pattern is "*" (or --dangerously-allow-all)
// otherwise lets any client turn the gateway into an internal HTTP proxy. That
// matters most for a shared gateway, whose network reach is not its clients'.
//
// The check runs in the dialer's Control hook, i.e. against the address actually
// resolved, so DNS names, redirects, and go-containerregistry's realm fetch are
// all covered by this one guard rather than by three separate string checks.
func DenyPrivateAddresses(rt http.RoundTripper) (http.RoundTripper, error) {
	transport, ok := rt.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("denying private upstreams needs an *http.Transport, got %T", rt)
	}
	guarded := transport.Clone()
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	dialer.Control = func(_, address string, _ syscall.RawConn) error {
		return checkDialAddress(address)
	}
	guarded.DialContext = dialer.DialContext
	return guarded, nil
}

// checkDialAddress is the guard [DenyPrivateAddresses] installs. It is a named
// function so it can be tested without opening a connection.
func checkDialAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: unparsable address %q", errPrivateUpstreamDial, address)
	}
	if ip := net.ParseIP(host); ip != nil && isPrivateAddress(ip) {
		return fmt.Errorf("%w: %s", errPrivateUpstreamDial, host)
	}
	return nil
}

// writeError records the failure under errType and writes an OCI-style error
// response. errType is one of the fixed error.type values from errclass.go; an
// empty errType records no error (used when the caller already classified one).
//
// The request path is logged quoted, because [url.URL.Path] is percent-decoded:
// a request for /v2/x%0A... would otherwise put a real newline in the log and let
// a client forge audit lines.
func (h *Handler) writeError(obs *observation, w http.ResponseWriter, r *http.Request, status int, code, errType, message string) {
	if errType != "" {
		obs.fail(r.Context(), errType)
	}
	h.log.Printf("%s %q (host=%s%s) -> %d %s: %s", r.Method, r.URL.EscapedPath(), obs.host, obs.logContext(), status, code, message)
	writeOCIError(w, status, code, message)
}

// writePeerAuthError answers a client whose credential the gateway rejected.
//
// It deliberately sends no WWW-Authenticate header: there is no
// challenge-response flow here, and advertising a realm would make a
// go-containerregistry client attempt a token exchange against the gateway. The
// [gatewayErrorHeader] lets a forwarding gateway tell this apart from the
// identical status an upstream registry would return.
func (h *Handler) writePeerAuthError(obs *observation, w http.ResponseWriter, r *http.Request, err error) {
	status, code, errType, message := peerAuthResponse(err)
	obs.fail(r.Context(), errType)
	h.log.Printf("%s %q -> %d %s: %s", r.Method, r.URL.EscapedPath(), status, code, message)
	w.Header().Set(gatewayErrorHeader, errType)
	writeOCIError(w, status, code, message)
}

// writeOCIError writes the error body shape the distribution spec defines.
func writeOCIError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	body := struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}{}
	body.Errors = append(body.Errors, struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message})
	_ = json.NewEncoder(w).Encode(body)
}

// hopByHopHeaders are connection-specific headers that must not be forwarded.
// See RFC 7230 section 6.1.
var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Proxy-Connection":    {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// copyHeader copies request headers to the upstream request, dropping
// hop-by-hop headers, the whole reserved X-rules_img- namespace, the Host header,
// any client-supplied Authorization (the auth transport sets its own), and the
// forwarding headers of the hop the request arrived on.
//
// Dropping the reserved namespace wholesale rather than naming individual
// headers means a control header added later cannot accidentally be forwarded to
// a registry, and a client cannot smuggle one there today. Authorization is the
// load-bearing one: it is what carries the credential on a gateway-to-gateway
// hop, and this is the barrier that proves it cannot reach a registry.
func copyHeader(dst, src http.Header) {
	skip := connectionHeaderSet(src)
	for k, vs := range src {
		ck := http.CanonicalHeaderKey(k)
		if !forwardable(ck, skip) {
			continue
		}
		if strings.HasPrefix(ck, reservedHeaderPrefix) {
			continue
		}
		switch ck {
		case "Host", "Authorization",
			"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded":
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// copyResponseHeader copies upstream response headers back to the client,
// dropping hop-by-hop headers and rewriting relative Location headers to
// absolute upstream URLs so follow-up requests are routed back through the
// gateway with the correct original host.
func copyResponseHeader(dst, src http.Header, repo name.Repository) {
	skip := connectionHeaderSet(src)
	for k, vs := range src {
		ck := http.CanonicalHeaderKey(k)
		if !forwardable(ck, skip) {
			continue
		}
		if ck == "Location" {
			for _, v := range vs {
				dst.Add(k, rewriteLocation(v, repo))
			}
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// forwardable reports whether a header (already canonicalized) may be forwarded:
// it must be neither a hop-by-hop header nor named in the Connection header.
func forwardable(canonicalKey string, connectionSkip map[string]struct{}) bool {
	if _, hop := hopByHopHeaders[canonicalKey]; hop {
		return false
	}
	if _, conn := connectionSkip[canonicalKey]; conn {
		return false
	}
	return true
}

// connectionHeaderSet returns the set of (canonicalized) header names listed in
// the Connection header, which are hop-by-hop by definition (RFC 7230 §6.1).
func connectionHeaderSet(src http.Header) map[string]struct{} {
	set := make(map[string]struct{})
	for _, v := range src["Connection"] {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				set[http.CanonicalHeaderKey(name)] = struct{}{}
			}
		}
	}
	return set
}

// rewriteLocation turns a relative Location into an absolute URL pointing at the
// upstream registry. Absolute Locations are left untouched: the client's gateway
// transport re-routes them (using the host encoded in the URL) back through the
// gateway.
//
// RawPath is carried across, not just Path: an upload-session reference is opaque
// and may contain percent-escapes, and rebuilding the URL from the decoded path
// alone would hand the client back a reference the registry does not recognize.
func rewriteLocation(loc string, repo name.Repository) string {
	if loc == "" {
		return loc
	}
	u, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	if u.IsAbs() {
		return loc
	}
	abs := url.URL{
		Scheme:   repo.Scheme(),
		Host:     repo.RegistryStr(),
		Path:     u.Path,
		RawPath:  u.RawPath,
		RawQuery: u.RawQuery,
	}
	return abs.String()
}

// authCache memoizes authenticated transports per repository+scope. Creation
// (which involves a network ping + token exchange) happens at most once per key
// while it succeeds; failures are not cached so later requests can retry.
type authCache struct {
	mu    sync.Mutex
	inner map[string]*authEntry
}

type authEntry struct {
	once sync.Once
	rt   http.RoundTripper
	err  error
}

func (c *authCache) get(key string, create func() (http.RoundTripper, error)) (http.RoundTripper, error) {
	c.mu.Lock()
	e, ok := c.inner[key]
	if !ok {
		e = &authEntry{}
		c.inner[key] = e
	}
	c.mu.Unlock()

	e.once.Do(func() {
		e.rt, e.err = create()
	})

	if e.err != nil {
		// Don't cache failures: drop the entry so the next request retries.
		c.mu.Lock()
		if c.inner[key] == e {
			delete(c.inner, key)
		}
		c.mu.Unlock()
	}
	return e.rt, e.err
}
