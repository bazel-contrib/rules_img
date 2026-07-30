package gateway

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	clientgateway "github.com/bazel-contrib/rules_img/img_tool/pkg/gateway"
)

// These tests cover the blob existence cache through the handler: what a client
// observes, and which requests reach the upstream registry. The data structure
// itself is tested in existencecache_test.go.

const testBlobPath = "/v2/app/blobs/" + testCacheDigest

// blobUpstream is a scripted upstream registry that counts the requests reaching
// it, which is how these tests tell a replayed answer from a forwarded one.
type blobUpstream struct {
	// status is answered to everything but the /v2/ ping.
	status int
	// contentLength is sent as the Content-Length header, when not empty.
	contentLength string
	requests      []*http.Request
}

func (u *blobUpstream) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
		return upstreamResponse(http.StatusOK, nil, ""), nil
	}
	u.requests = append(u.requests, r)
	header := http.Header{}
	if u.contentLength != "" {
		header.Set("Content-Length", u.contentLength)
	}
	// A header of the registry's own, which a cached answer must not replay to
	// some other client hours later.
	header.Set("Etag", `"upstream-etag"`)
	return upstreamResponse(u.status, header, ""), nil
}

// count reports how many requests for the given path reached the upstream.
func (u *blobUpstream) count(method, path string) int {
	n := 0
	for _, r := range u.requests {
		if r.Method == method && r.URL.EscapedPath() == path {
			n++
		}
	}
	return n
}

// newCachingHandler wires a gateway with the blob existence cache enabled, plus
// the clock the cache reads, so a test can let entries expire without sleeping.
func newCachingHandler(t *testing.T, cp *CompiledPolicy, base http.RoundTripper, ttl time.Duration) (*Handler, *fakeClock) {
	t.Helper()
	h := New(
		WithAuthorizer(cp),
		WithKeychain(authn.NewMultiKeychain()), // always anonymous, hermetic
		WithLogger(log.New(io.Discard, "", 0)),
		WithBaseTransport(base),
		WithBlobExistenceCache(ttl, 256*entryCost),
	)
	if h.blobCache == nil {
		t.Fatalf("WithBlobExistenceCache(%v, room for 256) left the cache disabled", ttl)
	}
	clock := &fakeClock{t: h.blobCache.base}
	h.blobCache.now = clock.now
	return h, clock
}

// head sends a blob existence check through the gateway, with any extra request
// headers applied as key/value pairs.
func head(h *Handler, host, path string, headers ...string) *http.Response {
	r, _ := http.NewRequest(http.MethodHead, "http://gateway"+path, nil)
	if host != "" {
		r.Header.Set(clientgateway.OriginalHostHeader, host)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Result()
}

// TestBlobExistenceCacheAnswersRepeatProbes is the point of the feature: the
// second probe for a blob the registry already confirmed costs no round trip.
func TestBlobExistenceCacheAnswersRepeatProbes(t *testing.T) {
	up := &blobUpstream{status: http.StatusOK, contentLength: "12345"}
	h, _ := newCachingHandler(t, allowHostPolicy(t, testUpstreamHost, "blob:read"), up, time.Hour)

	first := head(h, testUpstreamHost, testBlobPath)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first probe status = %d, want 200", first.StatusCode)
	}
	if got := up.count(http.MethodHead, testBlobPath); got != 1 {
		t.Fatalf("upstream saw %d probes, want 1", got)
	}

	second := head(h, testUpstreamHost, testBlobPath)
	if second.StatusCode != http.StatusOK {
		t.Fatalf("cached probe status = %d, want 200", second.StatusCode)
	}
	if got := up.count(http.MethodHead, testBlobPath); got != 1 {
		t.Fatalf("upstream saw %d probes, want the second answered from the cache", got)
	}

	// The replayed answer carries what a client reads off a blob HEAD: the size,
	// and the digest it asked about.
	if got := second.Header.Get("Content-Length"); got != "12345" {
		t.Errorf("cached Content-Length = %q, want %q", got, "12345")
	}
	if got := second.Header.Get("Docker-Content-Digest"); got != testCacheDigest {
		t.Errorf("cached Docker-Content-Digest = %q, want %q", got, testCacheDigest)
	}
	// ...and nothing else the registry happened to send.
	if got := second.Header.Get("Etag"); got != "" {
		t.Errorf("cached answer replayed the upstream Etag %q", got)
	}
	if got := first.Header.Get("Etag"); got == "" {
		t.Error("the forwarded answer dropped the upstream Etag, so the test above proves nothing")
	}
}

