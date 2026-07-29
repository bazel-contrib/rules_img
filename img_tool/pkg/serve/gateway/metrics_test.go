package gateway

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"

	clientgateway "github.com/bazel-contrib/rules_img/img_tool/pkg/gateway"
)

// upstreamFunc is a scripted upstream registry. The gateway pings /v2/ once per
// repository and scope while setting up its authenticated transport, which
// newMetricsHandler answers on its own, so a test only scripts the endpoint it
// cares about.
type upstreamFunc func(*http.Request) (*http.Response, error)

func (f upstreamFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	// A real transport reads the request body; the byte counters depend on it.
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, r.Body)
	}
	if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
		return upstreamResponse(http.StatusOK, nil, ""), nil
	}
	return f(r)
}

func upstreamResponse(status int, header http.Header, body string) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode:    status,
		Status:        http.StatusText(status),
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
	}
}

// newMetricsHandler returns a gateway that records into a manual reader, plus a
// collect function returning the metrics gathered so far.
func newMetricsHandler(t *testing.T, cp *CompiledPolicy, upstream upstreamFunc) (*Handler, func() *metricdata.ResourceMetrics) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutting down meter provider: %v", err)
		}
	})
	h := New(
		WithAuthorizer(cp),
		WithKeychain(authn.NewMultiKeychain()), // always anonymous, hermetic
		WithLogger(log.New(io.Discard, "", 0)),
		WithBaseTransport(upstream),
		WithMeterProvider(provider),
	)
	collect := func() *metricdata.ResourceMetrics {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collecting metrics: %v", err)
		}
		return &rm
	}
	return h, collect
}

// serve sends one request through the gateway and returns the recorded response.
func serve(h *Handler, method, host, target string, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	if host != "" {
		r.Header.Set(clientgateway.OriginalHostHeader, host)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// findMetric returns the aggregation recorded for the named instrument.
func findMetric(t *testing.T, rm *metricdata.ResourceMetrics, name string) metricdata.Aggregation {
	t.Helper()
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == name {
				return m.Data
			}
		}
	}
	t.Fatalf("metric %q was not recorded (recorded: %v)", name, recordedNames(rm))
	return nil
}

func recordedNames(rm *metricdata.ResourceMetrics) []string {
	var names []string
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			names = append(names, m.Name)
		}
	}
	return names
}

// hasMetric reports whether the named instrument recorded anything at all.
func hasMetric(rm *metricdata.ResourceMetrics, name string) bool {
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == name {
				return true
			}
		}
	}
	return false
}

// matches reports whether every wanted attribute is present in set.
func matches(set attribute.Set, want []attribute.KeyValue) bool {
	for _, kv := range want {
		got, ok := set.Value(kv.Key)
		if !ok || got != kv.Value {
			return false
		}
	}
	return true
}

// counterValue sums the counter data points carrying all of want.
func counterValue(t *testing.T, rm *metricdata.ResourceMetrics, name string, want ...attribute.KeyValue) int64 {
	t.Helper()
	sum, ok := findMetric(t, rm, name).(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %q is not an int64 sum", name)
	}
	var total int64
	for _, dp := range sum.DataPoints {
		if matches(dp.Attributes, want) {
			total += dp.Value
		}
	}
	return total
}

// histogramValue reports the count and summed value of the int64 histogram data
// points carrying all of want.
func histogramValue(t *testing.T, rm *metricdata.ResourceMetrics, name string, want ...attribute.KeyValue) (uint64, int64) {
	t.Helper()
	hist, ok := findMetric(t, rm, name).(metricdata.Histogram[int64])
	if !ok {
		t.Fatalf("metric %q is not an int64 histogram", name)
	}
	var (
		count uint64
		sum   int64
	)
	for _, dp := range hist.DataPoints {
		if matches(dp.Attributes, want) {
			count += dp.Count
			sum += dp.Sum
		}
	}
	return count, sum
}

