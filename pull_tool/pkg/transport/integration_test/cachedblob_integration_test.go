package integration_test

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bazel-contrib/rules_img/pull_tool/pkg/transport/cachedblob"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// spyTransport records all requests that pass through it
type spyTransport struct {
	mu       sync.Mutex
	requests []string
	base     http.RoundTripper
}

func newSpyTransport(base http.RoundTripper) *spyTransport {
	return &spyTransport{
		base:     base,
		requests: []string{},
	}
}

func (s *spyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.requests = append(s.requests, req.URL.Path)
	s.mu.Unlock()

	if s.base == nil {
		s.base = http.DefaultTransport
	}
	return s.base.RoundTrip(req)
}

func (s *spyTransport) getRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]string, len(s.requests))
	copy(result, s.requests)
	return result
}

func (s *spyTransport) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
}

// TestCachedBlobIntegration tests the cachedblob transport with real go-containerregistry
func TestCachedBlobIntegration(t *testing.T) {
	// Create temporary directory for cached blobs
	tempDir := t.TempDir()
	blobDir := filepath.Join(tempDir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatalf("Failed to create blob directory: %v", err)
	}

	// Set up transport chain:
	// outerSpy -> cachedblob -> innerSpy -> network
	innerSpy := newSpyTransport(http.DefaultTransport)
	cachedTransport := cachedblob.NewTransport(tempDir, innerSpy)
	outerSpy := newSpyTransport(cachedTransport)

	// Use a small public test image (alpine is small and widely available)
	ref, err := name.ParseReference("mirror.gcr.io/alpine:3.19")
	if err != nil {
		t.Fatalf("Failed to parse reference: %v", err)
	}

	// Configure remote options to use our transport and single job
	remoteOpts := []remote.Option{
		remote.WithTransport(outerSpy),
		remote.WithJobs(1), // Ensure sequential processing
	}

	// First pull: everything should go to network
	t.Run("first pull - all from network", func(t *testing.T) {
		outerSpy.reset()
		innerSpy.reset()

		// Fetch the image
		img, err := remote.Image(ref, remoteOpts...)
		if err != nil {
			t.Fatalf("Failed to fetch image: %v", err)
		}

		// Get the manifest to trigger blob downloads
		manifest, err := img.Manifest()
		if err != nil {
			t.Fatalf("Failed to get manifest: %v", err)
		}

		// Download config blob
		configName, err := img.ConfigName()
		if err != nil {
			t.Fatalf("Failed to get config name: %v", err)
		}
		configBlob, err := img.RawConfigFile()
		if err != nil {
			t.Fatalf("Failed to get config blob: %v", err)
		}

		// Write config to cache directory
		configPath := filepath.Join(blobDir, configName.Hex)
		if err := os.WriteFile(configPath, configBlob, 0o644); err != nil {
			t.Fatalf("Failed to write config blob: %v", err)
		}

		// Download all layer blobs
		layers, err := img.Layers()
		if err != nil {
			t.Fatalf("Failed to get layers: %v", err)
		}

		for _, layer := range layers {
			digest, err := layer.Digest()
			if err != nil {
				t.Fatalf("Failed to get layer digest: %v", err)
			}

			// Read the layer data to trigger actual download
			rc, err := layer.Compressed()
			if err != nil {
				t.Fatalf("Failed to get layer reader: %v", err)
			}

			// Write to cache directory
			layerPath := filepath.Join(blobDir, digest.Hex)
			outFile, err := os.Create(layerPath)
			if err != nil {
				rc.Close()
				t.Fatalf("Failed to create layer file: %v", err)
			}

			if _, err := io.Copy(outFile, rc); err != nil {
				rc.Close()
				outFile.Close()
				t.Fatalf("Failed to write layer: %v", err)
			}
			rc.Close()
			outFile.Close()
		}

		// Verify requests
		outerRequests := outerSpy.getRequests()
		innerRequests := innerSpy.getRequests()

		t.Logf("First pull - outer spy saw %d requests", len(outerRequests))
		t.Logf("First pull - inner spy saw %d requests", len(innerRequests))

		// On first pull, inner spy should see all requests (nothing cached)
		if len(innerRequests) == 0 {
			t.Errorf("Inner spy should see requests on first pull")
		}

		// All outer requests should equal inner requests (nothing cached yet)
		if len(outerRequests) != len(innerRequests) {
			t.Errorf("On first pull, outer and inner should see same number of requests: outer=%d, inner=%d",
				len(outerRequests), len(innerRequests))
		}

		// Verify we got manifest requests
		hasManifestRequest := false
		for _, req := range outerRequests {
			if strings.Contains(req, "/manifests/") {
				hasManifestRequest = true
				break
			}
		}
		if !hasManifestRequest {
			t.Errorf("Expected to see manifest request in outer spy")
		}

		// Verify we got blob requests
		hasBlobRequest := false
		for _, req := range outerRequests {
			if strings.Contains(req, "/blobs/sha256:") {
				hasBlobRequest = true
				break
			}
		}
		if !hasBlobRequest {
			t.Errorf("Expected to see blob request in outer spy")
		}

		// Store for comparison with second pull
		t.Logf("Manifest digest: %s", manifest.Config.Digest)
		t.Logf("Number of layers: %d", len(manifest.Layers))
	})

	// Second pull: blobs should be served from cache
	t.Run("second pull - blobs from cache", func(t *testing.T) {
		outerSpy.reset()
		innerSpy.reset()

		// Fetch the image again
		img, err := remote.Image(ref, remoteOpts...)
		if err != nil {
			t.Fatalf("Failed to fetch image: %v", err)
		}

		// Download config blob again
		if _, err := img.RawConfigFile(); err != nil {
			t.Fatalf("Failed to get config blob: %v", err)
		}

		// Download all layer blobs again
		layers, err := img.Layers()
		if err != nil {
			t.Fatalf("Failed to get layers: %v", err)
		}

		for _, layer := range layers {
			// Read the layer data
			rc, err := layer.Compressed()
			if err != nil {
				t.Fatalf("Failed to get layer reader: %v", err)
			}

			// Discard the data (we just want to trigger the request)
			if _, err := io.Copy(io.Discard, rc); err != nil {
				rc.Close()
				t.Fatalf("Failed to read layer: %v", err)
			}
			rc.Close()
		}

		// Verify requests
		outerRequests := outerSpy.getRequests()
		innerRequests := innerSpy.getRequests()

		t.Logf("Second pull - outer spy saw %d requests", len(outerRequests))
		t.Logf("Second pull - inner spy saw %d requests", len(innerRequests))

		// On second pull, outer spy should see requests but inner spy should see fewer
		// (blobs are cached, but manifest/tags may need to be checked)
		if len(outerRequests) == 0 {
			t.Errorf("Outer spy should see requests on second pull")
		}

		// Count blob requests in each spy
		outerBlobCount := 0
		innerBlobCount := 0
		for _, req := range outerRequests {
			if strings.Contains(req, "/blobs/sha256:") {
				outerBlobCount++
			}
		}
		for _, req := range innerRequests {
			if strings.Contains(req, "/blobs/sha256:") {
				innerBlobCount++
			}
		}

		t.Logf("Second pull - outer spy saw %d blob requests", outerBlobCount)
		t.Logf("Second pull - inner spy saw %d blob requests", innerBlobCount)

		// Inner spy should see zero blob requests (all served from cache)
		if innerBlobCount > 0 {
			t.Errorf("Inner spy should not see blob requests on second pull (all cached), but saw %d", innerBlobCount)
		}

		// Outer spy should see blob requests (they hit the cache layer)
		if outerBlobCount == 0 {
			t.Errorf("Outer spy should see blob requests on second pull")
		}

		// Verify order: manifest requests should come before blob requests
		firstBlobIdx := -1
		lastManifestIdx := -1
		for i, req := range outerRequests {
			if strings.Contains(req, "/blobs/sha256:") && firstBlobIdx == -1 {
				firstBlobIdx = i
			}
			if strings.Contains(req, "/manifests/") {
				lastManifestIdx = i
			}
		}

		if firstBlobIdx != -1 && lastManifestIdx != -1 {
			if lastManifestIdx > firstBlobIdx {
				t.Errorf("Expected manifest requests before blob requests, but manifest at %d, first blob at %d",
					lastManifestIdx, firstBlobIdx)
			}
		}
	})
}

