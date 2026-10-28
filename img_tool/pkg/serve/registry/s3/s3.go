package s3

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/aws"
	awss3 "github.com/bazel-contrib/rules_img/img_tool/pkg/aws/s3"
	registry "github.com/bazel-contrib/rules_img/img_tool/pkg/registry"
	registryv1 "github.com/google/go-containerregistry/pkg/v1"
)

type S3BlobHandler struct {
	expires     time.Duration
	minLifetime time.Duration
	s3Client    *awss3.Client
	objectName  func(repo string, hash registryv1.Hash) (bucket string, key string, err error)
	cache       map[string]cacheEntry
	mux         sync.RWMutex
}

func New(
	ctx context.Context,
	expires time.Duration,
	minLifetime time.Duration,
	objectName func(repo string, hash registryv1.Hash) (bucket string, key string, err error),
	optFns ...aws.LoadOption,
) (*S3BlobHandler, error) {
	awsConfig, err := aws.LoadDefaultConfig(ctx, optFns...)
	if err != nil {
		return nil, err
	}

	return &S3BlobHandler{
		expires:     expires,
		minLifetime: minLifetime,
		s3Client:    awss3.New(awsConfig),
		objectName:  objectName,
		cache:       make(map[string]cacheEntry),
	}, nil
}

func (h *S3BlobHandler) Get(ctx context.Context, repo string, hash registryv1.Hash) (io.ReadCloser, error) {
	// We always want to return a redirect
	// or some error if the blob is not found.
	// This is identical to the Stat handler.
	_, err := h.Stat(ctx, repo, hash)
	return nil, err
}

func (h *S3BlobHandler) Stat(ctx context.Context, repo string, hash registryv1.Hash) (int64, error) {
	bucket, key, err := h.objectName(repo, hash)
	if err != nil {
		return 0, err // Invalid object name.
	}
	cached, err := h.ensureCached(ctx, bucket, key)
	if err != nil {
		return cached.blobSize, err
	}
	return cached.blobSize, registry.RedirectError{
		Location: cached.presignedURL,
		Code:     http.StatusFound,
	}
}

type cacheEntry struct {
	blobSize     int64
	presignedURL string
	expires      time.Time
}

func (h *S3BlobHandler) getCached(bucket, key string) *cacheEntry {
	cacheKey := bucket + "/" + key
	h.mux.RLock()
	if entry, found := h.cache[cacheKey]; found {
		if entry.expires.After(time.Now().Add(-h.minLifetime)) {
			h.mux.RUnlock()
			return &entry // Return cached entry if it is still valid.
		}
		// Cache entry is expired, remove it.
		h.mux.RUnlock()
		h.mux.Lock()
		delete(h.cache, cacheKey)
		h.mux.Unlock()
	} else {
		// Cache miss, check S3.
		h.mux.RUnlock()
	}
	return nil // No valid cache entry found.
}

func (h *S3BlobHandler) ensureCached(ctx context.Context, bucket, key string) (cacheEntry, error) {
	if cached := h.getCached(bucket, key); cached != nil {
		return *cached, nil // Return cached entry if it exists.
	}

	size, err := h.s3Client.HeadObject(ctx, bucket, key)
	if err != nil {
		if errors.Is(err, awss3.ErrNotFound) {
			return cacheEntry{}, registry.ErrNotFound
		}
		return cacheEntry{}, err
	}

	// positive results can be cached
	presigned, err := h.s3Client.PresignGetObject(ctx, bucket, key, h.expires)
	if err != nil {
		return cacheEntry{}, err
	}

	h.mux.Lock()
	defer h.mux.Unlock()
	cacheKey := bucket + "/" + key
	h.cache[cacheKey] = cacheEntry{
		blobSize:     size,
		presignedURL: presigned,
		expires:      time.Now().Add(h.expires),
	}
	return h.cache[cacheKey], nil // Return the newly cached entry.
}
