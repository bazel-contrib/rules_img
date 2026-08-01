package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// This file implements authentication of the gateway's *clients*. It exists for
// the two-hop topology: a serving gateway that holds a build farm's registry
// credentials is reached over the network by many forwarding gateways, and a
// ClusterIP Service is reachable from every namespace in a cluster, so that
// listener has to know who is talking to it.
//
// Three methods are supported and any one of them is sufficient (they are OR'd),
// so an operator can migrate between them without downtime:
//
//  1. mTLS — a client certificate chaining to --client-ca-file, optionally
//     restricted to an allow-list of SPIFFE URI SANs or DNS SANs.
//  2. A static bearer token from a file, with several tokens valid at once so
//     rotation is publish-new, roll-clients, drop-old.
//  3. A Kubernetes projected ServiceAccount token with a dedicated audience,
//     validated by TokenReview against the API server. This is the only method
//     with real revocation: the API server rejects a token whose pod or
//     ServiceAccount is gone, so deleting a compromised worker pod immediately
//     invalidates its credential.
//
// Two rules run through all of it. The presented credential is never logged or
// embedded in an error — every failure is one of a handful of sentinels. And the
// certificate identity is re-checked on *every request*, not just per handshake:
// the whole point of the second hop is one long-lived multiplexed connection, so
// a handshake-only check would let an identity removed from the allow-list (or a
// leaf that has since expired) keep working for the unbounded life of that
// connection. With no CRL and no OCSP in this design, the allow-list is the
// revocation mechanism, so it has to bite at once.

// Errors reported by [PeerAuth.Authenticate]. They are deliberately opaque: they
// name what was wrong with the request, never what the client sent.
var (
	errNoPeerCredential       = errors.New("no client credential presented")
	errBadPeerCredential      = errors.New("client credential rejected")
	errPeerIdentityNotAllowed = errors.New("client identity is not allowed")
	errPeerAuthUnavailable    = errors.New("client credential could not be validated")
)

