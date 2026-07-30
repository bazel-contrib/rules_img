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

// TestClassifyBlobDigest covers the field the blob existence cache keys on. It is
// set only for a blob existence check, and only when the reference really is a
// digest — the repository grammar has capture groups of its own, so reading the
// reference out of the wrong submatch is the mistake to guard against.
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
		// Not a digest, so nothing immutable is named.
		{"tag", http.MethodHead, "/v2/app/blobs/latest", ""},
		{"too short to be a digest", http.MethodHead, "/v2/app/blobs/sha256:abc", ""},
		{"too long to be a digest", http.MethodHead, "/v2/app/blobs/sha256:" + strings.Repeat("a", 129), ""},
		{"no algorithm", http.MethodHead, "/v2/app/blobs/" + strings.Repeat("a", 64), ""},
		{"uppercase algorithm", http.MethodHead, "/v2/app/blobs/SHA256:" + strings.Repeat("a", 64), ""},
		// Only the existence check is cacheable; a read or a delete is not.
		{"blob get", http.MethodGet, "/v2/app/blobs/" + sha256Digest, ""},
		{"blob delete", http.MethodDelete, "/v2/app/blobs/" + sha256Digest, ""},
		{"manifest head", http.MethodHead, "/v2/app/manifests/" + sha256Digest, ""},
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
