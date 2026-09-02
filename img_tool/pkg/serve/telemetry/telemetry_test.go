package telemetry

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// clearOTelEnv empties every variable this package, the SDK, or the exporters
// read, so the developer's own OpenTelemetry environment cannot influence a
// test. An empty value is treated as unset by all of them.
func clearOTelEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		envExporters, envEndpoint, envEndpointMetr, envOTLPEndpoints,
		envProtocol, envProtocolMetr,
		"OTEL_EXPORTER_OTLP_COMPRESSION", "OTEL_EXPORTER_OTLP_METRICS_COMPRESSION",
		"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_METRICS_HEADERS",
		"OTEL_METRIC_EXPORT_INTERVAL",
	} {
		t.Setenv(key, "")
	}
}

func TestResolveExporters(t *testing.T) {
	for _, tc := range []struct {
		name      string
		flag      string
		endpoints []string
		env       map[string]string
		want      []string
		wantErr   bool
	}{
		{
			name: "off by default",
			want: nil,
		},
		{
			name: "an OTLP endpoint in the environment turns metrics on",
			env:  map[string]string{envEndpoint: "http://collector:4318"},
			want: []string{ExporterOTLP},
		},
		{
			name: "a metrics-only OTLP endpoint turns metrics on",
			env:  map[string]string{envEndpointMetr: "http://collector:4318/v1/metrics"},
			want: []string{ExporterOTLP},
		},
		{
			name:      "named endpoints turn metrics on",
			endpoints: []string{"http://collector-a:4318", "http://collector-b:4318"},
			want:      []string{ExporterOTLP},
		},
		{
			name:      "an explicit exporter wins over named endpoints",
			flag:      "prometheus",
			endpoints: []string{"http://collector-a:4318"},
			want:      []string{ExporterPrometheus},
		},
		{
			name:      "the environment's exporter wins over named endpoints",
			endpoints: []string{"http://collector-a:4318"},
			env:       map[string]string{envExporters: "console"},
			want:      []string{ExporterConsole},
		},
		{
			name:      "none disables named endpoints",
			flag:      "none",
			endpoints: []string{"http://collector-a:4318"},
			want:      nil,
		},
		{
			name: "the flag wins over the environment",
			flag: "prometheus",
			env:  map[string]string{envExporters: "otlp"},
			want: []string{ExporterPrometheus},
		},
		{
			name: "the environment selects the exporter",
			env:  map[string]string{envExporters: "console"},
			want: []string{ExporterConsole},
		},
		{
			name: "several exporters at once",
			flag: "otlp, prometheus",
			want: []string{ExporterOTLP, ExporterPrometheus},
		},
		{
			name: "duplicates collapse",
			flag: "otlp,otlp",
			want: []string{ExporterOTLP},
		},
		{
			name: "stdout is an alias of console",
			flag: "stdout",
			want: []string{ExporterConsole},
		},
		{
			name: "none disables an endpoint from the environment",
			flag: "none",
			env:  map[string]string{envEndpoint: "http://collector:4318"},
			want: nil,
		},
		{
			name:    "none cannot be combined",
			flag:    "none,otlp",
			wantErr: true,
		},
		{
			name:    "an unknown exporter is an error",
			flag:    "graphite",
			wantErr: true,
		},
		{
			name:    "an unknown exporter in the environment is an error",
			env:     map[string]string{envExporters: "graphite"},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearOTelEnv(t)
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			got, err := resolveExporters(tc.flag, tc.endpoints)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveExporters(%q) = %v, want an error", tc.flag, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveExporters(%q): %v", tc.flag, err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("resolveExporters(%q) = %v, want %v", tc.flag, got, tc.want)
			}
		})
	}
}

func TestResolveOTLPEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flags   []string
		env     string
		want    []string
		wantErr string // substring the error must name
	}{
		{
			name: "none configured keeps the environment-driven single exporter",
			want: nil,
		},
		{
			name:  "one endpoint",
			flags: []string{"http://collector:4318"},
			want:  []string{"http://collector:4318"},
		},
		{
			name:  "several endpoints keep their order",
			flags: []string{"http://collector-1:4317", "https://collector-0:4317"},
			want:  []string{"http://collector-1:4317", "https://collector-0:4317"},
		},
		{
			name:  "duplicates collapse",
			flags: []string{"http://collector:4318", "http://collector:4318"},
			want:  []string{"http://collector:4318"},
		},
		{
			name:  "surrounding space and empty values are ignored",
			flags: []string{" http://collector:4318 ", "", "  "},
			want:  []string{"http://collector:4318"},
		},
		{
			name: "the environment provides a comma-separated list",
			env:  "http://collector-0:4317, http://collector-1:4317",
			want: []string{"http://collector-0:4317", "http://collector-1:4317"},
		},
		{
			name:  "the flag wins over the environment",
			flags: []string{"http://flag:4318"},
			env:   "http://env:4318",
			want:  []string{"http://flag:4318"},
		},
		{
			// The exporter option would accept this silently (a bare host:port
			// parses as a URL whose scheme is the host) and then export to the
			// default collector address instead.
			name:    "a bare host and port is rejected",
			flags:   []string{"collector:4317"},
			wantErr: "collector:4317",
		},
		{
			name:    "a scheme other than http(s) is rejected",
			flags:   []string{"grpc://collector:4317"},
			wantErr: "grpc://collector:4317",
		},
		{
			name:    "a URL without a host is rejected",
			flags:   []string{"http:///v1/metrics"},
			wantErr: "http:///v1/metrics",
		},
		{
			name:    "an unparseable URL is rejected",
			flags:   []string{"http://[::1"},
			wantErr: "http://[::1",
		},
		{
			name:    "a bad value in the environment is rejected too",
			env:     "http://collector-0:4317,nonsense",
			wantErr: "nonsense",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearOTelEnv(t)
			if tc.env != "" {
				t.Setenv(envOTLPEndpoints, tc.env)
			}
			got, err := resolveOTLPEndpoints(tc.flags)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("resolveOTLPEndpoints(%v) = %v, want an error", tc.flags, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not name the offending value %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveOTLPEndpoints(%v): %v", tc.flags, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("resolveOTLPEndpoints(%v) = %v, want %v", tc.flags, got, tc.want)
			}
		})
	}
}

// TestOTLPHTTPEndpointURL checks the path an OTLP/HTTP collector is posted to.
// The exporter no longer appends the default metrics path itself, so a
// collector named by host alone would otherwise be posted to at its root.
func TestOTLPHTTPEndpointURL(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
		want     string
	}{
		{
			name:     "no path gains the default metrics path",
			endpoint: "http://collector:4318",
			want:     "http://collector:4318/v1/metrics",
		},
		{
			name:     "an explicit root path is left alone",
			endpoint: "http://collector:4318/",
			want:     "http://collector:4318/",
		},
		{
			name:     "a path of its own is used verbatim",
			endpoint: "https://collector:4318/custom/v1/metrics",
			want:     "https://collector:4318/custom/v1/metrics",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := otlpHTTPEndpointURL(tc.endpoint); got != tc.want {
				t.Errorf("otlpHTTPEndpointURL(%q) = %q, want %q", tc.endpoint, got, tc.want)
			}
		})
	}
}

func TestSetupDisabled(t *testing.T) {
	clearOTelEnv(t)
	provider, err := Setup(context.Background(), Config{ServiceName: "test"})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if provider.Enabled() {
		t.Error("metrics are enabled without any exporter configured")
	}
	if provider.MeterProvider == nil {
		t.Fatal("MeterProvider is nil; callers must always be able to record")
	}
	if provider.PrometheusHandler != nil {
		t.Error("a /metrics handler was built without the prometheus exporter")
	}
	// Recording into the disabled provider must not panic, and shutting it down
	// must be a no-op.
	counter, err := provider.MeterProvider.Meter("test").Int64Counter("test.counter")
	if err != nil {
		t.Fatalf("creating counter: %v", err)
	}
	counter.Add(context.Background(), 1)
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

func TestSetupRejectsUnknownExporter(t *testing.T) {
	if _, err := Setup(context.Background(), Config{MetricExporters: "graphite"}); err == nil {
		t.Fatal("Setup accepted an unknown exporter")
	}
}

func TestSetupRejectsUnusableOTLPEndpoint(t *testing.T) {
	clearOTelEnv(t)
	// Rejecting at startup, and even with metrics switched off, is deliberate: a
	// typo in a metrics endpoint that only stops the export is exactly the silent
	// failure this option exists to avoid.
	for _, cfg := range []Config{
		{OTLPEndpoints: []string{"collector:4317"}},
		{MetricExporters: ExporterNone, OTLPEndpoints: []string{"collector:4317"}},
	} {
		if _, err := Setup(context.Background(), cfg); err == nil {
			t.Errorf("Setup(%+v) accepted an endpoint that is not an absolute http(s) URL", cfg)
		}
	}
}

// stubOTLPTransport answers OTLP/HTTP exports in memory, recording what each
// endpoint received. The collectors are stubbed at the transport rather than
// served on a port, like the gateway's own tests, which keeps this hermetic.
type stubOTLPTransport struct {
	mu sync.Mutex
	// exports maps "scheme://host/path" to the bodies delivered to it.
	exports map[string][][]byte
	// attempts counts exports per host, including the ones that fail.
	attempts map[string]int
	// unreachable are hosts whose exports fail, standing in for a collector that
	// is down.
	unreachable map[string]bool
}

func newStubOTLPTransport(unreachable ...string) *stubOTLPTransport {
	s := &stubOTLPTransport{
		exports:     make(map[string][][]byte),
		attempts:    make(map[string]int),
		unreachable: make(map[string]bool),
	}
	for _, host := range unreachable {
		s.unreachable[host] = true
	}
	return s
}

func (s *stubOTLPTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		read, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		body = read
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts[r.URL.Host]++
	if s.unreachable[r.URL.Host] {
		return nil, errors.New("connection refused")
	}
	target := r.URL.Scheme + "://" + r.URL.Host + r.URL.Path
	s.exports[target] = append(s.exports[target], body)
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{},
		Body:       http.NoBody,
		Request:    r,
	}, nil
}

