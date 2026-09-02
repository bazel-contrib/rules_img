// Package telemetry wires OpenTelemetry metric exporters for the rules_img
// server binaries.
//
// It is deliberately the only place that touches the OpenTelemetry SDK: the
// servers themselves only use the metric API, so a binary that does not call
// [Setup] carries no exporter and records nothing.
//
// Configuration follows the standard OpenTelemetry environment variables so the
// usual Kubernetes tooling (the OpenTelemetry Operator's auto-instrumentation
// injection, a collector DaemonSet, a Prometheus ServiceMonitor) works without
// any rules_img specific setup:
//
//	OTEL_METRICS_EXPORTER                 otlp, prometheus, console, none
//	                                      (comma-separated; default: otlp when an
//	                                      OTLP endpoint is configured, else none)
//	OTEL_EXPORTER_OTLP_PROTOCOL           grpc or http/protobuf (default)
//	OTEL_EXPORTER_OTLP_METRICS_PROTOCOL   same, metrics only
//	OTEL_EXPORTER_OTLP_ENDPOINT           collector endpoint, e.g.
//	                                      http://otel-collector:4318
//	OTEL_EXPORTER_OTLP_METRICS_ENDPOINT   same, metrics only
//	OTEL_METRIC_EXPORT_INTERVAL           push interval in ms (default 60000)
//	OTEL_SERVICE_NAME                     overrides the service.name default
//	OTEL_RESOURCE_ATTRIBUTES              extra resource attributes
//
// The remaining OTLP variables (headers, TLS material, compression, timeouts)
// are read by the exporters themselves. Pushing to more than one collector is
// the one thing the specification has no variable for; see [Config.OTLPEndpoints].
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/google/uuid"
	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// Exporter names accepted by [Config.MetricExporters] and OTEL_METRICS_EXPORTER.
const (
	// ExporterNone disables metrics entirely.
	ExporterNone = "none"
	// ExporterOTLP pushes to an OpenTelemetry collector over OTLP.
	ExporterOTLP = "otlp"
	// ExporterPrometheus exposes a /metrics endpoint for Prometheus to scrape.
	ExporterPrometheus = "prometheus"
	// ExporterConsole prints metrics for debugging.
	ExporterConsole = "console"
)

// OTLP protocol names accepted by [Config.OTLPProtocol] and
// OTEL_EXPORTER_OTLP_PROTOCOL.
const (
	ProtocolGRPC = "grpc"
	ProtocolHTTP = "http/protobuf"
)

// Environment variables this package reads itself. The rest are read by the
// exporters and the SDK.
const (
	envExporters    = "OTEL_METRICS_EXPORTER"
	envProtocol     = "OTEL_EXPORTER_OTLP_PROTOCOL"
	envProtocolMetr = "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"
	envEndpoint     = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envEndpointMetr = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"

	// envOTLPEndpoints is a comma-separated list of OTLP metrics endpoints. The
	// name is deliberately *not* an OTEL_ one: the specification defines its
	// endpoint variables as single-valued, so overloading them with a list would
	// give a standard variable non-standard semantics. It exists because the
	// mechanisms that configure telemetry in a cluster (the OpenTelemetry
	// Operator, Helm values, ConfigMaps) inject environment variables, not
	// command-line arguments. See [Config.OTLPEndpoints].
	envOTLPEndpoints = "IMG_METRICS_OTLP_ENDPOINTS"
)

// Config configures [Setup]. Empty fields fall back to the standard
// OpenTelemetry environment variables, so a deployment can be configured either
// way; explicit values win.
type Config struct {
	// MetricExporters is a comma-separated exporter list, overriding
	// OTEL_METRICS_EXPORTER.
	MetricExporters string
	// OTLPProtocol overrides OTEL_EXPORTER_OTLP_[METRICS_]PROTOCOL.
	OTLPProtocol string
	// OTLPEndpoints are the OTLP metrics endpoints to push to, as absolute URLs
	// whose scheme decides transport security ("http://collector:4318" plaintext,
	// "https://collector:4318" TLS) and whose path, if any, is used as given.
	// Empty falls back to IMG_METRICS_OTLP_ENDPOINTS, and then to the single
	// endpoint the standard OTEL_EXPORTER_OTLP_[METRICS_]ENDPOINT variables name.
	// Naming any endpoint enables the OTLP exporter unless MetricExporters (or
	// OTEL_METRICS_EXPORTER) says otherwise.
	//
	// Each endpoint gets its own exporter and its own periodic reader, and the
	// same metrics go to all of them. That is what a set of collector replicas
	// where only one is active at a time (a leader-elected pair, the follower
	// discarding what it receives) needs, because a client that resolves the
	// service to a single address has a 1-in-N chance of talking to the follower
	// and silently dropping everything it exports. It is *wrong* for a
	// load-balanced collector fleet where every replica forwards: the backend
	// then sees each measurement N times, and every counter is multiplied by N
	// unless it deduplicates. Each endpoint also costs another serialization and
	// export of the full metric set per interval.
	OTLPEndpoints []string
	// ServiceName is the service.name reported unless OTEL_SERVICE_NAME (or
	// OTEL_RESOURCE_ATTRIBUTES) sets one.
	ServiceName string
	// Logger receives warnings and asynchronous exporter errors (for example a
	// collector that cannot be reached). Defaults to the standard logger.
	Logger *log.Logger
}

