package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"

	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/estargz/zstdchunked"
	"github.com/klauspost/compress/zstd"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/pkg/blobcache"
	"go.podman.io/image/v5/pkg/blobinfocache/none"
	"go.podman.io/image/v5/pkg/compression"
	imgstorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

var (
	_ types.ImageDestination = (*outputImageDestination)(nil)
	_ types.ImageReference   = (*outputImageReference)(nil)
)

func exportImage(ctx context.Context, store storage.Store, imageDigest, blobCacheDir, tmpDir string, estargz bool, baseLayerList []layerMetadata, baseLayers map[string]layerMetadata) (*outputImageDestination, error) {
	storeRef, err := imgstorage.Transport.ParseStoreReference(store, "@"+strings.TrimPrefix(imageDigest, "sha256:"))
	if err != nil {
		return nil, fmt.Errorf("building store reference for %s: %w", imageDigest, err)
	}

	policyCtx, err := insecurePolicyContext()
	if err != nil {
		return nil, err
	}
	defer func() { _ = policyCtx.Destroy() }()

	opts := &copy.Options{SourceCtx: &types.SystemContext{}}
	var source types.ImageSource
	var copySource types.ImageReference = storeRef
	if estargz {
		source, err = storeRef.NewImageSource(ctx, &types.SystemContext{})
		if err != nil {
			return nil, fmt.Errorf("opening store source for estargz layers: %w", err)
		}
		defer func() { _ = source.Close() }()
	} else {
		level := 1
		opts.DestinationCtx = &types.SystemContext{
			CompressionFormat: &compression.Zstd,
			CompressionLevel:  &level,
		}
		cacheRef, err := blobcache.NewBlobCache(storeRef, blobCacheDir, types.Compress, blobcache.WithCompressAlgorithm(&compression.Zstd))
		if err != nil {
			return nil, fmt.Errorf("wrapping store source with blob cache %q: %w", blobCacheDir, err)
		}
		copySource = &baseAwareSourceReference{ImageReference: cacheRef, baseLayerList: baseLayerList}
	}

	dst := newOutputImageDestination(tmpDir, baseLayers, estargz, source)
	if _, err := copy.Image(ctx, policyCtx, dst.ref, copySource, opts); err != nil {
		return nil, fmt.Errorf("exporting image from store: %w", err)
	}
	return dst, nil
}

type outputImageDestination struct {
	ref        types.ImageReference
	tmpDir     string
	baseLayers map[string]layerMetadata
	estargz    bool
	source     types.ImageSource

	mu        sync.Mutex
	blobFiles map[string]string
	manifest  []byte
}

func newOutputImageDestination(tmpDir string, baseLayers map[string]layerMetadata, estargz bool, source types.ImageSource) *outputImageDestination {
	dst := &outputImageDestination{
		tmpDir:     tmpDir,
		baseLayers: baseLayers,
		estargz:    estargz,
		source:     source,
		blobFiles:  map[string]string{},
	}
	dst.ref = &outputImageReference{dst: dst}
	return dst
}

func (d *outputImageDestination) Reference() types.ImageReference { return d.ref }
func (d *outputImageDestination) Close() error                    { return nil }
func (d *outputImageDestination) HasThreadSafePutBlob() bool      { return true }

func (d *outputImageDestination) SupportedManifestMIMETypes() []string     { return nil }
func (d *outputImageDestination) SupportsSignatures(context.Context) error { return nil }
func (d *outputImageDestination) DesiredLayerCompression() types.LayerCompression {
	return types.Compress
}
func (d *outputImageDestination) AcceptsForeignLayerURLs() bool        { return false }
func (d *outputImageDestination) MustMatchRuntimeOS() bool             { return false }
func (d *outputImageDestination) IgnoresEmbeddedDockerReference() bool { return true }

func (d *outputImageDestination) PutBlob(_ context.Context, stream io.Reader, inputInfo types.BlobInfo, _ types.BlobInfoCache, _ bool) (types.BlobInfo, error) {
	isFromBlobCache := hasFileInside(stream)

	f, err := os.CreateTemp(d.tmpDir, "blob_*")
	if err != nil {
		return types.BlobInfo{}, fmt.Errorf("creating blob temp file: %w", err)
	}
	digester := digest.Canonical.Digester()
	n, err := io.Copy(io.MultiWriter(f, digester.Hash()), stream)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return types.BlobInfo{}, fmt.Errorf("writing blob: %w", err)
	}

	dgst := inputInfo.Digest
	if dgst == "" {
		dgst = digester.Digest()
	}
	d.mu.Lock()
	d.blobFiles[dgst.String()] = f.Name()
	d.mu.Unlock()
	if isFromBlobCache {
		logf("Reused %s from blob cache", dgst.Hex())
	} else {
		logf("Exported %s", dgst.Hex())
	}
	return types.BlobInfo{Digest: dgst, Size: n}, nil
}

func hasFileInside(r io.Reader) bool {
	readerType := reflect.TypeFor[io.Reader]()
	fileType := reflect.TypeFor[*os.File]()
	visited := map[uintptr]bool{}
	queue := []reflect.Value{reflect.ValueOf(r)}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		if !v.IsValid() {
			continue
		}
		switch v.Kind() {
		case reflect.Interface:
			queue = append(queue, v.Elem())
		case reflect.Pointer:
			if v.Type() == fileType {
				return true
			}
			if v.IsNil() {
				continue
			}
			if ptr := v.Pointer(); !visited[ptr] {
				visited[ptr] = true
				queue = append(queue, v.Elem())
			}
		case reflect.Struct:
			for _, field := range v.Fields() {
				if leadsToReader(field.Type(), readerType) {
					queue = append(queue, field)
				}
			}
		case reflect.Slice, reflect.Array:
			for i := range v.Len() {
				queue = append(queue, v.Index(i))
			}
		default:
		}
	}
	return false
}

