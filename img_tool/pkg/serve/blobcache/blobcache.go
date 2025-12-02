package blobcache

import (
	"context"

	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"google.golang.org/grpc"

	blobcache_proto "github.com/bazel-contrib/rules_img/img_tool/pkg/proto/blobcache"
	remoteexecution_proto "github.com/bazel-contrib/rules_img/img_tool/pkg/proto/remote-apis/build/bazel/remote/execution/v2"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/serve/registry"
)

type server struct {
	blobcache_proto.UnimplementedBlobsServer
	casClient remoteexecution_proto.ContentAddressableStorageClient
	sizeCache *registry.BlobSizeCache
}

func NewServer(clientConn *grpc.ClientConn, sizeCache *registry.BlobSizeCache) blobcache_proto.BlobsServer {
	return &server{
		casClient: remoteexecution_proto.NewContentAddressableStorageClient(clientConn),
		sizeCache: sizeCache,
	}
}

func (s *server) Commit(ctx context.Context, req *blobcache_proto.CommitRequest) (*blobcache_proto.CommitResponse, error) {
	if len(req.BlobDigests) == 0 {
		return &blobcache_proto.CommitResponse{}, nil
	}

	// Just forward the request to the REAPI server.
	// We need to learn which blobs are missing from the CAS.
	missingBlobs, err := s.casClient.FindMissingBlobs(ctx, &remoteexecution_proto.FindMissingBlobsRequest{
		BlobDigests:    req.BlobDigests,
		DigestFunction: req.DigestFunction,
	})
	if err != nil {
		return nil, err
	}

	// Update the size cache with the sizes of the blobs that are present.
	missingBlobsMap := map[string]struct{}{}
	for _, digest := range missingBlobs.MissingBlobDigests {
		missingBlobsMap[digest.Hash] = struct{}{}
	}

	for _, digest := range req.BlobDigests {
		hash := hashFromProto(digest, req.DigestFunction)
		if hash.Algorithm == "" {
			continue // Skip unsupported digest algorithms.
		}
		s.sizeCache.Set(hash, digest.SizeBytes)
	}

	return &blobcache_proto.CommitResponse{
		MissingBlobDigests: missingBlobs.MissingBlobDigests,
	}, nil
}

func hashFromProto(digest *remoteexecution_proto.Digest, digestFunction remoteexecution_proto.DigestFunction_Value) registryv1.Hash {
	switch digestFunction {
	case remoteexecution_proto.DigestFunction_SHA256:
		return registryv1.Hash{
			Algorithm: "sha256",
			Hex:       digest.Hash,
		}
	case remoteexecution_proto.DigestFunction_SHA512:
		return registryv1.Hash{
			Algorithm: "sha512",
			Hex:       digest.Hash,
		}
	}
	// Handle other digest functions as needed.
	return registryv1.Hash{}
}
