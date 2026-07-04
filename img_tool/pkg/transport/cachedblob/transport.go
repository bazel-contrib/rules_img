package cachedblob

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// blobURLPattern matches registry blob URLs like /v2/{name}/blobs/{digest}
// The {name} pattern follows OCI Distribution Spec:
// [a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(\/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*
var blobURLPattern = regexp.MustCompile(`^/v2/([a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(\/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*)/blobs/(sha256:[a-f0-9]{64})$`)

// manifestURLPattern matches registry manifest URLs like /v2/{name}/manifests/{reference}
// The {name} pattern follows OCI Distribution Spec:
// [a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(\/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*
var manifestURLPattern = regexp.MustCompile(`^/v2/([a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(\/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*)/manifests/(sha256:[a-f0-9]{64})$`)

type fatalCacheError struct {
	inner error
}

func (e *fatalCacheError) Error() string {
	return fmt.Sprintf("fatal cache error: %v", e.inner)
}

const (
	// maxCacheSize is the maximum size of a blob to cache in memory (1 MiB)
	maxCacheSize = 1024 * 1024
)

// cachedBlob represents a cached blob's data
type cachedBlob struct {
	data []byte // nil if too large to cache
	size int64  // actual size of the blob
}

// Transport is an HTTP transport that serves blobs from local cache when available
type Transport struct {
	// Base is the underlying transport to use for non-cached requests
	Base http.RoundTripper
	// BlobDir is the directory containing cached blobs (the parent of "blobs/sha256")
	BlobDir string
	// Airgapped mode: if true, respond to /v2/ endpoint without forwarding to base transport
	Airgapped bool

	// blobCache stores validated blob data to avoid repeated disk reads
	// Only stores blobs up to maxCacheSize
	blobCache   map[string]*cachedBlob
	blobCacheMu sync.RWMutex

	// mediaTypeCache stores media types for manifests
	mediaTypeCache   map[string]string
	mediaTypeCacheMu sync.RWMutex
}

// RoundTrip implements http.RoundTripper
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil || req.Method != http.MethodGet {
		// Only handle GET requests with valid URL
		return t.fallbackRoundTrip(req)
	}

	// Handle /v2/ endpoint in airgapped mode
	if t.Airgapped && req.URL.Path == "/v2/" {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Body:          io.NopCloser(strings.NewReader("{}")),
			ContentLength: 2,
			Header: http.Header{
				"Content-Type":   []string{"application/json"},
				"Content-Length": []string{"2"},
			},
		}, nil
	}

	// Check if this is a blob or manifest request
	digest, isManifest := t.matchRequest(req)

	if len(digest) == 0 {
		// Not a blob/manifest request, fall back to base transport
		return t.fallbackRoundTrip(req)
	}

	// Try to serve from local cache
	if resp, err := t.serveFromCache(digest, isManifest); err == nil {
		return resp, nil
	} else if _, ok := err.(*fatalCacheError); ok {
		// Fatal cache error, do not fall back to network
		return nil, err
	}

	// Fall back to the base transport
	return t.fallbackRoundTripAndCache(req, digest, isManifest)
}

func (t *Transport) fallbackRoundTrip(req *http.Request) (*http.Response, error) {
	if t.Airgapped {
		return nil, fmt.Errorf("%s request to %s cannot be fulfilled in airgapped mode", req.Method, req.URL.String())
	}
	if t.Base == nil {
		t.Base = http.DefaultTransport
	}
	return t.Base.RoundTrip(req)
}

