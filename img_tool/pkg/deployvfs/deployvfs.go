package deployvfs

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/bazelbuild/rules_go/go/runfiles"
	registryname "github.com/google/go-containerregistry/pkg/name"
	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	registrytypes "github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
)

// VFS represents a virtual file system for deployment manifests and their associated blobs.
// It merges multiple data sources into a single coherent view:
// - runfiles tree of the push/load tool
// - registry of base image (if base image is shallow)
// - Bazel remote cache
type VFS struct {
	dm        api.DeployManifest
	blobs     map[string]blobEntry
	manifests map[string]blobEntry
}

func (vfs *VFS) Layer(digest registryv1.Hash) (registryv1.Layer, error) {
	entry, found := vfs.blobs[digest.String()]
	if !found {
		return nil, fmt.Errorf("layer with digest %s not found in VFS", digest.String())
	}
	return entry, nil
}

func (vfs *VFS) ManifestBlob(digest registryv1.Hash) (registryv1.Layer, error) {
	entry, found := vfs.manifests[digest.String()]
	if !found {
		return nil, fmt.Errorf("manifest with digest %s not found in VFS", digest.String())
	}
	return entry, nil
}

func (vfs *VFS) Image(digest registryv1.Hash) (registryv1.Image, error) {
	return newImage(vfs, digest)
}

func (vfs *VFS) ImageIndex(digest registryv1.Hash) (registryv1.ImageIndex, error) {
	return newIndex(vfs, digest)
}

func (vfs *VFS) Taggable(digest registryv1.Hash) (remote.Taggable, error) {
	root, found := vfs.manifests[digest.String()]
	if !found {
		return nil, fmt.Errorf("manifest with digest %s not found in VFS", digest.String())
	}
	mediaType, err := root.MediaType()
	if err != nil {
		return nil, fmt.Errorf("getting media type of manifest %s: %w", digest.String(), err)
	}
	switch mediaType {
	case registrytypes.OCIImageIndex, registrytypes.DockerManifestList:
		return vfs.ImageIndex(digest)
	case registrytypes.OCIManifestSchema1, registrytypes.DockerManifestSchema2:
		return vfs.Image(digest)
	}
	return nil, fmt.Errorf("unsupported media type %s for manifest %s", mediaType, digest.String())
}

func (vfs *VFS) Digests() ([]registryv1.Hash, error) {
	var digests []registryv1.Hash
	for digestStr := range vfs.blobs {
		digest, err := registryv1.NewHash(digestStr)
		if err != nil {
			return nil, fmt.Errorf("parsing blob digest %s: %w", digestStr, err)
		}
		digests = append(digests, digest)
	}
	slices.SortFunc(digests, func(a, b registryv1.Hash) int {
		return strings.Compare(a.String(), b.String())
	})
	digests = slices.Compact(digests)
	return digests, nil
}

func (vfs *VFS) LayersFromRoot(root registryv1.Hash) ([]registryv1.Hash, error) {
	manifest, found := vfs.manifests[root.String()]
	if !found {
		return nil, fmt.Errorf("manifest with digest %s not found in VFS", root.String())
	}
	mediaType, err := manifest.MediaType()
	if err != nil {
		return nil, fmt.Errorf("getting media type of manifest %s: %w", root.String(), err)
	}
	switch mediaType {
	case registrytypes.OCIImageIndex, registrytypes.DockerManifestList:
		return vfs.LayersFromImageIndex(root)
	case registrytypes.OCIManifestSchema1, registrytypes.DockerManifestSchema2:
		return vfs.LayersFromImage(root)
	}
	return nil, fmt.Errorf("unsupported media type %s for manifest %s", mediaType, root.String())
}