const (
	// maxTokenFileSize bounds a credential file. Tokens are short; anything this
	// large is a misconfigured path.
	maxTokenFileSize = 1 << 20
	// minTokenLength is the shortest static shared secret accepted. It also
	// catches a flag pointing at the wrong (or a truncated) file, which in a
	// partially-configured gateway would be a silent authentication bypass.
	minTokenLength = 32
	// tokenReviewCacheTTL is how long a TokenReview answer is reused, for
	// successes and failures alike. It matches the TTL every aggregated
	// Kubernetes API server uses for the same call; caching negatives also blunts
	// a token-guessing flood.
	tokenReviewCacheTTL = 10 * time.Second
	// serviceAccountTokenPath is where the gateway's own projected token lives.
	// It is re-read on every review: kubelet rotates it, and a client that caches
	// it at startup starts failing within the hour.
	serviceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // a path, not a credential
	// serviceAccountCAPath is the API server's CA inside a pod.
	serviceAccountCAPath = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// spoofableHeaders are request headers that carry identity or gateway
// diagnostics and that a client must never be able to set. They are deleted
// before authentication so nothing downstream can be fooled by them, including
// the headers a service mesh injects: those are only trustworthy when the
// listener is provably unreachable except through the mesh, which the gateway
// cannot verify.
var spoofableHeaders = []string{
	forwardedByHeader,
	gatewayErrorHeader,
	requestIDHeader,
	"X-Forwarded-For",
	"X-Forwarded-Host",
	"X-Forwarded-Proto",
	"Forwarded",
	"X-Forwarded-Client-Cert",
	"L5d-Client-Id",
}

// PeerAuthOptions configures a [PeerAuth]. Every field is optional; a PeerAuth
// with no method configured authenticates nobody and must not be installed.
type PeerAuthOptions struct {
	// TLS holds the client CA bundle. When it has one, client certificates are
	// accepted and verified against it.
	TLS *PeerTLS
	// AllowedClientIDs restricts which verified client certificates are accepted,
	// matched against the leaf's SPIFFE URI SAN or its DNS SANs (a single leading
	// "*." wildcard is allowed for DNS names). Empty means any certificate the CA
	// signed is accepted — which in a cluster with one shared internal CA is
	// effectively cluster-wide access, so an allow-list should be considered
	// mandatory in practice.
	AllowedClientIDs []string
	// TokenFiles hold static shared secrets, one per line, with "#" comments and
	// blank lines ignored.
	TokenFiles []string
	// ServiceAccountAudience enables Kubernetes TokenReview validation of bearer
	// tokens carrying this audience. It must be an audience dedicated to the
	// gateway: the token every pod is given by default is issued for the API
	// server's audience, and accepting that would authenticate the whole cluster.
	ServiceAccountAudience string
	// AllowedServiceAccounts restricts which ServiceAccounts are accepted, as
	// "system:serviceaccount:<namespace>:<name>". Empty means any ServiceAccount
	// holding a token for the audience is accepted.
	AllowedServiceAccounts []string
	// Reviewer validates projected ServiceAccount tokens. Defaults to an
	// in-cluster TokenReview client built from the pod's environment.
	Reviewer TokenReviewer
	// Logger records reloads. Defaults to the standard logger.
	Logger *log.Logger
	// OnReload mirrors [PeerTLSOptions.OnReload] for token files.
	OnReload func(material string, err error)
}

// PeerAuth authenticates the gateway's clients. It is safe for concurrent use.
type PeerAuth struct {
	opts PeerAuthOptions
	log  *log.Logger

	// tokenDigests holds sha256 of every valid static token. Digests rather than
	// the tokens themselves, so the comparison is over a fixed width: crypto/
	// subtle.ConstantTimeCompare returns 0 immediately when lengths differ, which
	// would leak the length of the expected token.
	tokenDigests atomic.Pointer[[][sha256.Size]byte]

	// allowedIDs is the compiled certificate allow-list.
	allowedIDs []identityPattern
	// allowedAccounts is the ServiceAccount allow-list, as a set.
	allowedAccounts map[string]struct{}

	reviewer TokenReviewer
	reviews  *reviewCache

	mu     sync.Mutex
	stamps map[string]fileStamp
}

// NewPeerAuth compiles the options and loads the token files once. A file that
// cannot be read, or a token that is too short, is a startup error.
func NewPeerAuth(opts PeerAuthOptions) (*PeerAuth, error) {
	a := &PeerAuth{opts: opts, log: opts.Logger, stamps: make(map[string]fileStamp)}
	if a.log == nil {
		a.log = log.New(os.Stderr, "", log.LstdFlags)
	}
	for _, id := range opts.AllowedClientIDs {
		pattern, err := compileIdentityPattern(id)
		if err != nil {
			return nil, err
		}
		a.allowedIDs = append(a.allowedIDs, pattern)
	}
	if len(opts.AllowedServiceAccounts) > 0 {
		a.allowedAccounts = make(map[string]struct{}, len(opts.AllowedServiceAccounts))
		for _, account := range opts.AllowedServiceAccounts {
			if !strings.HasPrefix(account, serviceAccountUsernamePrefix) {
				return nil, fmt.Errorf("--allowed-serviceaccount %q must have the form %s<namespace>:<name>", account, serviceAccountUsernamePrefix)
			}
			a.allowedAccounts[account] = struct{}{}
		}
	}
	if opts.ServiceAccountAudience != "" {
		a.reviewer = opts.Reviewer
		if a.reviewer == nil {
			reviewer, err := NewInClusterTokenReviewer()
			if err != nil {
				return nil, err
			}
			a.reviewer = reviewer
		}
		a.reviews = newReviewCache()
	}
	if err := a.loadTokens(); err != nil {
		return nil, err
	}
	if !a.enabled() {
		return nil, errors.New("client authentication needs at least one of a client CA, a token file, or a ServiceAccount audience")
	}
	return a, nil
}

// enabled reports whether any method is configured.
func (a *PeerAuth) enabled() bool {
	return a.acceptsCertificates() || a.acceptsStaticTokens() || a.reviewer != nil
}

func (a *PeerAuth) acceptsCertificates() bool {
	return a.opts.TLS != nil && a.opts.TLS.HasCA()
}

func (a *PeerAuth) acceptsStaticTokens() bool {
	digests := a.tokenDigests.Load()
	return digests != nil && len(*digests) > 0
}

// Methods lists the configured authentication methods, for the startup banner.
func (a *PeerAuth) Methods() []string {
	var methods []string
	if a.acceptsCertificates() {
		method := "mtls"
		if len(a.allowedIDs) > 0 {
			method = fmt.Sprintf("mtls(%d allowed identities)", len(a.allowedIDs))
		}
		methods = append(methods, method)
	}
	if a.acceptsStaticTokens() {
		methods = append(methods, fmt.Sprintf("token(%d valid)", len(*a.tokenDigests.Load())))
	}
	if a.reviewer != nil {
		methods = append(methods, fmt.Sprintf("serviceaccount(audience=%s)", a.opts.ServiceAccountAudience))
	}
	return methods
}

// ClientAuthType is the [tls.ClientAuthType] a listener should use.
// VerifyClientCertIfGiven rather than RequireAndVerifyClientCert is what makes
// mTLS optional: a presented certificate is fully verified, but a client without
// one still completes the handshake and can authenticate by token — and a health
// probe with no credential at all can still be answered.
func (a *PeerAuth) ClientAuthType() tls.ClientAuthType {
	if a.acceptsCertificates() {
		return tls.VerifyClientCertIfGiven
	}
	return tls.NoClientCert
}

// VerifyConnection is installed as [tls.Config.VerifyConnection]. It is a cheap
// early reject only — the authoritative identity check happens per request in
// [PeerAuth.Authenticate], because this runs once per handshake and connections
// here are long-lived and multiplexed.
//
// It must be VerifyConnection and not VerifyPeerCertificate: crypto/tls
// documents the latter as not invoked on resumed connections, and Go's server
// session tickets live seven days, so an identity removed from the allow-list
// could keep resuming for a week.
func (a *PeerAuth) VerifyConnection(state tls.ConnectionState) error {
	if len(state.PeerCertificates) == 0 {
		// No certificate: another method may still authenticate the request.
		return nil
	}
	if _, err := a.certificateIdentity(state.PeerCertificates[0], time.Now()); err != nil {
		return err
	}
	return nil
}

// Authenticate identifies the client of r and returns a principal for the
// decision log, or an error. On success the credential is removed from the
// request so it cannot travel any further; the upstream leg strips Authorization
// again independently.
func (a *PeerAuth) Authenticate(r *http.Request) (string, error) {
	// Strip anything a client could use to forge identity or provenance before
	// looking at the request at all.
	for _, header := range spoofableHeaders {
		r.Header.Del(header)
	}

	// A verified client certificate is the strongest signal, so try it first and
	// never fall through on a certificate that is present but not allowed: that
	// is a deliberate configuration decision, not a reason to look for a token.
	if a.acceptsCertificates() && r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		identity, err := a.certificateIdentity(r.TLS.PeerCertificates[0], time.Now())
		if err != nil {
			return "", err
		}
		return "cert:" + identity, nil
	}

	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		return "", errNoPeerCredential
	}
	// The credential has been read; make sure nothing downstream ever sees it.
	r.Header.Del("Authorization")

	if a.acceptsStaticTokens() && a.validStaticToken(token) {
		return "token", nil
	}
	if a.reviewer != nil {
		principal, err := a.reviewServiceAccountToken(r, token)
		if err == nil {
			return principal, nil
		}
		// An API server we cannot reach must not silently downgrade to "rejected
		// because the token was wrong": report it as its own failure so the
		// operator sees the cause, and fail closed either way.
		if errors.Is(err, errPeerAuthUnavailable) {
			return "", err
		}
	}
	return "", errBadPeerCredential
}

