package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/metric"

	clientgateway "github.com/bazel-contrib/rules_img/img_tool/pkg/gateway"
)

// This file implements forwarding mode: a gateway that holds no registry
// credentials and no policy, and relays requests to another gateway which does.
// It exists so a build farm can keep its registry credentials in one shared,
// auditable deployment instead of in every worker pod: the forwarder runs as a
// sidecar on a UNIX socket the build actions can reach, and speaks to the serving
// gateway over one multiplexed HTTP/2 connection.
//
// The hop it adds is byte-identical to the hop a build action already makes. The
// gateway protocol — an anonymous OCI distribution request plus the
// X-rules_img-Original-Host header — is already a forwarding protocol, so the
// serving gateway needs no protocol change at all; it only gains client
// authentication. Everything that gives an OCI proxy its fidelity (status codes,
// Docker-Content-Digest, Location, Range, WWW-Authenticate, the registry's own
// JSON error bodies, per-request cancellation, streaming with backpressure) is
// inherited from net/http rather than re-encoded, which is the whole reason not
// to tunnel this over an RPC of our own.
//
// The forwarder is a *pure pass-through*. It classifies requests for metrics but
// never rejects one on its own: sidecars live in long-lived worker pods and will
// lag the serving deployment's version, so a sidecar that second-guessed the
// policy could refuse something the serving gateway would have allowed. The
// serving gateway is the only authorization point, and it rejects a bad request
// before opening any upstream connection anyway.
//
// A forwarder does NOT re-export the registry-shaped instruments. It relays the
// very traffic a serving gateway then reports in full, so repeating blob counts,
// blob sizes, existence-check results or bandwidth would double every fleet-wide
// sum of them. It reports what only it can see instead — its own hop, under
// oci.gateway.forward.* names — plus the semantic-convention http.server.* pair,
// which every HTTP service exports and which is therefore always read with a
// service.name filter. The two roles carry different service.name values.

// copyBufferSize is the size of the buffers the response copy borrows from a
// pool. httputil.ReverseProxy otherwise allocates a fresh 32 KiB buffer for
// every request, and every blob transfer goes through this path.
const copyBufferSize = 256 << 10

// ForwardConfig configures a [ForwardHandler].
type ForwardConfig struct {
	// Peer is the serving gateway to relay to: scheme and host only.
	Peer *url.URL
	// Transport performs the peer round trips. It should be configured for
	// HTTP/2 so concurrent requests share one connection.
	Transport http.RoundTripper
	// Credential returns the bearer token to present to the peer, or "" for none.
	// It is called per request so a rotated token — in particular a projected
	// Kubernetes ServiceAccount token, which kubelet replaces under the running
	// pod — is picked up without a restart.
	Credential func(context.Context) (string, error)
	// ForwarderID identifies this forwarder in the peer's decision log. Defaults
	// to the hostname.
	ForwarderID string
	// Logger records forwarded requests. Defaults to the standard logger.
	Logger *log.Logger
	// MeterProvider is the OpenTelemetry meter provider. Nil means the global
	// one, which discards everything until a binary installs an SDK.
	MeterProvider metric.MeterProvider
}

// ForwardHandler is an [http.Handler] that relays gateway requests to a peer
// gateway.
type ForwardHandler struct {
	peer        *url.URL
	credential  func(context.Context) (string, error)
	forwarderID string
	log         *log.Logger
	metrics     *metrics
	proxy       *httputil.ReverseProxy
}

// NewForward constructs a [ForwardHandler].
func NewForward(cfg ForwardConfig) (*ForwardHandler, error) {
	if cfg.Peer == nil {
		return nil, errors.New("a peer gateway is required")
	}
	f := &ForwardHandler{
		peer:        cfg.Peer,
		credential:  cfg.Credential,
		forwarderID: cfg.ForwarderID,
		log:         cfg.Logger,
	}
	if f.log == nil {
		f.log = log.Default()
	}
	if f.forwarderID == "" {
		f.forwarderID, _ = os.Hostname()
	}
	// A forwarder reports its own hop, not the traffic the serving gateway already
	// reports in full: see newForwardMetrics.
	m, err := newForwardMetrics(cfg.MeterProvider)
	if err != nil {
		f.log.Printf("warning: creating metric instruments: %v", err)
	}
	f.metrics = m

	f.proxy = &httputil.ReverseProxy{
		Rewrite:        f.rewrite,
		Transport:      cfg.Transport,
		BufferPool:     &bufferPool{},
		ModifyResponse: f.modifyResponse,
		ErrorHandler:   f.peerError,
		ErrorLog:       f.log,
	}
	return f, nil
}