func (vfs *VFS) DigestsFromRoot(root registryv1.Hash) ([]registryv1.Hash, error) {
	manifest, found := vfs.manifests[root.String()]
	if !found {
		return nil, fmt.Errorf("manifest with digest %s not found in VFS", root.String())
	}
	mediaType, err := manifest.MediaType()
	if err != nil {
		return nil, fmt.Errorf("getting media type of manifest %s: %w", root.String(), err)
	}
	switch mediaType {
	case registrytypes.OCIImageIndex, registrytypes.DockerManifestList:
		return vfs.DigestsFromImageIndex(root)
	case registrytypes.OCIManifestSchema1, registrytypes.DockerManifestSchema2:
		return vfs.DigestsFromImage(root)
	}
	return nil, fmt.Errorf("unsupported media type %s for manifest %s", mediaType, root.String())
}

func (vfs *VFS) LayersFromImageIndex(root registryv1.Hash) ([]registryv1.Hash, error) {
	imageIndex, err := vfs.ImageIndex(root)
	if err != nil {
		return nil, fmt.Errorf("getting image index for manifest %s: %w", root.String(), err)
	}
	manifest, err := imageIndex.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("getting index manifest for manifest %s: %w", root.String(), err)
	}

	var layers []registryv1.Hash
	for _, manifestDesc := range manifest.Manifests {
		subLayers, err := vfs.LayersFromImage(manifestDesc.Digest)
		if err != nil {
			return nil, fmt.Errorf("getting layers from manifest %s in index %s: %w", manifestDesc.Digest.String(), root.String(), err)
		}
		layers = append(layers, subLayers...)
	}
	return layers, nil
}

func (vfs *VFS) LayersFromImage(root registryv1.Hash) ([]registryv1.Hash, error) {
	image, err := vfs.Image(root)
	if err != nil {
		return nil, fmt.Errorf("getting image for manifest %s: %w", root.String(), err)
	}
	layers, err := image.Layers()
	if err != nil {
		return nil, fmt.Errorf("getting layers for manifest %s: %w", root.String(), err)
	}
	var layerDigests []registryv1.Hash
	for _, layer := range layers {
		layerDigest, err := layer.Digest()
		if err != nil {
			return nil, fmt.Errorf("getting digest for layer of manifest %s: %w", root.String(), err)
		}
		layerDigests = append(layerDigests, layerDigest)
	}
	return layerDigests, nil
}

func (vfs *VFS) DigestsFromImageIndex(root registryv1.Hash) ([]registryv1.Hash, error) {
	imageIndex, err := vfs.ImageIndex(root)
	if err != nil {
		return nil, fmt.Errorf("getting image index for manifest %s: %w", root.String(), err)
	}
	manifest, err := imageIndex.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("getting index manifest for manifest %s: %w", root.String(), err)
	}

	var digests []registryv1.Hash
	for _, manifestDesc := range manifest.Manifests {
		subDigests, err := vfs.DigestsFromImage(manifestDesc.Digest)
		if err != nil {
			return nil, fmt.Errorf("getting digests from manifest %s in index %s: %w", manifestDesc.Digest.String(), root.String(), err)
		}
		digests = append(digests, subDigests...)
	}

	return digests, nil
}

func (vfs *VFS) DigestsFromImage(root registryv1.Hash) ([]registryv1.Hash, error) {
	image, err := vfs.Image(root)
	if err != nil {
		return nil, fmt.Errorf("getting image for manifest %s: %w", root.String(), err)
	}

	var digests []registryv1.Hash
	configDigest, err := image.ConfigName()
	if err != nil {
		return nil, fmt.Errorf("getting config digest for manifest %s: %w", root.String(), err)
	}
	digests = append(digests, configDigest)
	layers, err := image.Layers()
	if err != nil {
		return nil, fmt.Errorf("getting layers for manifest %s: %w", root.String(), err)
	}
	for _, layer := range layers {
		layerDigest, err := layer.Digest()
		if err != nil {
			return nil, fmt.Errorf("getting digest for layer of manifest %s: %w", root.String(), err)
		}
		digests = append(digests, layerDigest)
	}
	return digests, nil
}

func (vfs *VFS) SizeOf(digest registryv1.Hash) (int64, error) {
	entry, found := vfs.blobs[digest.String()]
	if !found {
		if entry, found = vfs.manifests[digest.String()]; !found {
			return 0, fmt.Errorf("blob or manifest with digest %s not found in VFS", digest.String())
		}
	}
	return entry.Size()
}

