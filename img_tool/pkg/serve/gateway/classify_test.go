package gateway

import (
	"net/http"
	"strings"
	"testing"
)

func req(method, path string) *http.Request {
	r, _ := http.NewRequest(method, "http://gw"+path, nil)
	return r
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name     string
		method   string
		path     string
		wantOK   bool
		wantRepo string
		wantReq  requirement
		wantWr   bool
	}{
		{"blob get", http.MethodGet, "/v2/library/ubuntu/blobs/sha256:abc", true, "library/ubuntu", reqBlobRead, false},
		{"blob head", http.MethodHead, "/v2/library/ubuntu/blobs/sha256:abc", true, "library/ubuntu", reqBlobReadOrWrite, false},
		{"blob delete", http.MethodDelete, "/v2/foo/blobs/sha256:abc", true, "foo", reqBlobWrite, true},
		{"upload post", http.MethodPost, "/v2/foo/blobs/uploads/", true, "foo", reqBlobWrite, true},
		{"upload patch", http.MethodPatch, "/v2/foo/blobs/uploads/uuid-123", true, "foo", reqBlobWrite, true},
		{"upload put", http.MethodPut, "/v2/foo/bar/blobs/uploads/uuid-123", true, "foo/bar", reqBlobWrite, true},
		{"manifest get", http.MethodGet, "/v2/foo/manifests/latest", true, "foo", reqManifestRead, false},
		{"manifest head", http.MethodHead, "/v2/foo/manifests/latest", true, "foo", reqManifestReadOrWrite, false},
		{"manifest put", http.MethodPut, "/v2/foo/manifests/latest", true, "foo", reqManifestWrite, true},
		{"manifest delete", http.MethodDelete, "/v2/foo/manifests/sha256:abc", true, "foo", reqManifestWrite, true},
		{"manifest digest ref", http.MethodGet, "/v2/foo/manifests/sha256:abcdef", true, "foo", reqManifestRead, false},
		{"tags list", http.MethodGet, "/v2/foo/tags/list", true, "foo", reqManifestRead, false},
		{"referrers", http.MethodGet, "/v2/foo/referrers/sha256:abc", true, "foo", reqManifestRead, false},
		{"nested repo", http.MethodGet, "/v2/a/b/c/blobs/sha256:abc", true, "a/b/c", reqBlobRead, false},
		{"unknown", http.MethodGet, "/v2/foo/other", false, "", reqUnknown, false},
		{"root", http.MethodGet, "/", false, "", reqUnknown, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := classify(req(tc.method, tc.path))
			if ok != tc.wantOK {
				t.Fatalf("classify ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got.repo != tc.wantRepo {
				t.Errorf("repo = %q, want %q", got.repo, tc.wantRepo)
			}
			if got.req != tc.wantReq {
				t.Errorf("req = %v, want %v", got.req, tc.wantReq)
			}
			if got.write != tc.wantWr {
				t.Errorf("write = %v, want %v", got.write, tc.wantWr)
			}
		})
	}
}

func TestClassifyMount(t *testing.T) {
	for _, tc := range []struct {
		name       string
		method     string
		path       string
		wantFrom   string
		wantBadReq bool
	}{
		{"plain upload post", http.MethodPost, "/v2/dest/blobs/uploads/", "", false},
		{"mount with from", http.MethodPost, "/v2/dest/blobs/uploads/?mount=sha256:abc&from=src/base", "src/base", false},
		{"mount without from", http.MethodPost, "/v2/dest/blobs/uploads/?mount=sha256:abc", "", false},
		{"from without mount", http.MethodPost, "/v2/dest/blobs/uploads/?from=src/base", "", false},
		{"mount not on post", http.MethodPatch, "/v2/dest/blobs/uploads/uuid?mount=sha256:abc&from=src/base", "", false},
		// Ambiguous queries must be rejected, not silently forwarded.
		{"semicolon separator", http.MethodPost, "/v2/dest/blobs/uploads/?mount=sha256:abc&from=src/base;evil", "", true},
		{"duplicate from", http.MethodPost, "/v2/dest/blobs/uploads/?mount=sha256:abc&from=a&from=b", "", true},
		{"duplicate mount", http.MethodPost, "/v2/dest/blobs/uploads/?mount=x&mount=y&from=a", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := classify(req(tc.method, tc.path))
			if !ok {
				t.Fatalf("classify ok = false, want true")
			}
			if got.mountFrom != tc.wantFrom {
				t.Errorf("mountFrom = %q, want %q", got.mountFrom, tc.wantFrom)
			}
			if got.malformedQuery != tc.wantBadReq {
				t.Errorf("malformedQuery = %v, want %v", got.malformedQuery, tc.wantBadReq)
			}
		})
	}
}