// validStaticToken reports whether token is one of the configured shared
// secrets. Every digest is compared and the results are OR-accumulated, with no
// early exit, so the time taken does not depend on which token matched.
func (a *PeerAuth) validStaticToken(token string) bool {
	digests := a.tokenDigests.Load()
	if digests == nil {
		return false
	}
	got := sha256.Sum256([]byte(token))
	match := 0
	for _, want := range *digests {
		match |= subtle.ConstantTimeCompare(got[:], want[:])
	}
	return match == 1
}

// reviewServiceAccountToken validates a projected ServiceAccount token through
// the TokenReview API, with a short-lived cache in front.
func (a *PeerAuth) reviewServiceAccountToken(r *http.Request, token string) (string, error) {
	key := sha256.Sum256([]byte(token))
	if principal, err, ok := a.reviews.get(key, time.Now()); ok {
		if err != nil {
			return "", err
		}
		return principal, nil
	}
	username, err := a.reviewer.Review(r.Context(), token, a.opts.ServiceAccountAudience)
	if err == nil {
		if len(a.allowedAccounts) > 0 {
			if _, allowed := a.allowedAccounts[username]; !allowed {
				err = errPeerIdentityNotAllowed
			}
		}
	}
	principal := "serviceaccount:" + username
	// Do not cache "the API server was unreachable": that is a condition of the
	// gateway, not a verdict about the token, and caching it would extend an
	// outage past its cause.
	if !errors.Is(err, errPeerAuthUnavailable) {
		a.reviews.put(key, principal, err, time.Now())
	}
	if err != nil {
		return "", err
	}
	return principal, nil
}

