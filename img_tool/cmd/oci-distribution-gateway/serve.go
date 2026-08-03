package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	reg "github.com/bazel-contrib/rules_img/img_tool/pkg/auth/registry"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/serve/gateway"
)

// serveFlags holds the flags of the serving mode.
type serveFlags struct {
	address              string
	port                 int
	unixSocket           string
	unixSocketMode       string
	defaultRegistry      string
	credentialHelperPath string
	policyFile           string
	validatePolicy       bool
	dangerouslyAllowAll  bool
	shutdownTimeout      time.Duration
	denyPrivateUpstreams bool

	// Blob existence cache.
	blobCacheTTL       time.Duration
	blobCacheMaxMemory byteSizeFlag

	// Replication of the blob existence cache between the instances of a serving
	// deployment.
	cachePeers              repeatedFlag
	cachePeerService        string
	cachePeerServerName     string
	cachePeerTokenFile      string
	allowedCachePeerIDs     repeatedFlag
	cacheBatchSize          int
	cacheWarmupTimeout      time.Duration
	cacheWarmupEntries      int
	allowPlaintextCachePeer bool

	// TLS and client authentication.
	tlsCertFile              string
	tlsKeyFile               string
	clientCAFile             string
	allowedClientIDs         repeatedFlag
	clientTokenFiles         repeatedFlag
	serviceAccountAudience   string
	allowedServiceAccounts   repeatedFlag
	allowPlaintextH2C        bool
	allowUnauthenticatedText bool

	// A second listener for peer (instance-to-instance) traffic, so a deployment
	// can serve its clients one way and its peers another.
	peerAddress                      string
	peerPort                         int
	peerTLSCertFile                  string
	peerTLSKeyFile                   string
	peerClientCAFile                 string
	allowUnauthenticatedPeerListener bool

	metrics metricsFlags
}

// defaultBlobCacheMemory is how much memory the blob existence cache is allowed
// by default. It holds on the order of a hundred thousand blob digests, which
// covers a build farm's working set several times over, and it is preallocated,
// so it is a fixed addition to the pod's memory request rather than a number that
// grows under load.
const defaultBlobCacheMemory = 64 << 20

// Defaults of cache replication. The warm-up numbers are the ones an operator is
// most likely to want to change: 20,000 entries is a large farm's hot working set
// and costs a few megabytes to transfer, and ten seconds is long enough for a
// peer to hand them over while being far inside any sensible readiness deadline.
const (
	defaultCacheBatchSize     = 256
	defaultCacheWarmupTimeout = 10 * time.Second
	defaultCacheWarmupEntries = 20_000
)

