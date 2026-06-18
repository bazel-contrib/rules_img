package cachedblob

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTransport_ServeFromCache(t *testing.T) {
	// Create temporary directory for test blobs
	tempDir := t.TempDir()
	blobDir := filepath.Join(tempDir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatalf("Failed to create blob directory: %v", err)
	}

	// Create a test blob
	testData := []byte("test blob content")
	hasher := sha256.New()
	hasher.Write(testData)
	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	sha256sum := digest[7:] // Remove "sha256:" prefix

	// Write blob to disk
	blobPath := filepath.Join(blobDir, sha256sum)
	if err := os.WriteFile(blobPath, testData, 0o644); err != nil {
		t.Fatalf("Failed to write test blob: %v", err)
	}

	// Create test server that should not be called if cache works
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Unexpected request to test server: %s", r.URL.Path)
		http.Error(w, "Should not reach here", http.StatusInternalServerError)
	}))
	defer testServer.Close()

	// Create transport with our cache - note we pass tempDir, not blobDir
	// The transport expects the parent directory containing "blobs/"
	transport := NewTransport(tempDir, http.DefaultTransport)

	// Test cases
	tests := []struct {
		name         string
		path         string
		expectCached bool
		expectStatus int
	}{
		{
			name:         "blob request simple repo - should be served from cache",
			path:         fmt.Sprintf("/v2/myrepo/blobs/%s", digest),
			expectCached: true,
			expectStatus: http.StatusOK,
		},
		{
			name:         "blob request repo with slash - should be served from cache",
			path:         fmt.Sprintf("/v2/library/ubuntu/blobs/%s", digest),
			expectCached: true,
			expectStatus: http.StatusOK,
		},
		{
			name:         "blob request repo with multiple slashes - should be served from cache",
			path:         fmt.Sprintf("/v2/myorg/myteam/myapp/blobs/%s", digest),
			expectCached: true,
			expectStatus: http.StatusOK,
		},
		{
			name:         "manifest request simple repo - should be served from cache",
			path:         fmt.Sprintf("/v2/myrepo/manifests/%s", digest),
			expectCached: true,
			expectStatus: http.StatusOK,
		},
		{
			name:         "manifest request repo with slash - should be served from cache",
			path:         fmt.Sprintf("/v2/library/nginx/manifests/%s", digest),
			expectCached: true,
			expectStatus: http.StatusOK,
		},
		{
			name:         "non-blob request - should not be cached",
			path:         "/v2/myrepo/tags/list",
			expectCached: false,
			expectStatus: http.StatusOK,
		},
		{
			name:         "blob not in cache - should use base transport",
			path:         "/v2/myrepo/blobs/sha256:0000000000000000000000000000000000000000000000000000000000000000",
			expectCached: false,
			expectStatus: http.StatusOK,
		},
		{
			name:         "manifest with non-sha256 reference - should not be cached",
			path:         "/v2/myrepo/manifests/latest",
			expectCached: false,
			expectStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest("GET", testServer.URL+tt.path, nil)
			if err != nil {
				t.Fatalf("Failed to create request: %v", err)
			}

			if !tt.expectCached {
				// For non-cached requests, use a test server that returns success
				successServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
					w.Write([]byte("from server"))
				}))
				defer successServer.Close()
				req.URL.Host = successServer.URL[7:] // Remove "http://"
				req.URL.Scheme = "http"
			}

			resp, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip failed: %v", err)
			}
			defer resp.Body.Close()

			// Check response
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("Failed to read response body: %v", err)
			}

			if tt.expectCached {
				if !bytes.Equal(body, testData) {
					t.Errorf("Expected cached content %q, got %q", testData, body)
				}
				if resp.Header.Get("Docker-Content-Digest") != digest {
					t.Errorf("Expected digest header %s, got %s", digest, resp.Header.Get("Docker-Content-Digest"))
				}
			} else {
				if bytes.Equal(body, testData) {
					t.Errorf("Should not have received cached content")
				}
			}

			if resp.StatusCode != tt.expectStatus {
				t.Errorf("Expected status %d, got %d", tt.expectStatus, resp.StatusCode)
			}
		})
	}
}