func (t *Transport) fallbackRoundTripAndCache(req *http.Request, digest string, isManifest bool) (*http.Response, error) {
	resp, err := t.fallbackRoundTrip(req)
	if err != nil {
		return nil, err
	}
	// If this was a manifest request, store the media type
	if isManifest && resp.StatusCode == http.StatusOK {
		contentType := resp.Header.Get("Content-Type")
		if contentType != "" {
			t.mediaTypeCacheMu.Lock()
			if t.mediaTypeCache == nil {
				t.mediaTypeCache = make(map[string]string)
			}
			t.mediaTypeCache[digest] = contentType
			t.mediaTypeCacheMu.Unlock()
		}
	}
	// If the blob is small enough, cache it
	if resp.StatusCode == http.StatusOK && resp.ContentLength > 0 && resp.ContentLength <= maxCacheSize {
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}

		// Validate digest
		hasher := sha256.New()
		hasher.Write(body)
		actualDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))

		if actualDigest == digest {
			// Cache the blob data
			t.blobCacheMu.Lock()
			if t.blobCache == nil {
				t.blobCache = make(map[string]*cachedBlob)
			}
			t.blobCache[digest] = &cachedBlob{data: body, size: int64(len(body))}
			t.blobCacheMu.Unlock()
		}
		// Return new response with body
		return &http.Response{
			StatusCode:    resp.StatusCode,
			Status:        resp.Status,
			Body:          io.NopCloser(bytes.NewReader(body)),
			Header:        resp.Header,
			ContentLength: resp.ContentLength,
		}, nil
	}
	return resp, nil
}

func (t *Transport) matchRequest(req *http.Request) (digest string, isManifest bool) {
	if matches := blobURLPattern.FindStringSubmatch(req.URL.Path); len(matches) > 7 {
		// The OCI name regex has multiple capture groups
		// matches[1] is the full repository name
		// matches[7] is the digest
		digest = matches[7]
		isManifest = false
	} else if matches := manifestURLPattern.FindStringSubmatch(req.URL.Path); len(matches) > 7 {
		// The OCI name regex has multiple capture groups
		// matches[1] is the full repository name
		// matches[7] is the digest
		digest = matches[7]
		isManifest = true
	}
	return digest, isManifest
}

// detectMediaType attempts to detect the media type of a manifest
func detectMediaType(data []byte) string {
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "application/octet-stream"
	}

	// Check if mediaType field exists
	if mt, ok := manifest["mediaType"].(string); ok {
		return mt
	}

	// Try to infer from structure
	if _, hasConfig := manifest["config"]; hasConfig {
		if _, hasLayers := manifest["layers"]; hasLayers {
			// Likely an OCI image manifest
			return "application/vnd.oci.image.manifest.v1+json"
		}
	}

	if _, hasManifests := manifest["manifests"]; hasManifests {
		// Likely an OCI image index
		return "application/vnd.oci.image.index.v1+json"
	}

	// Default to generic JSON
	return "application/json"
}

// serveFromCache attempts to serve a blob from the local cache
func (t *Transport) serveFromCache(digest string, isManifest bool) (*http.Response, error) {
	// Check blob cache first
	t.blobCacheMu.RLock()
	cached, found := t.blobCache[digest]
	t.blobCacheMu.RUnlock()

	if found {
		// We have a cached result
		// Determine content type
		contentType := "application/octet-stream"
		if isManifest {
			// Check media type cache
			t.mediaTypeCacheMu.RLock()
			if mt, ok := t.mediaTypeCache[digest]; ok {
				contentType = mt
			}
			t.mediaTypeCacheMu.RUnlock()
		}

		// If data is cached in memory, use it
		if cached.data != nil {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Status:        "200 OK",
				Body:          io.NopCloser(bytes.NewReader(cached.data)),
				ContentLength: int64(len(cached.data)),
				Header: http.Header{
					"Content-Type":          []string{contentType},
					"Content-Length":        []string{fmt.Sprintf("%d", len(cached.data))},
					"Docker-Content-Digest": []string{digest},
				},
			}, nil
		}

		// Data is too large, read from disk
		sha256sum := strings.TrimPrefix(digest, "sha256:")
		blobPath := filepath.Join(t.BlobDir, "blobs", "sha256", sha256sum)
		blobFile, err := os.Open(blobPath)
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Body:          blobFile,
			ContentLength: cached.size,
			Header: http.Header{
				"Content-Type":          []string{contentType},
				"Content-Length":        []string{fmt.Sprintf("%d", cached.size)},
				"Docker-Content-Digest": []string{digest},
			},
		}, nil
	}

	// Not in cache, try to load from disk
	return t.addToCache(digest, isManifest)
}

