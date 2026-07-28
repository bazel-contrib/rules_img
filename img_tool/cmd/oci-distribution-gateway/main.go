// Command oci-distribution-gateway runs a container registry gateway that only
// forwards requests to real upstream registries.
//
// Clients connect anonymously and must set the X-rules_img-Original-Host header
// to select the upstream registry. The gateway authenticates to that upstream
// using the ambient registry credentials (docker config, cloud keychains, or an
// optional Bazel credential helper) and authorizes every request against a
// policy file: an ordered list of allow/deny rules matched on the upstream
// registry host, repository path, and operation (blob/manifest read/write). The
// policy file can be reloaded at runtime by sending the process a SIGHUP.
//
// Traffic, blob transfers, and errors are reported as OpenTelemetry metrics,
// either pushed to a collector over OTLP or scraped from a Prometheus endpoint;
// see --metrics-exporter and //pkg/serve/telemetry.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	reg "github.com/bazel-contrib/rules_img/img_tool/pkg/auth/registry"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/serve/gateway"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/serve/telemetry"
)

// serviceName is the default OpenTelemetry service.name of the gateway. It is
// overridden by OTEL_SERVICE_NAME.
const serviceName = "oci-distribution-gateway"

// repeatedFlag collects every occurrence of a flag that may be given more than
// once, which the flag package only supports through a [flag.Value].
type repeatedFlag []string

func (f *repeatedFlag) String() string { return strings.Join(*f, ",") }

