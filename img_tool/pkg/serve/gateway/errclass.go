package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"syscall"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// This file defines the error taxonomy behind the error.type attribute of the
// oci.gateway.errors counter (and of http.server.request.duration). The values
// are a fixed, low-cardinality set — never a Go error string — and are grouped
// so that an operator can answer "is this us, the client, the network, or the
// upstream registry?" without reading logs.

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

// Error types for the network and the client connection.
const (
	errConnectionRefused  = "connection_refused"
	errConnectionReset    = "connection_reset"
	errNetworkUnreachable = "network_unreachable"
	errTimeout            = "timeout"
	errDNS                = "dns"
	errTLS                = "tls"
	errTLSCertificate     = "tls_certificate"
	errNetwork            = "network"
	errRedirectRefused    = "redirect_refused"
	errTooManyRedirects   = "too_many_redirects"
	errClientCanceled     = "client_canceled"
	errTransferAborted    = "transfer_aborted"
	errTypeUnknown        = "unknown"
)

// Sentinels for the redirect policy, so a refused redirect can be classified
// after net/http has wrapped it in a [url.Error].
var (
	errRefusedRedirect  = errors.New("refusing redirect")
	errRedirectsStopped = errors.New("stopped after 10 redirects")
)

// transportErrorType classifies an error from talking to an upstream registry
// into one of the fixed error.type values. Errors are unwrapped, so the
// [url.Error] and [transport.Error] wrappers net/http and
// go-containerregistry add do not hide the cause.
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
	}

	// An upstream HTTP status reported as an error (token exchange, ping).
	var terr *transport.Error
	if errors.As(err, &terr) {
		if t := statusErrorType(terr.StatusCode); t != "" {
			return t
		}
		return errUpstreamClientError
	}

	switch {
	case errors.Is(err, context.Canceled):
		// The request context is canceled when the client goes away (or when the
		// gateway shuts down), not by anything upstream.
		return errClientCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return errTimeout
	case errors.Is(err, syscall.ECONNREFUSED):
		return errConnectionRefused
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return errConnectionReset
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.ENETDOWN):
		return errNetworkUnreachable
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return errDNS
	}

	// TLS trust problems are their own class: they are almost always a
	// misconfigured CA bundle or an intercepting proxy, not a flaky network.
	var certErr *tls.CertificateVerificationError
	var authorityErr x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var certInvalidErr x509.CertificateInvalidError
	switch {
	case errors.As(err, &certErr), errors.As(err, &authorityErr),
		errors.As(err, &hostnameErr), errors.As(err, &certInvalidErr):
		return errTLSCertificate
	}
	var recordErr tls.RecordHeaderError
	var alertErr tls.AlertError
	switch {
	case errors.As(err, &recordErr), errors.As(err, &alertErr):
		return errTLS
	}

	// net.Error covers timeouts that are not context deadlines (dial and TLS
	// handshake deadlines, for example).
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return errTimeout
		}
		return errNetwork
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return errNetwork
	}
	return errTypeUnknown
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
// transfer rather than guessed at.
func transferErrorType(err error) string {
	if t := transportErrorType(err); t != errTypeUnknown {
		return t
	}
	return errTransferAborted
}
