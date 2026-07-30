// Command oci-distribution-gateway runs a container registry gateway that only
// forwards requests to real upstream registries. It has two modes:
//
//	oci-distribution-gateway serve   [OPTIONS]   # talk to registries (the default)
//	oci-distribution-gateway forward [OPTIONS]   # relay to another gateway
//
// In serving mode, clients set the X-rules_img-Original-Host header to select the
// upstream registry. The gateway authenticates to that upstream using the ambient
// registry credentials (docker config, cloud keychains, or an optional Bazel
// credential helper) and authorizes every request against a policy file: an
// ordered list of allow/deny rules matched on the upstream registry host,
// repository path, and operation (blob/manifest read/write). The policy file can
// be reloaded at runtime by sending the process a SIGHUP. Clients connect
// anonymously unless client authentication is configured (--client-ca-file,
// --client-token-file, --client-serviceaccount-audience), which a gateway
// reachable over the network should always do.
//
// In forwarding mode, the gateway holds no registry credentials and no policy at
// all: it relays the very same protocol to a serving gateway named by --peer over
// one multiplexed HTTP/2 connection, adding its peer credential. That is how a
// build farm keeps the registry credentials in one shared deployment instead of
// in every worker pod. See the "Two-hop deployment" section of this command's
// README.
//
// A bare invocation with no subcommand means serving mode, so existing
// deployments keep working unchanged.
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/serve/telemetry"
)

// Default OpenTelemetry service.name values, one per mode, so a sidecar fleet
// and the shared serving deployment keep their series apart in the backend
// without a metric attribute. Both are overridden by OTEL_SERVICE_NAME.
const (
	serveServiceName   = "oci-distribution-gateway"
	forwardServiceName = "oci-distribution-gateway-forward"
)

const usage = `Usage: oci-distribution-gateway [COMMAND] [OPTIONS]

Commands:
  serve     forward requests to real upstream registries (the default)
  forward   relay requests to another gateway running in serving mode

Run "oci-distribution-gateway <command> --help" for the options of a command.
With no command, the options are those of "serve", so existing deployments keep
working unchanged.
`

// repeatedFlag collects every occurrence of a flag that may be given more than
// once, which the flag package only supports through a [flag.Value].
type repeatedFlag []string

func (f *repeatedFlag) String() string { return strings.Join(*f, ",") }

func (f *repeatedFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

// parseSubcommand splits os.Args into a mode and that mode's arguments. A first
// argument that starts with "-" (or no argument at all) is a flag of the default
// serving mode, which is what keeps every pre-subcommand invocation working.
func parseSubcommand(args []string) (mode string, rest []string, err error) {
	rest = args[1:]
	if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
		return "serve", rest, nil
	}
	switch rest[0] {
	case "serve", "forward":
		return rest[0], rest[1:], nil
	default:
		return "", nil, fmt.Errorf("unknown oci-distribution-gateway command %q (want serve or forward)", rest[0])
	}
}

func Run(ctx context.Context, args []string) {
	mode, rest, err := parseSubcommand(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
	switch mode {
	case "forward":
		forwardProcess(ctx, rest)
	default:
		serveProcess(ctx, rest)
	}
}

// parseFlags parses a mode's flag set, handling --help and errors the way the
// gateway always has: --help prints usage and exits 0, anything else prints the
// error plus usage and exits 1.
func parseFlags(flagSet *flag.FlagSet, args []string) {
	if err := flagSet.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// flag already printed the usage.
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, err.Error())
		flagSet.Usage()
		os.Exit(1)
	}
}

// printUsage writes the shared shape of a mode's usage message: a description, a
// usage line, the flag defaults, and a list of examples.
func printUsage(flagSet *flag.FlagSet, description, usageLine string, examples []string) {
	fmt.Fprintf(flagSet.Output(), "%s\n\n", description)
	fmt.Fprintf(flagSet.Output(), "Usage: %s\n", usageLine)
	flagSet.PrintDefaults()
	fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
	for _, example := range examples {
		fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
	}
}

