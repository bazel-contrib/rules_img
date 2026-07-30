package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net/http"
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

	metrics metricsFlags
}

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

	flagSet.StringVar(&f.tlsCertFile, "tls-cert-file", "", "PEM certificate (leaf plus any intermediates) to serve TLS with. Enables HTTP/2 via ALPN. Re-read when the file changes and on SIGHUP.")
	flagSet.StringVar(&f.tlsKeyFile, "tls-key-file", "", "PEM private key matching --tls-cert-file. Both or neither.")
	flagSet.StringVar(&f.clientCAFile, "client-ca-file", "", "PEM bundle of CAs whose client certificates are accepted. Setting it enables mTLS and requires --tls-cert-file. Re-read when the file changes and on SIGHUP.")
	flagSet.Var(&f.allowedClientIDs, "allowed-client-id", "Client identity permitted to use this gateway: a SPIFFE ID (spiffe://trust-domain/path) or a DNS name (a single leading \"*.\" wildcard is allowed). Repeatable. Without it, any certificate signed by --client-ca-file is accepted, which with a shared cluster CA is effectively cluster-wide access.")
	flagSet.Var(&f.clientTokenFiles, "client-token-file", "File of bearer tokens permitted to use this gateway, one per line ('#' comments and blank lines ignored) so several are valid at once during a rotation. Repeatable. Re-read when the file changes and on SIGHUP.")
	flagSet.StringVar(&f.serviceAccountAudience, "client-serviceaccount-audience", "", "Accept Kubernetes projected ServiceAccount tokens issued for this audience, validated with the TokenReview API. Must be an audience dedicated to this gateway: the token every pod gets by default is issued for the API server's audience, and accepting it would authenticate the whole cluster. Needs RBAC to create tokenreviews (the system:auth-delegator ClusterRole).")
	flagSet.Var(&f.allowedServiceAccounts, "allowed-serviceaccount", "ServiceAccount permitted to use this gateway, as system:serviceaccount:<namespace>:<name>. Repeatable. Without it, any ServiceAccount holding a token for the audience is accepted.")
	flagSet.BoolVar(&f.allowPlaintextH2C, "dangerously-allow-plaintext-h2c", false, "Accept prior-knowledge HTTP/2 (h2c) on the plaintext listener. DANGEROUS: sniffing the h2c preface happens before any read deadline is set, so a client that connects and stalls holds a connection indefinitely. Only for a listener reachable solely through a service-mesh sidecar.")
	flagSet.BoolVar(&f.allowUnauthenticatedText, "dangerously-allow-unauthenticated-clients", false, "Serve a network-reachable address without client authentication. DANGEROUS: this listener holds the upstream registry credentials, and a Kubernetes ClusterIP Service is reachable from every namespace.")

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
	}
	if peerAuth != nil {
		handlerOpts = append(handlerOpts, gateway.WithPeerAuth(peerAuth))
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
		}
	}()

	(&gatewayServer{
		server:          server,
		serve:           serve,
		addr:            listener.Addr(),
		metrics:         metricsServer,
		shutdownTimeout: flags.shutdownTimeout,
		banner:          "oci-distribution-gateway listening",
	}).run()
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