// forwardState carries per-request state between ServeHTTP, the proxy's Rewrite
// hook, and its ModifyResponse hook. It travels on the request context, which the
// outbound request shares with the inbound one.
type forwardState struct {
	obs        *observation
	cls        request
	credential string
	started    time.Time
	// peerLatency is the time until the peer returned response headers, filled in
	// by modifyResponse.
	peerLatency time.Duration
	// proto is the HTTP version the peer answered over. It is the signal that says
	// whether the hop is really multiplexed or has silently fallen back to
	// HTTP/1.1, which costs one connection per concurrent request.
	proto string
	// newConnections counts connections this request had to open to the peer,
	// which is zero for every request that reused a multiplexed one.
	newConnections int64
	// responded reports that the peer answered at all. When it did not — a refused
	// dial, a TLS failure — there is no peer leg to measure and no transfer to
	// account for; the failure itself is already recorded by peerError.
	responded bool
	// gatewayError is the peer's X-rules_img-Gateway-Error, set only when the peer
	// itself rejected us rather than passing a registry's answer back.
	gatewayError string
}

type forwardStateKey struct{}

// ServeHTTP implements [http.Handler].
func (f *ForwardHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	obs, w, r := f.metrics.begin(w, r)
	defer obs.finish(r.Context())

	// The one thing a forwarder does not pass through. The reserved /_rules_img/
	// paths are the gateway's own control surface — cache replication, which is
	// authorized by the peer's *identity* rather than by the policy — and this
	// gateway's credential is what the serving gateway would see. Relaying one
	// would let a build action write to a shared existence cache under the
	// forwarder's identity, so it stops here.
	if strings.HasPrefix(r.URL.EscapedPath(), reservedPathPrefix) {
		obs.fail(r.Context(), errUnsupportedEndpoint)
		f.log.Printf("%s %q -> 404: a forwarding gateway does not relay the gateway's own control endpoints", r.Method, r.URL.EscapedPath())
		writeOCIError(w, http.StatusNotFound, "UNSUPPORTED", "a forwarding gateway relays registry requests only")
		return
	}

	// Classify for observation only. An unrecognized path is still forwarded: the
	// serving gateway decides what is allowed, and a sidecar must never reject
	// something its (possibly newer) peer would accept.
	cls, _ := classify(r)
	obs.setUpstream(r.Header.Get(clientgateway.OriginalHostHeader), cls)
	obs.peer = f.peer.Host

	state := &forwardState{obs: obs, cls: cls, started: time.Now()}
	if f.credential != nil {
		token, err := f.credential(r.Context())
		if err != nil {
			// Reading a mounted credential is normally instant, so this is either a
			// misconfigured path or a mount being swapped. Report it as a retryable
			// gateway condition rather than forwarding an anonymous request the peer
			// would reject with a far less useful message.
			obs.fail(r.Context(), errPeerUnauthorized)
			w.Header().Set("Retry-After", "1")
			w.Header().Set(gatewayErrorHeader, errPeerUnauthorized)
			f.log.Printf("%s %q -> 503: reading the peer credential failed: %v", r.Method, r.URL.EscapedPath(), err)
			writeOCIError(w, http.StatusServiceUnavailable, "UNAVAILABLE",
				"this gateway could not read its peer credential")
			return
		}
		state.credential = token
	}
	// Count the connections this request needed. GotConn fires once per request
	// with Reused=false only when a fresh connection was opened, so with HTTP/2
	// working this stays at zero for every request after the first — which is
	// exactly the signal that says multiplexing is happening.
	ctx := context.WithValue(r.Context(), forwardStateKey{}, state)
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			if !info.Reused {
				atomic.AddInt64(&state.newConnections, 1)
			}
		},
	})
	r = r.WithContext(ctx)

	// ReverseProxy signals a copy failure after the response header is already on
	// the wire by panicking with http.ErrAbortHandler. Record it, then let the
	// panic continue so net/http still aborts the response — otherwise an
	// interrupted multi-gigabyte download would be indistinguishable from a
	// completed one in this gateway's metrics. The deferred finish above still
	// runs, since deferred functions run while a panic unwinds.
	defer func() {
		if v := recover(); v != nil {
			if v == http.ErrAbortHandler { //nolint:errorlint // the sentinel is compared, not wrapped
				obs.fail(r.Context(), errTransferAborted)
				f.log.Printf("%s %q (host=%s%s): peer response aborted mid-body", r.Method, r.URL.EscapedPath(), obs.host, obs.logContext())
			}
			panic(v)
		}
	}()

	f.proxy.ServeHTTP(w, r)

	status := obs.w.statusCode()
	if state.responded {
		obs.peerResponse(r.Context(), status, state.proto, state.gatewayError,
			state.peerLatency, atomic.LoadInt64(&state.newConnections))
	}
	f.log.Printf("%s %q (host=%s%s) -> %d", r.Method, r.URL.EscapedPath(), obs.host, obs.logContext(), status)
}