func (f *serveFlags) register(flagSet *flag.FlagSet) {
	flagSet.StringVar(&f.address, "address", "localhost", "Address to bind the gateway to (ignored when --unix-socket is set)")
	flagSet.IntVar(&f.port, "port", 0, "Port to bind the gateway to (0 picks a free port; ignored when --unix-socket is set)")
	flagSet.StringVar(&f.unixSocket, "unix-socket", "", "Path to a UNIX domain socket to listen on instead of TCP")
	flagSet.StringVar(&f.unixSocketMode, "unix-socket-mode", "", "Octal mode to chmod the UNIX socket to after binding, e.g. 0660. Empty leaves the mode the OS created it with, which is what a build action running as another user usually needs.")
	flagSet.StringVar(&f.defaultRegistry, "default-registry", "", "Upstream registry to forward to when a request omits the X-rules_img-Original-Host header (must also be allowed by the policy)")
	flagSet.StringVar(&f.credentialHelperPath, "credential-helper", "", "Path to a Bazel credential helper binary used to authenticate to upstream registries (optional)")
	flagSet.StringVar(&f.policyFile, "policy-file", "", "Path to a JSON (or YAML) policy file with per-repository allow/deny rules. Required unless --dangerously-allow-all is set. Reloadable at runtime with SIGHUP.")
	flagSet.BoolVar(&f.validatePolicy, "validate-policy", false, "Load and validate --policy-file, then exit (0 if valid, non-zero otherwise). Does not start the gateway.")
	flagSet.BoolVar(&f.dangerouslyAllowAll, "dangerously-allow-all", false, "Allow every request to every upstream, ignoring the policy file. DANGEROUS: only for trusted, isolated environments.")
	flagSet.DurationVar(&f.shutdownTimeout, "shutdown-timeout", 30*time.Second, "How long in-flight requests may take to finish after a shutdown signal. Set this above your longest blob transfer, and give the pod a terminationGracePeriodSeconds larger still, or a rolling update cuts transfers short.")
	flagSet.BoolVar(&f.denyPrivateUpstreams, "deny-private-upstreams", false, "Refuse upstream registries that resolve to a loopback, link-local, or private address. Recommended for a gateway shared between workloads; leave off when your registry is reachable only through an in-cluster (private) address.")

	f.blobCacheMaxMemory = defaultBlobCacheMemory
	flagSet.DurationVar(&f.blobCacheTTL, "blob-existence-cache-ttl", 6*time.Hour, "How long the gateway may assume a blob it has seen -- probed for, or pushed through it -- is still in its repository, answering HEAD probes for it without a round trip. 0 disables the cache. Keep it well inside the window in which your registry could garbage-collect a blob: a client that trusts a stale hit skips re-uploading a layer that is gone.")
	flagSet.Var(&f.blobCacheMaxMemory, "blob-existence-cache-max-memory", "Memory the blob existence cache may use, e.g. 64MiB. It is allocated in full at startup and never grows; when it is full the least recently used blob makes room. 0 disables the cache.")

	flagSet.Var(&f.cachePeers, "blob-existence-cache-peer", "Another instance of this gateway to replicate blob existence facts to, as https://host:port. Repeatable. Without it (or --blob-existence-cache-peer-service) each replica learns every blob for itself, so a first-seen blob costs one upstream probe per replica.")
	flagSet.StringVar(&f.cachePeerService, "blob-existence-cache-peer-service", "", "Discover the peers to replicate to from the Kubernetes EndpointSlices of this Service, as [<namespace>/]<name>. The set follows scaling and rolling updates with no restart. Requires an explicit --port and RBAC to get, list and watch endpointslices.")
	flagSet.StringVar(&f.cachePeerServerName, "blob-existence-cache-peer-server-name", "", "Name to verify in a peer's certificate. Needed with --blob-existence-cache-peer-service, which dials pod IPs that a Service certificate does not name.")
	flagSet.StringVar(&f.cachePeerTokenFile, "blob-existence-cache-peer-token-file", "", "File holding the bearer token presented to peers. Not needed when the peers accept this gateway's own TLS certificate (--tls-cert-file), which is what a symmetric deployment does. Re-read periodically, so a projected ServiceAccount token keeps working.")
	flagSet.Var(&f.allowedCachePeerIDs, "allowed-cache-peer-id", "Client identity permitted to write to this gateway's blob existence cache: a SPIFFE ID, a DNS name (a single leading \"*.\" wildcard is allowed), or a system:serviceaccount:<namespace>:<name>, matched as --allowed-client-id and --allowed-serviceaccount are. Repeatable. Without it every authenticated client may insert entries, and a client that inserts a blob which is not there makes push clients skip an upload they still owe.")
	flagSet.IntVar(&f.cacheBatchSize, "blob-existence-cache-replication-batch-size", defaultCacheBatchSize, "How many facts one replication message may carry. Facts are batched for a few milliseconds and sent when the batch is full or the timer expires, whichever comes first.")
	flagSet.DurationVar(&f.cacheWarmupTimeout, "blob-existence-cache-warmup-timeout", defaultCacheWarmupTimeout, "How long a starting instance may spend seeding its cache from a peer before reporting itself healthy. /healthz answers 503 until then, so a readiness probe keeps it out of the Service while it warms up. 0 starts serving immediately with an empty cache.")
	flagSet.IntVar(&f.cacheWarmupEntries, "blob-existence-cache-warmup-entries", defaultCacheWarmupEntries, "How many of a peer's hottest entries a starting instance asks for. 0 disables seeding.")
	flagSet.BoolVar(&f.allowPlaintextCachePeer, "dangerously-allow-plaintext-cache-peer", false, "Replicate the cache over plaintext HTTP. DANGEROUS: a bearer token presented to a peer then crosses the network in the clear, and anything on the path can inject facts about which blobs exist. The legitimate case is a service mesh providing mTLS underneath.")

	flagSet.StringVar(&f.tlsCertFile, "tls-cert-file", "", "PEM certificate (leaf plus any intermediates) to serve TLS with. Enables HTTP/2 via ALPN. Re-read when the file changes and on SIGHUP.")
	flagSet.StringVar(&f.tlsKeyFile, "tls-key-file", "", "PEM private key matching --tls-cert-file. Both or neither.")
	flagSet.StringVar(&f.clientCAFile, "client-ca-file", "", "PEM bundle of CAs whose client certificates are accepted. Setting it enables mTLS and requires --tls-cert-file. Re-read when the file changes and on SIGHUP.")
	flagSet.Var(&f.allowedClientIDs, "allowed-client-id", "Client identity permitted to use this gateway: a SPIFFE ID (spiffe://trust-domain/path) or a DNS name (a single leading \"*.\" wildcard is allowed). Repeatable. Without it, any certificate signed by --client-ca-file is accepted, which with a shared cluster CA is effectively cluster-wide access.")
	flagSet.Var(&f.clientTokenFiles, "client-token-file", "File of bearer tokens permitted to use this gateway, one per line ('#' comments and blank lines ignored) so several are valid at once during a rotation. Repeatable. Re-read when the file changes and on SIGHUP.")
	flagSet.StringVar(&f.serviceAccountAudience, "client-serviceaccount-audience", "", "Accept Kubernetes projected ServiceAccount tokens issued for this audience, validated with the TokenReview API. Must be an audience dedicated to this gateway: the token every pod gets by default is issued for the API server's audience, and accepting it would authenticate the whole cluster. Needs RBAC to create tokenreviews (the system:auth-delegator ClusterRole).")
	flagSet.Var(&f.allowedServiceAccounts, "allowed-serviceaccount", "ServiceAccount permitted to use this gateway, as system:serviceaccount:<namespace>:<name>. Repeatable. Without it, any ServiceAccount holding a token for the audience is accepted.")
	flagSet.BoolVar(&f.allowPlaintextH2C, "dangerously-allow-plaintext-h2c", false, "Accept prior-knowledge HTTP/2 (h2c) on the plaintext listener. DANGEROUS: sniffing the h2c preface happens before any read deadline is set, so a client that connects and stalls holds a connection indefinitely. Only for a listener reachable solely through a service-mesh sidecar.")
	flagSet.BoolVar(&f.allowUnauthenticatedText, "dangerously-allow-unauthenticated-clients", false, "Serve a network-reachable address without client authentication. DANGEROUS: this listener holds the upstream registry credentials, and a Kubernetes ClusterIP Service is reachable from every namespace.")

	flagSet.StringVar(&f.peerAddress, "peer-address", "", "Address of a second listener carrying only cache replication between the instances of this deployment, so peers can be authenticated differently from clients -- mTLS between instances while clients connect over plaintext, for example. Setting it (or --peer-port) moves the /_rules_img/cache/ endpoints off the client listener entirely. Defaults to --address once --peer-port is set.")
	flagSet.IntVar(&f.peerPort, "peer-port", 0, "Port of the peer listener. Required to enable it, and it is also the port a discovered peer is reached on, so every instance must use the same one.")
	flagSet.StringVar(&f.peerTLSCertFile, "peer-tls-cert-file", "", "PEM certificate the peer listener serves TLS with. Defaults to --tls-cert-file. Re-read when the file changes and on SIGHUP.")
	flagSet.StringVar(&f.peerTLSKeyFile, "peer-tls-key-file", "", "PEM private key matching --peer-tls-cert-file. Both or neither.")
	flagSet.StringVar(&f.peerClientCAFile, "peer-client-ca-file", "", "PEM bundle of CAs whose client certificates the peer listener accepts. Setting it enables mTLS there and requires a certificate for that listener. Defaults to --client-ca-file. This is the flag that lets instances speak mTLS to each other while clients do not.")
	flagSet.BoolVar(&f.allowUnauthenticatedPeerListener, "dangerously-allow-unauthenticated-peer-listener", false, "Run the peer listener with no client authentication. DANGEROUS: anything able to reach it can insert blob existence facts, and a client that believes a false one skips an upload it still owes.")

	f.metrics.register(flagSet)
}

