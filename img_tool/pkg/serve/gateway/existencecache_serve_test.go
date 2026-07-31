package gateway

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"syscall"
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

// The read side: handing over a blob is a stronger statement than answering a probe
// about it, as long as the registry says which blob it handed over.

// readUpstream serves a blob read with a scripted status, digest header and body, and
// counts what reaches it.
type readUpstream struct {
	status int
	// digest is sent as Docker-Content-Digest, when not empty.
	digest string
	body   string
	reads  int
	heads  int
}

func (u *readUpstream) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
		return upstreamResponse(http.StatusOK, nil, ""), nil
	}
	header := http.Header{}
	if u.digest != "" {
		header.Set("Docker-Content-Digest", u.digest)
	}
	switch r.Method {
	case http.MethodHead:
		u.heads++
		// A length that is not the read's, so a cached probe says which of the two
		// answers it came from.
		header.Set("Content-Length", "12345")
		return upstreamResponse(http.StatusOK, header, ""), nil
	default:
		u.reads++
		header.Set("Content-Length", strconv.Itoa(len(u.body)))
		return upstreamResponse(u.status, header, u.body), nil
	}
}

// TestBlobExistenceCacheAdmitsServedBlobs: a fleet that pulls a layer today is one
// that will ask whether it needs to push it tomorrow, and the pull already answered
// that question.
func TestBlobExistenceCacheAdmitsServedBlobs(t *testing.T) {
	up := &readUpstream{status: http.StatusOK, digest: testCacheDigest, body: "layer bytes"}
	h, _ := newCachingHandler(t, allowHostPolicy(t, testUpstreamHost, "blob:read"), up, time.Hour)

	if got := serve(h, http.MethodGet, testUpstreamHost, testBlobPath, "").Code; got != http.StatusOK {
		t.Fatalf("read status = %d, want 200", got)
	}
	probe := head(h, testUpstreamHost, testBlobPath)
	if probe.StatusCode != http.StatusOK {
		t.Errorf("probe after the read = %d, want 200", probe.StatusCode)
	}
	if up.heads != 0 {
		t.Errorf("upstream saw %d probes, want the read to have answered them", up.heads)
	}
	// The blob's size is the one the read carried, not the registry's word for it.
	if got := probe.Header.Get("Content-Length"); got != "11" {
		t.Errorf("cached Content-Length = %q, want %q", got, "11")
	}
}

// TestBlobExistenceCacheNeedsTheDigestOnARead is where a read is held to a stricter
// standard than a commit. The gateway streams a blob body through without hashing
// it, and a read can be redirected to storage that serves whatever it is pointed at,
// so only the registry naming what it served makes the answer evidence.
func TestBlobExistenceCacheNeedsTheDigestOnARead(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		digest   string
		cached   bool
		wantHead int
	}{
		{"digest confirms the blob", http.StatusOK, testCacheDigest, true, 0},
		{"no digest header", http.StatusOK, "", false, 1},
		{"digest names another blob", http.StatusOK, "sha256:" + strings.Repeat("a", 64), false, 1},
		// A range request is answered with a slice of the blob, and its
		// Content-Length is the slice's.
		{"partial content", http.StatusPartialContent, testCacheDigest, false, 1},
		{"not found", http.StatusNotFound, "", false, 1},
		{"upstream error", http.StatusInternalServerError, testCacheDigest, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &readUpstream{status: tc.status, digest: tc.digest, body: "layer bytes"}
			h, _ := newCachingHandler(t, allowHostPolicy(t, testUpstreamHost, "blob:read"), up, time.Hour)

			serve(h, http.MethodGet, testUpstreamHost, testBlobPath, "")
			entries := h.blobCache.stats().entries
			if want := int64(0); !tc.cached && entries != want {
				t.Errorf("cache holds %d entries after the read, want %d", entries, want)
			}
			if tc.cached && entries != 1 {
				t.Errorf("cache holds %d entries after the read, want 1", entries)
			}
			head(h, testUpstreamHost, testBlobPath)
			if up.heads != tc.wantHead {
				t.Errorf("upstream saw %d probes, want %d", up.heads, tc.wantHead)
			}
		})
	}
}

