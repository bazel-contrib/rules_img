package registry

import (
	"context"
	"errors"
	"io"

	"github.com/google/go-containerregistry/pkg/registry"
	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

type combinedBlobStore struct {
	blobStores []Handler
	writer     Writer
	sizeCache  *BlobSizeCache
}

func NewCombinedBlobStore(sizeCache *BlobSizeCache, writer Writer, blobStores ...Handler) registry.BlobHandler {
	return &combinedBlobStore{
		blobStores: blobStores,
		writer:     writer,
		sizeCache:  sizeCache,
	}
}

func (c *combinedBlobStore) Get(ctx context.Context, repo string, hash registryv1.Hash) (io.ReadCloser, error) {
	for _, store := range c.blobStores {
		reader, err := store.Get(ctx, repo, hash)
		if err == nil {
			return reader, nil
		}
		var rerr registry.RedirectError
		if errors.As(err, &rerr) {
			// If we get a redirect error, we return it immediately.
			return nil, rerr
		}
		if err != registry.ErrNotFound {
			return nil, err
		}
		// not found errors are ignored, we try the next store.
	}
	return nil, registry.ErrNotFound
}

func (c *combinedBlobStore) Stat(ctx context.Context, repo string, hash registryv1.Hash) (int64, error) {
	for _, store := range c.blobStores {
		size, err := store.Stat(ctx, repo, hash)
		if err == nil {
			return size, nil
		}
		var rerr registry.RedirectError
		if errors.As(err, &rerr) {
			// If we get a redirect error, we return it immediately.
			return size, rerr
		}
		if err != registry.ErrNotFound {
			return size, err
		}
		// not found errors are ignored, we try the next store.
	}
	return 0, registry.ErrNotFound
}

func (c *combinedBlobStore) Put(ctx context.Context, repo string, h v1.Hash, rc io.ReadCloser) error {
	if c.writer == nil {
		return errors.New("registry is configured to be read-only")
	}
	return c.writer.Put(ctx, repo, h, rc)
}

type Handler interface {
	Stat(ctx context.Context, repo string, hash v1.Hash) (int64, error)
	Get(ctx context.Context, repo string, hash v1.Hash) (io.ReadCloser, error)
}

type Writer interface {
	Put(ctx context.Context, repo string, h v1.Hash, rc io.ReadCloser) error
}
