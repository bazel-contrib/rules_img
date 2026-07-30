package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newTestPeerAuth builds a PeerAuth with logs discarded.
func newTestPeerAuth(t *testing.T, opts PeerAuthOptions) *PeerAuth {
	t.Helper()
	opts.Logger = log.New(io.Discard, "", 0)
	a, err := NewPeerAuth(opts)
	if err != nil {
		t.Fatalf("NewPeerAuth: %v", err)
	}
	return a
}

// writeTokens writes a token file and returns its path.
func writeTokens(t *testing.T, lines ...string) string {
	t.Helper()
	return writeFile(t, t.TempDir(), "tokens", []byte(strings.Join(lines, "\n")+"\n"))
}

// tokenRequest builds a request presenting an Authorization header.
func tokenRequest(header string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://gateway/v2/app/manifests/v1", nil)
	if header != "" {
		r.Header.Set("Authorization", header)
	}
	return r
}

const testToken = "0123456789abcdef0123456789abcdef" // 32 chars, the minimum

func TestPeerAuthStaticTokens(t *testing.T) {
	const second = "fedcba9876543210fedcba9876543210"
	auth := newTestPeerAuth(t, PeerAuthOptions{
		TokenFiles: []string{writeTokens(t, "# a comment", "", testToken, second)},
	})

	for _, tc := range []struct {
		name    string
		header  string
		wantErr error
	}{
		// The scheme is matched case-insensitively, as RFC 7235 requires.
		{"lowercase scheme", "bearer " + testToken, nil},
		{"canonical scheme", "Bearer " + testToken, nil},
		{"uppercase scheme", "BEARER " + testToken, nil},
		{"second token in the file", "Bearer " + second, nil},
		{"no header", "", errNoPeerCredential},
		{"basic auth", "Basic " + testToken, errNoPeerCredential},
		{"empty token", "Bearer ", errNoPeerCredential},
		// Same length as a valid token, so only the digest comparison rejects it.
		{"wrong token, same length", "Bearer 00000000000000000000000000000000", errBadPeerCredential},
		{"wrong token, different length", "Bearer short", errBadPeerCredential},
	} {
		t.Run(tc.name, func(t *testing.T) {
			principal, err := auth.Authenticate(tokenRequest(tc.header))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Authenticate error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && principal != "token" {
				t.Errorf("principal = %q, want %q", principal, "token")
			}
		})
	}
}

func TestPeerAuthNeverLeaksTheCredential(t *testing.T) {
	auth := newTestPeerAuth(t, PeerAuthOptions{TokenFiles: []string{writeTokens(t, testToken)}})
	const presented = "a-secret-nobody-should-ever-see-in-a-log"

	_, err := auth.Authenticate(tokenRequest("Bearer " + presented))
	if err == nil {
		t.Fatal("Authenticate accepted an invalid token")
	}
	if strings.Contains(err.Error(), presented) {
		t.Errorf("the error message contains the presented credential: %v", err)
	}
}

func TestPeerAuthRemovesTheCredentialFromTheRequest(t *testing.T) {
	auth := newTestPeerAuth(t, PeerAuthOptions{TokenFiles: []string{writeTokens(t, testToken)}})
	r := tokenRequest("Bearer " + testToken)

	if _, err := auth.Authenticate(r); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	// Nothing downstream may see it; the upstream leg strips Authorization again
	// independently, so this is the first of two barriers.
	if got := r.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want it removed from the request", got)
	}
}

func TestPeerAuthStripsSpoofableHeaders(t *testing.T) {
	auth := newTestPeerAuth(t, PeerAuthOptions{TokenFiles: []string{writeTokens(t, testToken)}})
	r := tokenRequest("Bearer " + testToken)
	// A mesh's identity headers included: they are attacker-controlled unless the
	// listener is provably unreachable except through the mesh, which the gateway
	// cannot verify.
	for _, header := range spoofableHeaders {
		r.Header.Set(header, "forged")
	}

	if _, err := auth.Authenticate(r); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	for _, header := range spoofableHeaders {
		if got := r.Header.Get(header); got != "" {
			t.Errorf("%s = %q, want it stripped", header, got)
		}
	}
}

