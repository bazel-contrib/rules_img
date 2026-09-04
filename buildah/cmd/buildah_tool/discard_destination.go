package main

import (
	"context"
	"fmt"
	"io"

	digest "github.com/opencontainers/go-digest"
	"go.podman.io/image/v5/types"
)

func discardDestinationLookup(dest types.ImageReference) (types.ImageReference, error) {
	return discardImageReference{ImageReference: dest}, nil
}

type discardImageReference struct {
	types.ImageReference
}

func (r discardImageReference) NewImageDestination(context.Context, *types.SystemContext) (types.ImageDestination, error) {
	return discardImageDestination{ref: r}, nil
}

type discardImageDestination struct {
	ref types.ImageReference
}

func (d discardImageDestination) Reference() types.ImageReference { return d.ref }
func (d discardImageDestination) Close() error                    { return nil }
func (d discardImageDestination) HasThreadSafePutBlob() bool      { return true }

func (d discardImageDestination) SupportedManifestMIMETypes() []string     { return nil }
func (d discardImageDestination) SupportsSignatures(context.Context) error { return nil }
func (d discardImageDestination) DesiredLayerCompression() types.LayerCompression {
	return types.PreserveOriginal
}
func (d discardImageDestination) AcceptsForeignLayerURLs() bool        { return false }
func (d discardImageDestination) MustMatchRuntimeOS() bool             { return false }
func (d discardImageDestination) IgnoresEmbeddedDockerReference() bool { return true }

func (d discardImageDestination) PutBlob(_ context.Context, stream io.Reader, inputInfo types.BlobInfo, _ types.BlobInfoCache, _ bool) (types.BlobInfo, error) {
	digester := digest.Canonical.Digester()
	size, err := io.Copy(digester.Hash(), stream)
	if err != nil {
		return types.BlobInfo{}, fmt.Errorf("draining discarded blob: %w", err)
	}
	dgst := inputInfo.Digest
	if dgst == "" {
		dgst = digester.Digest()
	}
	return types.BlobInfo{Digest: dgst, Size: size}, nil
}

func (d discardImageDestination) TryReusingBlob(_ context.Context, info types.BlobInfo, _ types.BlobInfoCache, _ bool) (bool, types.BlobInfo, error) {
	if info.Size < 0 {
		return false, types.BlobInfo{}, nil
	}
	return true, info, nil
}

func (d discardImageDestination) PutManifest(context.Context, []byte, *digest.Digest) error {
	return nil
}

func (d discardImageDestination) PutSignatures(context.Context, [][]byte, *digest.Digest) error {
	return nil
}

func (d discardImageDestination) Commit(context.Context, types.UnparsedImage) error { return nil }