// TestBlobExistenceCacheOmitsUnknownContentLength keeps a registry that sends no
// Content-Length from being turned into one that claims a zero-byte blob.
func TestBlobExistenceCacheOmitsUnknownContentLength(t *testing.T) {
	up := &blobUpstream{status: http.StatusOK}
	h, _ := newCachingHandler(t, allowHostPolicy(t, testUpstreamHost, "blob:read"), up, time.Hour)

	head(h, testUpstreamHost, testBlobPath)
	cached := head(h, testUpstreamHost, testBlobPath)
	if got := cached.Header.Get("Content-Length"); got != "" {
		t.Errorf("cached Content-Length = %q, want it absent", got)
	}
	if cached.StatusCode != http.StatusOK {
		t.Errorf("cached probe status = %d, want 200", cached.StatusCode)
	}
}

// TestBlobExistenceCacheKeyedByRepositoryAndRegistry is the correctness property
// at the handler level: a blob is present in one repository of one registry, and
// nothing may be concluded about any other.
func TestBlobExistenceCacheKeyedByRepositoryAndRegistry(t *testing.T) {
	up := &blobUpstream{status: http.StatusOK, contentLength: "1"}
	h, _ := newCachingHandler(t, AllowAll(), up, time.Hour)

	head(h, testUpstreamHost, testBlobPath)

	otherRepo := "/v2/other/blobs/" + testCacheDigest
	head(h, testUpstreamHost, otherRepo)
	if got := up.count(http.MethodHead, otherRepo); got != 1 {
		t.Errorf("another repository was answered from the cache (%d upstream probes)", got)
	}

	// The same repository path on a different registry, which resolves to a
	// different upstream.
	head(h, "other.registry.test", testBlobPath)
	if got := up.count(http.MethodHead, testBlobPath); got != 2 {
		t.Errorf("another registry was answered from the cache (%d upstream probes)", got)
	}
}

// TestBlobExistenceCacheStoresOnlyPresentBlobs keeps the cache to the one answer
// that is safe to remember. A blob that is absent now can be pushed a second
// later, so a remembered 404 would tell a client to download something that is
// there, and a remembered error would outlive the outage that caused it.
func TestBlobExistenceCacheStoresOnlyPresentBlobs(t *testing.T) {
	for _, status := range []int{
		http.StatusNotFound,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusUnauthorized,
		// A partial answer is not "the blob is present" either.
		http.StatusPartialContent,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			up := &blobUpstream{status: status}
			h, _ := newCachingHandler(t, allowHostPolicy(t, testUpstreamHost, "blob:read"), up, time.Hour)

			head(h, testUpstreamHost, testBlobPath)
			head(h, testUpstreamHost, testBlobPath)
			if got := up.count(http.MethodHead, testBlobPath); got != 2 {
				t.Errorf("a %d answer was cached (%d upstream probes, want 2)", status, got)
			}
			if entries := h.blobCache.stats().entries; entries != 0 {
				t.Errorf("cache holds %d entries after a %d answer, want 0", entries, status)
			}
		})
	}
}

