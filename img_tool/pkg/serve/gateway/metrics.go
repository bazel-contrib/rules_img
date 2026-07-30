package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// This file implements the gateway's OpenTelemetry metrics.
//
// The gateway only uses the metric *API*, never the SDK: instruments come from
// the [metric.MeterProvider] passed with [WithMeterProvider] (by default the
// global one, which is a no-op until a binary installs an SDK). Wiring an
// exporter is therefore the binary's job — see
// //pkg/serve/telemetry, used by //cmd/oci-distribution-gateway.
//
// Every instrument is *additive*: counters and histograms from several gateway
// replicas sum into a fleet-wide number, so a cluster is monitored by summing
// over instances (there are no non-additive gauges of request state).
//
// Attribute cardinality is deliberately bounded. The upstream registry is
// attached to nearly every measurement (oci.registry) because it is the axis
// operators slice by, but it originates in a client-supplied header, so the set
// of reported values is capped (see [boundedValues]). Repository paths are never
// used as attributes: on a build farm they are effectively unbounded.

// meterName is the instrumentation scope name of the gateway's metrics.
const meterName = "github.com/bazel-contrib/rules_img/img_tool/pkg/serve/gateway"

// Attribute keys specific to the gateway. Keys that exist in the OpenTelemetry
// semantic conventions (http.*, network.*, error.type) are used from semconv
// instead of being redefined here.
const (
	// attrRegistry is the resolved upstream registry host (port stripped).
	attrRegistry = attribute.Key("oci.registry")
	// attrOperation is the classified registry operation, e.g. "blob.read".
	attrOperation = attribute.Key("oci.operation")
	// attrResult is a per-instrument outcome, e.g. "hit"/"miss" for existence
	// checks or "success"/"failure" for policy reloads.
	attrResult = attribute.Key("oci.result")
	// attrUploadKind describes how a blob reached the registry: "monolithic",
	// "chunked", "mount", or "unknown".
	attrUploadKind = attribute.Key("oci.blob.upload.kind")
	// attrDecision is the policy decision, "allow" or "deny".
	attrDecision = attribute.Key("oci.policy.decision")
	// attrMaterial names the on-disk material a reload concerned:
	// "certificate", "ca", or "token".
	attrMaterial = attribute.Key("oci.gateway.material")
	// attrEvictionReason says why a cache entry was dropped: "capacity" (it was
	// the least recently used entry when room was needed) or "expired" (its TTL
	// had passed).
	attrEvictionReason = attribute.Key("oci.gateway.cache.eviction.reason")
	// attrPeer is the peer gateway a forwarder relays to. It is constant for the
	// life of the process, so it costs no cardinality; it is attached anyway so a
	// dashboard panel says which hop it is describing.
	attrPeer = attribute.Key("oci.gateway.peer")
)

// Values of [attrResult] and [attrUploadKind].
const (
	resultHit     = "hit"
	resultMiss    = "miss"
	resultError   = "error"
	resultSuccess = "success"
	resultFailure = "failure"

	uploadMonolithic = "monolithic"
	uploadChunked    = "chunked"
	uploadMount      = "mount"
	uploadUnknown    = "unknown"

	// Values of [attrEvictionReason].
	evictedForCapacity = "capacity"
	evictedForExpiry   = "expired"
)

const (
	// maxRegistryValues bounds how many distinct hosts one process reports in the
	// oci.registry attribute. The host comes from a client-supplied header, and a
	// wildcard policy (or --dangerously-allow-all) does not constrain it, so
	// without a cap a misbehaving client could mint unbounded time series in the
	// monitoring backend. Everything past the cap is reported as registryOverflow.
	maxRegistryValues = 128
	// registryOverflow replaces upstream hosts beyond maxRegistryValues.
	registryOverflow = "_other"
	// registryUnknown is reported for requests rejected before the upstream
	// registry was resolved.
	registryUnknown = "unknown"

	// maxUploadSessions bounds the per-generation size of the blob upload session
	// table (see [uploadTracker]).
	maxUploadSessions = 4096
)

// durationBuckets are the boundaries of the request duration histograms. They
// extend the semantic-convention boundaries for http.server.request.duration
// (which stop at 10s) into the minutes: the gateway streams multi-gigabyte
// blobs, so plenty of requests legitimately take much longer than 10s and would
// otherwise all land in the overflow bucket.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10, 30, 60, 300, 900}