// certificateIdentity extracts an allow-listed identity from a verified client
// leaf. The chain, the CA and the client-auth extended key usage have already
// been checked by crypto/tls; expiry is re-checked here because a handshake that
// happened while the certificate was still valid must not keep authorizing
// requests after it expires.
func (a *PeerAuth) certificateIdentity(leaf *x509.Certificate, now time.Time) (string, error) {
	if now.After(leaf.NotAfter) || now.Before(leaf.NotBefore) {
		return "", errPeerIdentityNotAllowed
	}
	// A SPIFFE X.509-SVID carries exactly one URI SAN; more than one is not an
	// SVID, so it is not treated as one.
	var identity string
	if len(leaf.URIs) == 1 {
		if id, ok := canonicalSPIFFEID(leaf.URIs[0]); ok {
			identity = id
		}
	}
	if identity == "" && len(leaf.DNSNames) > 0 {
		identity = strings.ToLower(leaf.DNSNames[0])
	}
	if identity == "" {
		// Never fall back to the Subject Common Name: it is not a name any modern
		// verifier honors, and treating it as one would accept certificates whose
		// SANs say nothing about who they are.
		return "", errPeerIdentityNotAllowed
	}
	if len(a.allowedIDs) == 0 {
		return identity, nil
	}
	for _, candidate := range identityCandidates(leaf) {
		for _, pattern := range a.allowedIDs {
			if pattern.matches(candidate) {
				return candidate, nil
			}
		}
	}
	return "", errPeerIdentityNotAllowed
}

// identityCandidates lists every name of a leaf that may be matched against the
// allow-list: its SPIFFE URI SAN (when it has exactly one) and its DNS SANs.
func identityCandidates(leaf *x509.Certificate) []string {
	candidates := make([]string, 0, 1+len(leaf.DNSNames))
	if len(leaf.URIs) == 1 {
		if id, ok := canonicalSPIFFEID(leaf.URIs[0]); ok {
			candidates = append(candidates, id)
		}
	}
	for _, name := range leaf.DNSNames {
		candidates = append(candidates, strings.ToLower(name))
	}
	return candidates
}

// canonicalSPIFFEID normalizes a SPIFFE ID URI SAN to "spiffe://<trust-domain>/<path>",
// or reports that the URI is not one. The trust domain is case-insensitive and
// the path is case-sensitive. Every form that url.URL would happily round-trip
// but a SPIFFE ID may not contain is rejected explicitly rather than normalized
// away, so two spellings can never compare equal by accident.
func canonicalSPIFFEID(u *url.URL) (string, bool) {
	if u == nil || !strings.EqualFold(u.Scheme, "spiffe") {
		return "", false
	}
	if u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", false
	}
	if u.Port() != "" || u.Host == "" || u.Path == "" || u.Path == "/" {
		return "", false
	}
	return "spiffe://" + strings.ToLower(u.Host) + path.Clean(u.Path), true
}

