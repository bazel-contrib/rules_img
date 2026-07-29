package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

func TestTransportErrorType(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"no error", nil, ""},
		{
			// net/http wraps everything a RoundTripper returns in a url.Error, so
			// the classifier has to unwrap.
			name: "connection refused through the url.Error wrapper",
			err:  &url.Error{Op: "Get", URL: "https://registry.test/v2/", Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}},
			want: errConnectionRefused,
		},
		{"connection reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, errConnectionReset},
		{"broken pipe", &net.OpError{Op: "write", Err: syscall.EPIPE}, errConnectionReset},
		{"host unreachable", &net.OpError{Op: "dial", Err: syscall.EHOSTUNREACH}, errNetworkUnreachable},
		{"network unreachable", &net.OpError{Op: "dial", Err: syscall.ENETUNREACH}, errNetworkUnreachable},
		{"context deadline", fmt.Errorf("forwarding: %w", context.DeadlineExceeded), errTimeout},
		{"i/o timeout", &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}, errTimeout},
		{"client went away", fmt.Errorf("copying: %w", context.Canceled), errClientCanceled},
		{"name resolution", &net.DNSError{Err: "no such host", Name: "registry.test", IsNotFound: true}, errDNS},
		{
			name: "untrusted certificate",
			err:  &url.Error{Op: "Get", Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}},
			want: errTLSCertificate,
		},
		{"certificate for the wrong host", x509.HostnameError{Host: "registry.test"}, errTLSCertificate},
		{"not a TLS server", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, errTLS},
		{"TLS alert", tls.AlertError(80), errTLS},
		{"generic network failure", &net.OpError{Op: "dial", Err: errors.New("boom")}, errNetwork},
		{"upstream 401", &transport.Error{StatusCode: http.StatusUnauthorized}, errUpstreamUnauthorized},
		{"upstream 403", &transport.Error{StatusCode: http.StatusForbidden}, errUpstreamForbidden},
		{"upstream 429", &transport.Error{StatusCode: http.StatusTooManyRequests}, errUpstreamRateLimited},
		{"upstream 500", &transport.Error{StatusCode: http.StatusInternalServerError}, errUpstreamServerError},
		{"upstream 400", &transport.Error{StatusCode: http.StatusBadRequest}, errUpstreamClientError},
		{
			// A 404 is not an error in general, but a token exchange or ping that
			// 404s did fail, so it must still be classified as something.
			name: "upstream 404 during the handshake",
			err:  &transport.Error{StatusCode: http.StatusNotFound},
			want: errUpstreamClientError,
		},
		{
			name: "redirect to a private address",
			err:  &url.Error{Op: "Get", Err: fmt.Errorf("%w to private or link-local address %q", errRefusedRedirect, "169.254.169.254")},
			want: errRedirectRefused,
		},
		{"redirect loop", &url.Error{Op: "Get", Err: errRedirectsStopped}, errTooManyRedirects},
		{"unrecognized", errors.New("something else"), errTypeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := transportErrorType(tc.err); got != tc.want {
				t.Errorf("transportErrorType(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestStatusErrorType(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusOK, ""},
		{http.StatusCreated, ""},
		{http.StatusAccepted, ""},
		{http.StatusTemporaryRedirect, ""},
		// Probing for content that does not exist is normal registry traffic.
		{http.StatusNotFound, ""},
		{http.StatusUnauthorized, errUpstreamUnauthorized},
		{http.StatusForbidden, errUpstreamForbidden},
		{http.StatusTooManyRequests, errUpstreamRateLimited},
		{http.StatusBadRequest, errUpstreamClientError},
		{http.StatusRequestedRangeNotSatisfiable, errUpstreamClientError},
		{http.StatusInternalServerError, errUpstreamServerError},
		{http.StatusServiceUnavailable, errUpstreamServerError},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			if got := statusErrorType(tc.status); got != tc.want {
				t.Errorf("statusErrorType(%d) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

func TestAuthAndTransferErrorTypes(t *testing.T) {
	// A failure the transport classifier recognizes keeps its specific type...
	if got := authErrorType(&transport.Error{StatusCode: http.StatusUnauthorized}); got != errUpstreamUnauthorized {
		t.Errorf("authErrorType(401) = %q, want %q", got, errUpstreamUnauthorized)
	}
	// ...and anything else is attributed to the step that failed.
	if got := authErrorType(errors.New("resolving credentials: keychain empty")); got != errUpstreamAuth {
		t.Errorf("authErrorType(keychain error) = %q, want %q", got, errUpstreamAuth)
	}
	if got := transferErrorType(&net.OpError{Op: "write", Err: syscall.ECONNRESET}); got != errConnectionReset {
		t.Errorf("transferErrorType(reset) = %q, want %q", got, errConnectionReset)
	}
	if got := transferErrorType(errors.New("unexpected EOF")); got != errTransferAborted {
		t.Errorf("transferErrorType(EOF) = %q, want %q", got, errTransferAborted)
	}
}