// TestBlobExistenceCacheIgnoresMutableReferences covers what the cache must not
// touch. A tag or a manifest can be repointed at any moment, and a HEAD on one is
// exactly how a client finds that out.
func TestBlobExistenceCacheIgnoresMutableReferences(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{"manifest by tag", "/v2/app/manifests/latest"},
		{"manifest by digest", "/v2/app/manifests/" + testCacheDigest},
		{"blob by a reference that is not a digest", "/v2/app/blobs/latest"},
		{"blob by a truncated digest", "/v2/app/blobs/sha256:abc123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &blobUpstream{status: http.StatusOK, contentLength: "1"}
			h, _ := newCachingHandler(t, AllowAll(), up, time.Hour)

			head(h, testUpstreamHost, tc.path)
			head(h, testUpstreamHost, tc.path)
			if got := up.count(http.MethodHead, tc.path); got != 2 {
				t.Errorf("%s was cached (%d upstream probes, want 2)", tc.name, got)
			}
		})
	}
}

// TestBlobExistenceCacheBypassedByConditionalRequests checks the requests that
// are handed to the registry untouched, because their answer depends on more than
// whether the blob is there.
func TestBlobExistenceCacheBypassedByConditionalRequests(t *testing.T) {
	for _, header := range []struct{ key, value string }{
		{"Range", "bytes=0-1023"},
		{"If-Match", `"etag"`},
		{"If-None-Match", `"etag"`},
		{"If-Modified-Since", "Wed, 21 Oct 2015 07:28:00 GMT"},
		{"If-Unmodified-Since", "Wed, 21 Oct 2015 07:28:00 GMT"},
		{"If-Range", `"etag"`},
	} {
		t.Run(header.key, func(t *testing.T) {
			up := &blobUpstream{status: http.StatusOK, contentLength: "1"}
			h, _ := newCachingHandler(t, allowHostPolicy(t, testUpstreamHost, "blob:read"), up, time.Hour)

			// Warm the cache with a plain probe, then send a conditional one: it must
			// still reach the registry.
			head(h, testUpstreamHost, testBlobPath)
			head(h, testUpstreamHost, testBlobPath, header.key, header.value)
			if got := up.count(http.MethodHead, testBlobPath); got != 2 {
				t.Errorf("a request with %s was answered from the cache (%d upstream probes, want 2)", header.key, got)
			}
		})
	}
}

// TestBlobExistenceCacheConditionalAnswerNotStored is the other half: a
// conditional request's answer must not become the cache's idea of the blob.
func TestBlobExistenceCacheConditionalAnswerNotStored(t *testing.T) {
	up := &blobUpstream{status: http.StatusOK, contentLength: "1"}
	h, _ := newCachingHandler(t, allowHostPolicy(t, testUpstreamHost, "blob:read"), up, time.Hour)

	head(h, testUpstreamHost, testBlobPath, "Range", "bytes=0-1023")
	if entries := h.blobCache.stats().entries; entries != 0 {
		t.Fatalf("cache holds %d entries after a ranged probe, want 0", entries)
	}
}

// TestBlobExistenceCacheStillAsksThePolicy is the security property: the cache
// memoizes the registry's answer, never the authorization to ask for it.
func TestBlobExistenceCacheStillAsksThePolicy(t *testing.T) {
	up := &blobUpstream{status: http.StatusOK, contentLength: "1"}
	h, _ := newCachingHandler(t, allowHostPolicy(t, testUpstreamHost, "blob:read"), up, time.Hour)

	if got := head(h, testUpstreamHost, testBlobPath).StatusCode; got != http.StatusOK {
		t.Fatalf("warming probe status = %d, want 200", got)
	}
	// A reload that revokes access must take effect on the very next request, even
	// though the answer to it is sitting in the cache.
	h.policy.Store(&CompiledPolicy{})
	if got := head(h, testUpstreamHost, testBlobPath).StatusCode; got != http.StatusForbidden {
		t.Errorf("status after the policy stopped allowing the repository = %d, want 403", got)
	}
	if got := up.count(http.MethodHead, testBlobPath); got != 1 {
		t.Errorf("upstream saw %d probes, want the denied one to have gone nowhere", got)
	}
}