// metricsFlags holds the OpenTelemetry flags both modes accept, so the two flag
// sets and the Prometheus listener are declared in exactly one place.
type metricsFlags struct {
	exporter      string
	otlpProtocol  string
	otlpEndpoints repeatedFlag
	address       string
}

func (f *metricsFlags) register(flagSet *flag.FlagSet) {
	flagSet.StringVar(&f.exporter, "metrics-exporter", "", "OpenTelemetry metric exporters to enable, comma-separated: otlp, prometheus, console, or none. Defaults to $OTEL_METRICS_EXPORTER, or to otlp when an OTLP endpoint is configured.")
	flagSet.StringVar(&f.otlpProtocol, "metrics-otlp-protocol", "", "Protocol for the otlp exporter: grpc (collector port 4317) or http/protobuf (port 4318). Defaults to $OTEL_EXPORTER_OTLP_METRICS_PROTOCOL, $OTEL_EXPORTER_OTLP_PROTOCOL, then http/protobuf.")
	flagSet.Var(&f.otlpEndpoints, "metrics-otlp-endpoint", "OTLP metrics endpoint URL to push to, e.g. http://collector:4318 (https:// to use TLS). Repeat the flag to push the same metrics to several collectors, which is only correct when at most one of them forwards them (a leader-elected set) or the backend deduplicates: if they all forward, every counter is multiplied. Each endpoint also costs another export per interval. Defaults to $IMG_METRICS_OTLP_ENDPOINTS (comma-separated), then to the single $OTEL_EXPORTER_OTLP_[METRICS_]ENDPOINT.")
	flagSet.StringVar(&f.address, "metrics-address", ":9464", "Address the prometheus exporter serves /metrics on. Reachable from outside the pod by default; keep it on a trusted network.")
}

// setup installs the configured metric exporters and, when the pull-based
// prometheus exporter is enabled, starts serving /metrics on --metrics-address.
// The returned server (which may be nil) must be shut down by the caller; the
// returned flush function must be deferred.
//
// Metrics are opt-in: with no exporter configured, this returns a provider that
// discards every measurement.
func (f *metricsFlags) setup(ctx context.Context, serviceName string) (*telemetry.Provider, *http.Server, func()) {
	metrics, err := telemetry.Setup(ctx, telemetry.Config{
		MetricExporters: f.exporter,
		OTLPProtocol:    f.otlpProtocol,
		OTLPEndpoints:   f.otlpEndpoints,
		ServiceName:     serviceName,
	})
	if err != nil {
		log.Fatalf("Failed to set up metrics: %v", err)
	}
	flush := func() {
		// Flush the last measurements. The context is fresh: the one that stopped
		// the server may already be cancelled.
		flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := metrics.Shutdown(flushCtx); err != nil {
			log.Printf("metrics shutdown error: %v", err)
		}
	}
	if metrics.Enabled() {
		log.Printf("metrics enabled (exporters: %s)", strings.Join(metrics.Exporters, ", "))
	}

	// The Prometheus exporter is a pull exporter, so it needs an endpoint of its
	// own. It never shares the registry listener: that one is the gateway's
	// protocol surface (and may be a UNIX socket a scraper cannot reach).
	if metrics.PrometheusHandler == nil {
		return metrics, nil, flush
	}
	if f.address == "" {
		log.Fatalf("Failed to serve metrics: the prometheus exporter requires --metrics-address")
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.PrometheusHandler)
	metricsServer := &http.Server{
		Addr:              f.address,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       time.Minute,
	}
	metricsListener, err := net.Listen("tcp", f.address)
	if err != nil {
		log.Fatalf("Failed to listen on --metrics-address %s: %v", f.address, err)
	}
	go func() {
		if err := metricsServer.Serve(metricsListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("metrics endpoint error: %v", err)
		}
	}()
	fmt.Fprintf(os.Stderr, "oci-distribution-gateway serving metrics on %s/metrics\n", metricsListener.Addr())
	return metrics, metricsServer, flush
}

