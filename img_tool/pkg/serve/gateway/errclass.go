package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// This file defines the error taxonomy behind the error.type attribute of the
// oci.gateway.errors counter (and of http.server.request.duration). The values
// are a fixed, low-cardinality set — never a Go error string — and are grouped
// so that an operator can answer "is this us, the client, the network, or the
// upstream registry?" without reading logs.
//
// Within the network group the value also names the stage of the connection
// that failed — resolution, reaching the endpoint, TLS, or an established
// connection. That is the difference between a metric that says a request
// failed and one that says where to look: a name that does not resolve, a
// connect that times out, a rejected client certificate and a body that stops
// half way are four unrelated problems with four unrelated fixes.

// Error types the gateway reports for requests it rejects itself, before any
// upstream connection is made.
const (
	// errMissingHost: no X-rules_img-Original-Host header and no default registry.
	errMissingHost = "missing_host"
	// errInvalidRegistry: the requested host is not a valid registry name.
	errInvalidRegistry = "invalid_registry"
	// errInvalidRepository: the request path is not a valid repository name.
	errInvalidRepository = "invalid_repository"
	// errUnsupportedEndpoint: not an OCI distribution endpoint the gateway knows.
	errUnsupportedEndpoint = "unsupported_endpoint"
	// errMalformedQuery: an upload query the gateway cannot parse unambiguously.
	errMalformedQuery = "malformed_query"
	// errRegistryDenied: the upstream registry is not in the policy at all.
	errRegistryDenied = "registry_denied"
	// errPolicyDenied: a policy rule (or the default action) denied the operation.
	errPolicyDenied = "policy_denied"
	// errMountDenied: the source repository of a cross-repo blob mount is not
	// readable under the policy.
	errMountDenied = "mount_denied"
	// errPrivateUpstream: the upstream resolved to a loopback, link-local, or
	// private address and --deny-private-upstreams is set.
	errPrivateUpstream = "private_upstream"
	// errCacheSelfReplication: a cache replication request arrived from this very
	// instance, which means it is in its own peer list.
	errCacheSelfReplication = "cache_self_replication"
)

// Error types for authenticating the gateway's own clients, reported by a
// serving gateway when it rejects one.
const (
	// errPeerUnauthenticated: no client credential was presented.
	errPeerUnauthenticated = "peer_unauthenticated"
	// errPeerBadCredential: a credential was presented and rejected.
	errPeerBadCredential = "peer_bad_credential"
	// errPeerIdentityDenied: the credential is valid but its identity is not in
	// the allow-list.
	errPeerIdentityDenied = "peer_identity_denied"
	// errPeerAuthFailed: the credential could not be validated at all (for
	// example the Kubernetes API server was unreachable). Fails closed.
	errPeerAuthFailed = "peer_auth_failed"
)

// Error types a *forwarding* gateway reports about its peer. They are distinct
// from the upstream_* family on purpose: a 401 from the peer means our peer
// credential is wrong, which is a completely different problem from a registry
// rejecting the gateway's registry credential, and conflating them sends an
// operator hunting in the wrong place.
const (
	// errPeerUnauthorized: the peer rejected our credential.
	errPeerUnauthorized = "peer_unauthorized"
	// errPeerForbidden: the peer rejected our identity.
	errPeerForbidden = "peer_forbidden"
)

// Error types involving the upstream registry.
const (
	// errUpstreamAuth: resolving credentials, pinging, or exchanging a token with
	// the upstream failed for a reason that is not more specifically classified.
	errUpstreamAuth = "upstream_auth"
	// errUpstreamUnauthorized: the upstream answered 401. The gateway's own
	// credentials were rejected or are missing.
	errUpstreamUnauthorized = "upstream_unauthorized"
	// errUpstreamForbidden: the upstream answered 403. The credentials are valid
	// but not permitted to do this.
	errUpstreamForbidden = "upstream_forbidden"
	// errUpstreamRateLimited: the upstream answered 429.
	errUpstreamRateLimited = "upstream_rate_limited"
	// errUpstreamClientError: any other 4xx from the upstream (404 is not counted
	// as an error: existence checks and cache probes make it routine).
	errUpstreamClientError = "upstream_client_error"
	// errUpstreamServerError: a 5xx from the upstream.
	errUpstreamServerError = "upstream_server_error"
	// errBadUpstreamRequest: the gateway could not even build the upstream request.
	errBadUpstreamRequest = "bad_upstream_request"
)