// stateFrom recovers the per-request state from a request's context.
func stateFrom(r *http.Request) *forwardState {
	if r == nil {
		return nil
	}
	state, _ := r.Context().Value(forwardStateKey{}).(*forwardState)
	return state
}

// rewrite turns an inbound request into the request sent to the peer.
func (f *ForwardHandler) rewrite(pr *httputil.ProxyRequest) {
	pr.SetURL(f.peer)

	// Restore the query byte-for-byte. ReverseProxy re-encodes RawQuery through
	// url.ParseQuery/url.Values.Encode before Rewrite is called whenever it sees a
	// ';', a malformed '%' escape, or a very large number of parameters (and
	// unconditionally under GODEBUG=urlmaxqueryparams). That sorts the keys and
	// silently drops ';'-joined pairs — and an OCI upload session is addressed by
	// an opaque "?_state=<token>" the registry hands us, which has to come back
	// exactly as it was issued or the upload breaks.
	pr.Out.URL.RawQuery = pr.In.URL.RawQuery

	// Never let a build action's Authorization header be mistaken for (or shadow)
	// our peer credential. Delete before setting, in that order.
	pr.Out.Header.Del("Authorization")
	if state := stateFrom(pr.Out); state != nil && state.credential != "" {
		pr.Out.Header.Set("Authorization", "Bearer "+state.credential)
	}

	// A client must not be able to forge provenance in the peer's decision log,
	// nor inject a diagnostic header. ReverseProxy has already removed Forwarded
	// and X-Forwarded-{For,Host,Proto} from the outbound request, and
	// SetXForwarded is deliberately not called: a worker pod's IP tells the shared
	// gateway nothing it should trust, whereas the authenticated identity does.
	pr.Out.Header.Del(forwardedByHeader)
	pr.Out.Header.Del(requestIDHeader)
	pr.Out.Header.Del(gatewayErrorHeader)
	pr.Out.Header.Set(forwardedByHeader, f.forwarderID)
	pr.Out.Header.Set(requestIDHeader, uuid.NewString())

	// Expect: 100-continue was already answered by net/http on the first read of
	// the body, so forwarding it would only make the peer wait for nothing.
	pr.Out.Header.Del("Expect")

	// X-rules_img-Original-Host is passed through untouched, and deliberately not
	// resolved first: the peer must see byte-for-byte what a single-hop gateway
	// would have seen, so its policy reaches the same decision either way.
}

// modifyResponse inspects the peer's response before it is copied back.
//
// It records the peer's latency, refuses a protocol upgrade, and converts the
// peer's *own* authentication failure into an error so [ForwardHandler.peerError]
// can answer with a status the img client interprets correctly. A response
// without the gateway error header is the registry's own answer (or the peer's
// policy decision) travelling back, and is passed through untouched.
func (f *ForwardHandler) modifyResponse(resp *http.Response) error {
	if state := stateFrom(resp.Request); state != nil {
		state.responded = true
		state.peerLatency = time.Since(state.started)
		state.proto = resp.Proto
		state.gatewayError = resp.Header.Get(gatewayErrorHeader)
	}
	if resp.StatusCode == http.StatusSwitchingProtocols {
		// No OCI distribution endpoint upgrades a connection, and the gateway's
		// byte-counting ResponseWriter is not an http.Hijacker, so letting
		// ReverseProxy attempt the upgrade would fail far more confusingly.
		return errors.New("peer attempted a protocol upgrade, which the gateway does not support")
	}
	if rejection := peerRejectionFrom(resp); rejection != nil {
		return rejection
	}
	return nil
}