// sizeBuckets are the boundaries of the blob size histograms: 1 KiB to 16 GiB,
// covering everything from a tiny config blob to an oversized model layer.
var sizeBuckets = []float64{
	1 << 10, 1 << 13, 1 << 16, 1 << 18, 1 << 20, 1 << 22,
	1 << 24, 1 << 26, 1 << 28, 1 << 30, 1 << 32, 1 << 34,
}

// metrics holds the gateway's instruments plus the small amount of state needed
// to keep attributes bounded and to attribute upload sizes to the request that
// completes an upload. It is safe for concurrent use.
//
// The set of instruments depends on the role. A forwarding gateway relays the
// same traffic a serving gateway then reports in full, so it deliberately does
// *not* re-export the registry-shaped instruments: blob counts, blob sizes,
// existence-check hit rates and bandwidth are reported once, by the tier that
// actually talks to the registry. Summing `oci_gateway_*` across a fleet would
// otherwise double every one of them. What a forwarder reports instead is what
// only it can see — its own hop — under distinct `oci.gateway.forward.*` names
// that can never be added to the serving tier's series by accident.
type metrics struct {
	// Reported by both roles.
	//
	// requestDuration is the semantic-convention http.server.request.duration.
	// Its count series doubles as the gateway's request rate. Being semconv, it is
	// per-service by nature: every HTTP service in a cluster exports it, so it is
	// always read with a service.name filter.
	requestDuration metric.Float64Histogram
	// activeRequests is the semantic-convention http.server.active_requests.
	activeRequests metric.Int64UpDownCounter
	// materialReloads counts reloads of the TLS keypair, the CA bundle, and the
	// token files. A failed reload keeps the previous material, which is correct
	// but also makes a persistently broken file invisible until the certificate
	// expires — so failures are counted, not only logged. Both roles have material
	// to reload, and an operator wants to alert on either.
	materialReloads metric.Int64Counter

	// Reported by a serving gateway only; nil in a forwarder.
	//
	// io counts bytes moved between clients and the gateway, split by
	// network.io.direction: "receive" is what clients upload, "transmit" what
	// they download. Being a proxy, the same bytes cross the upstream leg.
	io metric.Int64Counter

	blobUploads      metric.Int64Counter
	blobUploadSize   metric.Int64Histogram
	blobDownloads    metric.Int64Counter
	blobDownloadSize metric.Int64Histogram

	// existenceChecks counts HEAD requests by hit/miss, the signal push clients
	// depend on to skip uploads that already exist upstream.
	existenceChecks metric.Int64Counter
	// blobCacheLookups counts the blob existence checks that could have been
	// answered from the blob existence cache, by whether they were. It is what
	// says whether the TTL and the memory bound are sized right; the requests it
	// counts are the subset of existenceChecks that are cacheable at all.
	blobCacheLookups metric.Int64Counter
	// errors counts failures by error.type and upstream registry.
	errors metric.Int64Counter

	// upstreamDuration measures the upstream leg (until response headers), so a
	// slow registry can be told apart from a slow gateway.
	upstreamDuration metric.Float64Histogram
	// authHandshakes counts upstream ping + token exchanges, which happen once
	// per repository and scope and are then cached.
	authHandshakes metric.Int64Counter

	policyDecisions metric.Int64Counter
	policyReloads   metric.Int64Counter

	// Reported by a forwarding gateway only; nil in a server.
	//
	// peerDuration measures the hop to the peer gateway, until it returned
	// response headers. Its count series is the relayed request rate, and its
	// network.protocol.version attribute is how an operator confirms the hop is
	// really using HTTP/2 rather than having silently fallen back.
	peerDuration metric.Float64Histogram
	// peerConnections counts connections opened to the peer. Divided into
	// peerDuration's count it gives requests per connection, which is the one
	// number that says whether multiplexing is working: it should be far above 1.
	peerConnections metric.Int64Counter
	// forwardErrors counts failures of a forwarder, by error.type. It is a
	// separate instrument from errors so a fleet-wide sum of either one is
	// meaningful on its own.
	forwardErrors metric.Int64Counter

	registries *boundedValues
	uploads    *uploadTracker
}

// failures returns the error counter of this role.
func (m *metrics) failures() metric.Int64Counter {
	if m.errors != nil {
		return m.errors
	}
	return m.forwardErrors
}