type vfsBuilder struct {
	dm                       api.DeployManifest
	casReader                casReader
	containerRegistryOptions []remote.Option
}

func Builder(dm api.DeployManifest) *vfsBuilder {
	return &vfsBuilder{dm: dm}
}

func (b *vfsBuilder) WithCASReader(br casReader) *vfsBuilder {
	b.casReader = br
	return b
}

func (b *vfsBuilder) WithContainerRegistryOption(o remote.Option) *vfsBuilder {
	b.containerRegistryOptions = append(b.containerRegistryOptions, o)
	return b
}

func (b *vfsBuilder) Build() (*VFS, error) {
	blobs, manifests, err := b.ingest()
	if err != nil {
		return nil, err
	}
	return &VFS{
		dm:        b.dm,
		blobs:     blobs,
		manifests: manifests,
	}, nil
}

func (b *vfsBuilder) ingest() (map[string]blobEntry, map[string]blobEntry, error) {
	blobs := make(map[string]blobEntry)
	manifests := make(map[string]blobEntry)

	baseOps, err := b.dm.BaseOperations()
	if err != nil {
		return nil, nil, fmt.Errorf("getting base operations: %w", err)
	}
	for i, op := range baseOps {
		var strategy string
		if op.Command == "push" {
			strategy = b.dm.Settings.PushStrategy
		} else {
			strategy = b.dm.Settings.LoadStrategy
		}
		if strategy == "bes" {
			// When pushing via the build event stream,
			// we assume the push happens as a side-effect of the "bazel build" command,
			// so we don't need to upload any blobs ourselves.
			continue
		}
		if op.RootKind == "index" {
			// There must be a "index.json" file in the runfiles
			manifests[op.Root.Digest] = localIndex(i, op.Root)
		}
		for manifestIndex, manifest := range op.Manifests {
			manifests[manifest.Descriptor.Digest] = localManifest(i, manifestIndex, manifest.Descriptor)
			blobs[manifest.Config.Digest] = localConfig(i, manifestIndex, manifest.Config)
			for layerIndex, layer := range manifest.LayerBlobs {
				blob, err := b.layerBlob(i, manifestIndex, layerIndex, strategy, op.PullInfo, manifest, layer)
				if err != nil {
					return nil, nil, fmt.Errorf("locating source for layer with digest %s with index %d in manifest %d of operation %d: %w", layer.Digest, layerIndex, manifestIndex, i, err)
				}
				if existing, found := blobs[layer.Digest]; found {
					// if we already have a blob with this digest, we need to decide which one to keep
					// we try to "upgrade" the source of the blob in the following order:
					// file > (registry == remote_cache) > stub
					if existing.Location == "file" {
						// prefer local file over other sources
						continue
					} else if blob.Location == "file" {
						// prefer local file over other sources
						blobs[layer.Digest] = blob
					} else if existing.Location == "stub" && blob.Location != "stub" {
						// prefer non-stub over stub
						blobs[layer.Digest] = blob
					}
					// else keep existing since we don't improve the source by switching
				} else {
					// this is the first time we see this blob
					blobs[layer.Digest] = blob
				}
			}
		}
	}

	return blobs, manifests, nil
}