// gatewayServer bundles what a gateway process serves so both modes start and
// stop it the same way.
type gatewayServer struct {
	server *http.Server
	// serve starts serving; it is server.Serve or server.ServeTLS depending on
	// whether the listener speaks TLS.
	serve func() error
	// addr is reported in the startup banner.
	addr net.Addr
	// metrics is the Prometheus endpoint, or nil when it is not enabled.
	metrics *http.Server
	// shutdownTimeout bounds how long in-flight transfers get to finish.
	shutdownTimeout time.Duration
	// banner names the mode in the startup line.
	banner string
}

// run serves until the server fails or the process is asked to stop, shutting
// down gracefully so in-flight uploads and downloads can finish (or the deadline
// forces them closed).
func (g *gatewayServer) run() {
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)
	serveErr := make(chan error, 1)
	go func() { serveErr <- g.serve() }()

	fmt.Fprintf(os.Stderr, "%s on %s\n", g.banner, g.addr)

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Failed to serve: %v", err)
		}
	case sig := <-shutdown:
		log.Printf("received %s, shutting down gracefully...", sig)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), g.shutdownTimeout)
		defer cancel()
		if err := g.server.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown error: %v", err)
		}
		if g.metrics != nil {
			// After the gateway itself: the last requests should still be counted,
			// and a scrape of the final numbers is still useful.
			if err := g.metrics.Shutdown(shutdownCtx); err != nil {
				log.Printf("metrics endpoint shutdown error: %v", err)
			}
		}
	}
}

// listen opens the configured listener. When unixSocket is non-empty it listens
// on that socket (removing any stale socket file first); otherwise it listens on
// TCP. socketMode, when non-zero, is applied to the socket file: the default
// leaves the mode net.Listen created it with, which is what a Buildbarn runner
// under a different uid needs. The returned cleanup removes the socket file on
// shutdown.
func listen(unixSocket, address string, port int, socketMode uint32) (net.Listener, func(), error) {
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
		cleanup := func() { _ = os.Remove(unixSocket) }
		if socketMode != 0 {
			if err := os.Chmod(unixSocket, os.FileMode(socketMode)); err != nil {
				l.Close()
				cleanup()
				return nil, func() {}, fmt.Errorf("setting mode of socket %q: %w", unixSocket, err)
			}
		}
		return l, cleanup, nil
	}
	l, err := net.Listen("tcp", fmt.Sprintf("%s:%d", address, port))
	if err != nil {
		return nil, func() {}, err
	}
	return l, func() {}, nil
}

// parseSocketMode parses an octal --unix-socket-mode value. An empty value (or
// "0") means "leave the mode net.Listen created the socket with".
func parseSocketMode(value string) (uint32, error) {
	if value == "" {
		return 0, nil
	}
	mode, err := strconv.ParseUint(value, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("--unix-socket-mode %q is not an octal file mode: %w", value, err)
	}
	if mode > 0o777 {
		return 0, fmt.Errorf("--unix-socket-mode %q is out of range (want 0 to 0777)", value)
	}
	return uint32(mode), nil
}

// reachableFromNetwork reports whether a TCP listener on address accepts
// connections from outside the machine, and is therefore a listener that needs
// client authentication. A UNIX socket (empty address, non-empty unixSocket) and
// a loopback address do not.
func reachableFromNetwork(unixSocket, address string) bool {
	if unixSocket != "" {
		return false
	}
	// An empty or wildcard address binds every interface.
	switch address {
	case "", "0.0.0.0", "[::]", "::":
		return true
	}
	if ip := net.ParseIP(strings.Trim(address, "[]")); ip != nil {
		return !ip.IsLoopback()
	}
	// A hostname: only the unambiguous loopback spellings are treated as local,
	// so anything else fails closed and requires authentication.
	return address != "localhost" && !strings.HasSuffix(address, ".localhost")
}

func main() {
	Run(context.Background(), os.Args)
}