func TestTransport_Memoization(t *testing.T) {
	// Create temporary directory for test blobs
	tempDir := t.TempDir()
	blobDir := filepath.Join(tempDir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatalf("Failed to create blob directory: %v", err)
	}

	// Create a test blob (small, will be cached)
	testData := []byte("test blob content for memoization")
	hasher := sha256.New()
	hasher.Write(testData)
	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	sha256sum := digest[7:]

	blobPath := filepath.Join(blobDir, sha256sum)
	if err := os.WriteFile(blobPath, testData, 0o644); err != nil {
		t.Fatalf("Failed to write test blob: %v", err)
	}

	// Create transport
	transport := NewTransport(tempDir, http.DefaultTransport)

	// First request - should read from disk
	req1, _ := http.NewRequest("GET", "http://test/v2/myrepo/blobs/"+digest, nil)
	resp1, err := transport.RoundTrip(req1)
	if err != nil {
		t.Fatalf("First request failed: %v", err)
	}
	body1, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()

	// Verify first request succeeded
	if !bytes.Equal(body1, testData) {
		t.Errorf("First request: expected %q, got %q", testData, body1)
	}

	// Verify content type for blob request
	if ct := resp1.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Expected Content-Type application/octet-stream for blob, got %s", ct)
	}

	// Delete the file to ensure second request uses memory cache
	if err := os.Remove(blobPath); err != nil {
		t.Fatalf("Failed to remove blob file: %v", err)
	}

	// Second request - should use memoized data
	req2, _ := http.NewRequest("GET", "http://test/v2/library/ubuntu/blobs/"+digest, nil)
	resp2, err := transport.RoundTrip(req2)
	if err != nil {
		t.Fatalf("Second request failed: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()

	// Verify second request still returns the cached data
	if !bytes.Equal(body2, testData) {
		t.Errorf("Second request: expected %q, got %q", testData, body2)
	}

	// Test that failed lookups are NOT cached (they should always fall back)
	missingDigest := "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"

	// Create fallback server
	callCount := 0
	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("from fallback"))
	}))
	defer fallbackServer.Close()

	// First request for missing blob - should check disk and fall back
	req3, _ := http.NewRequest("GET", fallbackServer.URL+"/v2/myrepo/blobs/"+missingDigest, nil)
	resp3, err := transport.RoundTrip(req3)
	if err != nil {
		t.Fatalf("Third request failed: %v", err)
	}
	resp3.Body.Close()

	// Second request for same missing blob - should also check disk and fall back (not cached)
	req4, _ := http.NewRequest("GET", fallbackServer.URL+"/v2/myrepo/blobs/"+missingDigest, nil)
	resp4, err := transport.RoundTrip(req4)
	if err != nil {
		t.Fatalf("Fourth request failed: %v", err)
	}
	resp4.Body.Close()

	// Both requests should have fallen back to the server (no error caching)
	if callCount != 2 {
		t.Errorf("Expected 2 fallback calls, got %d", callCount)
	}
}

func TestTransport_InvalidDigest(t *testing.T) {
	// Create temporary directory for test blobs
	tempDir := t.TempDir()
	blobDir := filepath.Join(tempDir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatalf("Failed to create blob directory: %v", err)
	}

	// Create a blob with incorrect content (digest mismatch)
	wrongData := []byte("wrong content")
	correctDigest := "sha256:1234567890123456789012345678901234567890123456789012345678901234"
	sha256sum := correctDigest[7:]

	blobPath := filepath.Join(blobDir, sha256sum)
	if err := os.WriteFile(blobPath, wrongData, 0o644); err != nil {
		t.Fatalf("Failed to write test blob: %v", err)
	}

	// Create transport
	transport := NewTransport(tempDir, http.DefaultTransport)

	// Create fallback server (should not be called)
	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Fallback server should not be called on digest mismatch")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("from fallback"))
	}))
	defer fallbackServer.Close()

	// Make request
	req, err := http.NewRequest("GET", fallbackServer.URL+"/v2/myrepo/blobs/"+correctDigest, nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("Expected fatal error on digest mismatch, got nil")
	}

	// Should be a fatal cache error, not fall back
	if !strings.Contains(err.Error(), "fatal cache error") {
		t.Errorf("Expected fatal cache error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Errorf("Expected digest mismatch error, got: %v", err)
	}

	// Check that invalid blob still exists (not removed)
	if _, err := os.Stat(blobPath); err != nil {
		t.Errorf("Blob should not be removed, got error: %v", err)
	}
}

