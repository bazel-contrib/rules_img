// Package gateway implements an OCI distribution gateway: an HTTP handler that
// speaks the container registry (Docker Registry v2 / OCI Distribution) protocol
// but only forwards requests to real upstream registries.
//
// Clients connect anonymously and must set the X-rules_img-Original-Host header
// (see [github.com/bazel-contrib/rules_img/img_tool/pkg/gateway.OriginalHostHeader])
// to tell the gateway which upstream registry they want to reach. The gateway
// authenticates to that upstream itself using the crane keychain + token
// exchange flow, and can allow or deny individual read/write operations for
// blobs and manifests. The set of reachable upstream registries is restricted by
// a list of hostname regular expressions.
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
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	clientgateway "github.com/bazel-contrib/rules_img/img_tool/pkg/gateway"
)

// authHandshakeTimeout bounds the initial per-upstream ping + token-exchange
// handshake, which is performed once per repository+scope and cached.
const authHandshakeTimeout = 2 * time.Minute

// Handler is an [http.Handler] that forwards registry requests to upstream
// registries, subject to a [Policy] and an allow-list of upstream hostnames.
type Handler struct {
	policy          Policy
	allowed         []*regexp.Regexp
	keychain        authn.Keychain
	base            http.RoundTripper
	defaultRegistry string
	log             *log.Logger

	cache authCache
}

// Option configures a [Handler].
type Option func(*Handler)

// WithPolicy sets the read/write policy enforced by the gateway.
func WithPolicy(p Policy) Option {
	return func(h *Handler) { h.policy = p }
}

// WithAllowedRegistries sets the list of regular expressions matched against the
// upstream hostname. A request is only forwarded if at least one expression
// matches. The expressions should be anchored by the caller (see the command
// wiring, which wraps each pattern in ^(?:...)$).
func WithAllowedRegistries(res []*regexp.Regexp) Option {
	return func(h *Handler) { h.allowed = res }
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
// subject to the allow-list and policy like any other upstream. An empty value
// keeps the header mandatory.
func WithDefaultRegistry(registry string) Option {
	return func(h *Handler) { h.defaultRegistry = registry }
}

// New constructs a gateway [Handler].
func New(opts ...Option) *Handler {
	h := &Handler{
		keychain: authn.DefaultKeychain,
		base:     defaultBaseTransport(),
		log:      log.New(os.Stderr, "", log.LstdFlags),
	}
	for _, o := range opts {
		o(h)
	}
	h.cache.inner = make(map[string]*authEntry)
	return h
}

func defaultBaseTransport() http.RoundTripper {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return t.Clone()
	}
	return http.DefaultTransport
}

// ServeHTTP implements [http.Handler].
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Header.Get(clientgateway.OriginalHostHeader)
	if host == "" {
		// Fall back to the configured default registry when the client did not
		// name a target registry. If no default is configured the header is
		// required.
		host = h.defaultRegistry
	}
	if host == "" {
		h.writeError(w, r, http.StatusBadRequest, "UNSUPPORTED",
			fmt.Sprintf("missing required %s header and no default registry configured", clientgateway.OriginalHostHeader))
		return
	}
	// The API version check has no repository; resolve just the registry so the
	// allow-list is enforced against the *resolved* upstream.
	if r.URL.Path == "/v2" || r.URL.Path == "/v2/" {
		reg, err := name.NewRegistry(host)
		if err != nil {
			h.writeError(w, r, http.StatusBadRequest, "NAME_INVALID",
				fmt.Sprintf("invalid registry %q: %v", host, err))
			return
		}
		if !h.registryAllowed(reg.RegistryStr()) {
			h.writeError(w, r, http.StatusForbidden, "DENIED",
				fmt.Sprintf("upstream registry %q is not allowed by this gateway", reg.RegistryStr()))
			return
		}
		// Answer anonymously so clients treat the gateway as an unauthenticated
		// registry and send us no credentials.
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
		h.log.Printf("%s %s (host=%s) -> 200 (version check)", r.Method, r.URL.Path, reg.RegistryStr())
		return
	}

	cls, ok := classify(r)
	if !ok {
		h.writeError(w, r, http.StatusNotFound, "UNSUPPORTED",
			"unsupported registry endpoint")
		return
	}

	repo, err := name.NewRepository(host + "/" + cls.repo)
	if err != nil {
		h.writeError(w, r, http.StatusBadRequest, "NAME_INVALID",
			fmt.Sprintf("invalid repository %q: %v", cls.repo, err))
		return
	}
	// Enforce the allow-list against the *resolved* registry, not the raw
	// header. name.NewRepository routes a header like "myregistry" (no dot) to
	// Docker Hub, so checking the header alone would not constrain the real
	// upstream the gateway connects to.
	if !h.registryAllowed(repo.RegistryStr()) {
		h.writeError(w, r, http.StatusForbidden, "DENIED",
			fmt.Sprintf("upstream registry %q is not allowed by this gateway", repo.RegistryStr()))
		return
	}
	if !h.policy.allows(cls.req) {
		h.writeError(w, r, http.StatusForbidden, "DENIED",
			fmt.Sprintf("%s is not permitted by this gateway's policy", cls.kind))
		return
	}

	h.forward(w, r, repo, cls)
}