// Provider is the result of [Setup]: a meter provider to hand to the
// instrumented code, plus the pieces the binary has to serve or shut down.
type Provider struct {
	// MeterProvider is never nil; with metrics disabled it discards everything.
	MeterProvider metric.MeterProvider
	// PrometheusHandler serves the /metrics endpoint. It is nil unless the
	// prometheus exporter is enabled, in which case the binary must serve it.
	PrometheusHandler http.Handler
	// Exporters names the enabled exporters, for logging.
	Exporters []string

	shutdown func(context.Context) error
}

// Enabled reports whether any exporter is active.
func (p *Provider) Enabled() bool { return len(p.Exporters) > 0 }

// Shutdown flushes and stops the exporters. It is safe to call on a disabled
// provider.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.shutdown == nil {
		return nil
	}
	return p.shutdown(ctx)
}

// Setup builds the meter provider described by cfg and installs it as the global
// OpenTelemetry meter provider. When no exporter is configured it returns a
// disabled provider (and no error), so callers can always use the result.
//
// The caller owns the returned provider's lifetime: call
// [Provider.Shutdown] before exiting so the final measurements are flushed.
func Setup(ctx context.Context, cfg Config) (*Provider, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}

	// An unusable endpoint is a startup error rather than a warning, even when
	// metrics turn out to be disabled: a typo in a metrics endpoint that only
	// silently stops the export is the failure this whole option exists to
	// prevent.
	endpoints, err := resolveOTLPEndpoints(cfg.OTLPEndpoints)
	if err != nil {
		return nil, err
	}

	exporters, err := resolveExporters(cfg.MetricExporters, endpoints)
	if err != nil {
		return nil, err
	}
	if len(exporters) == 0 {
		return &Provider{MeterProvider: noop.NewMeterProvider()}, nil
	}

	res, err := newResource(ctx, cfg.ServiceName)
	if err != nil {
		// A detector that could not fill in every attribute (or a schema URL
		// conflict) still leaves a usable resource; losing an attribute is not a
		// reason to refuse to serve.
		logger.Printf("warning: building OpenTelemetry resource: %v", err)
	}

	opts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	var promHandler http.Handler
	for _, name := range exporters {
		switch name {
		case ExporterOTLP:
			otlpExporters, err := newOTLPExporters(ctx, cfg.OTLPProtocol, endpoints)
			if err != nil {
				return nil, err
			}
			// One periodic reader per endpoint. Each reader has its own goroutine,
			// ticker and export timeout, so a collector that is unreachable holds up
			// neither the others nor the gateway; its failures reach the logger
			// through the error handler installed below. MeterProvider.Shutdown
			// flushes every registered reader.
			for _, exporter := range otlpExporters {
				opts = append(opts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)))
			}
		case ExporterPrometheus:
			// A dedicated registry keeps the endpoint to the metrics this process
			// records, independent of any global default registry.
			registry := promclient.NewRegistry()
			reader, err := prometheus.New(prometheus.WithRegisterer(registry))
			if err != nil {
				return nil, fmt.Errorf("creating prometheus exporter: %w", err)
			}
			opts = append(opts, sdkmetric.WithReader(reader))
			promHandler = promhttp.HandlerFor(registry, promhttp.HandlerOpts{
				ErrorHandling: promhttp.HTTPErrorOnError,
			})
		case ExporterConsole:
			// stderr, not stdout: the servers keep stdout free for their own output.
			exporter, err := stdoutmetric.New(stdoutmetric.WithWriter(os.Stderr))
			if err != nil {
				return nil, fmt.Errorf("creating console exporter: %w", err)
			}
			opts = append(opts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)))
		}
	}

	// Export failures are asynchronous (a collector may be down); route them to
	// the binary's log instead of OpenTelemetry's own stderr logger.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Printf("opentelemetry: %v", err)
	}))

	mp := sdkmetric.NewMeterProvider(opts...)
	otel.SetMeterProvider(mp)
	return &Provider{
		MeterProvider:     mp,
		PrometheusHandler: promHandler,
		Exporters:         exporters,
		shutdown:          mp.Shutdown,
	}, nil
}