// TestCachedBlobIntegration_ManifestCaching tests that manifests are also cached
func TestCachedBlobIntegration_ManifestCaching(t *testing.T) {
	// Create temporary directory for cached blobs
	tempDir := t.TempDir()
	blobDir := filepath.Join(tempDir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatalf("Failed to create blob directory: %v", err)
	}

	// Set up transport chain
	innerSpy := newSpyTransport(http.DefaultTransport)
	cachedTransport := cachedblob.NewTransport(tempDir, innerSpy)
	outerSpy := newSpyTransport(cachedTransport)

	ref, err := name.ParseReference("mirror.gcr.io/alpine:3.19")
	if err != nil {
		t.Fatalf("Failed to parse reference: %v", err)
	}

	remoteOpts := []remote.Option{
		remote.WithTransport(outerSpy),
		remote.WithJobs(1),
	}

	// Fetch image descriptor to get manifest digest
	desc, err := remote.Get(ref, remoteOpts...)
	if err != nil {
		t.Fatalf("Failed to get descriptor: %v", err)
	}

	manifestDigest := desc.Digest.String()
	t.Logf("Manifest digest: %s", manifestDigest)

	// Write manifest to cache
	manifestPath := filepath.Join(blobDir, desc.Digest.Hex)
	if err := os.WriteFile(manifestPath, desc.Manifest, 0o644); err != nil {
		t.Fatalf("Failed to write manifest: %v", err)
	}

	// Reset spies
	outerSpy.reset()
	innerSpy.reset()

	// Now fetch by digest (should be served from cache)
	digestRef, err := name.ParseReference("mirror.gcr.io/alpine@" + manifestDigest)
	if err != nil {
		t.Fatalf("Failed to parse digest reference: %v", err)
	}

	_, err = remote.Get(digestRef, remoteOpts...)
	if err != nil {
		t.Fatalf("Failed to get manifest by digest: %v", err)
	}

	outerRequests := outerSpy.getRequests()
	innerRequests := innerSpy.getRequests()

	t.Logf("Cached manifest fetch - outer spy saw %d requests", len(outerRequests))
	t.Logf("Cached manifest fetch - inner spy saw %d requests", len(innerRequests))

	// Outer spy should see the manifest request
	if len(outerRequests) == 0 {
		t.Errorf("Outer spy should see manifest request")
	}

	// Inner spy should NOT see the manifest request (served from cache)
	manifestRequestInInner := false
	for _, req := range innerRequests {
		if strings.Contains(req, "/manifests/"+manifestDigest) {
			manifestRequestInInner = true
			break
		}
	}

	if manifestRequestInInner {
		t.Errorf("Inner spy should not see manifest request when served from cache")
	}
}

