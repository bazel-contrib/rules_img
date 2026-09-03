package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// This file implements the on-disk TLS material of a gateway: the server (or
// client) keypair and the CA bundle used to verify the other side. All of it is
// re-read while the process runs, because in Kubernetes these files are rotated
// under a running pod and a gateway that only reads them at startup eventually
// serves an expired certificate.
//
// Two properties of net/http and crypto/tls shape the design:
//
//   - [http.Server.ServeTLS] clones its TLSConfig once, before serving, so
//     mutating that config afterwards has no effect. Every piece of rotatable
//     material must therefore be reached through a callback:
//     [PeerTLS.GetCertificate] for the keypair and [PeerTLS.GetConfigForClient]
//     for the CA bundle (an [x509.CertPool] is append-only, so the pool in the
//     cloned config can never be narrowed).
//   - Kubernetes does not write these files in place: it builds a new directory
//     and swaps the "..data" symlink atomically. A file watch on the leaf path
//     misses that, so reloads are driven by polling os.Stat plus SIGHUP. The swap
//     can also land between the two reads [tls.LoadX509KeyPair] performs, which
//     yields a mismatched pair — so a failed reload always keeps the previous
//     material rather than storing nothing.

// Material names reported by the reload hook and the oci.gateway.material.reloads
// metric.
const (
	materialCertificate = "certificate"
	materialCA          = "ca"
	materialToken       = "token"
)

// defaultReloadInterval is how often on-disk material is re-stat'ed. Kubernetes
// syncs mounted Secrets about once a minute, so polling faster mostly costs
// syscalls.
const defaultReloadInterval = 30 * time.Second

// PeerTLSOptions configures a [PeerTLS].
type PeerTLSOptions struct {
	// CertFile and KeyFile are a PEM keypair. Both or neither.
	CertFile, KeyFile string
	// CAFile is a PEM bundle of trust anchors: the client CAs of a serving
	// gateway, or the roots a forwarding gateway verifies its peer against. Empty
	// means "no bundle" (for a client, the system roots).
	CAFile string
	// Logger records reloads. Defaults to the standard logger.
	Logger *log.Logger
	// OnReload, when set, is called after every reload attempt with the material
	// that was reloaded and the outcome, so a persistently failing reload can be
	// counted rather than only logged. A stale certificate is otherwise invisible
	// until it expires.
	OnReload func(material string, err error)
}

// PeerTLS holds the TLS material of one endpoint and keeps it fresh.
//
// It is safe for concurrent use: readers go through the atomic pointers, and only
// reloads take the mutex.
type PeerTLS struct {
	opts PeerTLSOptions
	log  *log.Logger

	cert atomic.Pointer[tls.Certificate]
	pool atomic.Pointer[x509.CertPool]

	// mu guards stamps, so two concurrent reloads cannot interleave their
	// change detection.
	mu     sync.Mutex
	stamps map[string]fileStamp
}

// NewPeerTLS loads the configured material once and returns a [PeerTLS] serving
// it. A missing or malformed file at startup is a hard error: the process should
// not come up believing it has TLS when it does not.
func NewPeerTLS(opts PeerTLSOptions) (*PeerTLS, error) {
	if (opts.CertFile == "") != (opts.KeyFile == "") {
		return nil, errors.New("a TLS certificate and key must be given together")
	}
	t := &PeerTLS{opts: opts, log: opts.Logger, stamps: make(map[string]fileStamp)}
	if t.log == nil {
		t.log = log.Default()
	}
	if err := t.loadCertificate(); err != nil {
		return nil, err
	}
	if err := t.loadCA(); err != nil {
		return nil, err
	}
	return t, nil
}

// HasCertificate reports whether a keypair is configured.
func (t *PeerTLS) HasCertificate() bool { return t.opts.CertFile != "" }

// HasCA reports whether a trust bundle is configured.
func (t *PeerTLS) HasCA() bool { return t.opts.CAFile != "" }

// GetCertificate implements [tls.Config.GetCertificate]. Leave
// tls.Config.Certificates nil so this is always consulted: crypto/tls otherwise
// only calls it when the client sends SNI.
func (t *PeerTLS) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if c := t.cert.Load(); c != nil {
		return c, nil
	}
	return nil, errors.New("gateway: no TLS certificate configured")
}

// GetClientCertificate implements [tls.Config.GetClientCertificate], so the same
// reloader serves a forwarding gateway's client certificate. Returning an empty
// certificate (rather than an error) means "I have none", which is what lets a
// peer that only checks a bearer token still complete the handshake.
func (t *PeerTLS) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	if c := t.cert.Load(); c != nil {
		return c, nil
	}
	return &tls.Certificate{}, nil
}

// CertPool returns the current trust bundle, or nil when none is configured.
func (t *PeerTLS) CertPool() *x509.CertPool { return t.pool.Load() }

// Reload re-reads every configured file. On failure the previous material stays
// in force and the error is returned, so a half-written or swapped-out file never
// takes the gateway down or leaves it without a certificate.
func (t *PeerTLS) Reload() error {
	var errs []error
	if err := t.loadCertificate(); err != nil {
		errs = append(errs, err)
	}
	if err := t.loadCA(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Watch re-reads the material whenever the files change on disk, until ctx is
// done. Polling rather than watching is deliberate: Kubernetes swaps the whole
// "..data" directory symlink, which an inotify watch on the leaf path misses.
func (t *PeerTLS) Watch(done <-chan struct{}, every time.Duration) {
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
			if t.changed() {
				if err := t.Reload(); err != nil {
					t.log.Printf("TLS material reload FAILED, keeping previous material: %v", err)
				}
			}
		}
	}
}