func (t *Transport) addToCache(digest string, isManifest bool) (*http.Response, error) {
	// we already know the blob is not in cache, so read from disk
	sha256sum := strings.TrimPrefix(digest, "sha256:")
	blobPath := filepath.Join(t.BlobDir, "blobs", "sha256", sha256sum)
	blobFile, err := os.Open(blobPath)
	if err != nil {
		// This is fine: further up the call stack we will fall back to network
		return nil, err
	}
	// Get file info for size
	fileInfo, err := blobFile.Stat()
	if err != nil {
		blobFile.Close()
		return nil, err
	}
	if fileInfo.Size() <= maxCacheSize || isManifest {
		// read the whole file into memory
		data, err := io.ReadAll(blobFile)
		blobFile.Close()
		if err != nil {
			return nil, err
		}
		// validate digest
		hasher := sha256.New()
		hasher.Write(data)
		actualDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
		if actualDigest != digest {
			_ = blobFile.Close()
			return nil, &fatalCacheError{inner: fmt.Errorf("digest mismatch for blob %s: expected %s, got %s", blobPath, digest, actualDigest)}
		}
		// cache the data
		t.blobCacheMu.Lock()
		if t.blobCache == nil {
			t.blobCache = make(map[string]*cachedBlob)
		}
		t.blobCache[digest] = &cachedBlob{data: data, size: fileInfo.Size()}
		t.blobCacheMu.Unlock()

		// determine content type
		contentType := "application/octet-stream"
		if isManifest {
			contentType = detectMediaType(data)
			// cache the media type
			t.mediaTypeCacheMu.Lock()
			if t.mediaTypeCache == nil {
				t.mediaTypeCache = make(map[string]string)
			}
			t.mediaTypeCache[digest] = contentType
			t.mediaTypeCacheMu.Unlock()
		}

		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Body:          io.NopCloser(bytes.NewReader(data)),
			ContentLength: int64(len(data)),
			Header: http.Header{
				"Content-Type":          []string{contentType},
				"Content-Length":        []string{fmt.Sprintf("%d", len(data))},
				"Docker-Content-Digest": []string{digest},
			},
		}, nil
	}

	// we cache the size only
	t.blobCacheMu.Lock()
	if t.blobCache == nil {
		t.blobCache = make(map[string]*cachedBlob)
	}
	t.blobCache[digest] = &cachedBlob{data: nil, size: fileInfo.Size()}
	t.blobCacheMu.Unlock()
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Body:          blobFile,
		ContentLength: fileInfo.Size(),
		Header: http.Header{
			"Content-Type":          []string{"application/octet-stream"},
			"Content-Length":        []string{fmt.Sprintf("%d", fileInfo.Size())},
			"Docker-Content-Digest": []string{digest},
		},
	}, nil
}

// TransportOption is a functional option for configuring Transport
type TransportOption func(*Transport)

// WithAirgapped configures the transport to run in airgapped mode
func WithAirgapped(airgapped bool) TransportOption {
	return func(t *Transport) {
		t.Airgapped = airgapped
	}
}

// NewTransport creates a new cached blob transport with optional configuration
func NewTransport(blobDir string, base http.RoundTripper, opts ...TransportOption) *Transport {
	t := &Transport{
		Base:           base,
		BlobDir:        blobDir,
		blobCache:      make(map[string]*cachedBlob),
		mediaTypeCache: make(map[string]string),
	}

	for _, opt := range opts {
		opt(t)
	}

	return t
}