// TestCachedBlobIntegration_AirgappedMode tests pulling an image in airgapped mode
func TestCachedBlobIntegration_AirgappedMode(t *testing.T) {
	// Create temporary directory for cached blobs
	tempDir := t.TempDir()
	blobDir := filepath.Join(tempDir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatalf("Failed to create blob directory: %v", err)
	}

	// First, do a non-airgapped pull to populate the cache
	innerSpyPopulate := newSpyTransport(http.DefaultTransport)
	cachedTransportPopulate := cachedblob.NewTransport(tempDir, innerSpyPopulate)
	outerSpyPopulate := newSpyTransport(cachedTransportPopulate)

	ref, err := name.ParseReference("mirror.gcr.io/alpine:3.19")
	if err != nil {
		t.Fatalf("Failed to parse reference: %v", err)
	}

	remoteOptsPopulate := []remote.Option{
		remote.WithTransport(outerSpyPopulate),
		remote.WithJobs(1),
	}

	// Fetch the full image to populate cache
	img, err := remote.Image(ref, remoteOptsPopulate...)
	if err != nil {
		t.Fatalf("Failed to fetch image: %v", err)
	}

	// Get the actual image manifest (platform-specific)
	manifestDigest, err := img.Digest()
	if err != nil {
		t.Fatalf("Failed to get image digest: %v", err)
	}
	t.Logf("Image manifest digest: %s", manifestDigest.String())

	// Write the image manifest to cache
	manifestBytes, err := img.RawManifest()
	if err != nil {
		t.Fatalf("Failed to get raw manifest: %v", err)
	}
	manifestPath := filepath.Join(blobDir, manifestDigest.Hex)
	if err := os.WriteFile(manifestPath, manifestBytes, 0o644); err != nil {
		t.Fatalf("Failed to write manifest: %v", err)
	}

	// Download and cache all blobs
	configName, err := img.ConfigName()
	if err != nil {
		t.Fatalf("Failed to get config name: %v", err)
	}
	configBlob, err := img.RawConfigFile()
	if err != nil {
		t.Fatalf("Failed to get config blob: %v", err)
	}
	configPath := filepath.Join(blobDir, configName.Hex)
	if err := os.WriteFile(configPath, configBlob, 0o644); err != nil {
		t.Fatalf("Failed to write config blob: %v", err)
	}

	layers, err := img.Layers()
	if err != nil {
		t.Fatalf("Failed to get layers: %v", err)
	}

	for _, layer := range layers {
		digest, err := layer.Digest()
		if err != nil {
			t.Fatalf("Failed to get layer digest: %v", err)
		}

		rc, err := layer.Compressed()
		if err != nil {
			t.Fatalf("Failed to get layer reader: %v", err)
		}

		layerPath := filepath.Join(blobDir, digest.Hex)
		outFile, err := os.Create(layerPath)
		if err != nil {
			rc.Close()
			t.Fatalf("Failed to create layer file: %v", err)
		}

		if _, err := io.Copy(outFile, rc); err != nil {
			rc.Close()
			outFile.Close()
			t.Fatalf("Failed to write layer: %v", err)
		}
		rc.Close()
		outFile.Close()
	}

	t.Logf("Cache populated with manifest and %d layers", len(layers))

	// Now test in airgapped mode
	t.Run("airgapped pull by digest", func(t *testing.T) {
		// Set up new transport chain with airgapped mode
		innerSpy := newSpyTransport(http.DefaultTransport)
		cachedTransport := cachedblob.NewTransport(tempDir, innerSpy, cachedblob.WithAirgapped(true))
		outerSpy := newSpyTransport(cachedTransport)

		// Create reference by digest
		digestRef, err := name.ParseReference("mirror.gcr.io/alpine@" + manifestDigest.String())
		if err != nil {
			t.Fatalf("Failed to parse digest reference: %v", err)
		}

		remoteOpts := []remote.Option{
			remote.WithTransport(outerSpy),
			remote.WithJobs(1),
		}

		// Pull the image by digest
		img, err := remote.Image(digestRef, remoteOpts...)
		if err != nil {
			t.Fatalf("Failed to fetch image in airgapped mode: %v", err)
		}

		// Download config and all layers to verify everything works
		if _, err := img.RawConfigFile(); err != nil {
			t.Fatalf("Failed to get config in airgapped mode: %v", err)
		}

		layers, err := img.Layers()
		if err != nil {
			t.Fatalf("Failed to get layers in airgapped mode: %v", err)
		}

		for _, layer := range layers {
			rc, err := layer.Compressed()
			if err != nil {
				t.Fatalf("Failed to get layer reader in airgapped mode: %v", err)
			}
			if _, err := io.Copy(io.Discard, rc); err != nil {
				rc.Close()
				t.Fatalf("Failed to read layer in airgapped mode: %v", err)
			}
			rc.Close()
		}

		// Verify requests
		outerRequests := outerSpy.getRequests()
		innerRequests := innerSpy.getRequests()

		t.Logf("Airgapped mode - outer spy saw %d requests", len(outerRequests))
		t.Logf("Airgapped mode - inner spy saw %d requests", len(innerRequests))

		// In airgapped mode, ALL requests should be served from cache
		// Inner spy should see ZERO requests
		if len(innerRequests) != 0 {
			t.Errorf("Inner spy should see 0 requests in airgapped mode, but saw %d", len(innerRequests))
			for i, req := range innerRequests {
				t.Logf("  Request %d: %s", i+1, req)
			}
		}

		// Outer spy should see requests (to the cache layer)
		if len(outerRequests) == 0 {
			t.Errorf("Outer spy should see requests (they hit cache layer)")
		}

		// Verify /v2/ endpoint was handled
		hasV2Request := false
		for _, req := range outerRequests {
			if req == "/v2/" {
				hasV2Request = true
				break
			}
		}
		if hasV2Request {
			t.Logf("Verified /v2/ endpoint was called in airgapped mode")
		}
	})
}