func TestPeerAuthRejectsShortTokens(t *testing.T) {
	// A token shorter than the minimum is almost always a flag pointing at the
	// wrong (or a truncated) file, which in a partially-configured gateway would
	// be a silent authentication bypass.
	_, err := NewPeerAuth(PeerAuthOptions{
		Logger:     log.New(io.Discard, "", 0),
		TokenFiles: []string{writeTokens(t, "too-short")},
	})
	if err == nil {
		t.Fatal("NewPeerAuth accepted a token shorter than the minimum")
	}
}

func TestPeerAuthRejectsAnEmptyTokenFile(t *testing.T) {
	_, err := NewPeerAuth(PeerAuthOptions{
		Logger:     log.New(io.Discard, "", 0),
		TokenFiles: []string{writeFile(t, t.TempDir(), "tokens", []byte("# only a comment\n"))},
	})
	if err == nil {
		t.Fatal("NewPeerAuth accepted a file with no tokens")
	}
}

func TestPeerAuthRequiresAtLeastOneMethod(t *testing.T) {
	if _, err := NewPeerAuth(PeerAuthOptions{Logger: log.New(io.Discard, "", 0)}); err == nil {
		t.Fatal("NewPeerAuth accepted a configuration that authenticates nobody")
	}
}

func TestPeerAuthTokenReload(t *testing.T) {
	const rotated = "aaaabbbbccccddddaaaabbbbccccdddd"
	dir := t.TempDir()
	path := writeFile(t, dir, "tokens", []byte(testToken+"\n"))
	auth := newTestPeerAuth(t, PeerAuthOptions{TokenFiles: []string{path}})

	// Both tokens valid at once is the rotation story: publish the new one, roll
	// the clients, then drop the old one.
	writeFile(t, dir, "tokens", []byte(testToken+"\n"+rotated+"\n"))
	if err := auth.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	for _, token := range []string{testToken, rotated} {
		if _, err := auth.Authenticate(tokenRequest("Bearer " + token)); err != nil {
			t.Errorf("token %q rejected after reload: %v", token, err)
		}
	}

	// A broken file must keep the previous set rather than locking every client out.
	writeFile(t, dir, "tokens", []byte("x\n"))
	if err := auth.Reload(); err == nil {
		t.Fatal("Reload accepted a file with a too-short token")
	}
	if _, err := auth.Authenticate(tokenRequest("Bearer " + rotated)); err != nil {
		t.Errorf("a failed reload dropped the previous tokens: %v", err)
	}
}

// certRequest builds a request carrying a verified client certificate, which is
// what net/http hands a handler after a successful mTLS handshake.
func certRequest(leaf *x509.Certificate) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://gateway/v2/app/manifests/v1", nil)
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	return r
}

