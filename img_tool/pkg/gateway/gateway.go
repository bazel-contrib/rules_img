// Package gateway provides client-side support for routing container registry
// requests through an OCI distribution gateway (see
// //cmd/oci-distribution-gateway).
//
// When one of the IMG_REGISTRY_GATEWAY, IMG_REGISTRY_PUSH_GATEWAY, or
// IMG_REGISTRY_PULL_GATEWAY environment variables is set, registry requests are
// transparently redirected to the configured gateway endpoint instead of the
// real registry. The hostname of the registry the client actually wants to
// reach is preserved in the [OriginalHostHeader] request header so the gateway
// knows where to forward the request.
//
// The gateway performs its own authentication against the upstream registry, so
// this transport strips any Authorization header before forwarding: the client
// talks to the gateway anonymously.
package gateway

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// OriginalHostHeader carries the hostname (optionally host:port) of the registry
// the client actually wants to talk to. The gateway forwards the request to that
// upstream registry. Both the client transport and the gateway server must agree
// on this constant.
const OriginalHostHeader = "X-rules_img-Original-Host"

// Environment variables used to configure the gateway endpoints. The
// mode-specific variables take precedence over the shared fallback.
const (
	// EnvGateway is the fallback endpoint used for both push and pull when no
	// mode-specific variable is set.
	EnvGateway = "IMG_REGISTRY_GATEWAY"
	// EnvPushGateway is the endpoint used when pushing content.
	EnvPushGateway = "IMG_REGISTRY_PUSH_GATEWAY"
	// EnvPullGateway is the endpoint used when pulling content.
	EnvPullGateway = "IMG_REGISTRY_PULL_GATEWAY"
)

// Mode selects push or pull semantics when resolving the gateway endpoint.
type Mode int

const (
	// ModePull resolves the pull gateway (IMG_REGISTRY_PULL_GATEWAY, then
	// IMG_REGISTRY_GATEWAY).
	ModePull Mode = iota
	// ModePush resolves the push gateway (IMG_REGISTRY_PUSH_GATEWAY, then
	// IMG_REGISTRY_GATEWAY).
	ModePush
)

// Endpoint returns the configured gateway endpoint for the given mode, or the
// empty string if no gateway is configured. The mode-specific environment
// variable takes precedence over the shared IMG_REGISTRY_GATEWAY fallback.
func Endpoint(mode Mode) string {
	switch mode {
	case ModePush:
		if v := os.Getenv(EnvPushGateway); v != "" {
			return v
		}
	case ModePull:
		if v := os.Getenv(EnvPullGateway); v != "" {
			return v
		}
	}
	return os.Getenv(EnvGateway)
}

// WrapTransport wraps base so that outgoing requests are redirected to the
// gateway endpoint configured for the given mode. If no gateway is configured
// for the mode, base is returned unchanged.
func WrapTransport(base http.RoundTripper, mode Mode) (http.RoundTripper, error) {
	endpoint := Endpoint(mode)
	if endpoint == "" {
		return base, nil
	}
	return NewTransport(endpoint, base)
}

// NewTransport builds a RoundTripper that redirects requests to the gateway at
// endpoint. Supported endpoint forms are:
//
//	http://host[:port]
//	https://host[:port]
//	unix:<path-to-socket>
//
// For TCP endpoints the request URL's scheme and host are rewritten to the
// gateway. For unix sockets the connection is dialed against the socket while
// the request keeps addressing the original host (over plaintext HTTP).
func NewTransport(endpoint string, base http.RoundTripper) (http.RoundTripper, error) {
	if base == nil {
		base = http.DefaultTransport
	}
	if strings.HasPrefix(endpoint, "unix:") {
		socketPath := strings.TrimPrefix(endpoint, "unix:")
		if socketPath == "" {
			return nil, fmt.Errorf("gateway: empty unix socket path in %q", endpoint)
		}
		return &transport{
			scheme: "http",
			base:   unixTransport(base, socketPath),
			unix:   true,
		}, nil
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("gateway: parsing endpoint %q: %w", endpoint, err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf("gateway: unsupported endpoint %q (want http://, https://, or unix:)", endpoint)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("gateway: endpoint %q is missing a host", endpoint)
	}
	return &transport{
		scheme: u.Scheme,
		host:   u.Host,
		base:   base,
	}, nil
}

// transport rewrites requests so they are sent to the configured gateway.
type transport struct {
	// scheme is the scheme used to talk to the gateway ("http" or "https").
	scheme string
	// host is the gateway host:port for TCP endpoints; unused for unix sockets.
	host string
	// base performs the actual round trip. For unix sockets it dials the
	// configured socket regardless of the request host.
	base http.RoundTripper
	// unix reports whether the gateway is reached over a unix socket, in which
	// case the request host is left untouched (the socket is dialed instead).
	unix bool
}

// RoundTrip implements [http.RoundTripper].
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone so we never mutate the caller's request (it may be retried).
	out := req.Clone(req.Context())
	out.Header.Set(OriginalHostHeader, req.URL.Host)
	// The gateway authenticates to the upstream registry on our behalf, so we
	// must not leak the client's registry credentials to it.
	out.Header.Del("Authorization")

	out.URL.Scheme = t.scheme
	if !t.unix {
		out.URL.Host = t.host
		// Let net/http derive the Host header from the (gateway) URL host.
		out.Host = ""
	}
	return t.base.RoundTrip(out)
}

// unixTransport returns an [http.Transport] that dials the given unix socket for
// every request, inheriting connection-pool settings from base when it is an
// *http.Transport.
func unixTransport(base http.RoundTripper, socketPath string) http.RoundTripper {
	var t *http.Transport
	if bt, ok := base.(*http.Transport); ok {
		t = bt.Clone()
	} else {
		t = http.DefaultTransport.(*http.Transport).Clone()
	}
	dialer := &net.Dialer{}
	t.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "unix", socketPath)
	}
	// Unix sockets speak plaintext HTTP; make sure we never negotiate TLS or
	// attempt an HTTPS upgrade.
	t.TLSClientConfig = nil
	t.ForceAttemptHTTP2 = false
	return t
}