// newMetrics creates a serving gateway's instruments from mp (the global provider
// when mp is nil), plus the asynchronous instruments backed by the callbacks in
// sources.
//
// Instrument creation only fails for invalid names or units, which are compile
// time constants here; on failure the returned metrics is still fully usable
// (the API hands out no-op instruments alongside the error) so callers can log
// and carry on serving.
func newMetrics(mp metric.MeterProvider, sources gaugeSources) (*metrics, error) {
	b := newInstruments(mp)
	m := b.shared()
	m.io = b.counter("oci.gateway.io", "By",
		"Bytes transferred between clients and the gateway (receive: client uploads, transmit: client downloads).")
	m.blobUploads = b.counter("oci.gateway.blob.uploads", "{blob}",
		"Blob uploads completed upstream, including cross-repository mounts.")
	m.blobUploadSize = b.bytesHistogram("oci.gateway.blob.upload.size",
		"Size of blobs uploaded through the gateway.")
	m.blobDownloads = b.counter("oci.gateway.blob.downloads", "{blob}",
		"Blob downloads served to completion.")
	m.blobDownloadSize = b.bytesHistogram("oci.gateway.blob.download.size",
		"Size of blobs downloaded through the gateway.")
	m.existenceChecks = b.counter("oci.gateway.existence_checks", "{check}",
		"HEAD requests for blobs and manifests, by hit or miss upstream.")
	m.blobCacheLookups = b.counter("oci.gateway.blob_existence_cache.lookups", "{lookup}",
		"Cacheable blob existence checks, by whether the blob existence cache could answer them.")
	m.errors = b.counter("oci.gateway.errors", "{error}",
		"Failures serving or forwarding registry requests, by error type.")
	m.upstreamDuration = b.seconds("oci.gateway.upstream.duration",
		"Time until the upstream registry returned response headers.")
	m.authHandshakes = b.counter("oci.gateway.upstream.auth_handshakes", "{handshake}",
		"Upstream authentication handshakes (ping and token exchange), which are cached per repository and scope.")
	m.policyDecisions = b.counter("oci.gateway.policy.decisions", "{decision}",
		"Authorization decisions taken against the active policy.")
	m.policyReloads = b.counter("oci.gateway.policy.reloads", "{reload}",
		"Policy file reloads, by outcome. A failure means the previous policy is still in force.")
	m.uploads = newUploadTracker(maxUploadSessions)

	b.observe(sources)
	return m, b.err()
}

// gaugeSources are the callbacks behind a serving gateway's asynchronous
// instruments: state the process holds rather than events it counts. A nil field
// omits its instruments.
type gaugeSources struct {
	// policyRules reports how many rules the active policy has.
	policyRules func() int64
	// blobCache reports the blob existence cache's occupancy and evictions. It is
	// nil when the cache is disabled, which is how the cache's instruments stay
	// absent rather than reporting a flat zero.
	blobCache func() cacheStats
}

// observe registers the asynchronous instruments described by sources.
func (b *instruments) observe(sources gaugeSources) {
	if sources.policyRules != nil {
		_, err := b.meter.Int64ObservableGauge("oci.gateway.policy.rules",
			metric.WithUnit("{rule}"),
			metric.WithDescription("Number of rules in the policy this instance has loaded."),
			metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
				o.Observe(sources.policyRules())
				return nil
			}),
		)
		b.track(err)
	}
	if sources.blobCache == nil {
		return
	}
	// One callback for the three, so a scrape reads the cache's counters once and
	// reports a consistent picture of them.
	entries, err := b.meter.Int64ObservableGauge("oci.gateway.blob_existence_cache.entries",
		metric.WithUnit("{entry}"),
		metric.WithDescription("Blobs the existence cache is currently holding, including any whose TTL has passed but which have not been looked up or evicted since."),
	)
	b.track(err)
	capacity, err := b.meter.Int64ObservableGauge("oci.gateway.blob_existence_cache.capacity",
		metric.WithUnit("{entry}"),
		metric.WithDescription("Blobs the existence cache has room for. Entries divided by this is how full it is; the memory behind it is allocated up front, so this does not move."),
	)
	b.track(err)
	evictions, err := b.meter.Int64ObservableCounter("oci.gateway.blob_existence_cache.evictions",
		metric.WithUnit("{entry}"),
		metric.WithDescription("Entries the existence cache dropped, by reason. A capacity eviction rate above zero means the memory bound, not the TTL, is deciding how long blobs are remembered."),
	)
	b.track(err)
	_, err = b.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		stats := sources.blobCache()
		o.ObserveInt64(entries, stats.entries)
		o.ObserveInt64(capacity, stats.capacity)
		o.ObserveInt64(evictions, stats.evictedCapacity, metric.WithAttributes(attrEvictionReason.String(evictedForCapacity)))
		o.ObserveInt64(evictions, stats.evictedExpired, metric.WithAttributes(attrEvictionReason.String(evictedForExpiry)))
		return nil
	}, entries, capacity, evictions)
	b.track(err)
}

