package main

import (
	"context"
	"fmt"
	"io"
	"sync"

	digest "github.com/opencontainers/go-digest"
	"go.podman.io/image/v5/types"
)

type blobUpload struct {
	done chan struct{}
	info types.BlobInfo
	err  error
}

type blobCoordinator struct {
	mu    sync.Mutex
	blobs map[string]*blobUpload
}

func newBlobCoordinator() *blobCoordinator {
	return &blobCoordinator{blobs: map[string]*blobUpload{}}
}

func blobKey(repo string, dgst digest.Digest) string {
	return repo + "\n" + dgst.String()
}

func (c *blobCoordinator) lead(key string) (*blobUpload, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.blobs[key]; ok {
		return entry, false
	}
	entry := &blobUpload{done: make(chan struct{})}
	c.blobs[key] = entry
	return entry, true
}

func (c *blobCoordinator) finish(key string, entry *blobUpload, info types.BlobInfo, err error) {
	entry.info = info
	entry.err = err
	if err != nil {
		c.mu.Lock()
		delete(c.blobs, key)
		c.mu.Unlock()
	}
	close(entry.done)
}

func (c *blobCoordinator) recordPresent(key string, info types.BlobInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.blobs[key]; ok {
		return
	}
	entry := &blobUpload{done: make(chan struct{}), info: info}
	close(entry.done)
	c.blobs[key] = entry
}

func (c *blobCoordinator) lookup(key string) (*blobUpload, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.blobs[key]
	return entry, ok
}

type coordinatedImageReference struct {
	types.ImageReference
	coordinator *blobCoordinator
}

func (r *coordinatedImageReference) NewImageDestination(ctx context.Context, sys *types.SystemContext) (types.ImageDestination, error) {
	inner, err := r.ImageReference.NewImageDestination(ctx, sys)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	repo := ""
	if named := r.DockerReference(); named != nil {
		repo = named.Name()
	}
	return &coordinatedImageDestination{ImageDestination: inner, coordinator: r.coordinator, repo: repo}, nil
}

type coordinatedImageDestination struct {
	types.ImageDestination
	coordinator *blobCoordinator
	repo        string
}

func (d *coordinatedImageDestination) PutBlob(ctx context.Context, stream io.Reader, inputInfo types.BlobInfo, cache types.BlobInfoCache, isConfig bool) (types.BlobInfo, error) {
	if d.repo == "" || inputInfo.Digest == "" {
		if inputInfo.Digest == "" {
			logf("Uploading cache blob with unknown digest | repo: %s size: %d", d.repo, inputInfo.Size)
		}
		return d.ImageDestination.PutBlob(ctx, stream, inputInfo, cache, isConfig) //nolint:wrapcheck
	}
	key := blobKey(d.repo, inputInfo.Digest)
	entry, leader := d.coordinator.lead(key)
	if leader {
		info, err := d.ImageDestination.PutBlob(ctx, stream, inputInfo, cache, isConfig)
		d.coordinator.finish(key, entry, info, err)
		return info, err //nolint:wrapcheck
	}
	if _, err := io.Copy(io.Discard, stream); err != nil {
		return types.BlobInfo{}, fmt.Errorf("draining deduplicated blob %s: %w", inputInfo.Digest, err)
	}
	<-entry.done
	if entry.err != nil {
		return types.BlobInfo{}, fmt.Errorf("concurrent upload of blob %s failed: %w", inputInfo.Digest, entry.err)
	}
	logf("Deduplicated concurrent cache blob upload | repo: %s digest: %s", d.repo, inputInfo.Digest)
	return entry.info, nil
}

func (d *coordinatedImageDestination) TryReusingBlob(ctx context.Context, info types.BlobInfo, cache types.BlobInfoCache, canSubstitute bool) (bool, types.BlobInfo, error) {
	if d.repo == "" || info.Digest == "" {
		return d.ImageDestination.TryReusingBlob(ctx, info, cache, canSubstitute) //nolint:wrapcheck
	}
	key := blobKey(d.repo, info.Digest)
	if entry, ok := d.coordinator.lookup(key); ok {
		<-entry.done
		if entry.err == nil {
			if *flagDebug {
				logf("Reused already pushed cache blob | repo: %s digest: %s", d.repo, info.Digest)
			}
			return true, entry.info, nil
		}
	}
	reused, reusedInfo, err := d.ImageDestination.TryReusingBlob(ctx, info, cache, canSubstitute)
	if err == nil && reused {
		d.coordinator.recordPresent(key, reusedInfo)
	}
	return reused, reusedInfo, err //nolint:wrapcheck
}