// TestBlobExistenceCacheIgnoresReadsOfMutableReferences: a read is only evidence
// about content that is named by its digest.
func TestBlobExistenceCacheIgnoresReadsOfMutableReferences(t *testing.T) {
	for _, path := range []string{
		"/v2/app/blobs/latest",
		"/v2/app/manifests/latest",
		"/v2/app/manifests/" + testCacheDigest,
	} {
		up := &readUpstream{status: http.StatusOK, digest: testCacheDigest, body: "bytes"}
		h, _ := newCachingHandler(t, AllowAll(), up, time.Hour)

		serve(h, http.MethodGet, testUpstreamHost, path, "")
		if entries := h.blobCache.stats().entries; entries != 0 {
			t.Errorf("reading %q left %d entries in the cache, want 0", path, entries)
		}
	}
}

// The push side of the cache. A client that finished uploading a layer has told the
// gateway the answer to every probe for that layer which follows, so the upload fills
// the cache exactly as a probe of its own would have.

// uploadPath is the session an upload commit closes. Its reference is opaque to
// the gateway, which learns which blob was committed from the query.
const uploadPath = "/v2/app/blobs/uploads/uuid-123"

// pushPolicy allows both blob operations on the test upstream: a push needs the
// write, and a cross-repo mount needs read on the repository it mounts from.
func pushPolicy(t *testing.T) *CompiledPolicy {
	t.Helper()
	return allowHostPolicy(t, testUpstreamHost, "blob:read", "blob:write")
}

// TestBlobExistenceCacheAdmitsCommittedUploads covers the three requests that
// finish an upload: the commit of a streamed session, a whole blob in one POST, and
// a cross-repo mount.
func TestBlobExistenceCacheAdmitsCommittedUploads(t *testing.T) {
	for _, tc := range []struct{ name, method, target, body string }{
		{"session commit", http.MethodPut, uploadPath + "?digest=" + testCacheDigest, ""},
		{"monolithic upload", http.MethodPost, "/v2/app/blobs/uploads/?digest=" + testCacheDigest, "layer bytes"},
		{"cross-repo mount", http.MethodPost, "/v2/app/blobs/uploads/?mount=" + testCacheDigest + "&from=other/app", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &blobUpstream{status: http.StatusCreated}
			h, _ := newCachingHandler(t, pushPolicy(t), up, time.Hour)

			if got := serve(h, tc.method, testUpstreamHost, tc.target, tc.body).Code; got != http.StatusCreated {
				t.Fatalf("upload status = %d, want 201", got)
			}
			probe := head(h, testUpstreamHost, testBlobPath)
			if probe.StatusCode != http.StatusOK {
				t.Errorf("probe after the upload = %d, want 200", probe.StatusCode)
			}
			if got := up.count(http.MethodHead, testBlobPath); got != 0 {
				t.Errorf("upstream saw %d probes, want the push to have answered them", got)
			}
			if got := probe.Header.Get("Docker-Content-Digest"); got != testCacheDigest {
				t.Errorf("replayed Docker-Content-Digest = %q, want %q", got, testCacheDigest)
			}
		})
	}
}

