package main

import (
	"context"
	"fmt"

	digest "github.com/opencontainers/go-digest"
	"go.podman.io/image/v5/types"
)

var (
	_ types.ImageReference = (*baseAwareSourceReference)(nil)
	_ types.ImageSource    = (*baseAwareSource)(nil)
)

type baseAwareSourceReference struct {
	types.ImageReference
	baseLayerList []layerMetadata
}

func (r *baseAwareSourceReference) NewImageSource(ctx context.Context, sys *types.SystemContext) (types.ImageSource, error) {
	src, err := r.ImageReference.NewImageSource(ctx, sys)
	if err != nil {
		return nil, fmt.Errorf("opening base-aware image source: %w", err)
	}
	return &baseAwareSource{ImageSource: src, baseLayerList: r.baseLayerList}, nil
}

type baseAwareSource struct {
	types.ImageSource
	baseLayerList []layerMetadata
}

func (s *baseAwareSource) LayerInfosForCopy(ctx context.Context, instanceDigest *digest.Digest) ([]types.BlobInfo, error) {
	infos, err := s.ImageSource.LayerInfosForCopy(ctx, instanceDigest)
	if err != nil {
		return nil, fmt.Errorf("reading layer infos for copy: %w", err)
	}
	if infos == nil {
		return nil, nil
	}
	if len(infos) < len(s.baseLayerList) {
		return nil, fmt.Errorf("layer infos shorter than base layer list | infos: %d base: %d", len(infos), len(s.baseLayerList))
	}
	for i, base := range s.baseLayerList {
		if base.Digest == "" {
			continue
		}
		infos[i] = types.BlobInfo{Digest: base.Digest, Size: base.Size, MediaType: base.MediaType}
	}
	return infos, nil
}