func TestPeerAuthCertificateIdentity(t *testing.T) {
	ca := newTestCA(t, "test client CA")
	dir := t.TempDir()
	caFile := writeFile(t, dir, "ca.crt", ca.pem)
	peerTLS, err := NewPeerTLS(PeerTLSOptions{CAFile: caFile, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatalf("NewPeerTLS: %v", err)
	}

	for _, tc := range []struct {
		name      string
		allowed   []string
		leaf      leafOptions
		wantErr   error
		wantPrinc string
	}{{
		name:      "spiffe id in the allow-list",
		allowed:   []string{"spiffe://cluster.local/ns/bb/sa/worker"},
		leaf:      leafOptions{uris: []string{"spiffe://cluster.local/ns/bb/sa/worker"}},
		wantPrinc: "cert:spiffe://cluster.local/ns/bb/sa/worker",
	}, {
		name:      "spiffe trust domain is case-insensitive",
		allowed:   []string{"spiffe://cluster.local/ns/bb/sa/worker"},
		leaf:      leafOptions{uris: []string{"spiffe://CLUSTER.LOCAL/ns/bb/sa/worker"}},
		wantPrinc: "cert:spiffe://cluster.local/ns/bb/sa/worker",
	}, {
		// The path is case-sensitive, so a different case is a different identity.
		name:    "spiffe path is case-sensitive",
		allowed: []string{"spiffe://cluster.local/ns/bb/sa/worker"},
		leaf:    leafOptions{uris: []string{"spiffe://cluster.local/ns/bb/sa/WORKER"}},
		wantErr: errPeerIdentityNotAllowed,
	}, {
		name:    "spiffe id not in the allow-list",
		allowed: []string{"spiffe://cluster.local/ns/bb/sa/worker"},
		leaf:    leafOptions{uris: []string{"spiffe://cluster.local/ns/other/sa/attacker"}},
		wantErr: errPeerIdentityNotAllowed,
	}, {
		// A SPIFFE X.509-SVID carries exactly one URI SAN, so two is not an SVID.
		name:    "two uri sans is not an svid",
		allowed: []string{"spiffe://cluster.local/ns/bb/sa/worker"},
		leaf: leafOptions{uris: []string{
			"spiffe://cluster.local/ns/bb/sa/worker",
			"spiffe://cluster.local/ns/bb/sa/other",
		}},
		wantErr: errPeerIdentityNotAllowed,
	}, {
		name:      "dns san in the allow-list",
		allowed:   []string{"worker.bb.svc"},
		leaf:      leafOptions{dnsNames: []string{"worker.bb.svc"}},
		wantPrinc: "cert:worker.bb.svc",
	}, {
		name:      "dns san matched case-insensitively",
		allowed:   []string{"worker.bb.svc"},
		leaf:      leafOptions{dnsNames: []string{"WORKER.BB.SVC"}},
		wantPrinc: "cert:worker.bb.svc",
	}, {
		name:      "dns wildcard",
		allowed:   []string{"*.bb.svc"},
		leaf:      leafOptions{dnsNames: []string{"worker-7.bb.svc"}},
		wantPrinc: "cert:worker-7.bb.svc",
	}, {
		// "*.bb.svc" grants access, so it must not also match the bare suffix.
		name:    "dns wildcard does not match the bare suffix",
		allowed: []string{"*.bb.svc"},
		leaf:    leafOptions{dnsNames: []string{"bb.svc"}},
		wantErr: errPeerIdentityNotAllowed,
	}, {
		// Never fall back to the Subject Common Name: no modern verifier honors it,
		// and treating it as a name would accept certificates whose SANs say nothing.
		name:    "common name is never an identity",
		allowed: []string{"worker.bb.svc"},
		leaf:    leafOptions{commonName: "worker.bb.svc"},
		wantErr: errPeerIdentityNotAllowed,
	}, {
		name:    "no sans at all",
		allowed: nil,
		leaf:    leafOptions{commonName: "nameless"},
		wantErr: errPeerIdentityNotAllowed,
	}, {
		name:      "empty allow-list accepts any leaf from the CA",
		allowed:   nil,
		leaf:      leafOptions{dnsNames: []string{"anything.test"}},
		wantPrinc: "cert:anything.test",
	}, {
		// A handshake that happened while the certificate was valid must not keep
		// authorizing requests after it expires: the connection is long-lived.
		name:    "expired leaf",
		allowed: nil,
		leaf: leafOptions{
			dnsNames:  []string{"worker.bb.svc"},
			notBefore: time.Now().Add(-2 * time.Hour),
			notAfter:  time.Now().Add(-time.Hour),
		},
		wantErr: errPeerIdentityNotAllowed,
	}, {
		name:    "not yet valid leaf",
		allowed: nil,
		leaf: leafOptions{
			dnsNames:  []string{"worker.bb.svc"},
			notBefore: time.Now().Add(time.Hour),
			notAfter:  time.Now().Add(2 * time.Hour),
		},
		wantErr: errPeerIdentityNotAllowed,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			auth := newTestPeerAuth(t, PeerAuthOptions{TLS: peerTLS, AllowedClientIDs: tc.allowed})
			_, _, leaf := ca.issue(t, tc.leaf)
			principal, err := auth.Authenticate(certRequest(leaf))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Authenticate error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && principal != tc.wantPrinc {
				t.Errorf("principal = %q, want %q", principal, tc.wantPrinc)
			}
		})
	}
}