// durationCount reports how many requests the duration histogram observed with
// all of want, which is the series a request rate is computed from.
func durationCount(t *testing.T, rm *metricdata.ResourceMetrics, want ...attribute.KeyValue) uint64 {
	t.Helper()
	hist, ok := findMetric(t, rm, "http.server.request.duration").(metricdata.Histogram[float64])
	if !ok {
		t.Fatal("http.server.request.duration is not a float64 histogram")
	}
	var count uint64
	for _, dp := range hist.DataPoints {
		if matches(dp.Attributes, want) {
			count += dp.Count
		}
	}
	return count
}

func TestMetricsBlobDownload(t *testing.T) {
	const blob = "0123456789abcdef"
	h, collect := newMetricsHandler(t, allowHostPolicy(t, testUpstreamHost, "blob:read"),
		func(r *http.Request) (*http.Response, error) {
			return upstreamResponse(http.StatusOK, nil, blob), nil
		})

	if got := serve(h, http.MethodGet, testUpstreamHost, "/v2/app/blobs/sha256:abc", "").Code; got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	rm := collect()

	registry := attrRegistry.String(testUpstreamHost)
	if got := counterValue(t, rm, "oci.gateway.blob.downloads", registry, attrOperation.String(opNameBlobRead)); got != 1 {
		t.Errorf("blob downloads = %d, want 1", got)
	}
	count, sum := histogramValue(t, rm, "oci.gateway.blob.download.size", registry)
	if count != 1 || sum != int64(len(blob)) {
		t.Errorf("download size histogram = (%d, %d), want (1, %d)", count, sum, len(blob))
	}
	if got := counterValue(t, rm, "oci.gateway.io", registry, semconv.NetworkIODirectionTransmit); got != int64(len(blob)) {
		t.Errorf("transmitted bytes = %d, want %d", got, len(blob))
	}
	if got := counterValue(t, rm, "oci.gateway.io", registry, semconv.NetworkIODirectionReceive); got != 0 {
		t.Errorf("received bytes = %d, want 0", got)
	}
	if got := durationCount(t, rm,
		registry,
		attrOperation.String(opNameBlobRead),
		semconv.HTTPRoute(routeBlob),
		semconv.HTTPRequestMethodGet,
		semconv.HTTPResponseStatusCode(http.StatusOK),
	); got != 1 {
		t.Errorf("request duration count = %d, want 1", got)
	}
	if got := counterValue(t, rm, "oci.gateway.policy.decisions", registry, attrDecision.String("allow")); got != 1 {
		t.Errorf("policy allow decisions = %d, want 1", got)
	}
	if hasMetric(rm, "oci.gateway.errors") {
		t.Error("a successful download recorded an error")
	}
}

// TestMetricsBlobUploadSession covers the upload flow go-containerregistry (and
// therefore rules_img) uses: POST to open a session, PATCH carrying the bytes,
// PUT with an empty body to commit. The blob's size must be attributed to the
// PUT that completes it.
func TestMetricsBlobUploadSession(t *testing.T) {
	const blob = "layer-bytes-layer-bytes"
	h, collect := newMetricsHandler(t, allowHostPolicy(t, testUpstreamHost, "blob:write"),
		func(r *http.Request) (*http.Response, error) {
			switch r.Method {
			case http.MethodPost:
				header := http.Header{}
				header.Set("Location", "/v2/app/blobs/uploads/session-1?_state=x")
				return upstreamResponse(http.StatusAccepted, header, ""), nil
			case http.MethodPatch:
				return upstreamResponse(http.StatusAccepted, nil, ""), nil
			default:
				return upstreamResponse(http.StatusCreated, nil, ""), nil
			}
		})

	serve(h, http.MethodPost, testUpstreamHost, "/v2/app/blobs/uploads/", "")
	serve(h, http.MethodPatch, testUpstreamHost, "/v2/app/blobs/uploads/session-1?_state=x", blob)
	serve(h, http.MethodPut, testUpstreamHost, "/v2/app/blobs/uploads/session-1?_state=x&digest=sha256:abc", "")
	rm := collect()

	registry := attrRegistry.String(testUpstreamHost)
	if got := counterValue(t, rm, "oci.gateway.blob.uploads", registry); got != 1 {
		t.Errorf("blob uploads = %d, want 1 (only the committing PUT counts)", got)
	}
	if got := counterValue(t, rm, "oci.gateway.blob.uploads", registry, attrUploadKind.String(uploadChunked)); got != 1 {
		t.Errorf("chunked uploads = %d, want 1", got)
	}
	count, sum := histogramValue(t, rm, "oci.gateway.blob.upload.size", registry)
	if count != 1 || sum != int64(len(blob)) {
		t.Errorf("upload size histogram = (%d, %d), want (1, %d)", count, sum, len(blob))
	}
	if got := counterValue(t, rm, "oci.gateway.io", registry, semconv.NetworkIODirectionReceive); got != int64(len(blob)) {
		t.Errorf("received bytes = %d, want %d", got, len(blob))
	}
	if got := durationCount(t, rm, registry, attrOperation.String(opNameBlobUpload)); got != 3 {
		t.Errorf("upload requests = %d, want 3", got)
	}
	if got := durationCount(t, rm, semconv.HTTPRoute(routeUploadStart)); got != 1 {
		t.Errorf("session-start requests = %d, want 1", got)
	}
	if got := durationCount(t, rm, semconv.HTTPRoute(routeUploadSession)); got != 2 {
		t.Errorf("session requests = %d, want 2", got)
	}
}