func (f *repeatedFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

// newFlagSet builds the flag set with a usage banner and examples.
func newFlagSet() *flag.FlagSet {
	flagSet := flag.NewFlagSet("oci-distribution-gateway", flag.ContinueOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Run a forwarding OCI distribution gateway.\n\n")
		fmt.Fprintf(flagSet.Output(), "The gateway forwards requests to the upstream registry named in the\n")
		fmt.Fprintf(flagSet.Output(), "X-rules_img-Original-Host request header, subject to the policy file.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: oci-distribution-gateway --policy-file <path> [OPTIONS]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"oci-distribution-gateway --port 8080 --policy-file /etc/img/gateway-policy.json",
			"oci-distribution-gateway --unix-socket /run/gw.sock --policy-file /etc/img/gateway-policy.yaml",
			"oci-distribution-gateway --validate-policy --policy-file /etc/img/gateway-policy.json",
			"oci-distribution-gateway --unix-socket /run/gw.sock --dangerously-allow-all",
			"oci-distribution-gateway --port 8080 --policy-file /etc/img/policy.json --metrics-exporter prometheus",
		}
		fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
	}
	return flagSet
}

func Run(ctx context.Context, args []string) {
	var (
		address              string
		port                 int
		unixSocket           string
		defaultRegistry      string
		credentialHelperPath string
		policyFile           string
		validatePolicy       bool
		dangerouslyAllowAll  bool
		metricsExporter      string
		metricsOTLPProtocol  string
		metricsOTLPEndpoints repeatedFlag
		metricsAddress       string
	)

	flagSet := newFlagSet()
	flagSet.StringVar(&address, "address", "localhost", "Address to bind the gateway to (ignored when --unix-socket is set)")
	flagSet.IntVar(&port, "port", 0, "Port to bind the gateway to (0 picks a free port; ignored when --unix-socket is set)")
	flagSet.StringVar(&unixSocket, "unix-socket", "", "Path to a UNIX domain socket to listen on instead of TCP")
	flagSet.StringVar(&defaultRegistry, "default-registry", "", "Upstream registry to forward to when a request omits the X-rules_img-Original-Host header (must also be allowed by the policy)")
	flagSet.StringVar(&credentialHelperPath, "credential-helper", "", "Path to a Bazel credential helper binary used to authenticate to upstream registries (optional)")
	flagSet.StringVar(&policyFile, "policy-file", "", "Path to a JSON (or YAML) policy file with per-repository allow/deny rules. Required unless --dangerously-allow-all is set. Reloadable at runtime with SIGHUP.")
	flagSet.BoolVar(&validatePolicy, "validate-policy", false, "Load and validate --policy-file, then exit (0 if valid, non-zero otherwise). Does not start the gateway.")
	flagSet.BoolVar(&dangerouslyAllowAll, "dangerously-allow-all", false, "Allow every request to every upstream, ignoring the policy file. DANGEROUS: only for trusted, isolated environments.")
	flagSet.StringVar(&metricsExporter, "metrics-exporter", "", "OpenTelemetry metric exporters to enable, comma-separated: otlp, prometheus, console, or none. Defaults to $OTEL_METRICS_EXPORTER, or to otlp when an OTLP endpoint is configured.")
	flagSet.StringVar(&metricsOTLPProtocol, "metrics-otlp-protocol", "", "Protocol for the otlp exporter: grpc (collector port 4317) or http/protobuf (port 4318). Defaults to $OTEL_EXPORTER_OTLP_METRICS_PROTOCOL, $OTEL_EXPORTER_OTLP_PROTOCOL, then http/protobuf.")
	flagSet.Var(&metricsOTLPEndpoints, "metrics-otlp-endpoint", "OTLP metrics endpoint URL to push to, e.g. http://collector:4318 (https:// to use TLS). Repeat the flag to push the same metrics to several collectors, which is only correct when at most one of them forwards them (a leader-elected set) or the backend deduplicates: if they all forward, every counter is multiplied. Each endpoint also costs another export per interval. Defaults to $IMG_METRICS_OTLP_ENDPOINTS (comma-separated), then to the single $OTEL_EXPORTER_OTLP_[METRICS_]ENDPOINT.")
	flagSet.StringVar(&metricsAddress, "metrics-address", ":9464", "Address the prometheus exporter serves /metrics on. Reachable from outside the pod by default; keep it on a trusted network.")

	if err := flagSet.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// flag already printed the usage.
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, err.Error())
		flagSet.Usage()
		os.Exit(1)
	}

	if validatePolicy {
		if policyFile == "" {
			fmt.Fprintln(os.Stderr, "Error: --validate-policy requires --policy-file")
			os.Exit(1)
		}
		cp, err := gateway.LoadPolicyFile(policyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "policy %s is valid (%s)\n", policyFile, cp.Summary())
		os.Exit(0)
	}

	// Resolve the authorization policy. --dangerously-allow-all overrides (and
	// ignores) any policy file; otherwise a policy file is required.
	var authz *gateway.CompiledPolicy
	switch {
	case dangerouslyAllowAll:
		if policyFile != "" {
			log.Printf("warning: --dangerously-allow-all is set; ignoring --policy-file %s", policyFile)
		}
		log.Printf("WARNING: --dangerously-allow-all is set; the gateway will forward EVERY request to any upstream without policy checks")
		authz = gateway.AllowAll()
	case policyFile == "":
		fmt.Fprintln(os.Stderr, "Error: --policy-file is required (or pass --dangerously-allow-all to disable policy checks)")
		flagSet.Usage()
		os.Exit(1)
	default:
		cp, err := gateway.LoadPolicyFile(policyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		log.Printf("loaded policy from %s (%s)", policyFile, cp.Summary())
		authz = cp
	}

	if credentialHelperPath != "" {
		// reg.Keychain() resolves the OCI-registry credential helper from the
		// environment; wire the flag through it as the registry-scoped helper.
		if err := os.Setenv(reg.EnvCredentialHelperOCIRegistry, credentialHelperPath); err != nil {
			log.Fatalf("Failed to set credential helper: %v", err)
		}
	}

	// Metrics are opt-in: with no exporter configured, Setup returns a provider
	// that discards every measurement.
	metrics, err := telemetry.Setup(ctx, telemetry.Config{
		MetricExporters: metricsExporter,
		OTLPProtocol:    metricsOTLPProtocol,
		OTLPEndpoints:   metricsOTLPEndpoints,
		ServiceName:     serviceName,
	})
	if err != nil {
		log.Fatalf("Failed to set up metrics: %v", err)
	}
	defer func() {
		// Flush the last measurements. The context is fresh: the one that stopped
		// the server may already be cancelled.
		flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := metrics.Shutdown(flushCtx); err != nil {
			log.Printf("metrics shutdown error: %v", err)
		}
	}()
	if metrics.Enabled() {
		log.Printf("metrics enabled (exporters: %s)", strings.Join(metrics.Exporters, ", "))
	}

	handler := gateway.New(
		gateway.WithAuthorizer(authz),
		gateway.WithDefaultRegistry(defaultRegistry),
		gateway.WithKeychain(reg.Keychain()),
		gateway.WithMeterProvider(metrics.MeterProvider),
	)

	listener, cleanup, err := listen(unixSocket, address, port)
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}
	defer cleanup()

	// Force HTTP/1.1: the registry protocol is request/response and some
	// clients (and unix-socket transports) do not negotiate h2.
	protocols := &http.Protocols{}
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(false)
	protocols.SetUnencryptedHTTP2(false)

	server := &http.Server{
		Handler:           handler,
		Protocols:         protocols,
		ReadHeaderTimeout: 30 * time.Second,
		// Generous body timeouts: blob uploads and downloads can be large.
		ReadTimeout:  30 * time.Minute,
		WriteTimeout: 30 * time.Minute,
		IdleTimeout:  5 * time.Minute,
	}

	// The Prometheus exporter is a pull exporter, so it needs an endpoint of its
	// own. It never shares the registry listener: that one is the gateway's
	// protocol surface (and may be a UNIX socket a scraper cannot reach).
	var metricsServer *http.Server
	if metrics.PrometheusHandler != nil {
		if metricsAddress == "" {
			log.Fatalf("Failed to serve metrics: the prometheus exporter requires --metrics-address")
		}
		mux := http.NewServeMux()
		mux.Handle("/metrics", metrics.PrometheusHandler)
		metricsServer = &http.Server{
			Addr:              metricsAddress,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       time.Minute,
		}
		metricsListener, err := net.Listen("tcp", metricsAddress)
		if err != nil {
			log.Fatalf("Failed to listen on --metrics-address %s: %v", metricsAddress, err)
		}
		go func() {
			if err := metricsServer.Serve(metricsListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("metrics endpoint error: %v", err)
			}
		}()
		fmt.Fprintf(os.Stderr, "oci-distribution-gateway serving metrics on %s/metrics\n", metricsListener.Addr())
	}

	// Reload the policy file on SIGHUP. A failed reload keeps the previous
	// policy, so a bad edit never widens access or takes the gateway down.
	// (With --dangerously-allow-all there is no file to reload.)
	if policyFile != "" && !dangerouslyAllowAll {
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		go func() {
			for range hup {
				if cp, err := handler.Reload(policyFile); err != nil {
					log.Printf("policy reload FAILED, keeping previous policy: %v", err)
				} else {
					log.Printf("reloaded policy from %s (%s)", policyFile, cp.Summary())
				}
			}
		}()
	}

	// Shut down gracefully on SIGINT/SIGTERM so in-flight uploads/downloads can
	// finish (or the deadline forces them closed).
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	fmt.Fprintf(os.Stderr, "oci-distribution-gateway listening on %s\n", listener.Addr())

	select {
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to serve: %v", err)
		}
	case sig := <-shutdown:
		log.Printf("received %s, shutting down gracefully...", sig)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown error: %v", err)
		}
		if metricsServer != nil {
			// After the gateway itself: the last requests should still be counted,
			// and a scrape of the final numbers is still useful.
			if err := metricsServer.Shutdown(shutdownCtx); err != nil {
				log.Printf("metrics endpoint shutdown error: %v", err)
			}
		}
	}
}

// listen opens the configured listener. When unixSocket is non-empty it listens
// on that socket (removing any stale socket file first); otherwise it listens on
// TCP. The returned cleanup removes the socket file on shutdown.
func listen(unixSocket, address string, port int) (net.Listener, func(), error) {
	if unixSocket != "" {
		// Remove a stale socket left by a previous run, if any.
		if _, err := os.Stat(unixSocket); err == nil {
			if err := os.Remove(unixSocket); err != nil {
				return nil, func() {}, fmt.Errorf("removing stale socket %q: %w", unixSocket, err)
			}
		}
		l, err := net.Listen("unix", unixSocket)
		if err != nil {
			return nil, func() {}, err
		}
		return l, func() { _ = os.Remove(unixSocket) }, nil
	}
	l, err := net.Listen("tcp", fmt.Sprintf("%s:%d", address, port))
	if err != nil {
		return nil, func() {}, err
	}
	return l, func() {}, nil
}

func main() {
	Run(context.Background(), os.Args)
}