func (b *vfsBuilder) layerBlob(operationIndex int, manifestIndex int, layerIndex int, strategy string, pullInfo api.PullInfo, manifestInfo api.ManifestDeployInfo, desc api.Descriptor) (blobEntry, error) {
	// we try the following sources, in order:
	// 1. runfiles tree
	// 2. registry of base image (if base image is shallow, blob was marked as "missing blob" (exists remotely) and strategy allows it)
	// 3. bazel remote cache (lazy strategy)
	// 4. stub blob (cas_registry stategy where all blobs are assumed to already be in the remote CAS)

	if entry, found := b.layerFromFile(operationIndex, manifestIndex, layerIndex, desc); found {
		return entry, nil
	}
	if entry, found := b.layerFromRegistry(pullInfo, manifestInfo.MissingBlobs, desc); found {
		return entry, nil
	}
	switch strategy {
	case "eager":
		return blobEntry{}, fmt.Errorf("layer not found in runfiles (%s) or base image registry, cannot proceed with eager strategy", layerRunfilesPath(operationIndex, manifestIndex, layerIndex))
	case "lazy":
		if entry, found := b.layerFromCAS(desc); found {
			return entry, nil
		}
		return blobEntry{}, fmt.Errorf("layer not found in runfiles (%s) or base image registry, and not found in remote cache, cannot proceed with lazy strategy", layerRunfilesPath(operationIndex, manifestIndex, layerIndex))
	case "cas_registry", "bes":
		// create a stub blob that cannot be read.
		// The push code should never try to read it, since the remote CAS is assumed to already have it.
		// For the bes strategy, we should never try to upload blobs from the client anyways, so this is fine.
		return stubBlob(desc), nil
	}
	return blobEntry{}, fmt.Errorf("unknown push/load strategy: %s", strategy)
}

// layerFromFile tries to find the layer in the runfiles tree. If it exists, it returns the blobEntry and true.
func (b *vfsBuilder) layerFromFile(operationIndex int, manifestIndex int, layerIndex int, desc api.Descriptor) (blobEntry, bool) {
	fpath, err := runfiles.Rlocation(layerRunfilesPath(operationIndex, manifestIndex, layerIndex))
	if err != nil {
		return blobEntry{}, false
	}
	if _, err := os.Stat(fpath); err == nil {
		return blobEntry{
			Descriptor: desc,
			Location:   "file",
			Opener: func() (io.ReadCloser, error) {
				return os.Open(fpath)
			},
		}, true
	}
	return blobEntry{}, false
}

// layerFromRegistry tries to find the layer in the registry of the base image. It returns the blobEntry and true if found.
func (b *vfsBuilder) layerFromRegistry(pullInfo api.PullInfo, missingBlobs []string, desc api.Descriptor) (blobEntry, bool) {
	if len(pullInfo.OriginalBaseImageRegistries) == 0 {
		// no registries provided
		// the layer cannot come from any remote registry
		return blobEntry{}, false
	}

	// get sha256 hex of desc.Digest
	sha256Hex := strings.TrimPrefix(desc.Digest, "sha256:")
	for _, missing := range missingBlobs {
		if missing == sha256Hex {
			// the layer is marked as missing, so it must exist in one of the original registries
			return blobEntry{
				Descriptor: desc,
				Location:   "registry",
				Opener: func() (io.ReadCloser, error) {
					pullInfo := pullInfo
					for _, registry := range pullInfo.OriginalBaseImageRegistries {
						ref, err := registryname.NewDigest(fmt.Sprintf("%s/%s@%s", registry, pullInfo.OriginalBaseImageRepository, desc.Digest))
						if err != nil {
							continue
						}
						layer, err := remote.Layer(ref, b.containerRegistryOptions...)
						if err != nil {
							continue
						}
						rc, err := layer.Compressed()
						if err != nil {
							continue
						}
						return rc, nil
					}
					return nil, fmt.Errorf("layer %s not found in any of the original registries", desc.Digest)
				},
			}, true
		}
	}

	// the layer is not marked as missing, so it must not be a omitted blob
	// from a shallow base image
	return blobEntry{}, false
}

// layerFromCAS tries to find the layer in the bazel remote cache. If it exists, it returns the blobEntry and true.
func (b *vfsBuilder) layerFromCAS(desc api.Descriptor) (blobEntry, bool) {
	if b.casReader == nil {
		return blobEntry{}, false
	}
	return blobEntry{
		Descriptor: desc,
		Location:   "remote_cache",
		Opener: func() (io.ReadCloser, error) {
			casReader := b.casReader
			digest, err := digestFromDescriptor(desc)
			if err != nil {
				return nil, err
			}
			return casReader.ReaderForBlob(context.TODO(), digest)
		},
	}, true
}