// resolveExporters determines which exporters to enable from the flag value,
// falling back to OTEL_METRICS_EXPORTER and finally to a default that turns
// metrics on exactly when an OTLP endpoint is configured. It returns an empty
// slice when metrics are disabled.
func resolveExporters(flagValue string, otlpEndpoints []string) ([]string, error) {
	value := strings.TrimSpace(flagValue)
	source := "--metrics-exporter"
	if value == "" {
		value = strings.TrimSpace(os.Getenv(envExporters))
		source = envExporters
	}
	if value == "" {
		// Nothing asked for metrics explicitly. Enable OTLP when something points
		// at a collector — resolved endpoints, or the environment the
		// OpenTelemetry Operator and most Helm charts set — and stay silent
		// otherwise. An explicit exporter list (including "none") still wins,
		// because it was checked first.
		if len(otlpEndpoints) > 0 || os.Getenv(envEndpoint) != "" || os.Getenv(envEndpointMetr) != "" {
			return []string{ExporterOTLP}, nil
		}
		return nil, nil
	}

	var (
		exporters []string
		seen      = make(map[string]bool)
		none      bool
	)
	for _, field := range strings.Split(value, ",") {
		name := strings.ToLower(strings.TrimSpace(field))
		switch name {
		case "":
			continue
		case ExporterNone:
			none = true
			continue
		case ExporterOTLP, ExporterPrometheus, ExporterConsole:
		case "stdout":
			// Alias: the OpenTelemetry specification calls it "console", but
			// "stdout" is the Go exporter's package name and a common typo.
			name = ExporterConsole
		default:
			return nil, fmt.Errorf("%s: unknown metric exporter %q (want %s, %s, %s, or %s)",
				source, name, ExporterOTLP, ExporterPrometheus, ExporterConsole, ExporterNone)
		}
		if !seen[name] {
			seen[name] = true
			exporters = append(exporters, name)
		}
	}
	if none && len(exporters) > 0 {
		return nil, fmt.Errorf("%s: %q cannot be combined with other exporters", source, ExporterNone)
	}
	return exporters, nil
}

// resolveOTLPEndpoints returns the OTLP metrics endpoints to push to: the flag
// values, or else the comma-separated IMG_METRICS_OTLP_ENDPOINTS. An empty
// result means "one exporter, endpoint from the standard environment
// variables", which is the behavior when nobody asks for several collectors.
//
// Repeated identical endpoints collapse, so a duplicated argument does not
// double the export work, and an endpoint that is not an absolute http(s) URL is
// an error naming it: the exporter option would otherwise log a line and
// silently fall back to the default collector address.
func resolveOTLPEndpoints(flagValues []string) ([]string, error) {
	values, source := flagValues, "--metrics-otlp-endpoint"
	if len(values) == 0 {
		values, source = strings.Split(os.Getenv(envOTLPEndpoints), ","), envOTLPEndpoints
	}

	var (
		endpoints []string
		seen      = make(map[string]bool)
	)
	for _, value := range values {
		endpoint := strings.TrimSpace(value)
		if endpoint == "" {
			continue
		}
		if err := validateEndpointURL(endpoint); err != nil {
			return nil, fmt.Errorf("%s: invalid OTLP metrics endpoint %q: %w", source, endpoint, err)
		}
		if !seen[endpoint] {
			seen[endpoint] = true
			endpoints = append(endpoints, endpoint)
		}
	}
	return endpoints, nil
}

// validateEndpointURL rejects anything the OTLP exporters would not use as an
// endpoint. The scheme is what decides transport security, and a bare
// "host:port" parses as a URL with the host as its scheme, so both a scheme and
// a host are required.
func validateEndpointURL(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New(`want an absolute URL with an "http" or "https" scheme, e.g. "http://collector:4318"`)
	case u.Host == "":
		return errors.New("missing host")
	}
	return nil
}

// defaultMetricsPath is where OTLP/HTTP metrics are posted on a collector that
// was named by host alone.
const defaultMetricsPath = "/v1/metrics"