func TestBlobExistenceCacheExpiresThroughHandler(t *testing.T) {
	const ttl = 6 * time.Hour
	up := &blobUpstream{status: http.StatusOK, contentLength: "1"}
	h, clock := newCachingHandler(t, allowHostPolicy(t, testUpstreamHost, "blob:read"), up, ttl)

	head(h, testUpstreamHost, testBlobPath)
	clock.advance(ttl - time.Second)
	head(h, testUpstreamHost, testBlobPath)
	if got := up.count(http.MethodHead, testBlobPath); got != 1 {
		t.Fatalf("upstream saw %d probes before the TTL was up, want 1", got)
	}
	clock.advance(2 * time.Second)
	if got := head(h, testUpstreamHost, testBlobPath).StatusCode; got != http.StatusOK {
		t.Fatalf("status after expiry = %d, want 200", got)
	}
	if got := up.count(http.MethodHead, testBlobPath); got != 2 {
		t.Errorf("upstream saw %d probes after the TTL passed, want 2", got)
	}
}

// TestBlobExistenceCacheOffByDefault: the library default is no cache, so a
// gateway only starts answering from memory when an operator asks it to.
func TestBlobExistenceCacheOffByDefault(t *testing.T) {
	up := &blobUpstream{status: http.StatusOK, contentLength: "1"}
	h := newTestHandler(allowHostPolicy(t, testUpstreamHost, "blob:read"), up)
	if h.blobCache != nil {
		t.Fatal("a handler built without WithBlobExistenceCache has a cache")
	}
	head(h, testUpstreamHost, testBlobPath)
	head(h, testUpstreamHost, testBlobPath)
	if got := up.count(http.MethodHead, testBlobPath); got != 2 {
		t.Errorf("upstream saw %d probes with the cache off, want 2", got)
	}
}

func TestBlobExistenceCacheMetrics(t *testing.T) {
	up := upstreamFunc(func(r *http.Request) (*http.Response, error) {
		header := http.Header{}
		header.Set("Content-Length", "1")
		return upstreamResponse(http.StatusOK, header, ""), nil
	})
	h, collect := newMetricsHandler(t, allowHostPolicy(t, testUpstreamHost, "blob:read"), up,
		WithBlobExistenceCache(time.Hour, 256*entryCost))

	// One miss that goes upstream, then two answered from the cache.
	for range 3 {
		if got := head(h, testUpstreamHost, testBlobPath).StatusCode; got != http.StatusOK {
			t.Fatalf("probe status = %d, want 200", got)
		}
	}
	rm := collect()

	registry := attrRegistry.String(testUpstreamHost)
	if got := counterValue(t, rm, "oci.gateway.blob_existence_cache.lookups", registry, attrResult.String(resultHit)); got != 2 {
		t.Errorf("cache hits = %d, want 2", got)
	}
	if got := counterValue(t, rm, "oci.gateway.blob_existence_cache.lookups", registry, attrResult.String(resultMiss)); got != 1 {
		t.Errorf("cache misses = %d, want 1", got)
	}
	// A cached answer still counts as an existence check that found the blob, so
	// the hit rate an operator reads is not a function of the cache.
	if got := counterValue(t, rm, "oci.gateway.existence_checks", registry, attrResult.String(resultHit)); got != 3 {
		t.Errorf("existence checks reported as hits = %d, want 3", got)
	}
	// Only the forwarded probe has an upstream leg.
	if got := histogramCount(t, rm, "oci.gateway.upstream.duration", registry); got != 1 {
		t.Errorf("upstream durations recorded = %d, want 1 (the cached probes have no upstream leg)", got)
	}
	if got := gaugeValue(t, rm, "oci.gateway.blob_existence_cache.entries"); got != 1 {
		t.Errorf("cache entries = %d, want 1", got)
	}
	if got := gaugeValue(t, rm, "oci.gateway.blob_existence_cache.capacity"); got != 256 {
		t.Errorf("cache capacity = %d, want 256", got)
	}
	if got := counterValue(t, rm, "oci.gateway.blob_existence_cache.evictions",
		attrEvictionReason.String(evictedForCapacity)); got != 0 {
		t.Errorf("capacity evictions = %d, want 0", got)
	}
}

