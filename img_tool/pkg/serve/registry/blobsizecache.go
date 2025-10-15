package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"

	"github.com/malt3/go-containerregistry/pkg/registry"
	registryv1 "github.com/malt3/go-containerregistry/pkg/v1"
	"github.com/malt3/go-containerregistry/pkg/v1/types"
)

type BlobSizeCache struct {
	cache map[string]int64
	mux   sync.RWMutex
}

func NewBlobSizeCache() *BlobSizeCache {
	return &BlobSizeCache{
		cache: make(map[string]int64),
	}
}

func (b *BlobSizeCache) Get(hash registryv1.Hash) (int64, bool) {
	b.mux.RLock()
	defer b.mux.RUnlock()
	size, ok := b.cache[hash.String()]
	return size, ok
}

func (b *BlobSizeCache) Set(hash registryv1.Hash, size int64) {
	b.mux.Lock()
	defer b.mux.Unlock()
	b.cache[hash.String()] = size
}

type BlobSizeCacheCallback struct {
	sizeCache *BlobSizeCache
	handler   Handler
}

func NewBlobSizeCacheCallback(sizeCache *BlobSizeCache, handler Handler) BlobSizeCacheCallback {
	return BlobSizeCacheCallback{
		sizeCache: sizeCache,
		handler:   handler,
	}
}

func (b BlobSizeCacheCallback) ManifestPutCallback(repo, target, contentType string, blob []byte) error {
	mediaType := types.MediaType(contentType)
	if !mediaType.IsIndex() && !mediaType.IsImage() && !mediaType.IsConfig() {
		return nil // Not an image or index, no size to cache
	}

	hash, n, err := registryv1.SHA256(bytes.NewReader(blob))
	if err != nil {
		return err
	}
	b.sizeCache.Set(hash, n)

	if mediaType.IsIndex() {
		// For indexes, we cache the size of each referenced blob.
		index, err := registryv1.ParseIndexManifest(bytes.NewReader(blob))
		if err != nil {
			return err
		}

		return b.cacheFromIndex(repo, index)
	} else if mediaType.IsImage() {
		manifest, err := registryv1.ParseManifest(bytes.NewReader(blob))
		if err != nil {
			return err
		}

		b.cacheFromManifest(repo, manifest)
	}

	return nil
}

func (b BlobSizeCacheCallback) cacheFromIndex(repo string, index *registryv1.IndexManifest) error {
	for _, desc := range index.Manifests {
		if desc.Size > 0 {
			b.sizeCache.Set(desc.Digest, desc.Size)
		}
		if types.MediaType(desc.MediaType).IsImage() {
			manifestData, err := b.get(repo, desc.Digest)
			if err != nil {
				return err
			}
			manifest, err := registryv1.ParseManifest(bytes.NewReader(manifestData))
			if err != nil {
				return err
			}
			b.cacheFromManifest(repo, manifest)
		} else if types.MediaType(desc.MediaType).IsIndex() {
			manifestData, err := b.get(repo, desc.Digest)
			if err != nil {
				return err
			}
			indexManifest, err := registryv1.ParseIndexManifest(bytes.NewReader(manifestData))
			if err != nil {
				return err
			}
			if err := b.cacheFromIndex(repo, indexManifest); err != nil {
				return err
			}
		}

	}
	return nil
}

func (b BlobSizeCacheCallback) cacheFromManifest(repo string, manifest *registryv1.Manifest) {
	for _, layer := range manifest.Layers {
		if layer.Size > 0 {
			b.sizeCache.Set(layer.Digest, layer.Size)
			if layer.MediaType.IsLayer() {
				// internal consistency checks
				statSize, statErr := b.handler.Stat(context.TODO(), repo, layer.Digest)
				if statErr != nil {
					fmt.Fprintf(os.Stderr, "image PUT (%s) is not consistent. Missing layer blob %s: %v\n", repo, layer.Digest, statErr)
				} else if statSize != layer.Size {
					fmt.Fprintf(os.Stderr, "image PUT (%s) is not consistent. Layer blob %s size mismatch: expected %d, got %d\n", repo, layer.Digest, layer.Size, statSize)
				}
			}
		}
	}
	if manifest.Config.Size > 0 {
		b.sizeCache.Set(manifest.Config.Digest, manifest.Config.Size)
	}
}

func (b BlobSizeCacheCallback) get(repo string, hash registryv1.Hash) ([]byte, error) {
	reader, err := b.handler.Get(context.TODO(), repo, hash)
	if err == nil {
		defer func() {
			if closeErr := reader.Close(); closeErr != nil {
				fmt.Fprintf(os.Stderr, "Error closing reader: %v\n", closeErr)
			}
		}()
		data, err := io.ReadAll(reader)
		if err != nil {
			return nil, fmt.Errorf("failed to read blob %s: %w", hash.String(), err)
		}
		return data, nil
	}
	if err == registry.ErrNotFound {
		return nil, fmt.Errorf("blob %s not found in repository %s", hash.String(), repo)
	}
	var rerr registry.RedirectError
	if !errors.As(err, &rerr) {
		return nil, err
	}

	// let's hope and pray that the redirect location
	// is valid and can be fetched without further authentication
	resp, err := http.Get(rerr.Location)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch blob %s from redirect location %s: %w", hash.String(), rerr.Location, err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "Error closing response body: %v\n", closeErr)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch blob %s from redirect location %s: %s", hash.String(), rerr.Location, resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read blob %s from redirect location %s: %w", hash.String(), rerr.Location, err)
	}
	return data, nil
}
