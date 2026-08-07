package registry

import (
	"context"
	"errors"
	"io"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/registry"
	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// Member is one blob store in a combined store, together with whether this
// registry may delete from it.
//
// Prunable is off by default, and deliberately so: the stores this registry
// reads from are shared. Bazel's CAS and an upstream registry hold blobs that
// other clients put there and still expect to find, so a manifest going out of
// scope here says nothing about whether the bytes should go. Only a store this
// registry alone owns should be prunable.
type Member struct {
	Handler  Handler
	Prunable bool
}

type combinedBlobStore struct {
	blobStores []Handler
	writer     Writer
	sizeCache  *BlobSizeCache
}

// deletableCombinedBlobStore is a combined store with at least one prunable
// member. It is a separate type so that a store with nothing prunable does not
// advertise registry.BlobDeleteHandler at all, and blob deletes against it keep
// answering "unsupported" instead of quietly doing nothing.
type deletableCombinedBlobStore struct {
	*combinedBlobStore
	prunable []Handler
}

func NewCombinedBlobStore(sizeCache *BlobSizeCache, writer Writer, members ...Member) registry.BlobHandler {
	combined := &combinedBlobStore{
		writer:    writer,
		sizeCache: sizeCache,
	}
	var prunable []Handler
	for _, member := range members {
		combined.blobStores = append(combined.blobStores, member.Handler)
		if member.Prunable {
			prunable = append(prunable, member.Handler)
		}
	}
	if len(prunable) == 0 {
		return combined
	}
	return &deletableCombinedBlobStore{combinedBlobStore: combined, prunable: prunable}
}

// ReadOnlyMembers wraps handlers as members this registry must not delete from.
func ReadOnlyMembers(handlers ...Handler) []Member {
	members := make([]Member, 0, len(handlers))
	for _, handler := range handlers {
		members = append(members, Member{Handler: handler})
	}
	return members
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

// Delete removes a blob from every prunable member. A member that does not hold
// the blob is not an error: the point is that the blob is gone afterwards.
func (c *deletableCombinedBlobStore) Delete(ctx context.Context, repo string, h v1.Hash) error {
	var errs []error
	for _, store := range c.prunable {
		deleter, ok := store.(registry.BlobDeleteHandler)
		if !ok {
			continue
		}
		if err := deleter.Delete(ctx, repo, h); err != nil && !errors.Is(err, registry.ErrNotFound) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type Handler interface {
	Stat(ctx context.Context, repo string, hash v1.Hash) (int64, error)
	Get(ctx context.Context, repo string, hash v1.Hash) (io.ReadCloser, error)
}

type Writer interface {
	Put(ctx context.Context, repo string, h v1.Hash, rc io.ReadCloser) error
}