func TestMetricsBlobUploadMonolithicAndMount(t *testing.T) {
	const blob = "monolithic"
	h, collect := newMetricsHandler(t, allowHostPolicy(t, testUpstreamHost, "blob:read", "blob:write"),
		func(r *http.Request) (*http.Response, error) {
			return upstreamResponse(http.StatusCreated, nil, ""), nil
		})

	// Single-request upload: everything arrives in one PUT.
	serve(h, http.MethodPut, testUpstreamHost, "/v2/app/blobs/uploads/session-2?digest=sha256:abc", blob)
	// Cross-repository mount: the blob is added without transferring bytes.
	serve(h, http.MethodPost, testUpstreamHost, "/v2/app/blobs/uploads/?mount=sha256:abc&from=other/app", "")
	rm := collect()

	registry := attrRegistry.String(testUpstreamHost)
	if got := counterValue(t, rm, "oci.gateway.blob.uploads", registry, attrUploadKind.String(uploadMonolithic)); got != 1 {
		t.Errorf("monolithic uploads = %d, want 1", got)
	}
	if got := counterValue(t, rm, "oci.gateway.blob.uploads", registry, attrUploadKind.String(uploadMount)); got != 1 {
		t.Errorf("mounted uploads = %d, want 1", got)
	}
	count, sum := histogramValue(t, rm, "oci.gateway.blob.upload.size", registry)
	if count != 1 || sum != int64(len(blob)) {
		t.Errorf("upload size histogram = (%d, %d), want (1, %d): a mount transfers no bytes", count, sum, len(blob))
	}
}

func TestMetricsExistenceChecks(t *testing.T) {
	h, collect := newMetricsHandler(t, allowHostPolicy(t, testUpstreamHost, "blob:read", "manifest:read"),
		func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "missing") {
				return upstreamResponse(http.StatusNotFound, nil, ""), nil
			}
			return upstreamResponse(http.StatusOK, nil, ""), nil
		})

	serve(h, http.MethodHead, testUpstreamHost, "/v2/app/blobs/sha256:present", "")
	serve(h, http.MethodHead, testUpstreamHost, "/v2/app/blobs/sha256:missing", "")
	serve(h, http.MethodHead, testUpstreamHost, "/v2/app/manifests/missing", "")
	// A GET is not an existence check.
	serve(h, http.MethodGet, testUpstreamHost, "/v2/app/blobs/sha256:present", "")
	rm := collect()

	registry := attrRegistry.String(testUpstreamHost)
	if got := counterValue(t, rm, "oci.gateway.existence_checks", registry, attrResult.String(resultHit)); got != 1 {
		t.Errorf("hits = %d, want 1", got)
	}
	if got := counterValue(t, rm, "oci.gateway.existence_checks", registry, attrResult.String(resultMiss)); got != 2 {
		t.Errorf("misses = %d, want 2", got)
	}
	if got := counterValue(t, rm, "oci.gateway.existence_checks", attrOperation.String(opNameBlobHead)); got != 2 {
		t.Errorf("blob existence checks = %d, want 2", got)
	}
	if got := counterValue(t, rm, "oci.gateway.existence_checks", attrOperation.String(opNameManifestHead)); got != 1 {
		t.Errorf("manifest existence checks = %d, want 1", got)
	}
	// A 404 is routine registry traffic, not an error.
	if hasMetric(rm, "oci.gateway.errors") {
		t.Error("a 404 existence check was counted as an error")
	}
}

