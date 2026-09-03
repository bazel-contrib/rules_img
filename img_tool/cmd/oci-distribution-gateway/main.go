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
// A serving gateway also memoizes the blob existence checks a push begins with, so
// that a fleet asking whether the same layer is already upstream pays for one round
// trip rather than thousands. Reads and pushes it forwards keep that memo current,
// and a blob delete takes an entry back. See --blob-existence-cache-ttl and
// blob-existence-cache.md.
//
// Several serving instances can replicate that memo to each other, so that a
// deployment of N replicas pays for one upstream probe per blob rather than N. The
// peers are named with --blob-existence-cache-peer, or discovered from the
// EndpointSlices of a Service with --blob-existence-cache-peer-service; a starting
// instance seeds its cache from a peer before reporting itself healthy. Replication
// is best effort throughout: no client request ever waits for a peer.
//
// A bare invocation with no subcommand means serving mode, so existing
// deployments keep working unchanged.
//
// The decision the gateway took on every request, along with its reloads and
// warnings, is logged to stderr, or to the file named by --log-file for a
// gateway sharing its output with something else.
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
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
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

// byteSizeFlag is a flag value for an amount of memory: a plain byte count, or a
// number with a unit suffix. Binary units (KiB, MiB, GiB, and the Ki/Mi/Gi
// spellings a Kubernetes resource quantity uses) and decimal ones (KB, MB, GB)
// are both accepted.
//
// A bare K, M or G is deliberately rejected: Kubernetes reads them as powers of
// ten and Docker as powers of two, so a flag in a manifest that accepted both
// spellings would mean different things to whoever read it next.
type byteSizeFlag int64

var byteSizeUnits = []struct {
	suffix string
	scale  int64
}{
	// Longest suffix first: "MiB" has to be tried before "B".
	{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30},
	{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30},
	{"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9},
	{"B", 1},
}

func (f byteSizeFlag) String() string {
	if f == 0 {
		return "0"
	}
	for _, unit := range []struct {
		suffix string
		scale  int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if int64(f)%unit.scale == 0 {
			return strconv.FormatInt(int64(f)/unit.scale, 10) + unit.suffix
		}
	}
	return strconv.FormatInt(int64(f), 10)
}

func (f *byteSizeFlag) Set(value string) error {
	digits := strings.TrimSpace(value)
	scale := int64(1)
	for _, unit := range byteSizeUnits {
		if trimmed, ok := strings.CutSuffix(digits, unit.suffix); ok {
			digits, scale = strings.TrimSpace(trimmed), unit.scale
			break
		}
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n < 0 {
		return fmt.Errorf("%q is not a size: want a byte count, or a number with a KiB/MiB/GiB (or KB/MB/GB) suffix", value)
	}
	if n > math.MaxInt64/scale {
		return fmt.Errorf("%q is out of range", value)
	}
	*f = byteSizeFlag(n * scale)
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

// logFlags holds the logging flag both modes accept.
type logFlags struct {
	file string
}

func (f *logFlags) register(flagSet *flag.FlagSet) {
	flagSet.StringVar(&f.file, "log-file", "", "Append the gateway's log to this file instead of writing it to stderr. Everything the running gateway reports goes there: the decision it took on every request, reload results, warnings, and the startup banner. Created if it does not exist, and reopened on SIGHUP, which is what a log rotator needs after moving it aside.")
}

// setup points the gateway's logging at --log-file, or leaves it on stderr when
// the flag is unset. It must be called before anything is logged, and the
// returned sink must be closed by the caller (and reopened on SIGHUP).
//
// One redirection moves every line the process writes, because every part of it
// logs through the standard logger: the handler's decision log, the forwarder,
// client authentication, TLS and token reloads, peer discovery, and cache
// replication. What stays on stderr is the errors that reject the command line
// itself, which are reported before this is called — a misconfigured gateway
// still says so where it was started, rather than in a file it may not have been
// able to create.
func (f *logFlags) setup() *logSink {
	if f.file == "" {
		return nil
	}
	sink := &logSink{path: f.file}
	if err := sink.open(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	return sink
}

// logSink is the open --log-file. A nil sink is a gateway logging to stderr, so
// every method has to tolerate one.
type logSink struct {
	path string

	mu   sync.Mutex
	file *os.File
}

// open opens the log file and makes it the destination of the standard logger,
// closing whatever this sink had open before.
func (s *logSink) open() error {
	file, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening --log-file %q: %w", s.path, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.file
	s.file = file
	log.SetOutput(file)
	if previous != nil {
		previous.Close()
	}
	return nil
}

// reopen re-opens the log file, which is how a rotated log keeps being written:
// once the rotator has renamed the file, the gateway holds a descriptor to
// something no one can find any more until the path is opened again.
//
// A failure keeps the descriptor already open, so a rotation that goes wrong
// costs no log lines — the same rule the policy and TLS reloads follow.
func (s *logSink) reopen() {
	if s == nil {
		return
	}
	if err := s.open(); err != nil {
		log.Printf("log file reopen FAILED, keeping the open one: %v", err)
	}
}

// close returns logging to stderr and closes the file, so a line written while
// the process is on its way out does not go to a closed descriptor.
func (s *logSink) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return
	}
	log.SetOutput(os.Stderr)
	s.file.Close()
	s.file = nil
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
	log.Printf("serving metrics on %s/metrics", metricsListener.Addr())
	return metrics, metricsServer, flush
}

// gatewayListener is one HTTP server of a gateway process: the server itself, the
// call that starts it (Serve or ServeTLS, depending on whether it speaks TLS), and
// the address to report.
type gatewayListener struct {
	server *http.Server
	serve  func() error
	addr   net.Addr
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
	// peer is the second listener carrying cache replication between instances, or
	// nil when the gateway serves it on the main listener (or not at all).
	peer *gatewayListener
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
	serveErr := make(chan error, 2)
	go func() { serveErr <- g.serve() }()

	log.Printf("%s on %s", g.banner, g.addr)
	if g.peer != nil {
		go func() { serveErr <- g.peer.serve() }()
		log.Printf("%s for cache replication peers on %s", g.banner, g.peer.addr)
	}

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
		if g.peer != nil {
			// After the traffic it exists to support: a fact learned while the last
			// requests drain is still worth telling the fleet.
			if err := g.peer.server.Shutdown(shutdownCtx); err != nil {
				log.Printf("peer listener shutdown error: %v", err)
			}
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