// TestClassifyCommittedDigest covers the other way the cache key is found: the
// digest a blob upload puts in the repository when the registry commits it. Only
// the request that finishes an upload names one, and an ambiguous query names none
// — the gateway must not guess which of two blobs an upstream acted on.
func TestClassifyCommittedDigest(t *testing.T) {
	const sha256Digest = "sha256:6b0f2e1a4c3d5e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f7"
	for _, tc := range []struct {
		name       string
		method     string
		path       string
		wantDigest string
	}{
		// The three request shapes that finish an upload.
		{"session commit", http.MethodPut, "/v2/app/blobs/uploads/uuid-123?digest=" + sha256Digest, sha256Digest},
		{"session commit with opaque state", http.MethodPut, "/v2/app/blobs/uploads/uuid-123?_state=abc%3D&digest=" + sha256Digest, sha256Digest},
		{"monolithic upload", http.MethodPost, "/v2/app/blobs/uploads/?digest=" + sha256Digest, sha256Digest},
		{"cross-repo mount", http.MethodPost, "/v2/app/blobs/uploads/?mount=" + sha256Digest + "&from=other/app", sha256Digest},
		// Steps that commit nothing.
		{"session start", http.MethodPost, "/v2/app/blobs/uploads/", ""},
		{"chunk", http.MethodPatch, "/v2/app/blobs/uploads/uuid-123?digest=" + sha256Digest, ""},
		{"status query", http.MethodGet, "/v2/app/blobs/uploads/uuid-123?digest=" + sha256Digest, ""},
		{"cancellation", http.MethodDelete, "/v2/app/blobs/uploads/uuid-123?digest=" + sha256Digest, ""},
		// A mount without a source is not a mount, and a reference that is not a
		// digest names nothing immutable.
		{"mount without from", http.MethodPost, "/v2/app/blobs/uploads/?mount=" + sha256Digest, ""},
		{"digest that is not one", http.MethodPut, "/v2/app/blobs/uploads/uuid-123?digest=sha256:abc", ""},
		{"empty digest", http.MethodPut, "/v2/app/blobs/uploads/uuid-123?digest=", ""},
		// Queries naming two blobs, or naming one in two ways.
		{"duplicate digest", http.MethodPut, "/v2/app/blobs/uploads/uuid-123?digest=" + sha256Digest + "&digest=" + sha256Digest, ""},
		{"mount and digest", http.MethodPost, "/v2/app/blobs/uploads/?mount=" + sha256Digest + "&from=other/app&digest=" + sha256Digest, ""},
		{"duplicate mount", http.MethodPost, "/v2/app/blobs/uploads/?mount=" + sha256Digest + "&mount=x&from=other/app", ""},
		{"mount on a commit", http.MethodPut, "/v2/app/blobs/uploads/uuid-123?digest=" + sha256Digest + "&mount=" + sha256Digest, ""},
		// A query the gateway and a lenient upstream could read differently: the
		// ';' is dropped here and might be honoured there.
		{"semicolon separator", http.MethodPut, "/v2/app/blobs/uploads/uuid-123?_state=a;digest=" + sha256Digest, ""},
		// A manifest is not a blob, whatever its reference: the cache must never
		// hold one.
		{"manifest write by digest", http.MethodPut, "/v2/app/manifests/" + sha256Digest, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := classify(req(tc.method, tc.path))
			if !ok {
				t.Fatalf("classify ok = false, want true")
			}
			if got.digest != tc.wantDigest {
				t.Errorf("digest = %q, want %q", got.digest, tc.wantDigest)
			}
		})
	}
}