// identityPattern is an entry of the certificate allow-list: an exact identity,
// or a DNS name with a single leading "*." wildcard matching one or more leading
// labels. It is deliberately narrower than the policy file's repository globs —
// an identity is not a path, and a wildcard here grants access.
type identityPattern struct {
	// suffix is set for a wildcard pattern, and is the ".rest" the name must end
	// with (with at least one label before it).
	suffix string
	// exact is set for a non-wildcard pattern.
	exact string
}

func compileIdentityPattern(pattern string) (identityPattern, error) {
	if pattern == "" {
		return identityPattern{}, errors.New("--allowed-client-id must not be empty")
	}
	if rest, ok := strings.CutPrefix(pattern, "*."); ok {
		if rest == "" || strings.Contains(rest, "*") {
			return identityPattern{}, fmt.Errorf("--allowed-client-id %q: only a single leading \"*.\" wildcard is supported", pattern)
		}
		return identityPattern{suffix: "." + strings.ToLower(rest)}, nil
	}
	if strings.Contains(pattern, "*") {
		return identityPattern{}, fmt.Errorf("--allowed-client-id %q: only a single leading \"*.\" wildcard is supported", pattern)
	}
	if u, err := url.Parse(pattern); err == nil && strings.EqualFold(u.Scheme, "spiffe") {
		id, ok := canonicalSPIFFEID(u)
		if !ok {
			return identityPattern{}, fmt.Errorf("--allowed-client-id %q is not a valid SPIFFE ID", pattern)
		}
		return identityPattern{exact: id}, nil
	}
	return identityPattern{exact: strings.ToLower(pattern)}, nil
}

func (p identityPattern) matches(identity string) bool {
	if p.suffix != "" {
		// "*.example.com" matches "a.example.com" and "a.b.example.com", but not
		// bare "example.com".
		return strings.HasSuffix(identity, p.suffix) && len(identity) > len(p.suffix)
	}
	return identity == p.exact
}

// bearerToken extracts the token from an Authorization header value. The scheme
// is matched case-insensitively, as RFC 7235 requires.
func bearerToken(header string) (string, bool) {
	scheme, rest, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return "", false
	}
	token := strings.TrimSpace(rest)
	if token == "" {
		return "", false
	}
	return token, true
}

// Reload re-reads the token files, keeping the previous set on any error.
func (a *PeerAuth) Reload() error {
	return a.loadTokens()
}

// Watch re-reads the token files whenever they change, until done is closed.
func (a *PeerAuth) Watch(done <-chan struct{}, every time.Duration) {
	if len(a.opts.TokenFiles) == 0 {
		return
	}
	if every <= 0 {
		every = defaultReloadInterval
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if a.tokensChanged() {
				if err := a.Reload(); err != nil {
					a.log.Printf("token reload FAILED, keeping previous tokens: %v", err)
				}
			}
		}
	}
}

func (a *PeerAuth) tokensChanged() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, path := range a.opts.TokenFiles {
		stamp, err := stampOf(path)
		if err != nil || stamp != a.stamps[path] {
			return true
		}
	}
	return false
}

func (a *PeerAuth) loadTokens() error {
	if len(a.opts.TokenFiles) == 0 {
		return nil
	}
	var digests [][sha256.Size]byte
	var errs []error
	for _, file := range a.opts.TokenFiles {
		tokens, err := readTokenFile(file)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, token := range tokens {
			digests = append(digests, sha256.Sum256([]byte(token)))
		}
	}
	err := errors.Join(errs...)
	if err == nil && len(digests) == 0 {
		err = fmt.Errorf("no tokens found in %s", strings.Join(a.opts.TokenFiles, ", "))
	}
	if a.opts.OnReload != nil {
		a.opts.OnReload(materialToken, err)
	}
	if err != nil {
		return err
	}
	a.tokenDigests.Store(&digests)
	a.mu.Lock()
	for _, file := range a.opts.TokenFiles {
		if stamp, stampErr := stampOf(file); stampErr == nil {
			a.stamps[file] = stamp
		} else {
			delete(a.stamps, file)
		}
	}
	a.mu.Unlock()
	a.log.Printf("loaded %d client token(s) from %s", len(digests), strings.Join(a.opts.TokenFiles, ", "))
	return nil
}

