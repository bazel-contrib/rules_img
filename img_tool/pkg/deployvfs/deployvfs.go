package deployvfs

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/bazelbuild/rules_go/go/runfiles"
	registryname "github.com/malt3/go-containerregistry/pkg/name"
	registryv1 "github.com/malt3/go-containerregistry/pkg/v1"
	"github.com/malt3/go-containerregistry/pkg/v1/remote"
	registrytypes "github.com/malt3/go-containerregistry/pkg/v1/types"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
)

// Stats tracks statistics about blob access in the VFS.
// All fields are accessed atomically for thread safety.
type Stats struct {
	LayersFromLocalDisk   atomic.Uint64 // Layers opened from local disk
	LayersFromRegistry    atomic.Uint64 // Layers opened from registry
	LayersFromRemoteCache atomic.Uint64 // Layers opened from remote cache
}

// VFS represents a virtual file system for deployment manifests and their associated blobs.
// It merges multiple data sources into a single coherent view:
// - runfiles tree of the push/load tool
// - registry of base image (if base image is shallow)
// - undeclared local output files (via layer hints)
// - Bazel remote cache
type VFS struct {
	dm        api.DeployManifest
	blobs     map[string]blobEntry
	manifests map[string]blobEntry
	stats     *Stats
}

// Stats returns the current statistics for the VFS.
func (vfs *VFS) Stats() *Stats {
	return vfs.stats
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
	dm                         api.DeployManifest
	casReader                  casReader
	containerRegistryOptions   []remote.Option
	runfilesRootSymlinksPrefix string
	layerHints                 map[string][]string // digest -> []paths
	stats                      *Stats
}

func Builder(dm api.DeployManifest) *vfsBuilder {
	return &vfsBuilder{
		dm:    dm,
		stats: &Stats{},
	}
}

func (b *vfsBuilder) WithCASReader(br casReader) *vfsBuilder {
	b.casReader = br
	return b
}

func (b *vfsBuilder) WithContainerRegistryOption(o remote.Option) *vfsBuilder {
	b.containerRegistryOptions = append(b.containerRegistryOptions, o)
	return b
}

func (b *vfsBuilder) WithRunfilesRootSymlinksPrefix(prefix string) *vfsBuilder {
	b.runfilesRootSymlinksPrefix = prefix
	return b
}

// rlocation wraps runfiles.Rlocation and adds the runfiles root symlinks prefix if configured.
func (b *vfsBuilder) rlocation(runfilesPath string) (string, error) {
	fullPath := runfilesPath
	if b.runfilesRootSymlinksPrefix != "" {
		fullPath = path.Join(b.runfilesRootSymlinksPrefix, runfilesPath)
	}
	return runfiles.Rlocation(fullPath)
}

func (b *vfsBuilder) Build() (*VFS, error) {
	// Try to load layer hints if available
	if err := b.loadLayerHints(); err != nil {
		// Layer hints are optional, log but don't fail
		// We could add debug logging here if needed
		_ = err
	}

	blobs, manifests, err := b.ingest()
	if err != nil {
		return nil, err
	}
	return &VFS{
		dm:        b.dm,
		blobs:     blobs,
		manifests: manifests,
		stats:     b.stats,
	}, nil
}

// loadLayerHints attempts to load layer hints from the runfiles.
// Layer hints are only enabled if:
// 1. BUILD_WORKSPACE_DIRECTORY environment variable is set
// 2. A layer_hints file exists under the runfiles prefix
func (b *vfsBuilder) loadLayerHints() error {
	// Check if BUILD_WORKSPACE_DIRECTORY is set
	workspaceDir := os.Getenv("BUILD_WORKSPACE_DIRECTORY")
	if workspaceDir == "" {
		return fmt.Errorf("BUILD_WORKSPACE_DIRECTORY not set, layer hints disabled")
	}

	// Try to find layer_hints file in runfiles
	layerHintsPath, err := b.rlocation("layer_hints")
	if err != nil {
		return fmt.Errorf("layer_hints file not found in runfiles: %w", err)
	}

	// Check if file exists
	if _, err := os.Stat(layerHintsPath); err != nil {
		return fmt.Errorf("layer_hints file does not exist: %w", err)
	}

	// Parse the layer hints file
	hints, err := parseLayerHints(layerHintsPath, workspaceDir)
	if err != nil {
		return fmt.Errorf("parsing layer hints file: %w", err)
	}

	b.layerHints = hints
	return nil
}