func TestPeerAuthCertificateDoesNotFallThroughToTokens(t *testing.T) {
	// A certificate that is present but not allowed is a deliberate configuration
	// decision, not a reason to go looking for a token.
	ca := newTestCA(t, "test client CA")
	peerTLS, err := NewPeerTLS(PeerTLSOptions{
		CAFile: writeFile(t, t.TempDir(), "ca.crt", ca.pem),
		Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("NewPeerTLS: %v", err)
	}
	auth := newTestPeerAuth(t, PeerAuthOptions{
		TLS:              peerTLS,
		AllowedClientIDs: []string{"worker.bb.svc"},
		TokenFiles:       []string{writeTokens(t, testToken)},
	})
	_, _, leaf := ca.issue(t, leafOptions{dnsNames: []string{"stranger.test"}})

	r := certRequest(leaf)
	r.Header.Set("Authorization", "Bearer "+testToken)
	if _, err := auth.Authenticate(r); !errors.Is(err, errPeerIdentityNotAllowed) {
		t.Fatalf("Authenticate error = %v, want %v", err, errPeerIdentityNotAllowed)
	}
}

func TestPeerAuthIdentityIsRecheckedPerRequest(t *testing.T) {
	// The whole point of the second hop is one long-lived multiplexed connection,
	// and VerifyConnection runs once per handshake — so removing an identity from
	// the allow-list has to take effect on the next request, not on the next
	// connection. With no CRL and no OCSP anywhere in this design, the allow-list
	// *is* the revocation mechanism.
	ca := newTestCA(t, "test client CA")
	peerTLS, err := NewPeerTLS(PeerTLSOptions{
		CAFile: writeFile(t, t.TempDir(), "ca.crt", ca.pem),
		Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("NewPeerTLS: %v", err)
	}
	_, _, leaf := ca.issue(t, leafOptions{dnsNames: []string{"worker.bb.svc"}})

	allowed := newTestPeerAuth(t, PeerAuthOptions{TLS: peerTLS, AllowedClientIDs: []string{"worker.bb.svc"}})
	if err := allowed.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}); err != nil {
		t.Fatalf("VerifyConnection rejected an allowed identity: %v", err)
	}
	if _, err := allowed.Authenticate(certRequest(leaf)); err != nil {
		t.Fatalf("Authenticate rejected an allowed identity: %v", err)
	}

	// The same certificate, on a connection that was already admitted, against a
	// narrowed allow-list.
	narrowed := newTestPeerAuth(t, PeerAuthOptions{TLS: peerTLS, AllowedClientIDs: []string{"someone-else.bb.svc"}})
	if _, err := narrowed.Authenticate(certRequest(leaf)); !errors.Is(err, errPeerIdentityNotAllowed) {
		t.Fatalf("Authenticate error = %v, want %v", err, errPeerIdentityNotAllowed)
	}
}