// newForwardMetrics creates a forwarding gateway's instruments. It deliberately
// creates none of the registry-shaped ones: a forwarder relays traffic the serving
// gateway reports in full, so re-exporting blob counts, blob sizes,
// existence-check results or bandwidth would double every fleet-wide sum. What it
// adds instead describes the hop, which is the part only a forwarder can see.
func newForwardMetrics(mp metric.MeterProvider) (*metrics, error) {
	b := newInstruments(mp)
	m := b.shared()
	m.peerDuration = b.seconds("oci.gateway.forward.peer.duration",
		"Time until the peer gateway returned response headers. Its count is the relayed request rate.")
	m.peerConnections = b.counter("oci.gateway.forward.peer.connections", "{connection}",
		"Connections opened to the peer gateway. Relayed requests divided by this is requests per connection, which is how multiplexing is verified.")
	m.forwardErrors = b.counter("oci.gateway.forward.errors", "{error}",
		"Failures relaying requests to the peer gateway, by error type.")
	return m, b.err()
}

// instruments is the small builder both constructors share.
type instruments struct {
	meter metric.Meter
	errs  []error
}

func newInstruments(mp metric.MeterProvider) *instruments {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	return &instruments{meter: mp.Meter(meterName)}
}

// shared creates the instruments both roles report.
func (b *instruments) shared() *metrics {
	m := &metrics{
		requestDuration: b.seconds("http.server.request.duration",
			"Duration of registry requests served by the gateway."),
		materialReloads: b.counter("oci.gateway.material.reloads", "{reload}",
			"Reloads of on-disk TLS and token material, by material and outcome. A failure means the previous material is still in force."),
		registries: newBoundedValues(maxRegistryValues),
	}
	activeRequests, err := b.meter.Int64UpDownCounter("http.server.active_requests",
		metric.WithUnit("{request}"),
		metric.WithDescription("Registry requests the gateway is currently serving."),
	)
	b.track(err)
	m.activeRequests = activeRequests
	return m
}

func (b *instruments) track(err error) {
	if err != nil {
		b.errs = append(b.errs, err)
	}
}

func (b *instruments) err() error { return errors.Join(b.errs...) }

func (b *instruments) seconds(name, desc string) metric.Float64Histogram {
	h, err := b.meter.Float64Histogram(name,
		metric.WithUnit("s"),
		metric.WithDescription(desc),
		metric.WithExplicitBucketBoundaries(durationBuckets...),
	)
	b.track(err)
	return h
}

func (b *instruments) bytesHistogram(name, desc string) metric.Int64Histogram {
	h, err := b.meter.Int64Histogram(name,
		metric.WithUnit("By"),
		metric.WithDescription(desc),
		metric.WithExplicitBucketBoundaries(sizeBuckets...),
	)
	b.track(err)
	return h
}

func (b *instruments) counter(name, unit, desc string) metric.Int64Counter {
	c, err := b.meter.Int64Counter(name, metric.WithUnit(unit), metric.WithDescription(desc))
	b.track(err)
	return c
}

// observation accumulates one request's measurements. Attributes are discovered
// as the request is processed (the upstream registry and operation are only
// known after classification), so the request-level instruments are recorded
// once, from [observation.finish].
type observation struct {
	m     *metrics
	start time.Time

	// registry and operation form the attribute pair shared by most
	// measurements; they are filled in by setUpstream once resolved.
	registry  string
	operation string
	route     string
	// host is the resolved upstream host as it really is, without the cardinality
	// cap applied to the registry attribute. It is used for log lines and for
	// keying upload sessions, where an exact value matters.
	host string
	// errType is the error.type reported on the duration histogram. It is set by
	// the first failure recorded for the request.
	errType string
	// principal is the authenticated client, when the listener requires
	// authentication. It appears in log lines only, never as a metric attribute:
	// on a build farm the set of clients is operator-controlled but the audit
	// trail is the log, and keeping it out of the metrics keeps cardinality
	// bounded by construction.
	principal string
	// requestID is the cross-hop correlation id a forwarding gateway sent, so a
	// build action's request can be tied to this gateway's decision.
	requestID string
	// peer is the gateway a forwarder relays to; empty when serving.
	peer string

	method string
	scheme string
	// activeAttrs is held so the active-requests counter is decremented with
	// exactly the attribute set it was incremented with.
	activeAttrs attribute.Set

	w    *countingResponseWriter
	body *countingReader
}