// Error types for the network, grouped by the stage of the connection that
// failed. The stage is the point of the split: "the gateway could not talk to
// the registry" is not an actionable statement, while "the name did not
// resolve", "the TCP connect timed out", "the peer rejected our client
// certificate" and "the upstream closed the body half way" each send an
// operator to a different place. Every group keeps a generic member as the
// fallback for causes the classifier cannot place more precisely.
const (
	// Resolving the name.
	//
	// errDNSNotFound: NXDOMAIN. The host does not exist — a typo in the
	// registry name, or a split-horizon resolver that does not serve it.
	errDNSNotFound = "dns_not_found"
	// errDNSTimeout: the resolver did not answer. The registry may be fine.
	errDNSTimeout = "dns_timeout"
	// errDNS: any other resolution failure (SERVFAIL, no usable records).
	errDNS = "dns"

	// Reaching the endpoint.
	//
	// errConnectionRefused: something answered the port with a RST. The host is
	// up and nothing is listening, so this is a wrong port or a dead upstream.
	errConnectionRefused = "connection_refused"
	// errHostUnreachable: the network is up but that host is not (EHOSTUNREACH).
	errHostUnreachable = "host_unreachable"
	// errNetworkUnreachable: no route at all (ENETUNREACH, ENETDOWN) — this
	// gateway's own networking, not the upstream's.
	errNetworkUnreachable = "network_unreachable"
	// errDialTimeout: the TCP connect never completed. Packets are being dropped
	// silently, which is what a firewall or a black-holed address looks like.
	errDialTimeout = "dial_timeout"
	// errProxyConnect: the failure was reaching the HTTP proxy, not the registry.
	// Kept separate because the address to debug is a different one.
	errProxyConnect = "proxy_connect"
	// errLocalResourceExhausted: this process ran out of sockets or file
	// descriptors (EADDRNOTAVAIL, EADDRINUSE, EMFILE, ENFILE). Nothing is wrong
	// with the network: raise the limit, or find the connection leak.
	errLocalResourceExhausted = "local_resource_exhausted"

	// Establishing TLS.
	//
	// errTLSCertificateUntrusted: the chain does not lead to a trusted root — a
	// missing CA bundle or an intercepting proxy.
	errTLSCertificateUntrusted = "tls_certificate_untrusted"
	// errTLSCertificateExpired: the upstream's certificate is out of date.
	errTLSCertificateExpired = "tls_certificate_expired"
	// errTLSCertificateHostname: a valid certificate for a different name.
	errTLSCertificateHostname = "tls_certificate_hostname"
	// errTLSCertificate: any other verification failure.
	errTLSCertificate = "tls_certificate"
	// errTLSClientCertRejected: the other end rejected *our* certificate. On the
	// gateway-to-gateway hop this is the mTLS credential, and it is the failure
	// most easily mistaken for a generic network problem.
	errTLSClientCertRejected = "tls_client_certificate_rejected"
	// errTLSPlaintext: the endpoint answered a TLS handshake with something that
	// is not TLS — usually a plain HTTP service on an HTTPS port.
	errTLSPlaintext = "tls_plaintext"
	// errTLSHandshakeTimeout: connected, but the handshake never finished.
	errTLSHandshakeTimeout = "tls_handshake_timeout"
	// errTLS: any other handshake failure (version or cipher mismatch, alerts
	// that are not about a certificate).
	errTLS = "tls"

	// On an established connection.
	//
	// errConnectionReset: the other end sent a RST (ECONNRESET, ECONNABORTED).
	errConnectionReset = "connection_reset"
	// errBrokenPipe: we wrote to a connection the other end had already closed
	// (EPIPE). On the response path that is a client that hung up, not an
	// upstream fault, which is why it is not lumped in with a reset.
	errBrokenPipe = "broken_pipe"
	// errUnexpectedEOF: the body ended early. The bytes on the wire disagree with
	// the Content-Length, so the content is truncated, not merely late.
	errUnexpectedEOF = "unexpected_eof"
	// errReadTimeout: the other end stopped sending mid-transfer.
	errReadTimeout = "read_timeout"
	// errWriteTimeout: the other end stopped reading — backpressure, typically a
	// slow client or an upstream that stalled on an upload.
	errWriteTimeout = "write_timeout"
	// errHTTP2GoAway: the peer asked us to stop using the connection. Expected
	// during a rolling restart, and expected forever under a service mesh with a
	// max connection age; a steady rate outside those is worth chasing.
	errHTTP2GoAway = "http2_goaway"
	// errHTTP2Stream: a stream-level HTTP/2 error, including REFUSED_STREAM from
	// exceeding the peer's concurrent-stream limit.
	errHTTP2Stream = "http2_stream"
	// errTimeout: a timeout that cannot be attributed to a stage, such as an
	// overall request deadline.
	errTimeout = "timeout"
	// errNetwork: any other network failure.
	errNetwork = "network"

	// Not the network: the gateway's own redirect policy and the client.
	errRedirectRefused  = "redirect_refused"
	errTooManyRedirects = "too_many_redirects"
	errClientCanceled   = "client_canceled"
	errTransferAborted  = "transfer_aborted"
	errTypeUnknown      = "unknown"
)