// TestBlobExistenceCacheIgnoresUnfinishedUploads is the "not too early" property,
// and the one that decides whether this feature is safe at all. Until the commit
// succeeds there is no blob, and a client told otherwise would skip an upload it
// still owes and then commit a manifest referring to nothing.
func TestBlobExistenceCacheIgnoresUnfinishedUploads(t *testing.T) {
	for _, tc := range []struct {
		name, method, target, body string
		status                     int
	}{
		{"session opened", http.MethodPost, "/v2/app/blobs/uploads/", "", http.StatusAccepted},
		{"session opened for a named blob", http.MethodPost, "/v2/app/blobs/uploads/?digest=" + testCacheDigest, "", http.StatusAccepted},
		{"chunk accepted", http.MethodPatch, uploadPath, "layer bytes", http.StatusAccepted},
		// A registry that will not mount opens a session instead, and answers 202.
		{"mount declined", http.MethodPost, "/v2/app/blobs/uploads/?mount=" + testCacheDigest + "&from=other/app", "", http.StatusAccepted},
		{"commit accepted but not created", http.MethodPut, uploadPath + "?digest=" + testCacheDigest, "", http.StatusAccepted},
		{"commit rejected", http.MethodPut, uploadPath + "?digest=" + testCacheDigest, "", http.StatusBadRequest},
		{"commit failed upstream", http.MethodPut, uploadPath + "?digest=" + testCacheDigest, "", http.StatusInternalServerError},
		{"commit unauthorized", http.MethodPut, uploadPath + "?digest=" + testCacheDigest, "", http.StatusUnauthorized},
		{"session cancelled", http.MethodDelete, uploadPath, "", http.StatusNoContent},
		// A manifest is created with a 201 too, and is the one thing the cache must
		// never hold, whatever its reference.
		{"manifest created", http.MethodPut, "/v2/app/manifests/" + testCacheDigest, `{"schemaVersion":2}`, http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &blobUpstream{status: tc.status}
			h, _ := newCachingHandler(t, allowHostPolicy(t, testUpstreamHost,
				"blob:read", "blob:write", "manifest:write"), up, time.Hour)

			serve(h, tc.method, testUpstreamHost, tc.target, tc.body)
			if entries := h.blobCache.stats().entries; entries != 0 {
				t.Errorf("cache holds %d entries after %s -> %d, want 0", entries, tc.name, tc.status)
			}
			if got := head(h, testUpstreamHost, testBlobPath); got.StatusCode != tc.status {
				t.Errorf("probe status = %d, want the upstream's %d: it must not have been answered from the cache", got.StatusCode, tc.status)
			}
			if got := up.count(http.MethodHead, testBlobPath); got != 1 {
				t.Errorf("upstream saw %d probes, want 1", got)
			}
		})
	}
}

// TestBlobExistenceCacheMountTeachesNothingAboutTheSource keeps a mount to what it
// proves. The blob is now in the repository that asked for it; whether the registry
// really found it in the repository named by from — rather than in storage it
// shares between repositories — is the registry's business, not something the
// gateway may answer for.
func TestBlobExistenceCacheMountTeachesNothingAboutTheSource(t *testing.T) {
	const sourcePath = "/v2/other/app/blobs/" + testCacheDigest
	up := &blobUpstream{status: http.StatusCreated}
	h, _ := newCachingHandler(t, pushPolicy(t), up, time.Hour)

	serve(h, http.MethodPost, testUpstreamHost, "/v2/app/blobs/uploads/?mount="+testCacheDigest+"&from=other/app", "")

	head(h, testUpstreamHost, sourcePath)
	if got := up.count(http.MethodHead, sourcePath); got != 1 {
		t.Errorf("the mount source was answered from the cache (%d upstream probes, want 1)", got)
	}
}

// TestBlobExistenceCacheKeepsTheRegistrysDigest refuses the one case where the
// gateway's reading of an upload and the registry's disagree. Docker-Content-Digest
// is the registry's own account of what it created; when it names another blob,
// remembering either would be a guess.
func TestBlobExistenceCacheKeepsTheRegistrysDigest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reported string
		want     int64
	}{
		{"registry confirms the digest", testCacheDigest, 1},
		{"registry reports nothing", "", 1},
		{"registry names another blob", "sha256:" + strings.Repeat("a", 64), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := upstreamFunc(func(r *http.Request) (*http.Response, error) {
				header := http.Header{}
				if tc.reported != "" {
					header.Set("Docker-Content-Digest", tc.reported)
				}
				return upstreamResponse(http.StatusCreated, header, ""), nil
			})
			h, _ := newCachingHandler(t, pushPolicy(t), up, time.Hour)

			serve(h, http.MethodPut, testUpstreamHost, uploadPath+"?digest="+testCacheDigest, "")
			if got := h.blobCache.stats().entries; got != tc.want {
				t.Errorf("cache holds %d entries, want %d", got, tc.want)
			}
		})
	}
}