// begin starts observing a request: it starts the clock, counts the request as
// in flight, and returns wrappers that count the bytes crossing the client
// connection. The caller must call [observation.finish] when the request is done.
func (m *metrics) begin(w http.ResponseWriter, r *http.Request) (*observation, http.ResponseWriter, *http.Request) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	method := normalizedMethod(r.Method)
	o := &observation{
		m:         m,
		start:     time.Now(),
		registry:  registryUnknown,
		host:      registryUnknown,
		operation: opNameUnknown,
		method:    method,
		scheme:    scheme,
		activeAttrs: attribute.NewSet(
			semconv.HTTPRequestMethodKey.String(method),
			semconv.URLScheme(scheme),
		),
		w: &countingResponseWriter{ResponseWriter: w},
	}
	o.requestID = r.Header.Get(requestIDHeader)
	m.activeRequests.Add(r.Context(), 1, metric.WithAttributeSet(o.activeAttrs))

	// Count what the client uploads. The body is swapped on a shallow copy of the
	// request so the caller's request is left as it was. A server request always
	// has a non-nil Body, but a hand-built request in a test may not.
	if r.Body != nil {
		o.body = &countingReader{ReadCloser: r.Body}
		r = r.WithContext(r.Context())
		r.Body = o.body
	}
	return o, o.w, r
}

// setUpstream records the resolved upstream registry and the classified
// operation, which become attributes of every subsequent measurement.
func (o *observation) setUpstream(registry string, cls request) {
	if registry != "" {
		o.host = registry
	}
	o.registry = o.m.registries.value(registry)
	if cls.op != "" {
		o.operation = cls.op
	}
	o.route = cls.route
}

// attrs returns the attribute set shared by the gateway's domain metrics: the
// upstream registry and the operation, plus any extras.
func (o *observation) attrs(extra ...attribute.KeyValue) metric.MeasurementOption {
	kv := make([]attribute.KeyValue, 0, len(extra)+2)
	kv = append(kv, attrRegistry.String(o.registry), attrOperation.String(o.operation))
	kv = append(kv, extra...)
	return metric.WithAttributes(kv...)
}

// logContext renders the audit fields that belong on every log line of a request
// but are empty in the single-hop deployment: who the client authenticated as,
// and the id correlating this request with the gateway it came through. Both are
// quoted, because they can originate outside this process.
func (o *observation) logContext() string {
	if o == nil || (o.principal == "" && o.requestID == "") {
		return ""
	}
	var b strings.Builder
	if o.principal != "" {
		fmt.Fprintf(&b, " client=%q", o.principal)
	}
	if o.requestID != "" {
		fmt.Fprintf(&b, " request=%q", o.requestID)
	}
	return b.String()
}

// fail records a failure of errType and remembers it as the request's
// error.type. Only the first failure is reported for a request: the later ones
// are consequences of it (a refused upstream connection is also a failed
// request), and counting both would double count. The counter it lands in depends
// on the role, so a fleet-wide sum of either is meaningful on its own.
func (o *observation) fail(ctx context.Context, errType string) {
	if o.errType != "" {
		return
	}
	o.errType = errType
	o.m.failures().Add(ctx, 1, o.attrs(semconv.ErrorTypeKey.String(errType)))
}

// policyDecision records an authorization decision.
func (o *observation) policyDecision(ctx context.Context, allow bool) {
	decision := "deny"
	if allow {
		decision = "allow"
	}
	o.m.policyDecisions.Add(ctx, 1, o.attrs(attrDecision.String(decision)))
}

// authHandshake records the result of an upstream ping + token exchange.
func (o *observation) authHandshake(ctx context.Context, err error) {
	result := resultSuccess
	if err != nil {
		result = resultFailure
	}
	o.m.authHandshakes.Add(ctx, 1, o.attrs(attrResult.String(result)))
}

