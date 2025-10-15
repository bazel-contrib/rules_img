package s3

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	registry "github.com/malt3/go-containerregistry/pkg/registry"
	registryv1 "github.com/malt3/go-containerregistry/pkg/v1"
)

type S3BlobHandler struct {
	expires     time.Duration
	minLifetime time.Duration
	signer      *s3.PresignClient
	s3Client    *s3.Client
	objectName  func(repo string, hash registryv1.Hash) (bucket string, key string, err error)
	cache       map[string]cacheEntry
	mux         sync.RWMutex
}

func New(
	ctx context.Context,
	expires time.Duration,
	minLifetime time.Duration,
	objectName func(repo string, hash registryv1.Hash) (bucket string, key string, err error),
	optFns ...func(*config.LoadOptions) error,
) (*S3BlobHandler, error) {
	awsConfig, err := config.LoadDefaultConfig(ctx, optFns...)
	if err != nil {
		return nil, err
	}
	s3Client := s3.NewFromConfig(awsConfig)
	presignClient := s3.NewPresignClient(s3Client)

	return &S3BlobHandler{
		expires:     expires,
		minLifetime: minLifetime,
		signer:      presignClient,
		s3Client:    s3Client,
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

	input := &s3.HeadObjectInput{
		Bucket: &bucket,
		Key:    &key,
	}
	output, err := h.s3Client.HeadObject(ctx, input)
	if err != nil {
		var responseError *awshttp.ResponseError
		if errors.As(err, &responseError) && responseError.HTTPStatusCode() == http.StatusNotFound {
			return cacheEntry{}, registry.ErrNotFound
		}
		return cacheEntry{}, err
	}
	if output.ContentLength == nil {
		return cacheEntry{}, errors.New("ContentLength is nil")
	}

	// positive results can be cached
	presigned, err := h.signer.PresignGetObject(
		ctx,
		&s3.GetObjectInput{Bucket: &bucket, Key: &key},
		s3.WithPresignExpires(h.expires),
	)
	if err != nil {
		return cacheEntry{}, err
	}

	h.mux.Lock()
	defer h.mux.Unlock()
	cacheKey := bucket + "/" + key
	h.cache[cacheKey] = cacheEntry{
		blobSize:     *output.ContentLength,
		presignedURL: presigned.URL,
		expires:      time.Now().Add(h.expires),
	}
	return h.cache[cacheKey], nil // Return the newly cached entry.
}