// otlpHTTPEndpointURL returns the URL to hand to otlpmetrichttp. An endpoint
// with a path of its own is used verbatim; one without gains the default
// metrics path, so "http://collector:4318" reaches the collector rather than
// its bare root.
//
// The exporter used to append that path itself, but since v1.46.0
// WithEndpointURL normalizes an empty path to "/" and posts there, which a
// collector answers with 404. An unparseable endpoint cannot get here —
// validateEndpointURL already rejected it — and is passed through for the
// exporter to complain about.
func otlpHTTPEndpointURL(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Path != "" {
		return endpoint
	}
	u.Path = defaultMetricsPath
	return u.String()
}

// otlpHTTPClient overrides the HTTP client of the OTLP/HTTP exporters. It is nil
// in production; the tests set it to a client with a stub transport so they can
// assert what each endpoint receives without binding a listening socket (the
// gateway's own tests avoid listening sockets for the same reason).
var otlpHTTPClient *http.Client

// newOTLPExporters builds one OTLP exporter per endpoint, all speaking the
// configured protocol. With no endpoints it builds the single exporter the
// standard environment describes. Headers, TLS material, compression and
// timeouts always come from the standard OTEL_EXPORTER_OTLP_* variables; an
// endpoint passed here overrides only the destination.
func newOTLPExporters(ctx context.Context, protocol string, endpoints []string) ([]sdkmetric.Exporter, error) {
	resolved, source := protocol, "--metrics-otlp-protocol"
	for _, env := range []string{envProtocolMetr, envProtocol} {
		if resolved != "" {
			break
		}
		resolved, source = strings.TrimSpace(os.Getenv(env)), env
	}

	// newExporter builds one exporter; endpoint is empty when it should come from
	// the environment.
	var newExporter func(endpoint string) (sdkmetric.Exporter, error)
	switch strings.ToLower(resolved) {
	case ProtocolGRPC:
		newExporter = func(endpoint string) (sdkmetric.Exporter, error) {
			var opts []otlpmetricgrpc.Option
			if endpoint != "" {
				opts = append(opts, otlpmetricgrpc.WithEndpointURL(endpoint))
			}
			return otlpmetricgrpc.New(ctx, opts...)
		}
	case "", ProtocolHTTP:
		// http/protobuf is the specification's default. Note that a collector's
		// gRPC port (4317) will not answer it: set the protocol to match the
		// endpoint.
		newExporter = func(endpoint string) (sdkmetric.Exporter, error) {
			var opts []otlpmetrichttp.Option
			if endpoint != "" {
				opts = append(opts, otlpmetrichttp.WithEndpointURL(otlpHTTPEndpointURL(endpoint)))
			}
			if otlpHTTPClient != nil {
				opts = append(opts, otlpmetrichttp.WithHTTPClient(otlpHTTPClient))
			}
			return otlpmetrichttp.New(ctx, opts...)
		}
	default:
		return nil, fmt.Errorf("%s: unsupported OTLP protocol %q (want %s or %s)",
			source, resolved, ProtocolGRPC, ProtocolHTTP)
	}

	targets := endpoints
	if len(targets) == 0 {
		targets = []string{""}
	}
	exporters := make([]sdkmetric.Exporter, 0, len(targets))
	for _, endpoint := range targets {
		exporter, err := newExporter(endpoint)
		if err != nil {
			if endpoint == "" {
				return nil, fmt.Errorf("creating OTLP exporter: %w", err)
			}
			return nil, fmt.Errorf("creating OTLP exporter for %s: %w", endpoint, err)
		}
		exporters = append(exporters, exporter)
	}
	return exporters, nil
}

// newResource describes this process to the metrics backend. Several gateway
// instances usually run behind one service, so service.instance.id is always
// populated (with the hostname, which is the pod name in Kubernetes) to keep
// their time series apart.
func newResource(ctx context.Context, serviceName string) (*resource.Resource, error) {
	if serviceName == "" {
		serviceName = "unknown_service"
	}
	return resource.New(ctx,
		// The defaults come first: resource.WithFromEnv is applied after them, so
		// OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES override them.
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceInstanceID(instanceID()),
		),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
		resource.WithFromEnv(),
	)
}

// instanceID identifies this process. The hostname is preferred because it is
// the pod name under Kubernetes and the machine name elsewhere, which is what an
// operator looking at a fleet-wide dashboard wants to see; a random UUID is only
// a fallback.
func instanceID() string {
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return uuid.NewString()
}