// TestBlobExistenceCacheInstrumentsAbsentWhenDisabled keeps a disabled cache from
// exporting a flat zero that looks like a cache nobody is hitting.
func TestBlobExistenceCacheInstrumentsAbsentWhenDisabled(t *testing.T) {
	h, collect := newMetricsHandler(t, allowHostPolicy(t, testUpstreamHost, "blob:read"),
		func(r *http.Request) (*http.Response, error) {
			return upstreamResponse(http.StatusOK, nil, ""), nil
		})
	head(h, testUpstreamHost, testBlobPath)
	rm := collect()
	for _, name := range []string{
		"oci.gateway.blob_existence_cache.lookups",
		"oci.gateway.blob_existence_cache.entries",
		"oci.gateway.blob_existence_cache.capacity",
		"oci.gateway.blob_existence_cache.evictions",
	} {
		if hasMetric(rm, name) {
			t.Errorf("%s was reported by a gateway with the cache disabled", name)
		}
	}
}

// TestBlobExistenceCacheReplayIsWellFormedOnTheWire serves both the forwarded and
// the cached answer through a real net/http server, because a HEAD response is the
// one place where declaring a Content-Length and writing no body is correct — and
// getting it wrong would not show up in a ResponseRecorder. If net/http thought
// the handler had under-written the body it would close the connection after every
// reply, so the reused connection is the assertion that matters.
func TestBlobExistenceCacheReplayIsWellFormedOnTheWire(t *testing.T) {
	up := &blobUpstream{status: http.StatusOK, contentLength: "12345"}
	h, _ := newCachingHandler(t, allowHostPolicy(t, testUpstreamHost, "blob:read"), up, time.Hour)

	protocols := &http.Protocols{}
	protocols.SetHTTP1(true)
	transport, listener := serveInMemory(t, h, protocols, protocols, nil, nil)

	probe := func(t *testing.T) {
		t.Helper()
		r, err := http.NewRequest(http.MethodHead, "http://peer.test:8443"+testBlobPath, nil)
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		r.Header.Set(clientgateway.OriginalHostHeader, testUpstreamHost)
		resp, err := transport.RoundTrip(r)
		if err != nil {
			t.Fatalf("probing: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		// net/http fills ContentLength on a HEAD response from the header, so this
		// is what a go-containerregistry client reads to size the layer.
		if resp.ContentLength != 12345 {
			t.Errorf("ContentLength = %d, want 12345", resp.ContentLength)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading body: %v", err)
		}
		if len(body) != 0 {
			t.Errorf("HEAD response carried %d bytes of body", len(body))
		}
	}

	t.Run("forwarded", func(t *testing.T) { probe(t) })
	t.Run("cached", func(t *testing.T) { probe(t) })

	if got := up.count(http.MethodHead, testBlobPath); got != 1 {
		t.Errorf("upstream saw %d probes, want the second answered from the cache", got)
	}
	if dials := listener.dials(); dials != 1 {
		t.Errorf("the client opened %d connections, want 1: a response net/http considers short is not kept alive", dials)
	}
}

// gaugeValue sums the int64 gauge data points carrying all of want.
func gaugeValue(t *testing.T, rm *metricdata.ResourceMetrics, name string, want ...attribute.KeyValue) int64 {
	t.Helper()
	gauge, ok := findMetric(t, rm, name).(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("metric %q is not an int64 gauge", name)
	}
	var total int64
	for _, dp := range gauge.DataPoints {
		if matches(dp.Attributes, want) {
			total += dp.Value
		}
	}
	return total
}