// TestBlobExistenceCacheLengthOfAPushedBlob covers what a probe answered from a
// pushed entry reports as the blob's size. One request carries the whole blob and so
// proves its size; a session commit and a mount carry no content, and a probe then
// omits Content-Length rather than inventing one.
func TestBlobExistenceCacheLengthOfAPushedBlob(t *testing.T) {
	for _, tc := range []struct{ name, method, target, body, wantLength string }{
		{"monolithic upload is the whole blob", http.MethodPost, "/v2/app/blobs/uploads/?digest=" + testCacheDigest, "layer bytes", "11"},
		{"a session commit carries none of it", http.MethodPut, uploadPath + "?digest=" + testCacheDigest, "", ""},
		{"a mount transfers nothing", http.MethodPost, "/v2/app/blobs/uploads/?mount=" + testCacheDigest + "&from=other/app", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A registry declares Content-Length: 0 on the 201, for its own empty
			// body. That is the number a cached probe must never report as the size of
			// the blob.
			up := &blobUpstream{status: http.StatusCreated, contentLength: "0"}
			h, _ := newCachingHandler(t, pushPolicy(t), up, time.Hour)

			serve(h, tc.method, testUpstreamHost, tc.target, tc.body)
			probe := head(h, testUpstreamHost, testBlobPath)
			if probe.StatusCode != http.StatusOK {
				t.Fatalf("probe after the upload = %d, want 200 from the cache", probe.StatusCode)
			}
			if got := probe.Header.Get("Content-Length"); got != tc.wantLength {
				t.Errorf("cached Content-Length = %q, want %q", got, tc.wantLength)
			}
		})
	}
}

// TestBlobExistenceCachePushedEntryExpires: an entry a push admitted lives under
// the same TTL as one a probe admitted. It is the same failure it guards against —
// a garbage-collected blob — and the push is no better evidence hours later.
func TestBlobExistenceCachePushedEntryExpires(t *testing.T) {
	const ttl = 6 * time.Hour
	up := &blobUpstream{status: http.StatusCreated}
	h, clock := newCachingHandler(t, pushPolicy(t), up, ttl)

	serve(h, http.MethodPut, testUpstreamHost, uploadPath+"?digest="+testCacheDigest, "")
	clock.advance(ttl - time.Second)
	head(h, testUpstreamHost, testBlobPath)
	if got := up.count(http.MethodHead, testBlobPath); got != 0 {
		t.Fatalf("upstream saw %d probes before the TTL was up, want 0", got)
	}
	clock.advance(2 * time.Second)
	head(h, testUpstreamHost, testBlobPath)
	if got := up.count(http.MethodHead, testBlobPath); got != 1 {
		t.Errorf("upstream saw %d probes after the TTL passed, want 1", got)
	}
}

// TestBlobExistenceCachePushedEntryStillAsksThePolicy: an entry admitted by a push
// is subject to the same rule as any other, so a reload that revokes access takes
// effect on the next probe even though the answer to it is sitting in memory.
func TestBlobExistenceCachePushedEntryStillAsksThePolicy(t *testing.T) {
	up := &blobUpstream{status: http.StatusCreated}
	h, _ := newCachingHandler(t, pushPolicy(t), up, time.Hour)

	if got := serve(h, http.MethodPut, testUpstreamHost, uploadPath+"?digest="+testCacheDigest, "").Code; got != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201", got)
	}
	h.policy.Store(&CompiledPolicy{})
	if got := head(h, testUpstreamHost, testBlobPath).StatusCode; got != http.StatusForbidden {
		t.Errorf("probe status after the policy stopped allowing the repository = %d, want 403", got)
	}
}

// The other direction: a blob a client deletes through the gateway is a blob the
// cache must stop claiming is there, without waiting out its TTL.

// deleteUpstream answers an existence check with 200 and a delete with a scripted
// outcome, so a test can tell a probe that was forwarded from one that was replayed.
type deleteUpstream struct {
	status int
	// err, when set, is returned instead of a response: a delete whose answer never
	// arrives.
	err     error
	heads   int
	deletes int
}

func (u *deleteUpstream) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
		return upstreamResponse(http.StatusOK, nil, ""), nil
	}
	switch r.Method {
	case http.MethodHead:
		u.heads++
		header := http.Header{}
		header.Set("Content-Length", "12345")
		return upstreamResponse(http.StatusOK, header, ""), nil
	case http.MethodDelete:
		u.deletes++
		if u.err != nil {
			return nil, u.err
		}
		return upstreamResponse(u.status, nil, ""), nil
	default:
		return upstreamResponse(http.StatusMethodNotAllowed, nil, ""), nil
	}
}