func (s *stubOTLPTransport) received(target string) [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.exports[target])
}

func (s *stubOTLPTransport) targets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	targets := make([]string, 0, len(s.exports))
	for target := range s.exports {
		targets = append(targets, target)
	}
	slices.Sort(targets)
	return targets
}

func (s *stubOTLPTransport) attemptsFor(host string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[host]
}

// installStubOTLPTransport points the OTLP/HTTP exporters at stub for the rest
// of the test.
func installStubOTLPTransport(t *testing.T, stub *stubOTLPTransport) {
	t.Helper()
	previous := otlpHTTPClient
	otlpHTTPClient = &http.Client{Transport: stub}
	t.Cleanup(func() { otlpHTTPClient = previous })
}

// syncBuffer collects log output written from the readers' own goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestSetupOTLPEndpointsFanOut checks that every named endpoint gets its own
// reader and really receives the metrics, that a repeated endpoint does not
// double the work, and that the URL decides scheme and path.
func TestSetupOTLPEndpointsFanOut(t *testing.T) {
	clearOTelEnv(t)
	stub := newStubOTLPTransport()
	installStubOTLPTransport(t, stub)

	const (
		plaintext = "http://collector-0:4318"
		secure    = "https://collector-1:4318/custom/v1/metrics"
	)
	// No exporter is named: the endpoints alone must enable OTLP.
	provider, err := Setup(context.Background(), Config{
		OTLPProtocol:  ProtocolHTTP,
		OTLPEndpoints: []string{plaintext, secure, plaintext},
		ServiceName:   "oci-distribution-gateway",
		Logger:        log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if !provider.Enabled() {
		t.Fatal("naming OTLP endpoints did not enable metrics")
	}

	counter, err := provider.MeterProvider.Meter("test").Int64Counter("oci.gateway.errors")
	if err != nil {
		t.Fatalf("creating counter: %v", err)
	}
	counter.Add(context.Background(), 1)

	// Shutdown flushes every registered reader, so each endpoint sees exactly one
	// export of the counter.
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	want := []string{
		"http://collector-0:4318/v1/metrics", // no path in the URL: the OTLP default
		"https://collector-1:4318/custom/v1/metrics",
	}
	if got := stub.targets(); !slices.Equal(got, want) {
		t.Fatalf("exported to %v, want %v (the repeated endpoint must collapse)", got, want)
	}
	for _, target := range want {
		bodies := stub.received(target)
		if len(bodies) != 1 {
			t.Errorf("%s received %d exports, want 1", target, len(bodies))
			continue
		}
		// The body is a protobuf ExportMetricsServiceRequest; the instrument name
		// is in it verbatim.
		if !bytes.Contains(bodies[0], []byte("oci.gateway.errors")) {
			t.Errorf("%s received an export without the recorded metric", target)
		}
	}
}

// TestSetupOTLPEndpointFailureIsIsolated checks that an unreachable collector
// neither stops the others from exporting nor hides its own failure.
func TestSetupOTLPEndpointFailureIsIsolated(t *testing.T) {
	clearOTelEnv(t)
	// Export often enough that the periodic path runs during the test.
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "50")
	stub := newStubOTLPTransport("broken:4318")
	installStubOTLPTransport(t, stub)
	logs := &syncBuffer{}

	provider, err := Setup(context.Background(), Config{
		OTLPProtocol:  ProtocolHTTP,
		OTLPEndpoints: []string{"http://broken:4318", "http://healthy:4318"},
		ServiceName:   "oci-distribution-gateway",
		Logger:        log.New(logs, "", 0),
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	// Stop the readers even if an assertion below fails first. A second shutdown
	// is a no-op error, which is fine to drop here.
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	counter, err := provider.MeterProvider.Meter("test").Int64Counter("oci.gateway.errors")
	if err != nil {
		t.Fatalf("creating counter: %v", err)
	}
	counter.Add(context.Background(), 1)

	// The healthy collector keeps receiving while the broken one fails, and the
	// broken one's failures reach the logger through the error handler.
	const target = "http://healthy:4318/v1/metrics"
	deadline := time.Now().Add(10 * time.Second)
	for {
		exported := len(stub.received(target)) > 0
		logged := strings.Contains(logs.String(), "broken:4318")
		if exported && logged {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after 10s: healthy endpoint exported=%v, failure logged=%v (attempts: broken=%d healthy=%d, log: %s)",
				exported, logged, stub.attemptsFor("broken:4318"), stub.attemptsFor("healthy:4318"), logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The final flush reports the broken endpoint rather than swallowing it, and
	// still flushes the healthy one.
	before := len(stub.received(target))
	if err := provider.Shutdown(context.Background()); err == nil {
		t.Error("Shutdown hid the failing endpoint")
	}
	if got := len(stub.received(target)); got <= before {
		t.Errorf("the healthy endpoint was not flushed on shutdown (%d exports, was %d)", got, before)
	}
}

func TestSetupRejectsUnknownOTLPProtocol(t *testing.T) {
	if _, err := Setup(context.Background(), Config{MetricExporters: "otlp", OTLPProtocol: "carrier-pigeon"}); err == nil {
		t.Fatal("Setup accepted an unknown OTLP protocol")
	}
}

// TestSetupPrometheus checks the scrape endpoint end to end, including the
// Prometheus spelling of the metric names the documentation quotes.
func TestSetupPrometheus(t *testing.T) {
	provider, err := Setup(context.Background(), Config{
		MetricExporters: "prometheus",
		ServiceName:     "oci-distribution-gateway",
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	if !provider.Enabled() || provider.PrometheusHandler == nil {
		t.Fatal("the prometheus exporter did not produce a /metrics handler")
	}

	meter := provider.MeterProvider.Meter("test")
	errors, err := meter.Int64Counter("oci.gateway.errors", metric.WithUnit("{error}"))
	if err != nil {
		t.Fatalf("creating counter: %v", err)
	}
	errors.Add(context.Background(), 2, metric.WithAttributes(
		attribute.String("error.type", "connection_refused"),
		attribute.String("oci.registry", "docker.acme.corp"),
	))
	transferred, err := meter.Int64Counter("oci.gateway.io", metric.WithUnit("By"))
	if err != nil {
		t.Fatalf("creating counter: %v", err)
	}
	transferred.Add(context.Background(), 4096, metric.WithAttributes(semconv.NetworkIODirectionTransmit))

	recorder := httptest.NewRecorder()
	provider.PrometheusHandler.ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	if recorder.Code != 200 {
		t.Fatalf("scrape status = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()
	for _, want := range []string{
		`oci_gateway_errors_total{`,
		`error_type="connection_refused"`,
		`oci_registry="docker.acme.corp"`,
		`oci_gateway_io_bytes_total{`,
		`network_io_direction="transmit"`,
		// target_info carries the resource, which is what keeps the series of
		// several gateway instances apart.
		`service_name="oci-distribution-gateway"`,
		`service_instance_id="`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape output does not contain %q\n%s", want, body)
		}
	}
}

func TestNewResourceIdentifiesTheInstance(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	res, err := newResource(context.Background(), "oci-distribution-gateway")
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}
	attrs := res.Set()
	if got, _ := attrs.Value(semconv.ServiceNameKey); got.AsString() != "oci-distribution-gateway" {
		t.Errorf("service.name = %q, want oci-distribution-gateway", got.AsString())
	}
	if got, ok := attrs.Value(semconv.ServiceInstanceIDKey); !ok || got.AsString() == "" {
		t.Error("service.instance.id is empty; instances in a cluster would collide")
	}
}

func TestNewResourceEnvironmentWins(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "gateway-eu-west")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.instance.id=pod-7,deployment.environment=prod")
	res, err := newResource(context.Background(), "oci-distribution-gateway")
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}
	attrs := res.Set()
	if got, _ := attrs.Value(semconv.ServiceNameKey); got.AsString() != "gateway-eu-west" {
		t.Errorf("service.name = %q, want the OTEL_SERVICE_NAME value", got.AsString())
	}
	if got, _ := attrs.Value(semconv.ServiceInstanceIDKey); got.AsString() != "pod-7" {
		t.Errorf("service.instance.id = %q, want the OTEL_RESOURCE_ATTRIBUTES value", got.AsString())
	}
	if got, _ := attrs.Value(attribute.Key("deployment.environment")); got.AsString() != "prod" {
		t.Errorf("deployment.environment = %q, want prod", got.AsString())
	}
}