func stubBlob(desc api.Descriptor) blobEntry {
	return blobEntry{
		Descriptor: desc,
		Location:   "stub",
		Opener: func() (io.ReadCloser, error) {
			return nil, fmt.Errorf("stub blob: no data available for blob with digest %s", desc.Digest)
		},
	}
}

type blobEntry struct {
	api.Descriptor
	Location string // "file", "registry", "remote_cache", "stub"
	Opener   func() (io.ReadCloser, error)
}

func localIndex(operationIndex int, desc api.Descriptor) blobEntry {
	return blobEntry{
		Descriptor: desc,
		Location:   "file",
		Opener: func() (io.ReadCloser, error) {
			fpath, err := runfiles.Rlocation(path.Join(fmt.Sprintf("%d", operationIndex), "index.json"))
			if err != nil {
				return nil, err
			}
			return os.Open(fpath)
		},
	}
}

func localManifest(operationIndex int, manifestIndex int, desc api.Descriptor) blobEntry {
	return blobEntry{
		Descriptor: desc,
		Location:   "file",
		Opener: func() (io.ReadCloser, error) {
			fpath, err := runfiles.Rlocation(path.Join(fmt.Sprintf("%d", operationIndex), "manifests", fmt.Sprintf("%d", manifestIndex), "manifest.json"))
			if err != nil {
				return nil, err
			}
			return os.Open(fpath)
		},
	}
}

func localConfig(operationIndex int, manifestIndex int, desc api.Descriptor) blobEntry {
	return blobEntry{
		Descriptor: desc,
		Location:   "file",
		Opener: func() (io.ReadCloser, error) {
			fpath, err := runfiles.Rlocation(path.Join(fmt.Sprintf("%d", operationIndex), "manifests", fmt.Sprintf("%d", manifestIndex), "config.json"))
			if err != nil {
				return nil, err
			}
			return os.Open(fpath)
		},
	}
}

func (b blobEntry) Digest() (registryv1.Hash, error) {
	return registryv1.NewHash(b.Descriptor.Digest)
}

func (b blobEntry) DiffID() (registryv1.Hash, error) {
	panic("DiffID on vfs path is not implemented")
}

func (b blobEntry) Compressed() (io.ReadCloser, error) {
	return b.Opener()
}

func (b blobEntry) Uncompressed() (io.ReadCloser, error) {
	panic("Uncompressed on vfs path is not implemented")
}

func (b blobEntry) Size() (int64, error) {
	return b.Descriptor.Size, nil
}

func (b blobEntry) MediaType() (registrytypes.MediaType, error) {
	return registrytypes.MediaType(b.Descriptor.MediaType), nil
}

type casReader interface {
	FindMissingBlobs(ctx context.Context, digests []cas.Digest) ([]cas.Digest, error)
	ReadBlob(ctx context.Context, digest cas.Digest) ([]byte, error)
	ReaderForBlob(ctx context.Context, digest cas.Digest) (io.ReadCloser, error)
}

func digestFromDescriptor(blobMeta api.Descriptor) (cas.Digest, error) {
	hash, err := registryv1.NewHash(blobMeta.Digest)
	if err != nil {
		return cas.Digest{}, fmt.Errorf("failed to parse digest: %w", err)
	}
	return digestFromHashAndSize(hash, blobMeta.Size)
}

func digestFromHashAndSize(hash registryv1.Hash, sizeBytes int64) (cas.Digest, error) {
	rawHash, err := hex.DecodeString(hash.Hex)
	if err != nil {
		return cas.Digest{}, fmt.Errorf("failed to decode digest hash: %w", err)
	}

	switch hash.Algorithm {
	case "sha256":
		return cas.SHA256(rawHash, sizeBytes), nil
	case "sha512":
		return cas.SHA512(rawHash, sizeBytes), nil
	}
	return cas.Digest{}, fmt.Errorf("unsupported digest algorithm: %s", hash.Algorithm)
}

func layerRunfilesPath(operationIndex int, manifestIndex int, layerIndex int) string {
	return path.Join(fmt.Sprintf("%d", operationIndex), "manifests", fmt.Sprintf("%d", manifestIndex), "layer", fmt.Sprintf("%d", layerIndex))
}