func TestMetricsErrorTypes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		policy   func(*testing.T) *CompiledPolicy
		upstream upstreamFunc
		method   string
		host     string
		target   string
		want     string
		// wantRegistry is the oci.registry attribute expected on the error.
		wantRegistry string
	}{
		{
			name:         "policy denies the operation",
			policy:       func(t *testing.T) *CompiledPolicy { return allowHostPolicy(t, testUpstreamHost, "blob:read") },
			method:       http.MethodPut,
			host:         testUpstreamHost,
			target:       "/v2/app/manifests/latest",
			want:         errPolicyDenied,
			wantRegistry: testUpstreamHost,
		},
		{
			name:         "registry not in the policy",
			policy:       func(t *testing.T) *CompiledPolicy { return allowHostPolicy(t, testUpstreamHost, "blob:read") },
			method:       http.MethodGet,
			host:         "other.test",
			target:       "/v2/app/blobs/sha256:abc",
			want:         errRegistryDenied,
			wantRegistry: "other.test",
		},
		{
			name:         "mount source not readable",
			policy:       func(t *testing.T) *CompiledPolicy { return allowHostPolicy(t, testUpstreamHost, "blob:write") },
			method:       http.MethodPost,
			host:         testUpstreamHost,
			target:       "/v2/app/blobs/uploads/?mount=sha256:abc&from=secret/app",
			want:         errMountDenied,
			wantRegistry: testUpstreamHost,
		},
		{
			name:         "missing host header",
			policy:       func(t *testing.T) *CompiledPolicy { return AllowAll() },
			method:       http.MethodGet,
			target:       "/v2/app/blobs/sha256:abc",
			want:         errMissingHost,
			wantRegistry: registryUnknown,
		},
		{
			name:         "unsupported endpoint",
			policy:       func(t *testing.T) *CompiledPolicy { return AllowAll() },
			method:       http.MethodGet,
			host:         testUpstreamHost,
			target:       "/v2/app/other",
			want:         errUnsupportedEndpoint,
			wantRegistry: registryUnknown,
		},
		{
			name:         "ambiguous upload query",
			policy:       func(t *testing.T) *CompiledPolicy { return AllowAll() },
			method:       http.MethodPost,
			host:         testUpstreamHost,
			target:       "/v2/app/blobs/uploads/?mount=a&mount=b&from=c",
			want:         errMalformedQuery,
			wantRegistry: registryUnknown,
		},
		{
			name:   "upstream rejects our credentials",
			policy: func(t *testing.T) *CompiledPolicy { return AllowAll() },
			upstream: func(r *http.Request) (*http.Response, error) {
				return upstreamResponse(http.StatusUnauthorized, nil, ""), nil
			},
			method:       http.MethodGet,
			host:         testUpstreamHost,
			target:       "/v2/app/blobs/sha256:abc",
			want:         errUpstreamUnauthorized,
			wantRegistry: testUpstreamHost,
		},
		{
			name:   "upstream denies access",
			policy: func(t *testing.T) *CompiledPolicy { return AllowAll() },
			upstream: func(r *http.Request) (*http.Response, error) {
				return upstreamResponse(http.StatusForbidden, nil, ""), nil
			},
			method:       http.MethodGet,
			host:         testUpstreamHost,
			target:       "/v2/app/blobs/sha256:abc",
			want:         errUpstreamForbidden,
			wantRegistry: testUpstreamHost,
		},
		{
			name:   "upstream is broken",
			policy: func(t *testing.T) *CompiledPolicy { return AllowAll() },
			upstream: func(r *http.Request) (*http.Response, error) {
				return upstreamResponse(http.StatusBadGateway, nil, ""), nil
			},
			method:       http.MethodGet,
			host:         testUpstreamHost,
			target:       "/v2/app/blobs/sha256:abc",
			want:         errUpstreamServerError,
			wantRegistry: testUpstreamHost,
		},
		{
			name:   "upstream rate limits us",
			policy: func(t *testing.T) *CompiledPolicy { return AllowAll() },
			upstream: func(r *http.Request) (*http.Response, error) {
				return upstreamResponse(http.StatusTooManyRequests, nil, ""), nil
			},
			method:       http.MethodGet,
			host:         testUpstreamHost,
			target:       "/v2/app/blobs/sha256:abc",
			want:         errUpstreamRateLimited,
			wantRegistry: testUpstreamHost,
		},
		{
			name:   "upstream connection refused",
			policy: func(t *testing.T) *CompiledPolicy { return AllowAll() },
			upstream: func(r *http.Request) (*http.Response, error) {
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
			},
			method:       http.MethodGet,
			host:         testUpstreamHost,
			target:       "/v2/app/blobs/sha256:abc",
			want:         errConnectionRefused,
			wantRegistry: testUpstreamHost,
		},
		{
			name:   "upstream name does not resolve",
			policy: func(t *testing.T) *CompiledPolicy { return AllowAll() },
			upstream: func(r *http.Request) (*http.Response, error) {
				return nil, &net.DNSError{Err: "no such host", Name: testUpstreamHost, IsNotFound: true}
			},
			method:       http.MethodGet,
			host:         testUpstreamHost,
			target:       "/v2/app/blobs/sha256:abc",
			want:         errDNS,
			wantRegistry: testUpstreamHost,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := tc.upstream
			if upstream == nil {
				upstream = func(r *http.Request) (*http.Response, error) {
					return upstreamResponse(http.StatusOK, nil, ""), nil
				}
			}
			h, collect := newMetricsHandler(t, tc.policy(t), upstream)
			serve(h, tc.method, tc.host, tc.target, "")
			rm := collect()

			if got := counterValue(t, rm, "oci.gateway.errors",
				semconv.ErrorTypeKey.String(tc.want),
				attrRegistry.String(tc.wantRegistry),
			); got != 1 {
				t.Errorf("errors{error.type=%s, oci.registry=%s} = %d, want 1", tc.want, tc.wantRegistry, got)
			}
			// The same classification is attached to the request's duration
			// measurement, so failures are visible per route and status too.
			if got := durationCount(t, rm, semconv.ErrorTypeKey.String(tc.want)); got != 1 {
				t.Errorf("duration count with error.type=%s = %d, want 1", tc.want, got)
			}
		})
	}
}