// readTokenFile reads one token per line, ignoring blank lines and "#" comments,
// so several tokens can be valid at once: that is the rotation story — publish
// the new token, roll the clients, drop the old one.
func readTokenFile(path string) ([]string, error) {
	data, err := readTrimmedFile(path, maxTokenFileSize)
	if err != nil {
		return nil, fmt.Errorf("reading token file %q: %w", path, err)
	}
	var tokens []string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for line := 1; scanner.Scan(); line++ {
		token := strings.TrimSpace(scanner.Text())
		if token == "" || strings.HasPrefix(token, "#") {
			continue
		}
		if len(token) < minTokenLength {
			return nil, fmt.Errorf("%s:%d: token is shorter than %d characters", path, line, minTokenLength)
		}
		tokens = append(tokens, token)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading token file %q: %w", path, err)
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("token file %q contains no tokens", path)
	}
	return tokens, nil
}

// reviewCache memoizes TokenReview verdicts, keyed by the digest of the token so
// the token itself is never held. Failures are cached too, which is what keeps a
// flood of invalid tokens from turning into a flood of API server calls.
type reviewCache struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]reviewEntry
}

type reviewEntry struct {
	principal string
	err       error
	expires   time.Time
}

// maxReviewCacheEntries bounds the table so invalid tokens cannot grow it
// without limit. Past the cap the whole table is dropped, which costs one round
// of cache misses and needs no eviction bookkeeping.
const maxReviewCacheEntries = 4096

func newReviewCache() *reviewCache {
	return &reviewCache{entries: make(map[[sha256.Size]byte]reviewEntry)}
}

func (c *reviewCache) get(key [sha256.Size]byte, now time.Time) (string, error, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || now.After(entry.expires) {
		return "", nil, false
	}
	return entry.principal, entry.err, true
}

func (c *reviewCache) put(key [sha256.Size]byte, principal string, err error, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= maxReviewCacheEntries {
		c.entries = make(map[[sha256.Size]byte]reviewEntry, maxReviewCacheEntries)
	}
	c.entries[key] = reviewEntry{principal: principal, err: err, expires: now.Add(tokenReviewCacheTTL)}
}

// serviceAccountUsernamePrefix is the prefix Kubernetes gives every
// ServiceAccount username.
const serviceAccountUsernamePrefix = "system:serviceaccount:"

// TokenReviewer validates a bearer token for an audience and returns the
// username it authenticates as. It is an interface so tests need no API server.
type TokenReviewer interface {
	Review(ctx context.Context, token, audience string) (username string, err error)
}

// inClusterTokenReviewer validates tokens by POSTing a TokenReview to the
// Kubernetes API server. It speaks the API directly rather than through
// k8s.io/client-go: the request and response are a handful of fields, and the
// gateway keeps its dependencies to the standard library and
// go-containerregistry.
type inClusterTokenReviewer struct {
	endpoint  string
	client    *http.Client
	tokenPath string
}

// NewInClusterTokenReviewer builds a TokenReview client from the environment
// every pod has: the API server address, its CA, and the gateway's own projected
// ServiceAccount token.
//
// The gateway's ServiceAccount needs permission to create tokenreviews, which
// the built-in system:auth-delegator ClusterRole grants. It must be bound with a
// ClusterRoleBinding, since TokenReview is not namespaced.
func NewInClusterTokenReviewer() (TokenReviewer, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("ServiceAccount authentication needs to run in a Kubernetes pod (KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are unset)")
	}
	if _, err := os.Stat(serviceAccountTokenPath); err != nil {
		return nil, fmt.Errorf("reading the gateway's ServiceAccount token: %w", err)
	}
	client, err := inClusterAPIClient(10 * time.Second)
	if err != nil {
		return nil, err
	}
	return &inClusterTokenReviewer{
		endpoint:  fmt.Sprintf("https://%s/apis/authentication.k8s.io/v1/tokenreviews", net.JoinHostPort(host, port)),
		tokenPath: serviceAccountTokenPath,
		client:    client,
	}, nil
}