// upstreamResponse records the upstream leg of the request: how long the
// registry took to return headers, whether its status counts as a failure, and
// the hit/miss result of an existence check.
func (o *observation) upstreamResponse(ctx context.Context, cls request, status int, latency time.Duration) {
	o.m.upstreamDuration.Record(ctx, latency.Seconds(), o.attrs(
		semconv.HTTPRequestMethodKey.String(o.method),
		semconv.HTTPResponseStatusCode(status),
	))
	if cls.existenceCheck() {
		o.m.existenceChecks.Add(ctx, 1, o.attrs(attrResult.String(existenceResult(status))))
	}
	if errType := statusErrorType(status); errType != "" {
		o.fail(ctx, errType)
	}
}

// blobCacheLookup records whether a cacheable blob existence check was answered
// by the blob existence cache.
//
// A hit is *also* counted as an existence check that found the blob, which
// [observation.upstreamResponse] would have done had the request gone upstream.
// oci.gateway.existence_checks answers "how often was the content already
// there", and leaving cached answers out of it would turn it into a function of
// how the cache is configured.
func (o *observation) blobCacheLookup(ctx context.Context, hit bool) {
	result := resultMiss
	if hit {
		result = resultHit
	}
	o.m.blobCacheLookups.Add(ctx, 1, o.attrs(attrResult.String(result)))
	if hit {
		o.m.existenceChecks.Add(ctx, 1, o.attrs(attrResult.String(resultHit)))
	}
}

// peerResponse records the peer leg of a request a forwarding gateway relayed.
//
// It is deliberately *not* [observation.upstreamResponse]: bandwidth, blob counts
// and existence-check results are reported by the serving gateway for this very
// traffic, so a forwarder repeating them would double every fleet-wide sum. What
// is recorded here is only what the forwarder alone can see — how long its hop
// took, over which protocol version, and how many connections it needed.
//
// The failure classification is peer-aware: a 401 or 403 the peer produced itself
// means this gateway's peer credential is wrong, which is a completely different
// problem from a registry rejecting the gateway's registry credential.
// gatewayError is the peer's diagnostic header, empty when the status is the
// registry's own answer travelling back.
func (o *observation) peerResponse(ctx context.Context, status int, proto, gatewayError string, latency time.Duration, newConnections int64) {
	protoVersion := protocolVersion(proto)
	o.m.peerDuration.Record(ctx, latency.Seconds(), o.attrs(
		semconv.HTTPRequestMethodKey.String(o.method),
		semconv.HTTPResponseStatusCode(status),
		semconv.NetworkProtocolVersion(protoVersion),
	))
	if newConnections > 0 {
		o.m.peerConnections.Add(ctx, newConnections, metric.WithAttributes(
			attrPeer.String(o.peer),
			semconv.NetworkProtocolVersion(protoVersion),
		))
	}
	if errType := peerStatusErrorType(gatewayError, status); errType != "" {
		o.fail(ctx, errType)
	}
}

// protocolVersion maps an [http.Response.Proto] to the semantic-convention
// network.protocol.version value ("1.1", "2"), which is what makes it possible to
// see at a glance whether a hop is really multiplexed.
func protocolVersion(proto string) string {
	version := strings.TrimPrefix(proto, "HTTP/")
	if version == "2.0" {
		return "2"
	}
	if version == "" {
		return "unknown"
	}
	return version
}

// existenceResult maps the upstream status of a HEAD request to a hit, a miss,
// or an error.
func existenceResult(status int) string {
	switch {
	case status >= 200 && status < 300:
		return resultHit
	case status == http.StatusNotFound:
		return resultMiss
	default:
		return resultError
	}
}

// recordTransfer accounts for the blob bytes of a completed request: a download
// that was streamed to the client in full, or an upload that reached the
// registry. Requests that moved bytes without completing a blob transfer are
// still covered by the io counter recorded in finish.
func (o *observation) recordTransfer(ctx context.Context, r *http.Request, cls request, status int, copyErr error) {
	switch cls.op {
	case opNameBlobRead:
		// Count only complete downloads: a partial (206) response or an aborted
		// copy says nothing about the blob's size.
		if status == http.StatusOK && copyErr == nil {
			o.m.blobDownloads.Add(ctx, 1, o.attrs())
			o.m.blobDownloadSize.Record(ctx, o.w.written, o.attrs())
		}
	case opNameBlobUpload:
		o.recordUpload(ctx, r, cls, status)
	}
}

