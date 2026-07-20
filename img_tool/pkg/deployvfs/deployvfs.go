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
	registryname "github.com/google/go-containerregistry/pkg/name"
	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	registrytypes "github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/prefetch"
)

// Stats tracks statistics about blob access in the VFS.
// All fields are accessed atomically for thread safety.
type Stats struct {
	BlobsFromLocalDisk     atomic.Uint64 // Blobs opened from local disk (runfiles, OCI layouts, explicit layers)
	BlobsFromDiskCache     atomic.Uint64 // Blobs opened from Bazel disk cache
	BlobsFromRegistry      atomic.Uint64 // Blobs opened from container registry
	BlobsFromRemoteCache   atomic.Uint64 // Blobs opened from Bazel remote cache (RE API)
	BlobsFromCompactStream atomic.Uint64 // Blobs reconstructed from compact stream
}

// BlobSourceErrorKind categorizes why a blob source lookup failed.
type BlobSourceErrorKind int

const (
	BlobSourceUnconfigured BlobSourceErrorKind = iota
	BlobSourceBlobMissing
	BlobSourceAuth
	BlobSourceOther
)

func (k BlobSourceErrorKind) String() string {
	switch k {
	case BlobSourceUnconfigured:
		return "unconfigured"
	case BlobSourceBlobMissing:
		return "blob missing"
	case BlobSourceAuth:
		return "authentication issue"
	default:
		return "other"
	}
}

// BlobSourceError is a structured error returned when a blob source lookup fails.
type BlobSourceError struct {
	Source  string
	Digest  string
	Kind    BlobSourceErrorKind
	Message string
	Err     error
}

func (e *BlobSourceError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: [%s] %s: %v", e.Source, e.Kind, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: [%s] %s", e.Source, e.Kind, e.Message)
}

func (e *BlobSourceError) Unwrap() error {
	return e.Err
}

// VFS represents a virtual file system for deployment manifests and their associated blobs.
// It merges multiple data sources into a single coherent view:
// - runfiles tree of the push/load tool
// - registry of base image (if base image is shallow)
// - undeclared local output files (via layer hints)
// - Bazel remote cache
type VFS struct {
	dm              api.DeployManifest
	blobs           map[string]blobEntry
	crossMountHints map[string]api.CrossMountSource
	manifests       map[string]blobEntry
	stats           *Stats
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

	// Prefetch layer contents into memory ahead of the consumer so that a slow
	// consumer (e.g. a network upload) does not stall the underlying blob
	// source. This wraps the raw blob entry; any MountableLayer shim is applied
	// on top so remote.Write can still detect it for cross-repo mounts.
	//
	// When layer uploads are forbidden, serve a layer that reports its metadata
	// (so the manifest and cross-mount work) but fails if its bytes are read for
	// upload -- there is nothing to prefetch, and a read means an upload we must
	// not perform.
	var layer registryv1.Layer
	if vfs.dm.Settings.ForbidLayerPush {
		layer = noUploadLayer{entry}
	} else {
		layer = prefetch.NewLayer(entry)
	}

	if hint, found := vfs.crossMountHints[digest.String()]; found {
		reg, err := registryname.NewRegistry(hint.Registry, registryname.WithDefaultRegistry(""))
		if err != nil {
			return nil, fmt.Errorf("parsing cross-mount registry %q: %w", hint.Registry, err)
		}

		return &remote.MountableLayer{
			Layer:     layer,
			Reference: reg.Repo(hint.Repository).Digest(digest.String()),
		}, nil
	}

	return layer, nil
}

// RawLayer returns the blob for digest without any cross-mount wrapping, so
// callers that want to upload the raw bytes (e.g. pre-uploading blobs to a
// staging repository) get the plain layer rather than a remote.MountableLayer.
func (vfs *VFS) RawLayer(digest registryv1.Hash) (registryv1.Layer, error) {
	entry, found := vfs.blobs[digest.String()]
	if !found {
		return nil, fmt.Errorf("layer with digest %s not found in VFS", digest.String())
	}
	return prefetch.NewLayer(entry), nil
}

