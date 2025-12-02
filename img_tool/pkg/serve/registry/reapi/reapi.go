package reapi

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	registry "github.com/google/go-containerregistry/pkg/registry"
	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"google.golang.org/grpc"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
	combined "github.com/bazel-contrib/rules_img/img_tool/pkg/serve/registry"
)

type REAPIBlobHandler struct {
	upstream      registry.BlobStatHandler
	casReader     *cas.CAS
	blobSizeCache *combined.BlobSizeCache
}

func New(upstream registry.BlobStatHandler, clientConn *grpc.ClientConn, blobSizeCache *combined.BlobSizeCache) (*REAPIBlobHandler, error) {
	casReader, err := cas.New(clientConn, cas.WithLearnCapabilities(true))
	if err != nil {
		return nil, err
	}

	return &REAPIBlobHandler{
		upstream:      upstream,
		casReader:     casReader,
		blobSizeCache: blobSizeCache,
	}, nil
}

func (h *REAPIBlobHandler) Get(ctx context.Context, repo string, hash registryv1.Hash) (io.ReadCloser, error) {
	// since we need to know the size of the blob for any REAPI operations,
	// we ask the cache or upstream registry to find out if the blob exists.
	var upstreamSize int64
	if cachedSize, ok := h.blobSizeCache.Get(hash); ok {
		upstreamSize = cachedSize
	} else {
		var upstreamErr error
		upstreamSize, upstreamErr = h.upstream.Stat(ctx, repo, hash)
		if upstreamErr != nil {
			return nil, upstreamErr
		}
	}

	if upstreamSize < 0 {
		return nil, errors.New("unexpected negative blob size")
	}

	digest, err := digestFromDescriptor(hash, upstreamSize)
	if err != nil {
		return nil, fmt.Errorf("unsupported digest algorithm: %s", hash.Algorithm)
	}
	return h.casReader.ReaderForBlob(ctx, digest)
}

func (h *REAPIBlobHandler) Stat(ctx context.Context, repo string, hash registryv1.Hash) (int64, error) {
	// since we need to know the size of the blob for any REAPI operations,
	// we ask the cache or upstream registry to find out if the blob exists.
	var upstreamSize int64
	if cachedSize, ok := h.blobSizeCache.Get(hash); ok {
		upstreamSize = cachedSize
	} else {
		var upstreamErr error
		upstreamSize, upstreamErr = h.upstream.Stat(ctx, repo, hash)
		if upstreamErr != nil {
			return 0, upstreamErr
		}
	}
	if upstreamSize == 0 {
		return 0, nil
	}

	digest, err := digestFromDescriptor(hash, upstreamSize)
	if err != nil {
		return 0, fmt.Errorf("unsupported digest algorithm: %s", hash.Algorithm)
	}
	missing, err := h.casReader.FindMissingBlobs(ctx, []cas.Digest{digest})
	if err != nil {
		return 0, err
	}
	if len(missing) == 0 {
		return upstreamSize, nil // Blob is present.
	}
	return 0, registry.ErrNotFound // Blob is missing.
}

func (h *REAPIBlobHandler) Put(ctx context.Context, repo string, hash v1.Hash, rc io.ReadCloser) error {
	// since we need to know the size of the blob for any REAPI operations,
	// we ask the cache or upstream registry to find out if the blob exists.
	defer rc.Close() // Ensure the reader is closed after use.
	var upstreamSize int64
	if cachedSize, ok := h.blobSizeCache.Get(hash); ok {
		upstreamSize = cachedSize
	} else {
		var upstreamErr error
		upstreamSize, upstreamErr = h.upstream.Stat(ctx, repo, hash)
		if upstreamErr != nil {
			return upstreamErr
		}
	}
	digest, err := digestFromDescriptor(hash, upstreamSize)
	if err != nil {
		return err
	}
	return h.casReader.WriteBlob(ctx, digest, rc)
}

func digestFromDescriptor(hash registryv1.Hash, size int64) (cas.Digest, error) {
	rawHash, err := hex.DecodeString(hash.Hex)
	if err != nil {
		return cas.Digest{}, fmt.Errorf("failed to decode digest hash: %w", err)
	}

	switch hash.Algorithm {
	case "sha256":
		return cas.SHA256(rawHash, size), nil
	case "sha512":
		return cas.SHA512(rawHash, size), nil
	}
	return cas.Digest{}, fmt.Errorf("unsupported digest algorithm: %s", hash.Algorithm)
}