// recordUpload accounts for one step of a blob upload session. Only the request
// that completes the upload is counted as an upload; the bytes of the
// intermediate PATCH requests are remembered so the completing PUT can report
// the blob's full size.
func (o *observation) recordUpload(ctx context.Context, r *http.Request, cls request, status int) {
	key := o.host + "|" + r.URL.Path
	switch r.Method {
	case http.MethodPost:
		switch {
		case status != http.StatusCreated:
			// 202 Accepted only opens a session; nothing has been stored yet.
		case cls.mountFrom != "":
			// A cross-repository mount adds a blob to the repository without
			// transferring any bytes, so it has no size to record.
			o.m.blobUploads.Add(ctx, 1, o.attrs(attrUploadKind.String(uploadMount)))
		default:
			// Single request ("monolithic") upload: POST with ?digest= and body.
			o.completeUpload(ctx, o.received(), uploadMonolithic)
		}
	case http.MethodPatch:
		// A chunk was accepted. Remember it for the completing PUT.
		if status == http.StatusAccepted || status == http.StatusCreated {
			o.m.uploads.add(key, o.received())
		}
	case http.MethodPut:
		chunked := o.m.uploads.take(key)
		if status != http.StatusCreated {
			return
		}
		switch total := chunked + o.received(); {
		case chunked > 0:
			o.completeUpload(ctx, total, uploadChunked)
		case total > 0:
			o.completeUpload(ctx, total, uploadMonolithic)
		default:
			// The session's chunks were not seen by this instance (a registry may
			// hand out a different upload path per step, and in a cluster the
			// chunks may have gone to another replica), so the size is unknown.
			o.completeUpload(ctx, 0, uploadUnknown)
		}
	case http.MethodDelete:
		// The client cancelled the session; drop what we accumulated for it.
		o.m.uploads.take(key)
	}
}

// completeUpload counts one blob stored upstream, recording its size when known.
func (o *observation) completeUpload(ctx context.Context, size int64, kind string) {
	o.m.blobUploads.Add(ctx, 1, o.attrs(attrUploadKind.String(kind)))
	if size > 0 {
		o.m.blobUploadSize.Record(ctx, size, o.attrs(attrUploadKind.String(kind)))
	}
}

// received reports the bytes read from the client's request body so far.
func (o *observation) received() int64 {
	if o.body == nil {
		return 0
	}
	return o.body.count()
}

// finish records the request-level measurements and drops the request from the
// in-flight count. It must be called exactly once per [metrics.begin].
func (o *observation) finish(ctx context.Context) {
	o.m.activeRequests.Add(ctx, -1, metric.WithAttributeSet(o.activeAttrs))

	kv := make([]attribute.KeyValue, 0, 7)
	kv = append(kv,
		attrRegistry.String(o.registry),
		attrOperation.String(o.operation),
		semconv.HTTPRequestMethodKey.String(o.method),
		semconv.URLScheme(o.scheme),
		semconv.HTTPResponseStatusCode(o.w.statusCode()),
	)
	if o.route != "" {
		kv = append(kv, semconv.HTTPRoute(o.route))
	}
	if o.errType != "" {
		kv = append(kv, semconv.ErrorTypeKey.String(o.errType))
	}
	o.m.requestDuration.Record(ctx, time.Since(o.start).Seconds(), metric.WithAttributes(kv...))

	// Bandwidth is a serving-gateway measurement: a forwarder moves the very same
	// bytes, so counting them here too would double the fleet's reported traffic.
	if o.m.io == nil {
		return
	}
	if n := o.received(); n > 0 {
		o.m.io.Add(ctx, n, o.attrs(semconv.NetworkIODirectionReceive))
	}
	if n := o.w.written; n > 0 {
		o.m.io.Add(ctx, n, o.attrs(semconv.NetworkIODirectionTransmit))
	}
}

// recordReload records the outcome of a policy file reload. A failure means the
// gateway kept the previous policy, so it is reported as its own metric rather
// than as a request error.
func (m *metrics) recordReload(ctx context.Context, err error) {
	result := resultSuccess
	if err != nil {
		result = resultFailure
	}
	m.policyReloads.Add(ctx, 1, metric.WithAttributes(attrResult.String(result)))
}

