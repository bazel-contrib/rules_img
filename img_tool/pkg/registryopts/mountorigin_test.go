package registryopts

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// recordingTransport records the URL of every request it serves and answers 202
// Accepted, the way a registry opening an upload session does.
type recordingTransport struct {
	urls []*url.URL
}

func (t *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := *req.URL
	t.urls = append(t.urls, &clone)
	recorder := httptest.NewRecorder()
	recorder.WriteHeader(http.StatusAccepted)
	resp := recorder.Result()
	resp.Request = req
	return resp, nil
}

// roundTripURL sends one request through rt and returns the URL that reached the
// recording transport underneath it.
func roundTripURL(t *testing.T, rt http.RoundTripper, method, rawURL string) *url.URL {
	t.Helper()
	inner, ok := rt.(*mountOriginTransport).inner.(*recordingTransport)
	if !ok {
		t.Fatalf("transport %T does not wrap a recordingTransport", rt)
	}
	req, err := http.NewRequest(method, rawURL, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	before := req.URL.String()
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()
	// A RoundTripper may not modify the request it is handed: go-cr hands the same
	// one back when it retries.
	if got := req.URL.String(); got != before {
		t.Errorf("RoundTrip mutated the request URL: %q, want the untouched %q", got, before)
	}
	if len(inner.urls) == 0 {
		t.Fatal("nothing reached the inner transport")
	}
	return inner.urls[len(inner.urls)-1]
}

// TestWrapMountOriginDropsTheOrigin is what the wrapper exists for: the mount keeps
// naming its source repository, and the origin parameter go-containerregistry adds
// alongside it is gone.
func TestWrapMountOriginDropsTheOrigin(t *testing.T) {
	rt := WrapMountOrigin(&recordingTransport{})
	sent := roundTripURL(t, rt, http.MethodPost,
		"https://reg.example.com/v2/team/service/blobs/uploads/?from=team%2Fblobs&mount=sha256%3Adeadbeef&origin=reg.example.com")

	query := sent.Query()
	if _, found := query["origin"]; found {
		t.Errorf("mount kept origin=%q, want it dropped", query.Get("origin"))
	}
	// Dropping "mount" or "from" as well would turn the request into an ordinary
	// upload, which is the very thing a mount avoids.
	if got := query.Get("mount"); got != "sha256:deadbeef" {
		t.Errorf("mount = %q, want it kept", got)
	}
	if got := query.Get("from"); got != "team/blobs" {
		t.Errorf("from = %q, want it kept", got)
	}
	if got := sent.Path; got != "/v2/team/service/blobs/uploads/" {
		t.Errorf("path = %q, want it unchanged", got)
	}
}

// TestWrapMountOriginDropsAnOriginNamingAnotherRegistry covers the deliberate part
// of "every mount": a source on another registry loses its origin too, so the
// destination answers the mount the way a registry without cross-registry mounts
// does -- by asking for the bytes.
func TestWrapMountOriginDropsAnOriginNamingAnotherRegistry(t *testing.T) {
	rt := WrapMountOrigin(&recordingTransport{})
	sent := roundTripURL(t, rt, http.MethodPost,
		"https://reg.example.com/v2/team/service/blobs/uploads/?from=other%2Fbase&mount=sha256%3Adeadbeef&origin=other.example.com")

	if got := sent.Query().Get("origin"); got != "" {
		t.Errorf("origin = %q, want it dropped as well", got)
	}
	if got := sent.Query().Get("from"); got != "other/base" {
		t.Errorf("from = %q, want it kept", got)
	}
}

func TestWrapMountOriginLeavesOtherRequestsAlone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		url    string
	}{
		{
			name:   "plain upload session",
			method: http.MethodPost,
			url:    "https://reg.example.com/v2/team/service/blobs/uploads/",
		},
		{
			// go-cr sends an origin only alongside a mount, so an origin without one
			// is not a mount request and there is nothing to fix up.
			name:   "origin without a mount",
			method: http.MethodPost,
			url:    "https://reg.example.com/v2/team/service/blobs/uploads/?origin=reg.example.com",
		},
		{
			name:   "upload session continuation",
			method: http.MethodPatch,
			url:    "https://reg.example.com/v2/team/service/blobs/uploads/upload-1?origin=reg.example.com",
		},
		{
			name:   "manifest push",
			method: http.MethodPut,
			url:    "https://reg.example.com/v2/team/service/manifests/latest",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := WrapMountOrigin(&recordingTransport{})
			want, err := url.Parse(tc.url)
			if err != nil {
				t.Fatalf("parsing %q: %v", tc.url, err)
			}
			if got := roundTripURL(t, rt, tc.method, tc.url); got.String() != want.String() {
				t.Errorf("request URL = %q, want the untouched %q", got, want)
			}
		})
	}
}