// changed reports whether any configured file looks different from the copy
// currently in force.
func (t *PeerTLS) changed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, path := range []string{t.opts.CertFile, t.opts.KeyFile, t.opts.CAFile} {
		if path == "" {
			continue
		}
		stamp, err := stampOf(path)
		// An unreadable file counts as changed so the reload runs, fails loudly,
		// and is counted.
		if err != nil || stamp != t.stamps[path] {
			return true
		}
	}
	return false
}

func (t *PeerTLS) loadCertificate() error {
	if t.opts.CertFile == "" {
		return nil
	}
	cert, err := tls.LoadX509KeyPair(t.opts.CertFile, t.opts.KeyFile)
	t.report(materialCertificate, err)
	if err != nil {
		return fmt.Errorf("loading TLS keypair %q/%q: %w", t.opts.CertFile, t.opts.KeyFile, err)
	}
	t.cert.Store(&cert)
	t.remember(t.opts.CertFile, t.opts.KeyFile)
	// LoadX509KeyPair populates Leaf, so the expiry can be logged for free — the
	// number an operator wants when a rotation is in question.
	if cert.Leaf != nil {
		t.log.Printf("loaded TLS certificate from %s (expires %s)", t.opts.CertFile, cert.Leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	return nil
}

func (t *PeerTLS) loadCA() error {
	if t.opts.CAFile == "" {
		return nil
	}
	pem, err := os.ReadFile(t.opts.CAFile)
	if err == nil && len(pem) == 0 {
		err = errors.New("file is empty")
	}
	var pool *x509.CertPool
	if err == nil {
		pool = x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			err = errors.New("no PEM certificates found")
		}
	}
	t.report(materialCA, err)
	if err != nil {
		return fmt.Errorf("loading CA bundle %q: %w", t.opts.CAFile, err)
	}
	t.pool.Store(pool)
	t.remember(t.opts.CAFile)
	return nil
}

// remember records the current stamp of the files a successful load consumed.
func (t *PeerTLS) remember(paths ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, path := range paths {
		if stamp, err := stampOf(path); err == nil {
			t.stamps[path] = stamp
		} else {
			// Forget it so the next tick retries rather than assuming success.
			delete(t.stamps, path)
		}
	}
}

func (t *PeerTLS) report(material string, err error) {
	if t.opts.OnReload != nil {
		t.opts.OnReload(material, err)
	}
}

// ServerConfig assembles the [tls.Config] of a serving gateway.
//
// clientAuth should be [tls.VerifyClientCertIfGiven] when client certificates
// are accepted: the chain is then verified against the CA bundle whenever one is
// presented, but a client with no certificate still completes the handshake, so
// the same listener can authenticate by bearer token and can answer an
// unauthenticated health probe. verify, when set, is installed as
// [tls.Config.VerifyConnection] — deliberately not VerifyPeerCertificate, which
// crypto/tls documents as *not* running on resumed connections.
//
// nextProtos must list the ALPN protocols in preference order. It is set
// explicitly because [http.Server.ServeTLS] only fixes up the config it clones,
// not the one GetConfigForClient hands out.
func (t *PeerTLS) ServerConfig(clientAuth tls.ClientAuthType, verify func(tls.ConnectionState) error, nextProtos []string) *tls.Config {
	base := &tls.Config{
		MinVersion:       tls.VersionTLS13,
		NextProtos:       nextProtos,
		GetCertificate:   t.GetCertificate,
		ClientAuth:       clientAuth,
		ClientCAs:        t.CertPool(),
		VerifyConnection: verify,
	}
	// Hand out a fresh config per handshake so a rotated CA bundle takes effect:
	// x509.CertPool cannot be narrowed in place, and the config http.Server holds
	// is a clone we can no longer reach.
	base.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		current := base.Clone()
		current.ClientCAs = t.CertPool()
		// Avoid recursing: the returned config is used as-is.
		current.GetConfigForClient = nil
		return current, nil
	}
	return base
}

// ClientConfig assembles the [tls.Config] a forwarding gateway uses to reach its
// peer. serverName overrides the name verified in the peer's certificate, which
// is needed when dialing a pod IP directly.
func (t *PeerTLS) ClientConfig(serverName string, skipVerify bool) *tls.Config {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		RootCAs:            t.CertPool(), // nil means the system roots
		ServerName:         serverName,
		InsecureSkipVerify: skipVerify, //nolint:gosec // guarded by --dangerously-skip-peer-verification
	}
	if t.HasCertificate() {
		cfg.GetClientCertificate = t.GetClientCertificate
	}
	return cfg
}

// fileStamp is the cheap change signal for a file whose contents we cache.
type fileStamp struct {
	size    int64
	modTime time.Time
}

func stampOf(path string) (fileStamp, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{size: info.Size(), modTime: info.ModTime()}, nil
}

// readTrimmedFile reads path and returns its contents with surrounding
// whitespace removed, bounded so a mistakenly configured huge file cannot be
// pulled into memory. It is used for credential files, where the content is a
// token rather than a document.
func readTrimmedFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file is larger than %d bytes", limit)
	}
	return data, nil
}