func TestMetricsPolicyReload(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(good, []byte(`{"version":1,"rules":[{"action":"allow","registry":"registry.test","repository":"**","operations":["*"]}]}`), 0o600); err != nil {
		t.Fatalf("writing policy: %v", err)
	}
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte(`{"version":9}`), 0o600); err != nil {
		t.Fatalf("writing policy: %v", err)
	}

	h, collect := newMetricsHandler(t, AllowAll(), func(r *http.Request) (*http.Response, error) {
		return upstreamResponse(http.StatusOK, nil, ""), nil
	})
	if _, err := h.Reload(good); err != nil {
		t.Fatalf("Reload(good): %v", err)
	}
	if _, err := h.Reload(broken); err == nil {
		t.Fatal("Reload(broken) succeeded, want an error")
	}
	rm := collect()

	if got := counterValue(t, rm, "oci.gateway.policy.reloads", attrResult.String(resultSuccess)); got != 1 {
		t.Errorf("successful reloads = %d, want 1", got)
	}
	if got := counterValue(t, rm, "oci.gateway.policy.reloads", attrResult.String(resultFailure)); got != 1 {
		t.Errorf("failed reloads = %d, want 1", got)
	}
	gauge, ok := findMetric(t, rm, "oci.gateway.policy.rules").(metricdata.Gauge[int64])
	if !ok {
		t.Fatal("oci.gateway.policy.rules is not an int64 gauge")
	}
	if len(gauge.DataPoints) != 1 || gauge.DataPoints[0].Value != 1 {
		t.Errorf("policy rules gauge = %v, want a single data point of 1 (the last good policy)", gauge.DataPoints)
	}
}