func TestPeerAuthClientAuthType(t *testing.T) {
	// VerifyClientCertIfGiven, not RequireAndVerifyClientCert: that is what makes
	// mTLS optional, lets a token be the fallback, and lets an unauthenticated
	// health probe complete a handshake.
	ca := newTestCA(t, "test client CA")
	peerTLS, err := NewPeerTLS(PeerTLSOptions{
		CAFile: writeFile(t, t.TempDir(), "ca.crt", ca.pem),
		Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("NewPeerTLS: %v", err)
	}
	withCerts := newTestPeerAuth(t, PeerAuthOptions{TLS: peerTLS})
	if got := withCerts.ClientAuthType(); got != tls.VerifyClientCertIfGiven {
		t.Errorf("ClientAuthType = %v, want VerifyClientCertIfGiven", got)
	}
	tokensOnly := newTestPeerAuth(t, PeerAuthOptions{TokenFiles: []string{writeTokens(t, testToken)}})
	if got := tokensOnly.ClientAuthType(); got != tls.NoClientCert {
		t.Errorf("ClientAuthType = %v, want NoClientCert", got)
	}
}

func TestCanonicalSPIFFEID(t *testing.T) {
	// url.URL round-trips forms a SPIFFE ID may not contain, so each is rejected
	// explicitly rather than normalized away: two spellings must never compare
	// equal by accident.
	for _, tc := range []struct {
		raw  string
		want string
		ok   bool
	}{
		{"spiffe://cluster.local/ns/bb/sa/worker", "spiffe://cluster.local/ns/bb/sa/worker", true},
		{"SPIFFE://cluster.local/ns/bb/sa/worker", "spiffe://cluster.local/ns/bb/sa/worker", true},
		{"spiffe://CLUSTER.local/ns/bb/sa/worker", "spiffe://cluster.local/ns/bb/sa/worker", true},
		{"spiffe://cluster.local/ns/bb/sa/Worker", "spiffe://cluster.local/ns/bb/sa/Worker", true},
		{"spiffe://cluster.local/ns/bb/sa/worker/", "spiffe://cluster.local/ns/bb/sa/worker", true},
		{"spiffe://cluster.local/ns/bb/../bb/sa/worker", "spiffe://cluster.local/ns/bb/sa/worker", true},
		{"https://cluster.local/ns/bb/sa/worker", "", false},
		{"spiffe:opaque", "", false},
		{"spiffe://user@cluster.local/ns/bb", "", false},
		{"spiffe://cluster.local/ns/bb?x=1", "", false},
		{"spiffe://cluster.local/ns/bb#frag", "", false},
		{"spiffe://cluster.local:8443/ns/bb", "", false},
		{"spiffe:///ns/bb", "", false},
		{"spiffe://cluster.local", "", false},
		{"spiffe://cluster.local/", "", false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			u, err := url.Parse(tc.raw)
			if err != nil {
				t.Fatalf("parsing %q: %v", tc.raw, err)
			}
			got, ok := canonicalSPIFFEID(u)
			if ok != tc.ok {
				t.Fatalf("canonicalSPIFFEID(%q) ok = %v, want %v (got %q)", tc.raw, ok, tc.ok, got)
			}
			if ok && got != tc.want {
				t.Errorf("canonicalSPIFFEID(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestCompileIdentityPatternRejectsAmbiguousWildcards(t *testing.T) {
	// A wildcard here grants access, so the dialect is deliberately narrower than
	// the policy file's repository globs.
	for _, pattern := range []string{"", "*", "wor*.bb.svc", "*.*.svc", "spiffe://cluster.local"} {
		if _, err := compileIdentityPattern(pattern); err == nil {
			t.Errorf("compileIdentityPattern(%q) was accepted, want an error", pattern)
		}
	}
}

// stubReviewer is a TokenReviewer that answers from a table, counting calls so a
// test can prove the cache is used.
type stubReviewer struct {
	calls    int
	username string
	err      error
}

func (s *stubReviewer) Review(context.Context, string, string) (string, error) {
	s.calls++
	return s.username, s.err
}

func TestPeerAuthServiceAccountToken(t *testing.T) {
	reviewer := &stubReviewer{username: "system:serviceaccount:bb:worker"}
	auth := newTestPeerAuth(t, PeerAuthOptions{
		ServiceAccountAudience: "oci-distribution-gateway",
		AllowedServiceAccounts: []string{"system:serviceaccount:bb:worker"},
		Reviewer:               reviewer,
	})

	principal, err := auth.Authenticate(tokenRequest("Bearer projected-token"))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if want := "serviceaccount:system:serviceaccount:bb:worker"; principal != want {
		t.Errorf("principal = %q, want %q", principal, want)
	}

	// A second request with the same token is answered from the cache, so a busy
	// gateway does not call the API server per request.
	if _, err := auth.Authenticate(tokenRequest("Bearer projected-token")); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if reviewer.calls != 1 {
		t.Errorf("TokenReview calls = %d, want 1 (the second answer should be cached)", reviewer.calls)
	}
}

func TestPeerAuthServiceAccountAllowList(t *testing.T) {
	auth := newTestPeerAuth(t, PeerAuthOptions{
		ServiceAccountAudience: "oci-distribution-gateway",
		AllowedServiceAccounts: []string{"system:serviceaccount:bb:worker"},
		Reviewer:               &stubReviewer{username: "system:serviceaccount:other:attacker"},
	})

	if _, err := auth.Authenticate(tokenRequest("Bearer projected-token")); !errors.Is(err, errBadPeerCredential) {
		// Authenticate collapses a disallowed identity into "rejected" for the
		// bearer path, since an unauthenticated caller learns nothing either way.
		t.Fatalf("Authenticate error = %v, want a rejection", err)
	}
}

func TestPeerAuthRejectsMalformedServiceAccountAllowList(t *testing.T) {
	_, err := NewPeerAuth(PeerAuthOptions{
		Logger:                 log.New(io.Discard, "", 0),
		ServiceAccountAudience: "aud",
		AllowedServiceAccounts: []string{"bb:worker"},
		Reviewer:               &stubReviewer{},
	})
	if err == nil {
		t.Fatal("NewPeerAuth accepted a ServiceAccount without the system:serviceaccount: prefix")
	}
}

func TestPeerAuthFailsClosedWhenValidationIsUnavailable(t *testing.T) {
	// An API server we cannot reach must not silently become "the token was
	// wrong", and the verdict must not be cached: that would extend an outage
	// past its cause.
	reviewer := &stubReviewer{err: errPeerAuthUnavailable}
	auth := newTestPeerAuth(t, PeerAuthOptions{
		ServiceAccountAudience: "aud",
		Reviewer:               reviewer,
	})

	for i := range 2 {
		if _, err := auth.Authenticate(tokenRequest("Bearer projected-token")); !errors.Is(err, errPeerAuthUnavailable) {
			t.Fatalf("attempt %d: Authenticate error = %v, want %v", i, err, errPeerAuthUnavailable)
		}
	}
	if reviewer.calls != 2 {
		t.Errorf("TokenReview calls = %d, want 2 (an unavailable validator must not be cached)", reviewer.calls)
	}
}

func TestValidateTokenReview(t *testing.T) {
	const audience = "oci-distribution-gateway"
	for _, tc := range []struct {
		name    string
		status  tokenReviewStatus
		want    string
		wantErr error
	}{{
		name: "authenticated for our audience",
		status: tokenReviewStatus{
			Authenticated: true,
			Audiences:     []string{audience},
			User:          tokenReviewUserInfo{Username: "system:serviceaccount:bb:worker"},
		},
		want: "system:serviceaccount:bb:worker",
	}, {
		name:    "not authenticated",
		status:  tokenReviewStatus{Audiences: []string{audience}},
		wantErr: errBadPeerCredential,
	}, {
		name: "status carries an error",
		status: tokenReviewStatus{
			Authenticated: true, Error: "token expired", Audiences: []string{audience},
		},
		wantErr: errBadPeerCredential,
	}, {
		// An empty status.audiences with authenticated=true means the token was
		// valid against the *API server's* audience. Every pod in the cluster is
		// handed one of those, so accepting it would authenticate the whole
		// cluster. A TokenReview server that ignores spec.audiences looks the same
		// and must equally be refused.
		name: "empty audiences means the api server's own audience",
		status: tokenReviewStatus{
			Authenticated: true,
			User:          tokenReviewUserInfo{Username: "system:serviceaccount:bb:worker"},
		},
		wantErr: errBadPeerCredential,
	}, {
		name: "wrong audience",
		status: tokenReviewStatus{
			Authenticated: true,
			Audiences:     []string{"some-other-service"},
			User:          tokenReviewUserInfo{Username: "system:serviceaccount:bb:worker"},
		},
		wantErr: errBadPeerCredential,
	}, {
		name: "not a service account",
		status: tokenReviewStatus{
			Authenticated: true,
			Audiences:     []string{audience},
			User:          tokenReviewUserInfo{Username: "alice@example.com"},
		},
		wantErr: errPeerIdentityNotAllowed,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateTokenReview(tc.status, audience)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("validateTokenReview error = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("username = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPeerAuthResponseStatuses(t *testing.T) {
	// The forwarding side keys off these, and a 401 must never carry
	// WWW-Authenticate (see TestHandlerPeerAuthSendsNoChallenge).
	for _, tc := range []struct {
		err        error
		wantStatus int
		wantType   string
	}{
		{errNoPeerCredential, http.StatusUnauthorized, errPeerUnauthenticated},
		{errBadPeerCredential, http.StatusUnauthorized, errPeerBadCredential},
		{errPeerIdentityNotAllowed, http.StatusForbidden, errPeerIdentityDenied},
		{errPeerAuthUnavailable, http.StatusServiceUnavailable, errPeerAuthFailed},
	} {
		status, _, errType, message := peerAuthResponse(tc.err)
		if status != tc.wantStatus || errType != tc.wantType {
			t.Errorf("peerAuthResponse(%v) = (%d, %s), want (%d, %s)", tc.err, status, errType, tc.wantStatus, tc.wantType)
		}
		if message == "" {
			t.Errorf("peerAuthResponse(%v) produced no message", tc.err)
		}
	}
}