func leadsToReader(t, readerType reflect.Type) bool {
	if t.Implements(readerType) {
		return true
	}
	switch t.Kind() {
	case reflect.Slice, reflect.Array, reflect.Pointer:
		return leadsToReader(t.Elem(), readerType)
	default:
		return false
	}
}

func (d *outputImageDestination) TryReusingBlob(ctx context.Context, info types.BlobInfo, _ types.BlobInfoCache, canSubstitute bool) (bool, types.BlobInfo, error) {
	if base, ok := d.baseLayers[info.Digest.String()]; ok {
		reused := types.BlobInfo{Digest: base.Digest, Size: base.Size, MediaType: base.MediaType}
		if algo := layerCompressionAlgorithm(base.MediaType); algo != nil {
			reused.CompressionOperation = types.Compress
			reused.CompressionAlgorithm = algo
		}
		logf("Skipped %s from base", base.Digest.Hex())
		return true, reused, nil
	}
	if !d.estargz || !canSubstitute {
		return false, types.BlobInfo{}, nil
	}
	reused, err := d.estargzSubstitute(ctx, info)
	if err != nil {
		return false, types.BlobInfo{}, err
	}
	return true, reused, nil
}

func layerCompressionAlgorithm(mediaType string) *compression.Algorithm {
	switch {
	case strings.HasSuffix(mediaType, "+zstd"):
		return &compression.Zstd
	case strings.HasSuffix(mediaType, "+gzip"):
		return &compression.Gzip
	default:
		return nil
	}
}

func (d *outputImageDestination) estargzSubstitute(ctx context.Context, info types.BlobInfo) (types.BlobInfo, error) {
	rc, _, err := d.source.GetBlob(ctx, types.BlobInfo{Digest: info.Digest, Size: info.Size}, none.NoCache)
	if err != nil {
		return types.BlobInfo{}, fmt.Errorf("reading layer %s from store: %w", info.Digest, err)
	}
	defer func() { _ = rc.Close() }()

	f, err := os.CreateTemp(d.tmpDir, "estargz_*")
	if err != nil {
		return types.BlobInfo{}, fmt.Errorf("creating estargz temp file: %w", err)
	}
	defer func() { _ = f.Close() }()

	digester := digest.Canonical.Digester()
	w := estargz.NewWriterWithCompressor(io.MultiWriter(f, digester.Hash()), &zstdchunked.Compressor{CompressionLevel: zstd.SpeedFastest})
	if err := w.AppendTarLossLess(rc); err != nil {
		return types.BlobInfo{}, fmt.Errorf("writing estargz layer %s: %w", info.Digest, err)
	}
	if _, err := w.Close(); err != nil {
		return types.BlobInfo{}, fmt.Errorf("closing estargz layer %s: %w", info.Digest, err)
	}
	if w.DiffID() != info.Digest.String() {
		return types.BlobInfo{}, fmt.Errorf("estargz changed diffID of layer %s to %s", info.Digest, w.DiffID())
	}
	fi, err := f.Stat()
	if err != nil {
		return types.BlobInfo{}, fmt.Errorf("stat estargz layer %s: %w", info.Digest, err)
	}

	dgst := digester.Digest()
	d.mu.Lock()
	d.blobFiles[dgst.String()] = f.Name()
	d.mu.Unlock()
	logf("Recompressed %s as estargz", dgst.Hex())
	return types.BlobInfo{
		Digest:               dgst,
		Size:                 fi.Size(),
		MediaType:            v1.MediaTypeImageLayerZstd,
		CompressionOperation: types.Compress,
		CompressionAlgorithm: &compression.ZstdChunked,
	}, nil
}

func (d *outputImageDestination) PutManifest(_ context.Context, manifest []byte, _ *digest.Digest) error {
	d.mu.Lock()
	d.manifest = manifest
	d.mu.Unlock()
	return nil
}

func (d *outputImageDestination) PutSignatures(context.Context, [][]byte, *digest.Digest) error {
	return nil
}

func (d *outputImageDestination) Commit(context.Context, types.UnparsedImage) error { return nil }

type outputImageReference struct {
	dst *outputImageDestination
}

func (r *outputImageReference) Transport() types.ImageTransport { return baseImageTransport{} }

func (r *outputImageReference) StringWithinTransport() string           { return "buildah-tool-output" }
func (r *outputImageReference) DockerReference() reference.Named        { return nil }
func (r *outputImageReference) PolicyConfigurationIdentity() string     { return "" }
func (r *outputImageReference) PolicyConfigurationNamespaces() []string { return nil }

func (r *outputImageReference) NewImageDestination(context.Context, *types.SystemContext) (types.ImageDestination, error) {
	return r.dst, nil
}

func (r *outputImageReference) NewImageSource(context.Context, *types.SystemContext) (types.ImageSource, error) {
	return nil, errors.New("output image reference is destination-only")
}

func (r *outputImageReference) NewImage(context.Context, *types.SystemContext) (types.ImageCloser, error) {
	return nil, errors.New("output image reference is destination-only")
}

func (r *outputImageReference) DeleteImage(context.Context, *types.SystemContext) error {
	return errors.New("output image reference is destination-only")
}