// TestClassifyMountStaysStrictOnCommits guards the blast radius of parsing a PUT's
// query: reading it must teach the cache nothing new about mounts, and must not
// turn a request the gateway used to forward into one it rejects.
func TestClassifyMountStaysStrictOnCommits(t *testing.T) {
	for _, path := range []string{
		"/v2/app/blobs/uploads/uuid-123?mount=sha256:abc&from=other/app",
		"/v2/app/blobs/uploads/uuid-123?from=a&from=b",
		"/v2/app/blobs/uploads/uuid-123?_state=a;b",
	} {
		got, ok := classify(req(http.MethodPut, path))
		if !ok {
			t.Fatalf("classify(PUT %q) ok = false, want true", path)
		}
		if got.mountFrom != "" {
			t.Errorf("classify(PUT %q) mountFrom = %q, want it read on a POST only", path, got.mountFrom)
		}
		if got.malformedQuery {
			t.Errorf("classify(PUT %q) is now rejected as malformed, which it was not before", path)
		}
	}
}

// TestClassifyBlobDigest covers the field the blob existence cache keys on when it
// is read off the path: the blob a HEAD asks after, the blob a GET returns, and the
// blob a DELETE takes away. It is set only when the reference really is a digest —
// the repository grammar has capture groups of its own, so reading the reference out
// of the wrong submatch is the mistake to guard against.
func TestClassifyBlobDigest(t *testing.T) {
	const sha256Digest = "sha256:6b0f2e1a4c3d5e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f7"
	for _, tc := range []struct {
		name       string
		method     string
		path       string
		wantDigest string
	}{
		{"blob head", http.MethodHead, "/v2/app/blobs/" + sha256Digest, sha256Digest},
		{"nested repository", http.MethodHead, "/v2/team/service/app/blobs/" + sha256Digest, sha256Digest},
		{"repository with separators", http.MethodHead, "/v2/a.b__c-d/blobs/" + sha256Digest, sha256Digest},
		{"sha512", http.MethodHead, "/v2/app/blobs/sha512:" + strings.Repeat("ab", 64), "sha512:" + strings.Repeat("ab", 64)},
		{"multipart algorithm", http.MethodHead, "/v2/app/blobs/multihash+base58:" + strings.Repeat("c", 46), "multihash+base58:" + strings.Repeat("c", 46)},
		// A read names the blob it returns, and a delete the one whose entry it
		// unmakes.
		{"blob get", http.MethodGet, "/v2/app/blobs/" + sha256Digest, sha256Digest},
		{"blob delete", http.MethodDelete, "/v2/app/blobs/" + sha256Digest, sha256Digest},
		// Not a digest, so nothing immutable is named.
		{"tag", http.MethodHead, "/v2/app/blobs/latest", ""},
		{"get by tag", http.MethodGet, "/v2/app/blobs/latest", ""},
		{"delete by tag", http.MethodDelete, "/v2/app/blobs/latest", ""},
		{"too short to be a digest", http.MethodHead, "/v2/app/blobs/sha256:abc", ""},
		{"too long to be a digest", http.MethodHead, "/v2/app/blobs/sha256:" + strings.Repeat("a", 129), ""},
		{"no algorithm", http.MethodHead, "/v2/app/blobs/" + strings.Repeat("a", 64), ""},
		{"uppercase algorithm", http.MethodHead, "/v2/app/blobs/SHA256:" + strings.Repeat("a", 64), ""},
		// A manifest is never in the cache, whatever its reference.
		{"manifest head", http.MethodHead, "/v2/app/manifests/" + sha256Digest, ""},
		{"manifest get", http.MethodGet, "/v2/app/manifests/" + sha256Digest, ""},
		{"manifest delete", http.MethodDelete, "/v2/app/manifests/" + sha256Digest, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := classify(req(tc.method, tc.path))
			if !ok {
				t.Fatalf("classify ok = false, want true")
			}
			if got.digest != tc.wantDigest {
				t.Errorf("digest = %q, want %q", got.digest, tc.wantDigest)
			}
		})
	}
}