// parseLayerHints reads a layer hints file and returns a map of digest -> []paths.
// File format: digest\0path1\0path2...\n
// Paths are made absolute by joining with workspaceDir.
func parseLayerHints(hintsPath string, workspaceDir string) (map[string][]string, error) {
	file, err := os.Open(hintsPath)
	if err != nil {
		return nil, fmt.Errorf("opening layer hints file: %w", err)
	}
	defer file.Close()

	hints := make(map[string][]string)
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue // skip empty lines
		}

		// Split by null byte
		parts := strings.Split(line, "\x00")
		if len(parts) < 1 {
			return nil, fmt.Errorf("invalid line format: expected at least 1 part")
		}

		digest := parts[0]
		paths := parts[1:]

		// Make paths absolute by joining with workspace directory
		absolutePaths := make([]string, len(paths))
		for i, p := range paths {
			absolutePaths[i] = filepath.FromSlash(path.Join(workspaceDir, p))
		}

		hints[digest] = absolutePaths
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading layer hints file: %w", err)
	}

	return hints, nil
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
			manifests[op.Root.Digest] = b.localIndex(i, op.Root)
		}
		for manifestIndex, manifest := range op.Manifests {
			manifests[manifest.Descriptor.Digest] = b.localManifest(i, manifestIndex, manifest.Descriptor)
			blobs[manifest.Config.Digest] = b.localConfig(i, manifestIndex, manifest.Config)
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
	fpath, err := b.rlocation(layerRunfilesPath(operationIndex, manifestIndex, layerIndex))
	if err != nil {
		return blobEntry{}, false
	}
	if _, err := os.Stat(fpath); err == nil {
		stats := b.stats
		return blobEntry{
			Descriptor: desc,
			Location:   "file",
			stats:      stats,
			Opener: func() (io.ReadCloser, error) {
				stats.LayersFromLocalDisk.Add(1)
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
			stats := b.stats
			return blobEntry{
				Descriptor: desc,
				Location:   "registry",
				stats:      stats,
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
						stats.LayersFromRegistry.Add(1)
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

// layerFromCAS tries to find the layer using a two-step fallback strategy:
// 1. If layer hints are available, try to read from local paths in BUILD_WORKSPACE_DIRECTORY
// 2. Fall back to reading from the bazel remote cache
// If it exists in either location, it returns the blobEntry and true.
func (b *vfsBuilder) layerFromCAS(desc api.Descriptor) (blobEntry, bool) {
	if b.casReader == nil && b.layerHints == nil {
		return blobEntry{}, false
	}

	// Get potential local paths from layer hints
	var hintPaths []string
	if b.layerHints != nil {
		hintPaths = b.layerHints[desc.Digest]
	}

	stats := b.stats
	return blobEntry{
		Descriptor: desc,
		Location:   "remote_cache",
		stats:      stats,
		Opener: func() (io.ReadCloser, error) {
			// First, try to open from local paths if we have hints
			for _, localPath := range hintPaths {
				if file, err := os.Open(localPath); err == nil {
					// Successfully opened local file from layer hints
					stats.LayersFromLocalDisk.Add(1)
					return file, nil
				}
				// If open failed, try the next path
			}

			// All local paths failed (or no hints), fall back to remote cache
			casReader := b.casReader
			if casReader == nil {
				return nil, fmt.Errorf("blob with digest %s not found in local hints and no remote cache configured", desc.Digest)
			}
			digest, err := digestFromDescriptor(desc)
			if err != nil {
				return nil, err
			}
			stats.LayersFromRemoteCache.Add(1)
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
	stats    *Stats // reference to VFS stats for tracking
}

func (b *vfsBuilder) localIndex(operationIndex int, desc api.Descriptor) blobEntry {
	return blobEntry{
		Descriptor: desc,
		Location:   "file",
		Opener: func() (io.ReadCloser, error) {
			fpath, err := b.rlocation(path.Join(fmt.Sprintf("%d", operationIndex), "index.json"))
			if err != nil {
				return nil, err
			}
			return os.Open(fpath)
		},
	}
}

func (b *vfsBuilder) localManifest(operationIndex int, manifestIndex int, desc api.Descriptor) blobEntry {
	return blobEntry{
		Descriptor: desc,
		Location:   "file",
		Opener: func() (io.ReadCloser, error) {
			fpath, err := b.rlocation(path.Join(fmt.Sprintf("%d", operationIndex), "manifests", fmt.Sprintf("%d", manifestIndex), "manifest.json"))
			if err != nil {
				return nil, err
			}
			return os.Open(fpath)
		},
	}
}

func (b *vfsBuilder) localConfig(operationIndex int, manifestIndex int, desc api.Descriptor) blobEntry {
	return blobEntry{
		Descriptor: desc,
		Location:   "file",
		Opener: func() (io.ReadCloser, error) {
			fpath, err := b.rlocation(path.Join(fmt.Sprintf("%d", operationIndex), "manifests", fmt.Sprintf("%d", manifestIndex), "config.json"))
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
