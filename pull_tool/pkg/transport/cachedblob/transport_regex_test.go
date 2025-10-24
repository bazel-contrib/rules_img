package cachedblob

import (
	"testing"
)

func TestURLPatternMatching(t *testing.T) {
	tests := []struct {
		name        string
		url         string
		shouldMatch bool
		wantDigest  string
	}{
		// Blob URLs - should match
		{
			name:        "simple repo blob",
			url:         "/v2/myrepo/blobs/sha256:abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234",
			shouldMatch: true,
			wantDigest:  "sha256:abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234",
		},
		{
			name:        "repo with one slash blob",
			url:         "/v2/library/ubuntu/blobs/sha256:abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234",
			shouldMatch: true,
			wantDigest:  "sha256:abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234",
		},
		{
			name:        "repo with multiple slashes blob",
			url:         "/v2/myorg/myteam/myapp/blobs/sha256:abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234",
			shouldMatch: true,
			wantDigest:  "sha256:abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234",
		},
		{
			name:        "gcr.io style repo blob",
			url:         "/v2/gcr.io/my-project/my-app/blobs/sha256:abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234",
			shouldMatch: true,
			wantDigest:  "sha256:abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234",
		},

		// Manifest URLs - should match
		{
			name:        "simple repo manifest",
			url:         "/v2/myrepo/manifests/sha256:abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234",
			shouldMatch: true,
			wantDigest:  "sha256:abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234",
		},
		{
			name:        "repo with slash manifest",
			url:         "/v2/library/nginx/manifests/sha256:abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234",
			shouldMatch: true,
			wantDigest:  "sha256:abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234",
		},
		{
			name:        "repo with multiple slashes manifest",
			url:         "/v2/org/team/project/app/manifests/sha256:abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234",
			shouldMatch: true,
			wantDigest:  "sha256:abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234",
		},

		// Should NOT match
		{
			name:        "manifest with tag instead of digest",
			url:         "/v2/myrepo/manifests/latest",
			shouldMatch: false,
		},
		{
			name:        "manifest with sha but not sha256",
			url:         "/v2/myrepo/manifests/sha512:abcd1234",
			shouldMatch: false,
		},
		{
			name:        "manifest with short sha256",
			url:         "/v2/myrepo/manifests/sha256:abcd1234",
			shouldMatch: false,
		},
		{
			name:        "tags list endpoint",
			url:         "/v2/myrepo/tags/list",
			shouldMatch: false,
		},
		{
			name:        "v2 endpoint",
			url:         "/v2/",
			shouldMatch: false,
		},
		{
			name:        "missing v2 prefix",
			url:         "/myrepo/blobs/sha256:abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234",
			shouldMatch: false,
		},
		{
			name:        "blob with uppercase in digest",
			url:         "/v2/myrepo/blobs/sha256:ABCD1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234",
			shouldMatch: false,
		},
		{
			name:        "blob with invalid hex char",
			url:         "/v2/myrepo/blobs/sha256:gbcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234",
			shouldMatch: false,
		},
		{
			name:        "empty repo name",
			url:         "/v2//blobs/sha256:abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234",
			shouldMatch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var digest string

			// Test blob pattern
			if matches := blobURLPattern.FindStringSubmatch(tt.url); len(matches) > 7 {
				digest = matches[7]
			}

			// Test manifest pattern if blob didn't match
			if digest == "" {
				if matches := manifestURLPattern.FindStringSubmatch(tt.url); len(matches) > 7 {
					digest = matches[7]
				}
			}

			matched := digest != ""

			if matched != tt.shouldMatch {
				t.Errorf("URL %q: expected match=%v, got match=%v", tt.url, tt.shouldMatch, matched)
			}

			if tt.shouldMatch && digest != tt.wantDigest {
				t.Errorf("URL %q: expected digest %q, got %q", tt.url, tt.wantDigest, digest)
			}
		})
	}
}

func TestExtractRepositoryName(t *testing.T) {
	tests := []struct {
		url      string
		wantRepo string
	}{
		{
			url:      "/v2/myrepo/blobs/sha256:abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234",
			wantRepo: "myrepo",
		},
		{
			url:      "/v2/library/ubuntu/blobs/sha256:abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234",
			wantRepo: "library/ubuntu",
		},
		{
			url:      "/v2/myorg/myteam/myapp/blobs/sha256:abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234",
			wantRepo: "myorg/myteam/myapp",
		},
		{
			url:      "/v2/gcr.io/my-project/my-app/manifests/sha256:abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234",
			wantRepo: "gcr.io/my-project/my-app",
		},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			var repo string

			// Try blob pattern first
			if matches := blobURLPattern.FindStringSubmatch(tt.url); len(matches) > 7 {
				repo = matches[1]
			} else if matches := manifestURLPattern.FindStringSubmatch(tt.url); len(matches) > 7 {
				repo = matches[1]
			}

			if repo != tt.wantRepo {
				t.Errorf("URL %q: expected repo %q, got %q", tt.url, tt.wantRepo, repo)
			}
		})
	}
}