func newServeFlagSet() (*flag.FlagSet, *serveFlags) {
	flagSet := flag.NewFlagSet("oci-distribution-gateway serve", flag.ContinueOnError)
	flags := &serveFlags{}
	flags.register(flagSet)
	flagSet.Usage = func() {
		printUsage(flagSet,
			"Run a forwarding OCI distribution gateway.\n\n"+
				"The gateway forwards requests to the upstream registry named in the\n"+
				"X-rules_img-Original-Host request header, subject to the policy file.",
			"oci-distribution-gateway serve --policy-file <path> [OPTIONS]",
			[]string{
				"oci-distribution-gateway serve --port 8080 --policy-file /etc/img/gateway-policy.json",
				"oci-distribution-gateway serve --unix-socket /run/gw.sock --policy-file /etc/img/gateway-policy.yaml",
				"oci-distribution-gateway serve --validate-policy --policy-file /etc/img/gateway-policy.json",
				"oci-distribution-gateway serve --unix-socket /run/gw.sock --dangerously-allow-all",
				"oci-distribution-gateway serve --port 8080 --policy-file /etc/img/policy.json --metrics-exporter prometheus",
				"oci-distribution-gateway serve --address 0.0.0.0 --port 8443 --policy-file /etc/img/policy.json \\\n      --tls-cert-file /tls/tls.crt --tls-key-file /tls/tls.key \\\n      --client-ca-file /tls/ca.crt --allowed-client-id spiffe://cluster.local/ns/bb/sa/worker",
				"oci-distribution-gateway serve --address 0.0.0.0 --port 8080 --policy-file /etc/img/policy.json \\\n      --dangerously-allow-unauthenticated-clients \\\n      --peer-port 8443 --peer-tls-cert-file /tls/tls.crt --peer-tls-key-file /tls/tls.key \\\n      --peer-client-ca-file /tls/ca.crt --allowed-cache-peer-id oci-gateway.img-gateway.svc \\\n      --blob-existence-cache-peer-service oci-distribution-gateway",
			})
	}
	return flagSet, flags
}