// recordMaterialReload records the outcome of reloading one piece of on-disk TLS
// or token material.
func (m *metrics) recordMaterialReload(ctx context.Context, material string, err error) {
	result := resultSuccess
	if err != nil {
		result = resultFailure
	}
	m.materialReloads.Add(ctx, 1, metric.WithAttributes(
		attrMaterial.String(material), attrResult.String(result)))
}

// normalizedMethod maps an HTTP method to the semantic-convention
// http.request.method value, collapsing anything unrecognized into "_OTHER" so
// a client cannot mint one attribute value per invented method.
func normalizedMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions,
		http.MethodConnect, http.MethodTrace:
		return method
	default:
		return "_OTHER"
	}
}

// countingResponseWriter counts the response body bytes written to the client
// and remembers the status code.
type countingResponseWriter struct {
	http.ResponseWriter
	status  int
	written int64
}

func (w *countingResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *countingResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.written += int64(n)
	return n, err
}

// Flush forwards to the wrapped writer so streaming responses are not held back
// by the wrapper.
func (w *countingResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets [http.ResponseController] reach the underlying writer.
func (w *countingResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// statusCode reports the response status, defaulting to the 200 net/http writes
// implicitly when a handler only writes a body.
func (w *countingResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// countingReader counts the bytes read from a request body. The count is read
// from the serving goroutine after the body has been forwarded, but a client
// disconnect can make net/http close the body concurrently, so it is guarded.
type countingReader struct {
	io.ReadCloser
	mu sync.Mutex
	n  int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.mu.Lock()
		r.n += int64(n)
		r.mu.Unlock()
	}
	return n, err
}

func (r *countingReader) count() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// boundedValues caps the number of distinct values reported for an attribute
// whose values are not under the gateway's control. The first limit values are
// reported as they are; every later one collapses into [registryOverflow], so a
// client cannot drive unbounded time series creation in the metrics backend.
type boundedValues struct {
	mu    sync.RWMutex
	seen  map[string]struct{}
	limit int
}

func newBoundedValues(limit int) *boundedValues {
	return &boundedValues{seen: make(map[string]struct{}), limit: limit}
}

func (b *boundedValues) value(v string) string {
	if v == "" {
		return registryUnknown
	}
	b.mu.RLock()
	_, ok := b.seen[v]
	full := len(b.seen) >= b.limit
	b.mu.RUnlock()
	switch {
	case ok:
		return v
	case full:
		return registryOverflow
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.seen[v]; !ok {
		if len(b.seen) >= b.limit {
			return registryOverflow
		}
		b.seen[v] = struct{}{}
	}
	return v
}

// uploadTracker remembers how many bytes each in-flight blob upload session has
// transferred, so the PUT that completes the session can report the blob's total
// size. Clients (including go-containerregistry, which rules_img uses) upload a
// blob as POST, one or more PATCH requests carrying the bytes, then a PUT with
// no body, so without this the completing request would report a zero-byte blob.
//
// Sessions are keyed by upstream registry and upload path, and are dropped when
// the upload completes or is cancelled. Sessions that are simply abandoned would
// otherwise accumulate, so the table is generational: once the live generation
// reaches limit entries it becomes the old generation and a fresh one starts,
// which bounds the table at 2*limit entries without any timers or scans.
type uploadTracker struct {
	mu       sync.Mutex
	cur, old map[string]int64
	limit    int
}

func newUploadTracker(limit int) *uploadTracker {
	return &uploadTracker{cur: make(map[string]int64), old: make(map[string]int64), limit: limit}
}

// add accumulates n bytes for the session identified by key.
func (t *uploadTracker) add(key string, n int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if have, ok := t.cur[key]; ok {
		t.cur[key] = have + n
		return
	}
	if have, ok := t.old[key]; ok {
		delete(t.old, key)
		n += have
	}
	if len(t.cur) >= t.limit {
		t.old = t.cur
		t.cur = make(map[string]int64, t.limit)
	}
	t.cur[key] = n
}

// take removes the session identified by key and returns the bytes accumulated
// for it, or 0 if it is not tracked.
func (t *uploadTracker) take(key string) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n, ok := t.cur[key]; ok {
		delete(t.cur, key)
		return n
	}
	if n, ok := t.old[key]; ok {
		delete(t.old, key)
		return n
	}
	return 0
}