// Sentinels for the redirect policy, so a refused redirect can be classified
// after net/http has wrapped it in a [url.Error].
var (
	errRefusedRedirect  = errors.New("refusing redirect")
	errRedirectsStopped = errors.New("stopped after 10 redirects")
	// errPrivateUpstreamDial is returned by the dial guard installed by
	// [DenyPrivateAddresses].
	errPrivateUpstreamDial = errors.New("refusing to connect to a private address")
)

// transportErrorType classifies an error from talking to an upstream registry
// into one of the fixed error.type values. Errors are unwrapped, so the
// [url.Error] and [transport.Error] wrappers net/http and
// go-containerregistry add do not hide the cause.
//
// The order of the checks is the order of the connection: a cause found early
// is the one that actually failed, and a later, vaguer match (every network
// error is ultimately a [net.OpError]) would only bury it.
func transportErrorType(err error) string {
	if err == nil {
		return ""
	}

	// The gateway's own redirect policy comes back wrapped in a url.Error.
	switch {
	case errors.Is(err, errRefusedRedirect):
		return errRedirectRefused
	case errors.Is(err, errRedirectsStopped):
		return errTooManyRedirects
	case errors.Is(err, errPrivateUpstreamDial):
		return errPrivateUpstream
	}

	// An upstream HTTP status reported as an error (token exchange, ping).
	var terr *transport.Error
	if errors.As(err, &terr) {
		if t := statusErrorType(terr.StatusCode); t != "" {
			return t
		}
		return errUpstreamClientError
	}

	// The request context is canceled when the client goes away (or when the
	// gateway shuts down), not by anything upstream.
	if errors.Is(err, context.Canceled) {
		return errClientCanceled
	}

	// Name resolution, before anything reaches the wire.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		switch {
		case dnsErr.IsNotFound:
			return errDNSNotFound
		case dnsErr.IsTimeout:
			return errDNSTimeout
		}
		return errDNS
	}

	// net.OpError carries the stage that failed in its Op ("dial", "read",
	// "write", "proxyconnect", "remote error"), which is what makes a timeout or
	// a TLS alert attributable to a stage rather than to "the network". It stays
	// nil when there is none: errors.As only assigns on a match.
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "proxyconnect" {
		return errProxyConnect
	}

	if t := tlsErrorType(err, opErr); t != "" {
		return t
	}
	if t := syscallErrorType(err); t != "" {
		return t
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return errUnexpectedEOF
	}
	if t := http2ErrorType(err); t != "" {
		return t
	}

	// Timeouts, attributed to the stage that timed out. net.Error covers the
	// ones that are not context deadlines (dial and I/O deadlines).
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		if opErr != nil {
			switch opErr.Op {
			case "dial":
				return errDialTimeout
			case "read":
				return errReadTimeout
			case "write":
				return errWriteTimeout
			}
		}
		return errTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errTimeout
	}

	if netErr != nil || opErr != nil {
		return errNetwork
	}
	return errTypeUnknown
}

// tlsErrorType classifies a failure to establish or hold up TLS, or returns the
// empty string when the error is not a TLS one.
func tlsErrorType(err error, opErr *net.OpError) string {
	// Verifying the *other* end's certificate. The specific reasons come first:
	// tls.CertificateVerificationError wraps them, so a check for it would match
	// them all.
	var authorityErr x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var certInvalidErr x509.CertificateInvalidError
	var certErr *tls.CertificateVerificationError
	switch {
	case errors.As(err, &authorityErr):
		return errTLSCertificateUntrusted
	case errors.As(err, &hostnameErr):
		return errTLSCertificateHostname
	case errors.As(err, &certInvalidErr):
		if certInvalidErr.Reason == x509.Expired {
			return errTLSCertificateExpired
		}
		return errTLSCertificate
	case errors.As(err, &certErr):
		return errTLSCertificate
	}

	// Not TLS on the other side at all. crypto/tls reports this as a record
	// header error, which [http.Client] replaces with [http.ErrSchemeMismatch]
	// when the bytes it saw are an HTTP response, so both spellings have to be
	// caught.
	var recordErr tls.RecordHeaderError
	if errors.As(err, &recordErr) || errors.Is(err, http.ErrSchemeMismatch) {
		return errTLSPlaintext
	}

	// An alert. tls.AlertError is only produced over QUIC; a TLS connection
	// surfaces the alert it received as a net.OpError with Op "remote error"
	// wrapping an unexported type, so its message is all there is to match on.
	// "remote error" plus a certificate is the far end rejecting ours — the
	// gateway-to-gateway mTLS credential, on the forwarding hop.
	var alertErr tls.AlertError
	if errors.As(err, &alertErr) {
		return errTLS
	}
	if opErr != nil && (opErr.Op == "remote error" || opErr.Op == "local error") {
		if opErr.Op == "remote error" && opErr.Err != nil && strings.Contains(opErr.Err.Error(), "certificate") {
			return errTLSClientCertRejected
		}
		return errTLS
	}

	// net/http's stalled-handshake error is an unexported type whose only
	// distinguishing feature is its message.
	if strings.Contains(err.Error(), "TLS handshake timeout") {
		return errTLSHandshakeTimeout
	}
	return ""
}