func serveProcess(ctx context.Context, args []string) {
	flagSet, flags := newServeFlagSet()
	parseFlags(flagSet, args)

	if flags.validatePolicy {
		if flags.policyFile == "" {
			fmt.Fprintln(os.Stderr, "Error: --validate-policy requires --policy-file")
			os.Exit(1)
		}
		cp, err := gateway.LoadPolicyFile(flags.policyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "policy %s is valid (%s)\n", flags.policyFile, cp.Summary())
		os.Exit(0)
	}

	socketMode, err := parseSocketMode(flags.unixSocketMode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if (flags.tlsCertFile == "") != (flags.tlsKeyFile == "") {
		fmt.Fprintln(os.Stderr, "Error: --tls-cert-file and --tls-key-file must be given together")
		os.Exit(1)
	}
	if flags.clientCAFile != "" && flags.tlsCertFile == "" {
		fmt.Fprintln(os.Stderr, "Error: --client-ca-file requires --tls-cert-file (a client certificate can only be presented over TLS)")
		os.Exit(1)
	}
	if err := flags.validatePeerListener(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	// A bound too small to hold one blob would leave a cache that stores nothing
	// while looking configured, so say so instead of starting up like that.
	if flags.blobCacheTTL > 0 && flags.blobCacheMaxMemory > 0 && int64(flags.blobCacheMaxMemory) < gateway.MinBlobExistenceCacheBytes {
		fmt.Fprintf(os.Stderr, "Error: --blob-existence-cache-max-memory %s is below the %d bytes one cached blob costs (pass 0 to disable the cache)\n",
			flags.blobCacheMaxMemory, gateway.MinBlobExistenceCacheBytes)
		os.Exit(1)
	}

	// Resolve the authorization policy. --dangerously-allow-all overrides (and
	// ignores) any policy file; otherwise a policy file is required.
	var authz *gateway.CompiledPolicy
	switch {
	case flags.dangerouslyAllowAll:
		if flags.policyFile != "" {
			log.Printf("warning: --dangerously-allow-all is set; ignoring --policy-file %s", flags.policyFile)
		}
		log.Printf("WARNING: --dangerously-allow-all is set; the gateway will forward EVERY request to any upstream without policy checks")
		authz = gateway.AllowAll()
	case flags.policyFile == "":
		fmt.Fprintln(os.Stderr, "Error: --policy-file is required (or pass --dangerously-allow-all to disable policy checks)")
		flagSet.Usage()
		os.Exit(1)
	default:
		cp, err := gateway.LoadPolicyFile(flags.policyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		log.Printf("loaded policy from %s (%s)", flags.policyFile, cp.Summary())
		authz = cp
	}
	// The upstream host comes from a client-supplied header, so a policy that puts
	// no bound on it lets a client aim the gateway at anything the gateway can
	// reach — including a plaintext internal service, since go-containerregistry
	// resolves private and loopback hosts to http://.
	if authz.ReachesAnyHost() && !flags.denyPrivateUpstreams {
		log.Printf("WARNING: this policy allows requests to hosts it does not name, so a client can aim the gateway at any address it can reach; consider --deny-private-upstreams and an egress NetworkPolicy")
	}

	if flags.credentialHelperPath != "" {
		// reg.Keychain() resolves the OCI-registry credential helper from the
		// environment; wire the flag through it as the registry-scoped helper.
		if err := os.Setenv(reg.EnvCredentialHelperOCIRegistry, flags.credentialHelperPath); err != nil {
			log.Fatalf("Failed to set credential helper: %v", err)
		}
	}

	metrics, metricsServer, flush := flags.metrics.setup(ctx, serveServiceName)
	defer flush()

	// recordReload is wired to the handler below; the reload watchers only start
	// after that, so the indirection is never read before it is set.
	var recordReload func(material string, err error)
	onReload := func(material string, err error) {
		if recordReload != nil {
			recordReload(material, err)
		}
	}

	var serverTLS *gateway.PeerTLS
	if flags.tlsCertFile != "" || flags.clientCAFile != "" {
		serverTLS, err = gateway.NewPeerTLS(gateway.PeerTLSOptions{
			CertFile: flags.tlsCertFile,
			KeyFile:  flags.tlsKeyFile,
			CAFile:   flags.clientCAFile,
			OnReload: onReload,
		})
		if err != nil {
			log.Fatalf("Failed to load TLS material: %v", err)
		}
	}

	var peers *peerListener
	peers, err = flags.buildPeerListener(onReload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	var peerAuth *gateway.PeerAuth
	if flags.clientCAFile != "" || len(flags.clientTokenFiles) > 0 || flags.serviceAccountAudience != "" {
		peerAuth, err = gateway.NewPeerAuth(gateway.PeerAuthOptions{
			TLS:                    serverTLS,
			AllowedClientIDs:       flags.allowedClientIDs,
			TokenFiles:             flags.clientTokenFiles,
			ServiceAccountAudience: flags.serviceAccountAudience,
			AllowedServiceAccounts: flags.allowedServiceAccounts,
			OnReload:               onReload,
		})
		if err != nil {
			log.Fatalf("Failed to configure client authentication: %v", err)
		}
		log.Printf("client authentication enabled (%s)", strings.Join(peerAuth.Methods(), ", "))
	}

	// A listener reachable from the network holds this fleet's registry
	// credentials, and in Kubernetes a ClusterIP Service is reachable from every
	// namespace. Refuse to serve one anonymously unless the operator says so out
	// loud. The default --address is localhost, so this only fires for a listener
	// that was deliberately opened up.
	if peerAuth == nil && reachableFromNetwork(flags.unixSocket, flags.address) && !flags.allowUnauthenticatedText {
		fmt.Fprintf(os.Stderr, "Error: refusing to serve %s:%d without client authentication.\n", flags.address, flags.port)
		fmt.Fprintln(os.Stderr, "Configure --client-ca-file, --client-token-file, or --client-serviceaccount-audience,")
		fmt.Fprintln(os.Stderr, "bind a loopback address or a UNIX socket instead, or pass")
		fmt.Fprintln(os.Stderr, "--dangerously-allow-unauthenticated-clients if the address really is private.")
		os.Exit(1)
	}

	handlerOpts := []gateway.Option{
		gateway.WithAuthorizer(authz),
		gateway.WithDefaultRegistry(flags.defaultRegistry),
		gateway.WithKeychain(reg.Keychain()),
		gateway.WithMeterProvider(metrics.MeterProvider),
		gateway.WithBlobExistenceCache(flags.blobCacheTTL, int64(flags.blobCacheMaxMemory)),
	}
	if peerAuth != nil {
		handlerOpts = append(handlerOpts, gateway.WithPeerAuth(peerAuth))
	}
	replication, err := flags.cacheReplication(serverTLS, peers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if replication != nil {
		handlerOpts = append(handlerOpts, gateway.WithCacheReplication(replication))
	}
	if flags.denyPrivateUpstreams {
		guarded, err := gateway.DenyPrivateAddresses(http.DefaultTransport)
		if err != nil {
			log.Fatalf("Failed to configure --deny-private-upstreams: %v", err)
		}
		handlerOpts = append(handlerOpts, gateway.WithBaseTransport(guarded))
	}
	handler := gateway.New(handlerOpts...)
	recordReload = handler.RecordMaterialReload

	// Split cache replication off onto its own listener when one is configured. It
	// has to happen before the client listener opens: from this call on, the client
	// listener answers the replication endpoints with 404, and a window in which it
	// still served them would be exactly the anonymous write path into this
	// instance's cache that the separate listener exists to close.
	var peerServer *gatewayListener
	if peers.enabled() {
		replicationHandler := handler.SeparateReplicationHandler(peers.auth)
		if replicationHandler == nil {
			fmt.Fprintln(os.Stderr, "Error: --peer-port configures a listener for cache replication, which is not enabled")
			os.Exit(1)
		}
		peerServer, err = peers.serve(replicationHandler)
		if err != nil {
			log.Fatalf("Failed to listen for peers: %v", err)
		}
		log.Printf("cache replication served on its own listener (%s)", peers.summary())
	}

	listener, cleanup, err := listen(flags.unixSocket, flags.address, flags.port, socketMode)
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}
	defer cleanup()

	server := &http.Server{
		Handler:           handler,
		Protocols:         serveProtocols(flags.unixSocket != "", flags.allowPlaintextH2C),
		HTTP2:             serveHTTP2Config(),
		ReadHeaderTimeout: 30 * time.Second,
		// Generous body timeouts: blob uploads and downloads can be large.
		ReadTimeout:  30 * time.Minute,
		WriteTimeout: 30 * time.Minute,
		IdleTimeout:  5 * time.Minute,
	}
	serve := func() error { return server.Serve(listener) }
	if serverTLS != nil && serverTLS.HasCertificate() {
		clientAuth := tls.NoClientCert
		var verify func(tls.ConnectionState) error
		if peerAuth != nil {
			clientAuth = peerAuth.ClientAuthType()
			verify = peerAuth.VerifyConnection
		}
		server.TLSConfig = serverTLS.ServerConfig(clientAuth, verify, nextProtosFor(server.Protocols))
		// The empty paths are correct: ServeTLS accepts them when the config can
		// produce a certificate, which ours does through GetCertificate.
		serve = func() error { return server.ServeTLS(listener, "", "") }
	}

	// Reload the policy file and every piece of on-disk TLS/token material on
	// SIGHUP, and poll for changes in between (Kubernetes swaps the whole "..data"
	// directory symlink, which a watch on the leaf path would miss). A failed
	// reload keeps what is already in force, so a bad edit never widens access or
	// takes the gateway down.
	done := make(chan struct{})
	defer close(done)
	if serverTLS != nil {
		go serverTLS.Watch(done, 0)
	}
	if peerAuth != nil {
		go peerAuth.Watch(done, 0)
	}
	peers.watch(done)
	// Peer discovery, the warm-up that seeds this instance's cache from a peer, and
	// the batching of outbound facts. The listener is already open, so a peer's
	// broadcast is accepted from here on — including while the warm-up runs, which
	// is why it is started before the server rather than after.
	go handler.RunCacheReplication(done)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if flags.policyFile != "" && !flags.dangerouslyAllowAll {
				if cp, err := handler.Reload(flags.policyFile); err != nil {
					log.Printf("policy reload FAILED, keeping previous policy: %v", err)
				} else {
					log.Printf("reloaded policy from %s (%s)", flags.policyFile, cp.Summary())
				}
			}
			if serverTLS != nil {
				if err := serverTLS.Reload(); err != nil {
					log.Printf("TLS material reload FAILED, keeping previous material: %v", err)
				}
			}
			if peerAuth != nil {
				if err := peerAuth.Reload(); err != nil {
					log.Printf("token reload FAILED, keeping previous tokens: %v", err)
				}
			}
			peers.reload()
		}
	}()

	(&gatewayServer{
		server:          server,
		serve:           serve,
		addr:            listener.Addr(),
		peer:            peerServer,
		metrics:         metricsServer,
		shutdownTimeout: flags.shutdownTimeout,
		banner:          "oci-distribution-gateway listening",
	}).run()
}

// cacheReplication builds the replication of the blob existence cache from the
// flags, or nil when no peers are configured.
//
// The credential this gateway presents to its peers deliberately defaults to its
// *serving* identity: in a symmetric deployment — one Deployment, one keypair, one
// CA — every instance already holds a certificate its peers accept and a CA bundle
// that verifies theirs, so replication needs no material of its own. A fleet whose
// peers authenticate by token instead passes --blob-existence-cache-peer-token-file.
//
// peers, when set, is the second listener replication is served on, and it takes
// over from the client listener in every respect: its TLS material is what this
// instance presents to its peers and verifies them with, and its port is the one a
// discovered peer is dialled on.
func (f *serveFlags) cacheReplication(serverTLS *gateway.PeerTLS, peers *peerListener) (*gateway.CacheReplication, error) {
	if len(f.cachePeers) == 0 && f.cachePeerService == "" {
		// Refuse to ignore a flag that only makes sense with peers: silently doing
		// nothing with --allowed-cache-peer-id would look like a control that is in
		// force when it is not.
		for _, orphan := range []struct {
			flag string
			set  bool
		}{
			{"--blob-existence-cache-peer-server-name", f.cachePeerServerName != ""},
			{"--blob-existence-cache-peer-token-file", f.cachePeerTokenFile != ""},
			{"--allowed-cache-peer-id", len(f.allowedCachePeerIDs) > 0},
			{"--dangerously-allow-plaintext-cache-peer", f.allowPlaintextCachePeer},
			{"--peer-port", peers.enabled()},
		} {
			if orphan.set {
				return nil, fmt.Errorf("%s configures cache replication, which needs --blob-existence-cache-peer or --blob-existence-cache-peer-service", orphan.flag)
			}
		}
		return nil, nil
	}
	if len(f.cachePeers) > 0 && f.cachePeerService != "" {
		return nil, errors.New("--blob-existence-cache-peer and --blob-existence-cache-peer-service name the peers two different ways; use one of them")
	}
	if f.blobCacheTTL <= 0 || f.blobCacheMaxMemory <= 0 {
		return nil, errors.New("cache replication needs the blob existence cache: --blob-existence-cache-ttl and --blob-existence-cache-max-memory must both be above zero")
	}
	if f.unixSocket != "" && !peers.enabled() {
		return nil, errors.New("cache replication needs a TCP listener that peers can reach: give the gateway --address/--port instead of --unix-socket, or serve replication on a peer listener of its own with --peer-port")
	}

	// The peer listener's material is what talks to peers when there is one: on a
	// gateway whose clients are anonymous over plaintext, the client listener's
	// (absent) TLS says nothing about the hop between instances.
	replicationTLS := serverTLS
	if peers.enabled() {
		replicationTLS = peers.tls
	}
	peerSource, err := f.cachePeerSource(replicationTLS, peers)
	if err != nil {
		return nil, err
	}
	var credential func(context.Context) (string, error)
	if f.cachePeerTokenFile != "" {
		credential = newTokenReader(f.cachePeerTokenFile).token
	}
	if replicationTLS == nil && credential == nil {
		log.Printf("WARNING: this gateway presents no credential to its cache replication peers; they must accept anonymous clients for replication to work")
	}
	if len(f.allowedCachePeerIDs) == 0 {
		log.Printf("WARNING: no --allowed-cache-peer-id is set, so every client this gateway authenticates may insert blob existence facts into its cache; a false one makes push clients skip an upload they still owe")
	}
	return gateway.NewCacheReplication(gateway.ReplicationConfig{
		Peers:          peerSource,
		Client:         cachePeerClient(replicationTLS, f.cachePeerServerName),
		Credential:     credential,
		AllowedPeerIDs: f.allowedCachePeerIDs,
		BatchSize:      f.cacheBatchSize,
		WarmupTimeout:  f.cacheWarmupTimeout,
		WarmupEntries:  f.cacheWarmupEntries,
	})
}

// cachePeerSource resolves the configured peers: either the static list, whose
// URLs are validated here, or a Kubernetes Service whose endpoints are watched.
func (f *serveFlags) cachePeerSource(replicationTLS *gateway.PeerTLS, peers *peerListener) (gateway.PeerSource, error) {
	// Whether the *replication* hop is encrypted, which on a gateway with a peer
	// listener is that listener's business and not the client listener's.
	replicationTLSServed := replicationTLS != nil && replicationTLS.HasCertificate()
	if f.cachePeerService == "" {
		for _, peer := range f.cachePeers {
			if err := f.validateCachePeer(peer); err != nil {
				return nil, err
			}
		}
		return gateway.StaticPeers(f.cachePeers), nil
	}
	// A discovered peer is a pod of this same Deployment, so it is reached the way
	// this instance is: the same scheme, and the same container port — the peer
	// listener's when there is one, since that is where replication is served.
	port, portFlag := f.port, "--port"
	if peers.enabled() {
		port, portFlag = peers.port, "--peer-port"
	}
	if port == 0 {
		return nil, fmt.Errorf("--blob-existence-cache-peer-service needs an explicit %s, since a discovered peer is reached on the port this gateway itself serves replication on", portFlag)
	}
	scheme := "https"
	if !replicationTLSServed {
		if !f.allowPlaintextCachePeer {
			return nil, errors.New("this gateway serves replication over plaintext, so replication to its peers would be plaintext too: configure --tls-cert-file (or --peer-tls-cert-file for a peer listener), or pass --dangerously-allow-plaintext-cache-peer when a service mesh secures the hop")
		}
		scheme = "http"
	}
	if replicationTLSServed && f.cachePeerServerName == "" {
		log.Printf("warning: --blob-existence-cache-peer-service dials pod IPs, which a certificate issued for the Service name does not cover; set --blob-existence-cache-peer-server-name if replication fails to verify its peers")
	}
	return gateway.NewKubernetesPeers(gateway.KubernetesPeerOptions{
		Service: f.cachePeerService,
		Scheme:  scheme,
		Port:    port,
	})
}

// validateCachePeer checks one --blob-existence-cache-peer value.
func (f *serveFlags) validateCachePeer(peer string) error {
	peerURL, err := url.Parse(peer)
	if err != nil {
		return fmt.Errorf("--blob-existence-cache-peer %q is not a URL: %w", peer, err)
	}
	switch peerURL.Scheme {
	case "https":
	case "http":
		if !f.allowPlaintextCachePeer {
			return fmt.Errorf("--blob-existence-cache-peer %q is plaintext; use https:// or pass --dangerously-allow-plaintext-cache-peer", peer)
		}
	default:
		return fmt.Errorf("--blob-existence-cache-peer %q must use https:// or http://", peer)
	}
	if peerURL.Host == "" {
		return fmt.Errorf("--blob-existence-cache-peer %q is missing a host", peer)
	}
	if peerURL.RawQuery != "" || peerURL.User != nil || strings.Trim(peerURL.Path, "/") != "" {
		return fmt.Errorf("--blob-existence-cache-peer %q must be a scheme and host only", peer)
	}
	return nil
}

// cachePeerClient builds the client that talks to peers.
//
// It is a plain HTTP/1.1-or-HTTP/2 client with pooled connections: replication is a
// handful of small requests per second, so nothing here needs the flow control
// tuning the forwarding hop does. No client timeout is set — every request is
// bounded by its own context, and the warm-up donation is deliberately allowed to
// take longer than a batch.
func cachePeerClient(serverTLS *gateway.PeerTLS, serverName string) *http.Client {
	transport := &http.Transport{
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	if serverTLS != nil {
		// The serving keypair is presented as a client certificate, and the client CA
		// bundle verifies the peers: a symmetric deployment trusts its own CA both
		// ways. A nil pool means the system roots.
		transport.TLSClientConfig = serverTLS.ClientConfig(serverName, false)
	}
	return &http.Client{Transport: transport}
}

// serveProtocols is the protocol set of a serving listener.
//
// HTTP/1.1 is what direct img clients speak. HTTP/2 is added for TCP listeners so
// a forwarding gateway can multiplex its requests over one connection; it is only
// reachable through a TLS handshake (ALPN), so a plaintext single-hop deployment
// is unaffected by enabling it. Prior-knowledge h2c is opt-in because sniffing its
// preface happens before net/http sets any read deadline, which would turn an
// unauthenticated stalled connection into an indefinite resource hold.
//
// A UNIX socket stays HTTP/1.1 only: the registry protocol is request/response,
// and the img tool's unix transport does not attempt h2 anyway.
func serveProtocols(unixSocket, allowH2C bool) *http.Protocols {
	protocols := &http.Protocols{}
	protocols.SetHTTP1(true)
	if unixSocket {
		return protocols
	}
	protocols.SetHTTP2(true)
	protocols.SetUnencryptedHTTP2(allowH2C)
	return protocols
}

// serveHTTP2Config tunes the HTTP/2 server for blob traffic.
// The receive windows are the part that matters: Go defaults to a 1 MiB flow
// control window for a whole connection, shared by every stream on it, so without
// raising it a forwarding gateway multiplexing several blob uploads starves itself
// and one slow stream holds up the rest. The API caps the connection window just
// below 4 MiB, which bounds aggregate upload throughput per connection at roughly
// window/RTT — ample in-cluster, and the reason this hop is not meant to cross
// regions.
//
// MaxConcurrentStreams bounds the worst case: flow control windows are credits
// rather than preallocations, but a stalled upstream registry realises them, so
// budget streams x per-stream window x connections when sizing the pod.
//
// The ping timeouts are not optional in Kubernetes: without them a peer that
// vanishes (a rolling update, a lost node) leaves a stream hanging until the
// kernel gives up, tens of minutes later.
func serveHTTP2Config() *http.HTTP2Config {
	return &http.HTTP2Config{
		MaxConcurrentStreams:          64,
		MaxReceiveBufferPerConnection: 4<<20 - 1,
		MaxReceiveBufferPerStream:     2 << 20,
		SendPingTimeout:               30 * time.Second,
		PingTimeout:                   15 * time.Second,
	}
}

// nextProtosFor returns the ALPN protocol list matching a protocol set, in the
// order [http.Server.ServeTLS] would settle on.
//
// It has to be computed here rather than left to net/http: ServeTLS fixes up the
// NextProtos of the config it clones, but the TLS config a gateway hands out from
// GetConfigForClient (which is how a rotated client CA bundle takes effect) is
// used as-is, so an ALPN list missing "h2" there would silently give up HTTP/2.
func nextProtosFor(protocols *http.Protocols) []string {
	if protocols == nil {
		return []string{"h2", "http/1.1"}
	}
	var protos []string
	if protocols.HTTP2() {
		protos = append(protos, "h2")
	}
	if protocols.HTTP1() {
		protos = append(protos, "http/1.1")
	}
	return protos
}