func TestMetricsVersionCheck(t *testing.T) {
	h, collect := newMetricsHandler(t, allowHostPolicy(t, testUpstreamHost, "blob:read"), nil)
	if got := serve(h, http.MethodGet, testUpstreamHost, "/v2/", "").Code; got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	rm := collect()

	if got := durationCount(t, rm,
		attrRegistry.String(testUpstreamHost),
		attrOperation.String(opNameVersionCheck),
		semconv.HTTPRoute(routeVersion),
	); got != 1 {
		t.Errorf("version check requests = %d, want 1", got)
	}
}

// TestMetricsRegistryCardinality checks that a client cannot mint unbounded
// oci.registry attribute values, which would multiply time series in the
// monitoring backend.
func TestMetricsRegistryCardinality(t *testing.T) {
	h, collect := newMetricsHandler(t, AllowAll(), func(r *http.Request) (*http.Response, error) {
		return upstreamResponse(http.StatusOK, nil, ""), nil
	})
	h.metrics.registries = newBoundedValues(2)

	for _, host := range []string{"a.test", "b.test", "c.test", "d.test"} {
		serve(h, http.MethodHead, host, "/v2/app/blobs/sha256:abc", "")
	}
	rm := collect()

	for _, host := range []string{"a.test", "b.test"} {
		if got := durationCount(t, rm, attrRegistry.String(host)); got != 1 {
			t.Errorf("requests for %s = %d, want 1", host, got)
		}
	}
	if got := durationCount(t, rm, attrRegistry.String(registryOverflow)); got != 2 {
		t.Errorf("requests reported as %s = %d, want 2", registryOverflow, got)
	}
	for _, host := range []string{"c.test", "d.test"} {
		if got := durationCount(t, rm, attrRegistry.String(host)); got != 0 {
			t.Errorf("requests for %s = %d, want 0 (past the cap)", host, got)
		}
	}
}

func TestUploadTracker(t *testing.T) {
	tracker := newUploadTracker(2)

	tracker.add("a", 10)
	tracker.add("a", 5)
	if got := tracker.take("a"); got != 15 {
		t.Errorf("take(a) = %d, want 15", got)
	}
	if got := tracker.take("a"); got != 0 {
		t.Errorf("take(a) after take = %d, want 0", got)
	}

	// Abandoned sessions must not grow the table without bound: filling the live
	// generation rotates it, and a second rotation drops the oldest entries.
	tracker.add("x", 1)
	tracker.add("y", 2)
	tracker.add("z", 3) // rotates: x and y move to the old generation
	if got := tracker.take("x"); got != 1 {
		t.Errorf("take(x) = %d, want 1 (still in the old generation)", got)
	}
	tracker.add("w", 4)
	tracker.add("v", 5) // rotates again: y is dropped
	if got := tracker.take("y"); got != 0 {
		t.Errorf("take(y) = %d, want 0 (evicted)", got)
	}
	if got := tracker.take("z"); got != 3 {
		t.Errorf("take(z) = %d, want 3", got)
	}
}

func TestBoundedValues(t *testing.T) {
	b := newBoundedValues(2)
	if got := b.value(""); got != registryUnknown {
		t.Errorf("value(\"\") = %q, want %q", got, registryUnknown)
	}
	for range 3 {
		if got := b.value("a"); got != "a" {
			t.Errorf("value(a) = %q, want a", got)
		}
	}
	if got := b.value("b"); got != "b" {
		t.Errorf("value(b) = %q, want b", got)
	}
	if got := b.value("c"); got != registryOverflow {
		t.Errorf("value(c) = %q, want %q", got, registryOverflow)
	}
	// Known values keep being reported after the cap is reached.
	if got := b.value("a"); got != "a" {
		t.Errorf("value(a) after the cap = %q, want a", got)
	}
}

func TestNormalizedMethod(t *testing.T) {
	if got := normalizedMethod(http.MethodPatch); got != http.MethodPatch {
		t.Errorf("normalizedMethod(PATCH) = %q, want PATCH", got)
	}
	if got := normalizedMethod("BREW"); got != "_OTHER" {
		t.Errorf("normalizedMethod(BREW) = %q, want _OTHER", got)
	}
}