// registryAllowed reports whether the resolved upstream registry is permitted.
// The port, if any, is stripped so patterns match on the hostname.
func (h *Handler) registryAllowed(registryStr string) bool {
	hostname := registryStr
	if hn, _, err := net.SplitHostPort(registryStr); err == nil {
		hostname = hn
	}
	for _, re := range h.allowed {
		if re.MatchString(hostname) {
			return true
		}
	}
	return false
}

// forward proxies the request to the upstream registry using an authenticated
// transport and streams the response back to the client.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, repo name.Repository, cls request) {
	action := transport.PullScope
	if cls.write {
		action = transport.PushScope
	}

	rt, err := h.authTransport(repo, action)
	if err != nil {
		h.writeError(w, r, http.StatusBadGateway, "UNAUTHORIZED",
			fmt.Sprintf("authenticating to upstream %s: %v", repo.RegistryStr(), err))
		return
	}

	// Preserve the exact request URI (path + query) as received.
	upstreamURL := repo.Scheme() + "://" + repo.RegistryStr() + r.URL.RequestURI()
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, r.Body)
	if err != nil {
		h.writeError(w, r, http.StatusBadGateway, "UNKNOWN",
			fmt.Sprintf("building upstream request: %v", err))
		return
	}
	copyHeader(outReq.Header, r.Header)
	outReq.ContentLength = r.ContentLength

	// Use an http.Client so redirects (e.g. a blob GET pointing at CDN/blob
	// storage) are followed transparently. checkRedirect only follows for safe
	// read methods and refuses redirects to private/link-local addresses.
	client := &http.Client{Transport: rt, CheckRedirect: checkRedirect(r.Method)}
	resp, err := client.Do(outReq)
	if err != nil {
		h.writeError(w, r, http.StatusBadGateway, "UNKNOWN",
			fmt.Sprintf("forwarding to upstream %s: %v", repo.RegistryStr(), err))
		return
	}
	defer resp.Body.Close()

	copyResponseHeader(w.Header(), resp.Header, repo)
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		if _, err := io.Copy(w, resp.Body); err != nil {
			// The status/header are already written; we can only log.
			h.log.Printf("%s %s (host=%s): error copying response body: %v", r.Method, r.URL.Path, repo.RegistryStr(), err)
		}
	}
	h.log.Printf("%s %s (host=%s) -> %d", r.Method, r.URL.Path, repo.RegistryStr(), resp.StatusCode)
}

// authTransport returns a cached authenticated RoundTripper for the given
// repository and scope action ("pull" or "push,pull"). It resolves credentials
// from the keychain and performs the crane ping + token-exchange handshake.
func (h *Handler) authTransport(repo name.Repository, action string) (http.RoundTripper, error) {
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
			return nil, fmt.Errorf("resolving credentials: %w", err)
		}
		rt, err := transport.NewWithContext(ctx, repo.Registry, auth, h.base, []string{repo.Scope(action)})
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
			return fmt.Errorf("stopped after 10 redirects")
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
// apply network-level controls if needed.
func validateRedirectTarget(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("refusing redirect to non-http(s) URL %q", u.Redacted())
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsPrivate() || ip.IsUnspecified() {
			return fmt.Errorf("refusing redirect to private or link-local address %q", u.Hostname())
		}
	}
	return nil
}

// writeError writes an OCI-style error response.
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	h.log.Printf("%s %s -> %d %s: %s", r.Method, r.URL.Path, status, code, message)
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
// hop-by-hop headers, the gateway control header, the Host header, and any
// client-supplied Authorization (the auth transport sets its own).
func copyHeader(dst, src http.Header) {
	skip := connectionHeaderSet(src)
	for k, vs := range src {
		ck := http.CanonicalHeaderKey(k)
		if !forwardable(ck, skip) {
			continue
		}
		switch ck {
		case http.CanonicalHeaderKey(clientgateway.OriginalHostHeader), "Host", "Authorization":
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