// deletePolicy allows the probe and the delete, which is a blob write.
func deletePolicy(t *testing.T) *CompiledPolicy {
	t.Helper()
	return allowHostPolicy(t, testUpstreamHost, "blob:read", "blob:write")
}

// TestBlobExistenceCacheForgetsDeletedBlobs is the cache's own correction. A blob
// the registry garbage-collects disappears unseen, which is what the TTL is for; a
// delete travels through the gateway, so that entry need not outlive it.
//
// The upstream's answer is deliberately not consulted, and the table says why: only
// a refusal leaves the blob certainly in place, while a 5xx or a 404 (which is also
// how a registry with deletes disabled answers) leaves the gateway unable to tell.
// Dropping the entry costs one probe; keeping a false one costs a client the layer it
// decided not to re-upload.
func TestBlobExistenceCacheForgetsDeletedBlobs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"accepted", http.StatusAccepted},
		{"ok", http.StatusOK},
		{"no content", http.StatusNoContent},
		{"not found", http.StatusNotFound},
		{"upstream error", http.StatusInternalServerError},
		{"refused", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &deleteUpstream{status: tc.status}
			h, _ := newCachingHandler(t, deletePolicy(t), up, time.Hour)

			// Warm the cache, and check it is warm.
			head(h, testUpstreamHost, testBlobPath)
			head(h, testUpstreamHost, testBlobPath)
			if up.heads != 1 {
				t.Fatalf("upstream saw %d probes while warming, want 1", up.heads)
			}

			serve(h, http.MethodDelete, testUpstreamHost, testBlobPath, "")
			if entries := h.blobCache.stats().entries; entries != 0 {
				t.Errorf("cache holds %d entries after a delete answered %d, want 0", entries, tc.status)
			}
			if got := head(h, testUpstreamHost, testBlobPath).StatusCode; got != http.StatusOK {
				t.Errorf("probe after the delete = %d, want 200", got)
			}
			if up.heads != 2 {
				t.Errorf("upstream saw %d probes, want the one after the delete to have been forwarded", up.heads)
			}
		})
	}
}

// TestBlobExistenceCacheForgetsADeleteWithNoAnswer covers the delete whose outcome
// the gateway never learns. The registry may have carried it out regardless, so the
// entry goes.
func TestBlobExistenceCacheForgetsADeleteWithNoAnswer(t *testing.T) {
	up := &deleteUpstream{err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}}
	h, _ := newCachingHandler(t, deletePolicy(t), up, time.Hour)

	head(h, testUpstreamHost, testBlobPath)
	if got := serve(h, http.MethodDelete, testUpstreamHost, testBlobPath, "").Code; got != http.StatusBadGateway {
		t.Fatalf("delete status = %d, want 502", got)
	}
	if entries := h.blobCache.stats().entries; entries != 0 {
		t.Errorf("cache holds %d entries after a delete that got no answer, want 0", entries)
	}
}

// TestBlobExistenceCacheForgetsOnlyTheDeletedBlob: an invalidation is not a flush.
func TestBlobExistenceCacheForgetsOnlyTheDeletedBlob(t *testing.T) {
	const otherDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	up := &deleteUpstream{status: http.StatusAccepted}
	h, _ := newCachingHandler(t, AllowAll(), up, time.Hour)

	// Three entries: the blob about to be deleted, the same digest in another
	// repository, and another digest in the same one.
	head(h, testUpstreamHost, testBlobPath)
	head(h, testUpstreamHost, "/v2/other/blobs/"+testCacheDigest)
	head(h, testUpstreamHost, "/v2/app/blobs/"+otherDigest)
	if entries := h.blobCache.stats().entries; entries != 3 {
		t.Fatalf("cache holds %d entries after warming, want 3", entries)
	}

	serve(h, http.MethodDelete, testUpstreamHost, testBlobPath, "")

	if entries := h.blobCache.stats().entries; entries != 2 {
		t.Errorf("cache holds %d entries after one delete, want 2", entries)
	}
	before := up.heads
	head(h, testUpstreamHost, "/v2/other/blobs/"+testCacheDigest)
	head(h, testUpstreamHost, "/v2/app/blobs/"+otherDigest)
	if up.heads != before {
		t.Errorf("upstream saw %d further probes, want the other two blobs still answered from the cache", up.heads-before)
	}
}