// inClusterAPIServerURL is the API server a pod reaches, from the environment
// every pod has.
func inClusterAPIServerURL() (string, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return "", errors.New("this needs to run in a Kubernetes pod (KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are unset)")
	}
	return "https://" + net.JoinHostPort(host, port), nil
}

// inClusterAPIClient builds a client that trusts the Kubernetes API server, using
// the CA every pod has mounted. A timeout of zero leaves the client without one,
// which is what a long-lived watch needs — such a caller must bound its requests
// with a context instead.
func inClusterAPIClient(timeout time.Duration) (*http.Client, error) {
	caPEM, err := os.ReadFile(serviceAccountCAPath)
	if err != nil {
		return nil, fmt.Errorf("reading the API server CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no PEM certificates in %s", serviceAccountCAPath)
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}, nil
}

// tokenReview is the subset of authentication.k8s.io/v1 TokenReview the gateway
// sends and reads.
type tokenReview struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Spec       tokenReviewSpec   `json:"spec"`
	Status     tokenReviewStatus `json:"status,omitempty"`
}

type tokenReviewSpec struct {
	Token     string   `json:"token"`
	Audiences []string `json:"audiences,omitempty"`
}

type tokenReviewStatus struct {
	Authenticated bool                `json:"authenticated"`
	User          tokenReviewUserInfo `json:"user"`
	Audiences     []string            `json:"audiences,omitempty"`
	Error         string              `json:"error,omitempty"`
}

type tokenReviewUserInfo struct {
	Username string              `json:"username"`
	UID      string              `json:"uid"`
	Groups   []string            `json:"groups,omitempty"`
	Extra    map[string][]string `json:"extra,omitempty"`
}

func (v *inClusterTokenReviewer) Review(ctx context.Context, token, audience string) (string, error) {
	// Read our own token per call: kubelet rotates it at 80% of its lifetime, so
	// a copy cached at startup starts failing within the hour.
	ownToken, err := readTrimmedFile(v.tokenPath, maxTokenFileSize)
	if err != nil {
		return "", fmt.Errorf("%w: reading our ServiceAccount token: %v", errPeerAuthUnavailable, err)
	}
	body, err := json.Marshal(tokenReview{
		APIVersion: "authentication.k8s.io/v1",
		Kind:       "TokenReview",
		Spec:       tokenReviewSpec{Token: token, Audiences: []string{audience}},
	})
	if err != nil {
		return "", fmt.Errorf("%w: %v", errPeerAuthUnavailable, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("%w: %v", errPeerAuthUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+string(bytes.TrimSpace(ownToken)))

	resp, err := v.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: TokenReview request: %v", errPeerAuthUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: TokenReview returned %d", errPeerAuthUnavailable, resp.StatusCode)
	}
	var review tokenReview
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&review); err != nil {
		return "", fmt.Errorf("%w: decoding TokenReview response: %v", errPeerAuthUnavailable, err)
	}
	return validateTokenReview(review.Status, audience)
}

// validateTokenReview applies the three checks a TokenReview client must make.
// Skipping any of them turns the gateway into a confused deputy.
func validateTokenReview(status tokenReviewStatus, audience string) (string, error) {
	if status.Error != "" || !status.Authenticated {
		return "", errBadPeerCredential
	}
	// An empty status.audiences with authenticated=true means the token was valid
	// against the *API server's* audience. Every pod in the cluster is handed one
	// of those, so accepting it would authenticate the entire cluster. A
	// TokenReview server that ignores spec.audiences also shows up this way, and
	// must equally be refused.
	if len(status.Audiences) == 0 {
		return "", errBadPeerCredential
	}
	if !slices.Contains(status.Audiences, audience) {
		return "", errBadPeerCredential
	}
	if !strings.HasPrefix(status.User.Username, serviceAccountUsernamePrefix) {
		// Not a ServiceAccount: a user or another kind of identity. Not what this
		// method authenticates.
		return "", errPeerIdentityNotAllowed
	}
	return status.User.Username, nil
}