func TestTransport_LargeBlobCaching(t *testing.T) {
	// Create temporary directory for test blobs
	tempDir := t.TempDir()
	blobDir := filepath.Join(tempDir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatalf("Failed to create blob directory: %v", err)
	}

	// Create a large blob (> 1 MiB)
	largeData := make([]byte, 2*1024*1024) // 2 MiB
	for i := range largeData {
		largeData[i] = byte(i % 256)
	}
	hasher := sha256.New()
	hasher.Write(largeData)
	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	sha256sum := digest[7:]

	blobPath := filepath.Join(blobDir, sha256sum)
	if err := os.WriteFile(blobPath, largeData, 0o644); err != nil {
		t.Fatalf("Failed to write large blob: %v", err)
	}

	// Create transport
	transport := NewTransport(tempDir, http.DefaultTransport)

	// First request - should read from disk and NOT cache data in memory
	req1, _ := http.NewRequest("GET", "http://test/v2/myrepo/blobs/"+digest, nil)
	resp1, err := transport.RoundTrip(req1)
	if err != nil {
		t.Fatalf("First request failed: %v", err)
	}
	body1, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()

	// Verify first request succeeded
	if len(body1) != len(largeData) {
		t.Errorf("First request: expected %d bytes, got %d", len(largeData), len(body1))
	}

	// Second request - should read from disk again (not from memory cache)
	req2, _ := http.NewRequest("GET", "http://test/v2/myrepo/blobs/"+digest, nil)
	resp2, err := transport.RoundTrip(req2)
	if err != nil {
		t.Fatalf("Second request failed: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()

	// Verify second request succeeded
	if len(body2) != len(largeData) {
		t.Errorf("Second request: expected %d bytes, got %d", len(largeData), len(body2))
	}

	// Verify that blob metadata is cached (even though data isn't)
	transport.blobCacheMu.RLock()
	cached, found := transport.blobCache[digest]
	transport.blobCacheMu.RUnlock()

	if !found {
		t.Errorf("Expected blob metadata to be cached")
	} else if cached.data != nil {
		t.Errorf("Large blob data should not be cached in memory")
	} else if cached.size != int64(len(largeData)) {
		t.Errorf("Expected cached size %d, got %d", len(largeData), cached.size)
	}
}

func TestTransport_ManifestMediaType(t *testing.T) {
	// Create temporary directory for test blobs
	tempDir := t.TempDir()
	blobDir := filepath.Join(tempDir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatalf("Failed to create blob directory: %v", err)
	}

	// Test manifests with different media types
	manifests := []struct {
		name      string
		content   string
		mediaType string
	}{
		{
			name: "OCI image manifest",
			content: `{
				"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"config": {"digest": "sha256:abc"},
				"layers": [{"digest": "sha256:def"}]
			}`,
			mediaType: "application/vnd.oci.image.manifest.v1+json",
		},
		{
			name: "OCI image index",
			content: `{
				"mediaType": "application/vnd.oci.image.index.v1+json",
				"manifests": [{"digest": "sha256:abc"}]
			}`,
			mediaType: "application/vnd.oci.image.index.v1+json",
		},
		{
			name: "Manifest without explicit mediaType (has config and layers)",
			content: `{
				"config": {"digest": "sha256:abc"},
				"layers": [{"digest": "sha256:def"}]
			}`,
			mediaType: "application/vnd.oci.image.manifest.v1+json",
		},
		{
			name: "Index without explicit mediaType (has manifests)",
			content: `{
				"manifests": [{"digest": "sha256:abc"}]
			}`,
			mediaType: "application/vnd.oci.image.index.v1+json",
		},
		{
			name:      "Unknown manifest structure",
			content:   `{"unknown": "structure"}`,
			mediaType: "application/json",
		},
	}

	// Create transport
	transport := NewTransport(tempDir, http.DefaultTransport)

	for _, test := range manifests {
		t.Run(test.name, func(t *testing.T) {
			// Create manifest file
			hasher := sha256.New()
			hasher.Write([]byte(test.content))
			digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
			sha256sum := digest[7:]

			manifestPath := filepath.Join(blobDir, sha256sum)
			if err := os.WriteFile(manifestPath, []byte(test.content), 0o644); err != nil {
				t.Fatalf("Failed to write manifest: %v", err)
			}

			// Request as manifest (should detect and cache media type)
			req, _ := http.NewRequest("GET", "http://test/v2/myrepo/manifests/"+digest, nil)
			resp, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatalf("Request failed: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			// Verify response
			if string(body) != test.content {
				t.Errorf("Expected content %q, got %q", test.content, body)
			}

			// Verify Content-Type header
			if ct := resp.Header.Get("Content-Type"); ct != test.mediaType {
				t.Errorf("Expected Content-Type %s, got %s", test.mediaType, ct)
			}

			// Verify media type is cached
			transport.mediaTypeCacheMu.RLock()
			cachedType, found := transport.mediaTypeCache[digest]
			transport.mediaTypeCacheMu.RUnlock()

			if !found {
				t.Errorf("Expected media type to be cached")
			} else if cachedType != test.mediaType {
				t.Errorf("Expected cached media type %s, got %s", test.mediaType, cachedType)
			}

			// Clean up
			os.Remove(manifestPath)
		})
	}

	// Test that blob requests don't get media type detection
	t.Run("blob request should use octet-stream", func(t *testing.T) {
		// Create a JSON blob (not requested as manifest)
		jsonContent := `{"some": "json"}`
		hasher := sha256.New()
		hasher.Write([]byte(jsonContent))
		digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
		sha256sum := digest[7:]

		blobPath := filepath.Join(blobDir, sha256sum)
		if err := os.WriteFile(blobPath, []byte(jsonContent), 0o644); err != nil {
			t.Fatalf("Failed to write blob: %v", err)
		}

		// Request as blob (not manifest)
		req, _ := http.NewRequest("GET", "http://test/v2/myrepo/blobs/"+digest, nil)
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}
		resp.Body.Close()

		// Should use application/octet-stream for blobs
		if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
			t.Errorf("Expected Content-Type application/octet-stream for blob, got %s", ct)
		}

		// Should NOT cache media type for blob requests
		transport.mediaTypeCacheMu.RLock()
		_, found := transport.mediaTypeCache[digest]
		transport.mediaTypeCacheMu.RUnlock()

		if found {
			t.Errorf("Media type should not be cached for blob requests")
		}
	})
}

func TestTransport_AirgappedMode(t *testing.T) {
	tempDir := t.TempDir()

	t.Run("non-airgapped mode forwards /v2/ to base", func(t *testing.T) {
		// Create a test server that tracks if it was called
		called := false
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"version":"test"}`))
		}))
		defer testServer.Close()

		// Create transport with Airgapped = false (default)
		transport := NewTransport(tempDir, http.DefaultTransport, WithAirgapped(false))

		// Make request to /v2/
		req, err := http.NewRequest("GET", testServer.URL+"/v2/", nil)
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}

		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip failed: %v", err)
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)

		// Should have called the base transport
		if !called {
			t.Errorf("Expected base transport to be called in non-airgapped mode")
		}

		// Should get response from server
		if !strings.Contains(string(body), "version") {
			t.Errorf("Expected server response, got: %s", body)
		}
	})

	t.Run("airgapped mode responds to /v2/ without forwarding", func(t *testing.T) {
		// Create a test server that should NOT be called
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("Base transport should not be called in airgapped mode for /v2/")
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer testServer.Close()

		// Create transport with Airgapped = true
		transport := NewTransport(tempDir, http.DefaultTransport, WithAirgapped(true))

		// Make request to /v2/
		req, err := http.NewRequest("GET", testServer.URL+"/v2/", nil)
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}

		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip failed: %v", err)
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)

		// Should get 200 OK
		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected status 200, got %d", resp.StatusCode)
		}

		// Should get JSON response
		if resp.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Expected Content-Type application/json, got %s", resp.Header.Get("Content-Type"))
		}

		// Should get {} body
		if string(body) != "{}" {
			t.Errorf("Expected {} body, got %s", body)
		}

		// Should have correct Content-Length
		if resp.ContentLength != 2 {
			t.Errorf("Expected ContentLength 2, got %d", resp.ContentLength)
		}
	})
}

func TestTransport_NetworkCaching(t *testing.T) {
	// Create temporary directory (no blobs on disk)
	tempDir := t.TempDir()

	// Create test data
	smallData := []byte("small blob from network")
	hasher := sha256.New()
	hasher.Write(smallData)
	smallDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))

	// Create large data (> 1 MiB)
	largeData := make([]byte, 2*1024*1024)
	for i := range largeData {
		largeData[i] = byte(i % 256)
	}
	hasher2 := sha256.New()
	hasher2.Write(largeData)
	largeDigest := "sha256:" + hex.EncodeToString(hasher2.Sum(nil))

	// Create test server that returns blobs
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, smallDigest) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(smallData)))
			w.WriteHeader(http.StatusOK)
			w.Write(smallData)
		} else if strings.Contains(r.URL.Path, largeDigest) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(largeData)))
			w.WriteHeader(http.StatusOK)
			w.Write(largeData)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer testServer.Close()

	// Create transport
	transport := NewTransport(tempDir, http.DefaultTransport)

	t.Run("small blob from network should be cached", func(t *testing.T) {
		// First request - should fetch from network and cache
		req1, _ := http.NewRequest("GET", testServer.URL+"/v2/myrepo/blobs/"+smallDigest, nil)
		resp1, err := transport.RoundTrip(req1)
		if err != nil {
			t.Fatalf("First request failed: %v", err)
		}
		body1, _ := io.ReadAll(resp1.Body)
		resp1.Body.Close()

		if !bytes.Equal(body1, smallData) {
			t.Errorf("First request: expected %q, got %q", smallData, body1)
		}

		// Verify data was cached
		transport.blobCacheMu.RLock()
		cached, found := transport.blobCache[smallDigest]
		transport.blobCacheMu.RUnlock()

		if !found {
			t.Errorf("Expected blob to be cached")
		} else if cached.data == nil {
			t.Errorf("Expected blob data to be cached in memory")
		} else if !bytes.Equal(cached.data, smallData) {
			t.Errorf("Cached data mismatch")
		}

		// Second request - should be served from cache
		req2, _ := http.NewRequest("GET", testServer.URL+"/v2/myrepo/blobs/"+smallDigest, nil)
		resp2, err := transport.RoundTrip(req2)
		if err != nil {
			t.Fatalf("Second request failed: %v", err)
		}
		body2, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()

		if !bytes.Equal(body2, smallData) {
			t.Errorf("Second request: expected %q, got %q", smallData, body2)
		}
	})

	t.Run("large blob from network should not cache contents", func(t *testing.T) {
		// Request large blob
		req, _ := http.NewRequest("GET", testServer.URL+"/v2/myrepo/blobs/"+largeDigest, nil)
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if len(body) != len(largeData) {
			t.Errorf("Expected %d bytes, got %d", len(largeData), len(body))
		}

		// Verify not cached
		transport.blobCacheMu.RLock()
		cached, found := transport.blobCache[largeDigest]
		transport.blobCacheMu.RUnlock()

		if found && cached.data != nil {
			t.Errorf("Large blob data should not be cached in memory")
		}
	})

	t.Run("manifest from network should cache media type", func(t *testing.T) {
		// Create a manifest
		manifestContent := `{
			"mediaType": "application/vnd.oci.image.manifest.v1+json",
			"config": {"digest": "sha256:abc"},
			"layers": []
		}`
		hasher := sha256.New()
		hasher.Write([]byte(manifestContent))
		manifestDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))

		// Create server that returns the manifest
		manifestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(manifestContent)))
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(manifestContent))
		}))
		defer manifestServer.Close()

		// Request as manifest
		req, _ := http.NewRequest("GET", manifestServer.URL+"/v2/myrepo/manifests/"+manifestDigest, nil)
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if string(body) != manifestContent {
			t.Errorf("Expected %q, got %q", manifestContent, body)
		}

		// Verify media type was cached
		transport.mediaTypeCacheMu.RLock()
		cachedType, found := transport.mediaTypeCache[manifestDigest]
		transport.mediaTypeCacheMu.RUnlock()

		if !found {
			t.Errorf("Expected media type to be cached")
		} else if cachedType != "application/vnd.oci.image.manifest.v1+json" {
			t.Errorf("Expected cached media type %s, got %s", "application/vnd.oci.image.manifest.v1+json", cachedType)
		}
	})
}