// TestBlobExistenceCacheKeptByRequestsThatDeleteNoBlob covers the requests that look
// like a blob delete without being one. Dropping an entry is never a wrong answer,
// only a wasted probe, but a session cancellation happens on every abandoned upload
// and must not evict the layer the fleet is asking about.
func TestBlobExistenceCacheKeptByRequestsThatDeleteNoBlob(t *testing.T) {
	for _, tc := range []struct{ name, method, target string }{
		{"upload session cancelled", http.MethodDelete, "/v2/app/blobs/uploads/uuid-123"},
		{"manifest deleted", http.MethodDelete, "/v2/app/manifests/" + testCacheDigest},
		{"blob deleted by a reference that is not a digest", http.MethodDelete, "/v2/app/blobs/latest"},
		{"blob read", http.MethodGet, testBlobPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &deleteUpstream{status: http.StatusAccepted}
			h, _ := newCachingHandler(t, AllowAll(), up, time.Hour)

			head(h, testUpstreamHost, testBlobPath)
			serve(h, tc.method, testUpstreamHost, tc.target, "")
			if entries := h.blobCache.stats().entries; entries != 1 {
				t.Errorf("cache holds %d entries after %s, want the blob's entry kept", entries, tc.name)
			}
			if got := head(h, testUpstreamHost, testBlobPath).StatusCode; got != http.StatusOK {
				t.Fatalf("probe status = %d, want 200", got)
			}
			if up.heads != 1 {
				t.Errorf("upstream saw %d probes, want the second answered from the cache", up.heads)
			}
		})
	}
}

// TestBlobExistenceCacheKeptByADeniedDelete is the security half: a client the policy
// turns away has not reached the registry, so it has learned nothing and changed
// nothing — including in the cache.
func TestBlobExistenceCacheKeptByADeniedDelete(t *testing.T) {
	up := &deleteUpstream{status: http.StatusAccepted}
	h, _ := newCachingHandler(t, allowHostPolicy(t, testUpstreamHost, "blob:read"), up, time.Hour)

	head(h, testUpstreamHost, testBlobPath)
	if got := serve(h, http.MethodDelete, testUpstreamHost, testBlobPath, "").Code; got != http.StatusForbidden {
		t.Fatalf("delete status = %d, want 403", got)
	}
	if up.deletes != 0 {
		t.Fatalf("a denied delete reached the upstream %d times", up.deletes)
	}
	if entries := h.blobCache.stats().entries; entries != 1 {
		t.Errorf("cache holds %d entries after a denied delete, want the entry kept", entries)
	}
}

// TestBlobExistenceCacheDeleteEvictionMetric: an entry dropped by a delete is
// reported under its own reason, so an operator can see invalidations happening
// rather than reading them as capacity pressure.
func TestBlobExistenceCacheDeleteEvictionMetric(t *testing.T) {
	up := upstreamFunc(func(r *http.Request) (*http.Response, error) {
		header := http.Header{}
		header.Set("Content-Length", "1")
		return upstreamResponse(http.StatusOK, header, ""), nil
	})
	h, collect := newMetricsHandler(t, deletePolicy(t), up, WithBlobExistenceCache(time.Hour, 256*entryCost))

	head(h, testUpstreamHost, testBlobPath)
	serve(h, http.MethodDelete, testUpstreamHost, testBlobPath, "")
	// A second delete for a blob the cache no longer holds is not counted twice.
	serve(h, http.MethodDelete, testUpstreamHost, testBlobPath, "")
	rm := collect()

	if got := counterValue(t, rm, "oci.gateway.blob_existence_cache.evictions",
		attrEvictionReason.String(evictedForDelete)); got != 1 {
		t.Errorf("delete evictions = %d, want 1", got)
	}
	for _, reason := range []string{evictedForCapacity, evictedForExpiry} {
		if got := counterValue(t, rm, "oci.gateway.blob_existence_cache.evictions",
			attrEvictionReason.String(reason)); got != 0 {
			t.Errorf("%s evictions = %d, want 0", reason, got)
		}
	}
	if got := gaugeValue(t, rm, "oci.gateway.blob_existence_cache.entries"); got != 0 {
		t.Errorf("cache entries = %d, want 0", got)
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