// NewLayer builds a registryv1.Layer from a known descriptor and an opener that
// returns the compressed blob bytes. The digest, media type and size are taken
// from the descriptor (the blob is not re-hashed), so this is suitable for
// pushing a single blob whose metadata is already known (e.g. `img push blob`).
func NewLayer(desc api.Descriptor, opener func() (io.ReadCloser, error)) registryv1.Layer {
	return blobEntry{
		Descriptor: desc,
		Location:   "file",
		Opener:     opener,
	}
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

// Builder constructs a VFS by configuring blob sources and resolving layers.
// Use NewBuilder to create one, configure with With* methods, then call Build().
// Use Clone() to create an independent copy for per-request customization.
type Builder struct {
	dm                         api.DeployManifest
	casReader                  casReader
	diskCachePath              string // path to Bazel disk cache directory (contains cas/ subdirectory)
	containerRegistryOptions   []remote.Option
	runfilesRootSymlinksPrefix string
	ociLayouts                 []string            // paths to OCI layout directories (sparse or standard)
	explicitLayers             map[string]string   // digest -> file path (raw compressed layer blob)
	compactStreamLayers        map[string]string   // layer digest -> .cstream file path (reconstructed on demand)
	layerHints                 map[string][]string // digest -> []paths
	// extraCrossMountHints registers additional per-digest cross-mount sources
	// beyond those recorded in the deploy manifest's per-operation CrossMountHint.
	// They are merged into the resulting VFS's cross-mount hints (taking
	// precedence), so VFS.Layer wraps those blobs as remote.MountableLayer and the
	// manifest push mounts them from the given repository. Used for the blob-staging
	// repository feature (blobs pushed to a staging repo, mounted into the real one).
	extraCrossMountHints map[string]api.CrossMountSource
	stats                *Stats
	// layerSpecErr holds the first error encountered while classifying a --layer
	// spec in WithLayer (detecting a compact stream, reading its embedded digest,
	// or hashing a raw layer file). It is deferred until Build() so the fluent
	// With* chain stays error-free, mirroring how layer hints surface at Build().
	layerSpecErr error
	// ctx scopes long-running, lazily-triggered work created by the resulting
	// VFS — notably on-demand compact-stream reconstruction, which may fetch CAS
	// blobs from the remote cache. It is captured into blob openers so that
	// cancelling the surrounding deploy aborts those fetches. nil means
	// context.Background().
	ctx context.Context
}

func NewBuilder(dm api.DeployManifest) *Builder {
	return &Builder{
		dm:    dm,
		stats: &Stats{},
	}
}

func (b *Builder) Clone() *Builder {
	clone := *b
	clone.ociLayouts = slices.Clone(b.ociLayouts)
	clone.containerRegistryOptions = slices.Clone(b.containerRegistryOptions)
	if b.explicitLayers != nil {
		clone.explicitLayers = make(map[string]string, len(b.explicitLayers))
		for k, v := range b.explicitLayers {
			clone.explicitLayers[k] = v
		}
	}
	if b.compactStreamLayers != nil {
		clone.compactStreamLayers = make(map[string]string, len(b.compactStreamLayers))
		for k, v := range b.compactStreamLayers {
			clone.compactStreamLayers[k] = v
		}
	}
	if b.extraCrossMountHints != nil {
		clone.extraCrossMountHints = make(map[string]api.CrossMountSource, len(b.extraCrossMountHints))
		for k, v := range b.extraCrossMountHints {
			clone.extraCrossMountHints[k] = v
		}
	}
	clone.layerHints = nil
	clone.stats = &Stats{}
	return &clone
}

func (b *Builder) WithDeployManifest(dm api.DeployManifest) *Builder {
	b.dm = dm
	return b
}

func (b *Builder) WithCASReader(br casReader) *Builder {
	b.casReader = br
	return b
}

func (b *Builder) WithDiskCache(path string) *Builder {
	b.diskCachePath = path
	return b
}

// WithContext sets the context used to scope lazily-triggered work performed by
// the resulting VFS (e.g. on-demand compact-stream reconstruction and its CAS
// blob fetches). Pass the deploy/load operation's context so cancellation
// propagates to that work.
func (b *Builder) WithContext(ctx context.Context) *Builder {
	b.ctx = ctx
	return b
}

// context returns the configured context, defaulting to context.Background().
func (b *Builder) context() context.Context {
	if b.ctx != nil {
		return b.ctx
	}
	return context.Background()
}

func (b *Builder) WithContainerRegistryOption(o remote.Option) *Builder {
	b.containerRegistryOptions = append(b.containerRegistryOptions, o)
	return b
}

func (b *Builder) WithRunfilesRootSymlinksPrefix(prefix string) *Builder {
	b.runfilesRootSymlinksPrefix = prefix
	return b
}

func (b *Builder) WithOCILayout(layoutPath string) *Builder {
	b.ociLayouts = append(b.ociLayouts, layoutPath)
	return b
}

func (b *Builder) WithExplicitLayer(digest string, filePath string) *Builder {
	if b.explicitLayers == nil {
		b.explicitLayers = make(map[string]string)
	}
	b.explicitLayers[digest] = filePath
	return b
}

// WithCrossMountSource registers a cross-mount source for a blob digest. The
// resulting VFS wraps that blob as a remote.MountableLayer referencing
// src.Registry/src.Repository, so a manifest push mounts it from there instead of
// re-uploading. Used for the blob-staging repository feature.
func (b *Builder) WithCrossMountSource(digest string, src api.CrossMountSource) *Builder {
	if b.extraCrossMountHints == nil {
		b.extraCrossMountHints = make(map[string]api.CrossMountSource)
	}
	b.extraCrossMountHints[digest] = src
	return b
}

// rlocation wraps runfiles.Rlocation and adds the runfiles root symlinks prefix if configured.
func (b *Builder) rlocation(runfilesPath string) (string, error) {
	fullPath := runfilesPath
	if b.runfilesRootSymlinksPrefix != "" {
		fullPath = path.Join(b.runfilesRootSymlinksPrefix, runfilesPath)
	}
	return runfiles.Rlocation(fullPath)
}

func (b *Builder) Build() (*VFS, error) {
	// Surface any error captured while classifying a --layer spec (see WithLayer).
	if b.layerSpecErr != nil {
		return nil, b.layerSpecErr
	}

	// Try to load layer hints if available
	if err := b.loadLayerHints(); err != nil {
		// Layer hints are optional, log but don't fail
		// We could add debug logging here if needed
		_ = err
	}

	blobs, manifests, crossMountHints, err := b.ingest()
	if err != nil {
		return nil, err
	}
	return &VFS{
		dm:              b.dm,
		blobs:           blobs,
		crossMountHints: crossMountHints,
		manifests:       manifests,
		stats:           b.stats,
	}, nil
}

// loadLayerHints attempts to load layer hints from the runfiles.
// Layer hints are only enabled if:
// 1. BUILD_WORKSPACE_DIRECTORY environment variable is set
// 2. A layer_hints file exists under the runfiles prefix
func (b *Builder) loadLayerHints() error {
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
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // default 64 KB is too small when many operations share a layer

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

func (b *Builder) ingest() (map[string]blobEntry, map[string]blobEntry, map[string]api.CrossMountSource, error) {
	blobs := make(map[string]blobEntry)
	manifests := make(map[string]blobEntry)
	crossMountHints := make(map[string]api.CrossMountSource)

	baseOps, err := b.dm.BaseOperations()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("getting base operations: %w", err)
	}
	for i, op := range baseOps {
		var strategy string
		switch op.Command {
		case "push":
			strategy = b.dm.Settings.PushStrategy
		case "load":
			strategy = b.dm.Settings.LoadStrategy
		case "registry_tag":
			// No new blobs; the referenced manifest was ingested by the sibling push op.
			continue
		default:
			continue
		}
		if strategy == "bes" {
			// When pushing via the build event stream,
			// we assume the push happens as a side-effect of the "bazel build" command,
			// so we don't need to upload any blobs ourselves.
			continue
		}
		if op.RootKind == "index" {
			manifests[op.Root.Digest] = b.resolveManifestBlob(i, op.Root)
		}
		for manifestIndex, manifest := range op.Manifests {
			manifests[manifest.Descriptor.Digest] = b.resolveManifestBlob(i, manifest.Descriptor)
			blobs[manifest.Config.Digest] = b.resolveConfigBlob(i, manifest.Config)
			for layerIndex, layer := range manifest.LayerBlobs {
				blob, err := b.layerBlob(i, manifestIndex, layerIndex, strategy, layer)
				if err != nil {
					return nil, nil, nil, fmt.Errorf("locating source for layer with digest %s with index %d in manifest %d of operation %d: %w", layer.Digest, layerIndex, manifestIndex, i, err)
				}
				if op.CrossMountHint != nil {
					crossMountHints[layer.Digest] = *op.CrossMountHint
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

	// Merge explicitly-registered cross-mount sources (e.g. blob-staging
	// repository). These take precedence over per-operation hints.
	for digest, src := range b.extraCrossMountHints {
		crossMountHints[digest] = src
	}

	return blobs, manifests, crossMountHints, nil
}

func (b *Builder) layerBlob(operationIndex int, manifestIndex int, layerIndex int, strategy string, layer api.LayerBlob) (blobEntry, error) {
	// we try the following sources, in order:
	// 1. OCI layouts (--oci-layout flags, supports both sparse and standard formats)
	// 2. explicit layer files (--layer flags pointing at a raw compressed blob)
	// 3. runfiles tree
	// 4. explicit compact stream (--layer flag pointing at a .cstream index)
	// 5. compact stream in an OCI layout (--oci-layout, <layout>/blobs/<algo>/<hex>.cstream)
	// 6. compact stream in runfiles (.cstream + .inputfilecas content-addressed directory)
	// 7. registry (if the layer records upstream sources, i.e. it came from a pulled base image)
	// 8. layer hints (local paths from BUILD_WORKSPACE_DIRECTORY, populated by lazy builds)
	// 9. bazel disk cache (if configured via IMG_DISK_CACHE)
	// 10. bazel remote cache (if configured via IMG_REAPI_ENDPOINT)
	// 11. stub blob (cas_registry strategy where all blobs are assumed to already be in the remote CAS)

	desc := layer.Descriptor

	var sourceErrors []*BlobSourceError

	if entry, err := b.layerFromOCILayouts(desc); err == nil {
		return entry, nil
	} else {
		sourceErrors = append(sourceErrors, err.(*BlobSourceError))
	}
	if entry, err := b.layerFromExplicit(desc); err == nil {
		return entry, nil
	} else {
		sourceErrors = append(sourceErrors, err.(*BlobSourceError))
	}
	if entry, err := b.layerFromFile(operationIndex, manifestIndex, layerIndex, desc); err == nil {
		return entry, nil
	} else {
		sourceErrors = append(sourceErrors, err.(*BlobSourceError))
	}
	if entry, err := b.layerFromExplicitCompactStream(desc); err == nil {
		return entry, nil
	} else {
		sourceErrors = append(sourceErrors, err.(*BlobSourceError))
	}
	if entry, err := b.layerFromOCILayoutCompactStream(desc); err == nil {
		return entry, nil
	} else {
		sourceErrors = append(sourceErrors, err.(*BlobSourceError))
	}
	if entry, err := b.layerFromRunfilesCompactStream(operationIndex, manifestIndex, layerIndex, desc); err == nil {
		return entry, nil
	} else {
		sourceErrors = append(sourceErrors, err.(*BlobSourceError))
	}
	if entry, err := b.layerFromRegistry(layer.Sources, desc); err == nil {
		return entry, nil
	} else {
		sourceErrors = append(sourceErrors, err.(*BlobSourceError))
	}
	if entry, err := b.layerFromHints(desc); err == nil {
		return entry, nil
	} else {
		sourceErrors = append(sourceErrors, err.(*BlobSourceError))
	}
	if entry, err := b.blobFromDiskCache(desc); err == nil {
		return entry, nil
	} else {
		sourceErrors = append(sourceErrors, err.(*BlobSourceError))
	}
	if entry, err := b.layerFromCAS(desc); err == nil {
		return entry, nil
	} else {
		sourceErrors = append(sourceErrors, err.(*BlobSourceError))
	}

	// If a cross-mount source is registered for this blob (the blob-staging
	// repository, or an explicit result from a per-layer `img push blob`), its
	// bytes are not needed here: the manifest push mounts it from that repository
	// (or finds it already present). Return a stub, which only fails if a mount is
	// actually attempted and fails. This lets an eager-strategy manifest push
	// proceed without shipping the layer blobs as inputs.
	if _, ok := b.extraCrossMountHints[desc.Digest]; ok {
		return stubBlob(desc), nil
	}

	switch strategy {
	case "eager", "lazy":
		var sb strings.Builder
		fmt.Fprintf(&sb, "layer with digest %s not found in any source:", desc.Digest)
		for _, bse := range sourceErrors {
			fmt.Fprintf(&sb, "\n  - %s", bse.Error())
		}
		return blobEntry{}, fmt.Errorf("%s", sb.String())
	case "cas_registry", "bes":
		return stubBlob(desc), nil
	}
	return blobEntry{}, fmt.Errorf("unknown push/load strategy: %s", strategy)
}

// layerFromOCILayouts tries to find the layer in any OCI layout directory (sparse or standard).
func (b *Builder) layerFromOCILayouts(desc api.Descriptor) (blobEntry, error) {
	if len(b.ociLayouts) == 0 {
		return blobEntry{}, &BlobSourceError{Source: "OCI layouts", Digest: desc.Digest, Kind: BlobSourceUnconfigured, Message: "no OCI layouts configured"}
	}
	var checkedPaths []string
	for _, layoutPath := range b.ociLayouts {
		blobPath := sparseLayoutBlobPathInDir(layoutPath, desc.Digest)
		checkedPaths = append(checkedPaths, blobPath)
		if _, err := os.Stat(blobPath); err == nil {
			fpath := blobPath
			stats := b.stats
			return blobEntry{
				Descriptor: desc,
				Location:   "file",
				stats:      stats,
				Opener: func() (io.ReadCloser, error) {
					stats.BlobsFromLocalDisk.Add(1)
					return os.Open(fpath)
				},
			}, nil
		}
	}
	return blobEntry{}, &BlobSourceError{Source: "OCI layouts", Digest: desc.Digest, Kind: BlobSourceBlobMissing, Message: fmt.Sprintf("not found in %d OCI layout(s) (checked: %s)", len(b.ociLayouts), strings.Join(checkedPaths, ", "))}
}

// layerFromExplicit tries to find the layer in the explicit layer map.
func (b *Builder) layerFromExplicit(desc api.Descriptor) (blobEntry, error) {
	if b.explicitLayers == nil {
		return blobEntry{}, &BlobSourceError{Source: "explicit layers", Digest: desc.Digest, Kind: BlobSourceUnconfigured, Message: "no explicit layers configured"}
	}
	fpath, found := b.explicitLayers[desc.Digest]
	if !found {
		return blobEntry{}, &BlobSourceError{Source: "explicit layers", Digest: desc.Digest, Kind: BlobSourceBlobMissing, Message: fmt.Sprintf("digest not in explicit layer map (%d entries)", len(b.explicitLayers))}
	}
	if _, err := os.Stat(fpath); err != nil {
		return blobEntry{}, &BlobSourceError{Source: "explicit layers", Digest: desc.Digest, Kind: BlobSourceOther, Message: fmt.Sprintf("file %s", fpath), Err: err}
	}
	stats := b.stats
	return blobEntry{
		Descriptor: desc,
		Location:   "file",
		stats:      stats,
		Opener: func() (io.ReadCloser, error) {
			stats.BlobsFromLocalDisk.Add(1)
			return os.Open(fpath)
		},
	}, nil
}

// layerFromFile tries to find the layer in the runfiles tree. If it exists, it returns the blobEntry.
func (b *Builder) layerFromFile(operationIndex int, manifestIndex int, layerIndex int, desc api.Descriptor) (blobEntry, error) {
	runfilesPath := layerRunfilesPath(operationIndex, manifestIndex, layerIndex)
	fpath, err := b.rlocation(runfilesPath)
	if err != nil {
		return blobEntry{}, &BlobSourceError{Source: "runfiles", Digest: desc.Digest, Kind: BlobSourceOther, Message: fmt.Sprintf("rlocation(%s)", runfilesPath), Err: err}
	}
	if _, err := os.Stat(fpath); err != nil {
		return blobEntry{}, &BlobSourceError{Source: "runfiles", Digest: desc.Digest, Kind: BlobSourceBlobMissing, Message: fpath, Err: err}
	}
	stats := b.stats
	return blobEntry{
		Descriptor: desc,
		Location:   "file",
		stats:      stats,
		Opener: func() (io.ReadCloser, error) {
			stats.BlobsFromLocalDisk.Add(1)
			return os.Open(fpath)
		},
	}, nil
}

// layerFromRegistry tries to fetch the layer from an upstream registry using the
// per-layer sources recorded for it (the registry/repository combinations it was
// pulled from). The blob is content-addressed, so it is fetched by its own digest.
func (b *Builder) layerFromRegistry(sources []api.LayerSource, desc api.Descriptor) (blobEntry, error) {
	if len(sources) == 0 {
		return blobEntry{}, &BlobSourceError{Source: "base image registry", Digest: desc.Digest, Kind: BlobSourceUnconfigured, Message: "layer has no upstream sources (not from a pulled base image)"}
	}
	stats := b.stats
	opts := b.containerRegistryOptions
	return blobEntry{
		Descriptor: desc,
		Location:   "registry",
		stats:      stats,
		Opener: func() (io.ReadCloser, error) {
			var attempts []string
			for _, source := range sources {
				ref, err := registryname.NewDigest(fmt.Sprintf("%s/%s@%s", source.Registry, source.Repository, desc.Digest))
				if err != nil {
					attempts = append(attempts, fmt.Sprintf("%s/%s: %v", source.Registry, source.Repository, err))
					continue
				}
				layer, err := remote.Layer(ref, opts...)
				if err != nil {
					attempts = append(attempts, fmt.Sprintf("%s: %v", ref, err))
					continue
				}
				rc, err := layer.Compressed()
				if err != nil {
					attempts = append(attempts, fmt.Sprintf("%s: %v", ref, err))
					continue
				}
				stats.BlobsFromRegistry.Add(1)
				return rc, nil
			}
			return nil, fmt.Errorf("layer %s not found in any of its %d source(s): %s", desc.Digest, len(sources), strings.Join(attempts, "; "))
		},
	}, nil
}

// layerFromHints tries to find the layer from local paths provided by layer hints.
// Layer hints are local file paths from BUILD_WORKSPACE_DIRECTORY populated by lazy builds.
func (b *Builder) layerFromHints(desc api.Descriptor) (blobEntry, error) {
	if b.layerHints == nil {
		return blobEntry{}, &BlobSourceError{Source: "layer hints", Digest: desc.Digest, Kind: BlobSourceUnconfigured, Message: "no layer hints configured"}
	}
	hintPaths := b.layerHints[desc.Digest]
	if len(hintPaths) == 0 {
		return blobEntry{}, &BlobSourceError{Source: "layer hints", Digest: desc.Digest, Kind: BlobSourceBlobMissing, Message: "digest not in layer hints"}
	}
	var foundPath string
	for _, localPath := range hintPaths {
		if _, err := os.Stat(localPath); err == nil {
			foundPath = localPath
			break
		}
	}
	if foundPath == "" {
		return blobEntry{}, &BlobSourceError{Source: "layer hints", Digest: desc.Digest, Kind: BlobSourceBlobMissing, Message: "digest not in layer hints"}
	}
	stats := b.stats
	return blobEntry{
		Descriptor: desc,
		Location:   "file",
		stats:      stats,
		Opener: func() (io.ReadCloser, error) {
			file, err := os.Open(foundPath)
			if err != nil {
				return nil, fmt.Errorf("layer %s not found in hint path: %w", desc.Digest, err)
			}
			stats.BlobsFromLocalDisk.Add(1)
			return file, nil
		},
	}, nil
}

// layerFromCAS tries to find the layer in the bazel remote cache.
func (b *Builder) layerFromCAS(desc api.Descriptor) (blobEntry, error) {
	if b.casReader == nil {
		return blobEntry{}, &BlobSourceError{Source: "remote CAS", Digest: desc.Digest, Kind: BlobSourceUnconfigured, Message: "no CAS reader configured"}
	}
	digest, err := digestFromDescriptor(desc)
	if err != nil {
		return blobEntry{}, &BlobSourceError{Source: "remote CAS", Digest: desc.Digest, Kind: BlobSourceBlobMissing, Err: err}
	}
	if missing, err := b.casReader.FindMissingBlobs(context.TODO(), []cas.Digest{digest}); err == nil && len(missing) > 0 {
		return blobEntry{}, &BlobSourceError{Source: "remote CAS", Digest: desc.Digest, Kind: BlobSourceBlobMissing, Message: "blob not found in remote CAS"}
	}
	stats := b.stats
	return blobEntry{
		Descriptor: desc,
		Location:   "remote_cache",
		stats:      stats,
		Opener: func() (io.ReadCloser, error) {
			stats.BlobsFromRemoteCache.Add(1)
			return b.casReader.ReaderForBlob(context.TODO(), digest)
		},
	}, nil
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
	Location string // "file", "registry", "remote_cache", "compact_stream", "stub"
	Opener   func() (io.ReadCloser, error)
	stats    *Stats // reference to VFS stats for tracking
}

// resolveManifestBlob resolves a manifest or index blob from available sources.
// Priority: OCI layouts → runfiles sparse layout path → disk cache → remote CAS.
func (b *Builder) resolveManifestBlob(operationIndex int, desc api.Descriptor) blobEntry {
	if entry, err := b.blobFromOCILayouts(desc); err == nil {
		return entry
	}
	if entry, err := b.blobFromRunfilesSparseLayout(operationIndex, desc); err == nil {
		return entry
	}
	if entry, err := b.blobFromDiskCache(desc); err == nil {
		return entry
	}
	if entry, err := b.blobFromCAS(desc); err == nil {
		return entry
	}
	return blobEntry{
		Descriptor: desc,
		Opener: func() (io.ReadCloser, error) {
			return nil, fmt.Errorf("manifest blob %s not found in any source (OCI layouts, runfiles, disk cache, remote CAS)", desc.Digest)
		},
	}
}

// resolveConfigBlob resolves a config blob from available sources.
// Priority: OCI layouts → runfiles sparse layout path → disk cache → remote CAS.
func (b *Builder) resolveConfigBlob(operationIndex int, desc api.Descriptor) blobEntry {
	if entry, err := b.blobFromOCILayouts(desc); err == nil {
		return entry
	}
	if entry, err := b.blobFromRunfilesSparseLayout(operationIndex, desc); err == nil {
		return entry
	}
	if entry, err := b.blobFromDiskCache(desc); err == nil {
		return entry
	}
	if entry, err := b.blobFromCAS(desc); err == nil {
		return entry
	}
	return blobEntry{
		Descriptor: desc,
		Opener: func() (io.ReadCloser, error) {
			return nil, fmt.Errorf("config blob %s not found in any source (OCI layouts, runfiles, disk cache, remote CAS)", desc.Digest)
		},
	}
}

// blobFromCAS tries to resolve a blob from the Bazel remote cache.
func (b *Builder) blobFromCAS(desc api.Descriptor) (blobEntry, error) {
	if b.casReader == nil {
		return blobEntry{}, &BlobSourceError{Source: "remote CAS", Digest: desc.Digest, Kind: BlobSourceUnconfigured, Message: "no CAS reader configured"}
	}
	digest, err := digestFromDescriptor(desc)
	if err != nil {
		return blobEntry{}, &BlobSourceError{Source: "remote CAS", Digest: desc.Digest, Kind: BlobSourceBlobMissing, Err: err}
	}
	if missing, err := b.casReader.FindMissingBlobs(context.TODO(), []cas.Digest{digest}); err == nil && len(missing) > 0 {
		return blobEntry{}, &BlobSourceError{Source: "remote CAS", Digest: desc.Digest, Kind: BlobSourceBlobMissing, Message: "blob not found in remote CAS"}
	}
	stats := b.stats
	return blobEntry{
		Descriptor: desc,
		Location:   "remote_cache",
		stats:      stats,
		Opener: func() (io.ReadCloser, error) {
			stats.BlobsFromRemoteCache.Add(1)
			return b.casReader.ReaderForBlob(context.TODO(), digest)
		},
	}, nil
}

// blobFromDiskCache tries to resolve a blob from the Bazel disk cache.
// The disk cache layout is: {diskCachePath}/cas/{first2hex}/{fullhex}
func (b *Builder) blobFromDiskCache(desc api.Descriptor) (blobEntry, error) {
	if b.diskCachePath == "" {
		return blobEntry{}, &BlobSourceError{Source: "disk cache", Digest: desc.Digest, Kind: BlobSourceUnconfigured, Message: "no disk cache path configured"}
	}
	fpath := diskCacheBlobPath(b.diskCachePath, desc.Digest)
	if _, err := os.Stat(fpath); err != nil {
		return blobEntry{}, &BlobSourceError{Source: "disk cache", Digest: desc.Digest, Kind: BlobSourceBlobMissing, Message: fpath, Err: err}
	}
	stats := b.stats
	return blobEntry{
		Descriptor: desc,
		Location:   "file",
		stats:      stats,
		Opener: func() (io.ReadCloser, error) {
			stats.BlobsFromDiskCache.Add(1)
			return os.Open(fpath)
		},
	}, nil
}

// diskCacheBlobPath returns the path to a blob in Bazel's disk cache.
// Layout: {cacheDir}/cas/{first2hex}/{fullhex}
func diskCacheBlobPath(cacheDir string, digest string) string {
	_, hex, _ := strings.Cut(digest, ":")
	return filepath.Join(cacheDir, "cas", hex[:2], hex)
}

// blobFromOCILayouts tries to find a blob in any of the configured OCI layout directories.
func (b *Builder) blobFromOCILayouts(desc api.Descriptor) (blobEntry, error) {
	if len(b.ociLayouts) == 0 {
		return blobEntry{}, &BlobSourceError{Source: "OCI layouts", Digest: desc.Digest, Kind: BlobSourceUnconfigured, Message: "no OCI layouts configured"}
	}
	var checkedPaths []string
	for _, layoutPath := range b.ociLayouts {
		blobPath := sparseLayoutBlobPathInDir(layoutPath, desc.Digest)
		checkedPaths = append(checkedPaths, blobPath)
		if _, err := os.Stat(blobPath); err == nil {
			fpath := blobPath
			stats := b.stats
			return blobEntry{
				Descriptor: desc,
				Location:   "file",
				stats:      stats,
				Opener: func() (io.ReadCloser, error) {
					stats.BlobsFromLocalDisk.Add(1)
					return os.Open(fpath)
				},
			}, nil
		}
	}
	return blobEntry{}, &BlobSourceError{Source: "OCI layouts", Digest: desc.Digest, Kind: BlobSourceBlobMissing, Message: fmt.Sprintf("not found in %d OCI layout(s) (checked: %s)", len(b.ociLayouts), strings.Join(checkedPaths, ", "))}
}

// blobFromRunfilesSparseLayout resolves a blob from the runfiles sparse layout tree.
func (b *Builder) blobFromRunfilesSparseLayout(operationIndex int, desc api.Descriptor) (blobEntry, error) {
	runfilesPath := sparseLayoutBlobPath(operationIndex, desc.Digest)
	fpath, err := b.rlocation(runfilesPath)
	if err != nil {
		return blobEntry{}, &BlobSourceError{Source: "runfiles", Digest: desc.Digest, Kind: BlobSourceOther, Message: fmt.Sprintf("rlocation(%s)", runfilesPath), Err: err}
	}
	if _, err := os.Stat(fpath); err != nil {
		return blobEntry{}, &BlobSourceError{Source: "runfiles", Digest: desc.Digest, Kind: BlobSourceBlobMissing, Message: fpath, Err: err}
	}
	stats := b.stats
	return blobEntry{
		Descriptor: desc,
		Location:   "file",
		stats:      stats,
		Opener: func() (io.ReadCloser, error) {
			stats.BlobsFromLocalDisk.Add(1)
			return os.Open(fpath)
		},
	}, nil
}

// sparseLayoutBlobPathInDir returns the absolute path to a blob within a sparse layout directory.
func sparseLayoutBlobPathInDir(layoutDir string, digest string) string {
	algo, hex, _ := strings.Cut(digest, ":")
	return filepath.Join(layoutDir, "blobs", algo, hex)
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

// noUploadLayer wraps a blob entry so that reading its bytes (Compressed /
// Uncompressed) always fails, while its descriptor-derived metadata (Digest,
// Size, MediaType) still works. It is used when Settings.ForbidLayerPush is set:
// the manifest push can still mount the layer (or skip it when already present),
// but an actual upload -- which reads Compressed -- fails loudly instead of
// silently re-uploading a blob that was expected to be pushed at build time.
type noUploadLayer struct {
	blobEntry
}

func (l noUploadLayer) Compressed() (io.ReadCloser, error) {
	return nil, l.uploadForbiddenError()
}

func (l noUploadLayer) Uncompressed() (io.ReadCloser, error) {
	return nil, l.uploadForbiddenError()
}

func (l noUploadLayer) uploadForbiddenError() error {
	return fmt.Errorf("refusing to upload layer %s: layer uploads are forbidden (forbid_layer_push); the blob is expected to already be in the registry (e.g. pushed at build time and mounted server-side)", l.Descriptor.Digest)
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

func sparseLayoutBlobPath(operationIndex int, digest string) string {
	algo, hex, _ := strings.Cut(digest, ":")
	return path.Join(fmt.Sprintf("%d", operationIndex), "sparse_oci_layout", "blobs", algo, hex)
}

func layerRunfilesPath(operationIndex int, manifestIndex int, layerIndex int) string {
	return path.Join(fmt.Sprintf("%d", operationIndex), "manifests", fmt.Sprintf("%d", manifestIndex), "layer", fmt.Sprintf("%d", layerIndex))
}