// peerRejection is the error [ForwardHandler.modifyResponse] returns when the
// peer rejected this gateway's credential or identity, rather than relaying a
// registry's answer.
type peerRejection struct {
	// gatewayError is the peer's X-rules_img-Gateway-Error value.
	gatewayError string
}

func (e *peerRejection) Error() string {
	return "gateway peer rejected this gateway: " + e.gatewayError
}

// peerRejectionFrom reports whether resp is the peer refusing us. The header is
// what makes this unambiguous: a 401 or 403 is otherwise indistinguishable from
// the same status returned by the upstream registry. The status is checked too, so
// a stray header on an otherwise successful response cannot turn it into an error.
func peerRejectionFrom(resp *http.Response) *peerRejection {
	if resp.StatusCode < 400 {
		return nil
	}
	switch gatewayError := resp.Header.Get(gatewayErrorHeader); gatewayError {
	case errPeerUnauthenticated, errPeerBadCredential, errPeerIdentityDenied, errPeerAuthFailed:
		return &peerRejection{gatewayError: gatewayError}
	}
	return nil
}

// peerError answers a client when the peer could not be reached or refused us.
//
// The statuses are chosen against go-containerregistry's retry behaviour, since
// that is what the img client on the other end of hop 1 uses:
//
//   - A rejected credential becomes 502, never 401. A 401 would make go-cr's
//     bearer transport start a token exchange against this gateway's realm and
//     send an operator hunting for registry credentials instead of for a missing
//     client certificate. 502 is in go-cr's retry set, which is what we want:
//     with both ends polling their credential files, a token rotation leaves a
//     brief skew window that the client's patient backoff rides straight through.
//   - A rejected *identity* becomes 403, which go-cr does not retry: an
//     allow-list decision is deliberate and will not fix itself.
//   - A peer that could not *validate* our credential at all (its own validator
//     was unreachable) becomes 503 with Retry-After, like an unreachable peer:
//     nothing about the credential is known to be wrong.
//   - An unreachable peer becomes 503 with Retry-After: 1, which go-cr retries and
//     which the img tool's own Retry-After pacer honours.
func (f *ForwardHandler) peerError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := http.StatusServiceUnavailable, "UNAVAILABLE",
		fmt.Sprintf("gateway peer %s is unreachable: %v", f.peer.Host, err)
	errType := transportErrorType(err)

	var rejection *peerRejection
	switch {
	case errors.As(err, &rejection):
		errType = peerStatusErrorType(rejection.gatewayError, 0)
		switch rejection.gatewayError {
		case errPeerIdentityDenied:
			status, code = http.StatusForbidden, "DENIED"
			message = fmt.Sprintf("gateway peer %s does not allow this gateway's identity; check its --allowed-client-id or --allowed-serviceaccount", f.peer.Host)
		case errPeerAuthFailed:
			message = fmt.Sprintf("gateway peer %s could not validate this gateway's credential", f.peer.Host)
		default:
			status, code = http.StatusBadGateway, "UNAUTHORIZED"
			message = fmt.Sprintf("gateway peer %s rejected this gateway's credential (%s); check --peer-token-file and --peer-cert-file", f.peer.Host, rejection.gatewayError)
		}
	case errors.Is(err, context.Canceled):
		// The build action gave up; there is nobody left to answer.
		status, code = 499, "UNAVAILABLE"
		message = "client canceled the request"
	}

	if state := stateFrom(r); state != nil {
		state.obs.fail(r.Context(), errType)
	}
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "1")
	}
	w.Header().Set(gatewayErrorHeader, errType)
	f.log.Printf("%s %q -> %d %s: %s", r.Method, r.URL.EscapedPath(), status, code, message)
	writeOCIError(w, status, code, message)
}

// bufferPool hands out reusable copy buffers to [httputil.ReverseProxy].
type bufferPool struct {
	pool sync.Pool
}

func (p *bufferPool) Get() []byte {
	if b, ok := p.pool.Get().(*[]byte); ok {
		return *b
	}
	return make([]byte, copyBufferSize)
}

func (p *bufferPool) Put(b []byte) { p.pool.Put(&b) }

// RecordMaterialReload counts a reload of on-disk material, mirroring
// [Handler.RecordMaterialReload] so a forwarder reports its own certificate and
// token reloads.
func (f *ForwardHandler) RecordMaterialReload(material string, err error) {
	f.metrics.recordMaterialReload(context.Background(), material, err)
}