// syscallErrorType classifies the kernel-level network errors, or returns the
// empty string for anything else.
func syscallErrorType(err error) string {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return errConnectionRefused
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.ECONNABORTED):
		return errConnectionReset
	case errors.Is(err, syscall.EPIPE):
		return errBrokenPipe
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.EHOSTDOWN):
		return errHostUnreachable
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.ENETDOWN):
		return errNetworkUnreachable
	case errors.Is(err, syscall.EADDRNOTAVAIL), errors.Is(err, syscall.EADDRINUSE),
		errors.Is(err, syscall.EMFILE), errors.Is(err, syscall.ENFILE):
		return errLocalResourceExhausted
	}
	return ""
}

// http2ErrorType classifies the HTTP/2 conditions that matter on a long-lived
// multiplexed connection — the one the forwarding hop pins for its life. The
// HTTP/2 implementation net/http bundles keeps its error types unexported, so
// matching the message is the only option; an unrecognized message simply falls
// through to the general classification.
func http2ErrorType(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "GOAWAY"):
		return errHTTP2GoAway
	case strings.Contains(msg, "stream error:"):
		return errHTTP2Stream
	}
	return ""
}

// statusErrorType maps an upstream response status to an error.type, or to the
// empty string when the status is not a gateway-visible failure. 404 is not an
// error: clients probe for blobs and manifests that do not exist as a matter of
// course, and those show up as existence-check misses instead.
func statusErrorType(status int) string {
	switch {
	case status < 400, status == http.StatusNotFound:
		return ""
	case status == http.StatusUnauthorized:
		return errUpstreamUnauthorized
	case status == http.StatusForbidden:
		return errUpstreamForbidden
	case status == http.StatusTooManyRequests:
		return errUpstreamRateLimited
	case status >= 500:
		return errUpstreamServerError
	default:
		return errUpstreamClientError
	}
}

// authErrorType classifies a failed upstream authentication handshake. Anything
// the transport classifier cannot place is reported as an auth failure, since
// that is the step that failed.
func authErrorType(err error) string {
	if t := transportErrorType(err); t != errTypeUnknown {
		return t
	}
	return errUpstreamAuth
}

// transferErrorType classifies a failure that happened while streaming a
// response body to the client. io.Copy cannot say whether the upstream read or
// the client write failed, so an unrecognized cause is reported as an aborted
// transfer rather than guessed at. The classifier does distinguish the two most
// common recognizable halves: a broken pipe is a client that hung up, and an
// unexpected EOF is an upstream that stopped short of its Content-Length.
func transferErrorType(err error) string {
	if t := transportErrorType(err); t != errTypeUnknown {
		return t
	}
	return errTransferAborted
}

// peerAuthResponse maps a client-authentication failure to the response a
// serving gateway sends: an HTTP status, the OCI error code, the error.type to
// record, and a message that names what to fix without ever echoing what the
// client sent.
func peerAuthResponse(err error) (status int, code, errType, message string) {
	switch {
	case errors.Is(err, errNoPeerCredential):
		return http.StatusUnauthorized, "UNAUTHORIZED", errPeerUnauthenticated,
			"this gateway requires client authentication and no credential was presented"
	case errors.Is(err, errPeerIdentityNotAllowed):
		return http.StatusForbidden, "DENIED", errPeerIdentityDenied,
			"this gateway does not allow the presented client identity"
	case errors.Is(err, errPeerAuthUnavailable):
		return http.StatusServiceUnavailable, "UNAVAILABLE", errPeerAuthFailed,
			"this gateway could not validate the presented credential"
	default:
		return http.StatusUnauthorized, "UNAUTHORIZED", errPeerBadCredential,
			"this gateway rejected the presented client credential"
	}
}

// peerStatusErrorType classifies the response a *forwarding* gateway got from its
// peer. gatewayError is the peer's [gatewayErrorHeader], which is set only when
// the peer itself rejected us; anything else is the upstream registry's own
// answer travelling back through, and is classified as such.
func peerStatusErrorType(gatewayError string, status int) string {
	switch gatewayError {
	case errPeerUnauthenticated, errPeerBadCredential:
		return errPeerUnauthorized
	case errPeerIdentityDenied:
		return errPeerForbidden
	case errPeerAuthFailed:
		return errPeerUnauthorized
	}
	return statusErrorType(status)
}
