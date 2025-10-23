package load

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"time"

	registryv1 "github.com/malt3/go-containerregistry/pkg/v1"
	ocidigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/containerd"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/docker"
)

type builder struct {
	vfs       vfs
	platforms []string
}

func NewBuilder(vfs vfs) *builder {
	return &builder{vfs: vfs}
}

func (b *builder) WithPlatforms(platforms []string) *builder {
	b.platforms = platforms
	return b
}

func (b *builder) Build() *loader {
	return &loader{
		vfs:       b.vfs,
		platforms: b.platforms,
		taskSet:   newTaskSet(b.vfs, b.platforms),
	}
}

type loader struct {
	vfs             vfs
	platforms       []string
	taskSet         *taskSet
	clientConn      *containerd.Client
	triedContainerd bool
	haveContainerd  bool
}

func (l *loader) LoadAll(ctx context.Context, ops []api.IndexedLoadDeployOperation) ([]string, error) {
	ctx = containerd.WithNamespace(ctx, "moby")
	var pushedTags []string

	// try to connect to containerd once
	client, err := l.connect(ctx, "containerd")
	if err == nil {
		defer client.Close()
	}

	for _, op := range ops {
		if l.haveContainerd && op.Daemon == "docker" {
			// upgrade docker loads to containerd loads if possible
			op.Daemon = "containerd"
		}
		if err := l.taskSet.addOperation(op); err != nil {
			return nil, fmt.Errorf("adding operation for daemon %s: %w", op.Daemon, err)
		}
	}

	for _, daemon := range l.taskSet.daemons() {
		ops := l.taskSet.operations(daemon)
		blobs := l.taskSet.blobs(daemon)

		switch daemon {
		case "containerd":
			if !l.haveContainerd {
				return nil, fmt.Errorf("containerd not available for loading images, but containerd daemon requested as load target")
			}

			leaseService := client.LeaseService()

			lease, err := leaseService.Create(ctx, map[string]string{
				// max age of the lease
				"containerd.io/gc.expire": time.Now().Add(1 * time.Hour).Format(time.RFC3339),
			})
			if err != nil {
				return nil, fmt.Errorf("creating lease: %w", err)
			}
			defer leaseService.Delete(ctx, lease)

			ctx = containerd.WithLease(ctx, lease)

			// Load all blobs in parallel...
			contentStore := client.ContentStore()
			uploadBlobsParallel(ctx, contentStore, blobs, defaultWorkers)

			// ...then all images
			for _, op := range ops {
				loadedTags, err := l.loadContainerd(ctx, op)
				if err != nil {
					return nil, fmt.Errorf("loading image via containerd: %w", err)
				}
				pushedTags = append(pushedTags, loadedTags...)
			}
		case "docker":
			// Load all images via docker load
			for _, op := range ops {
				loadedTags, err := l.loadViaDocker(ctx, op)
				if err != nil {
					return nil, fmt.Errorf("loading image via docker: %w", err)
				}
				pushedTags = append(pushedTags, loadedTags...)
			}
		default:
			return nil, fmt.Errorf("unsupported daemon: %s", daemon)
		}
	}
	return pushedTags, nil
}

// loadContainerd loads an image into containerd
// Assumes blobs are already uploaded
func (l *loader) loadContainerd(ctx context.Context, op api.IndexedLoadDeployOperation) ([]string, error) {
	client, err := l.connect(ctx, op.Daemon)
	if err != nil {
		return nil, fmt.Errorf("connecting to containerd: %w", err)
	}

	ctx = containerd.WithNamespace(ctx, "moby")

	ociDigest, err := ocidigest.Parse(op.Root.Digest)
	if err != nil {
		return nil, fmt.Errorf("parsing root digest %s: %w", op.Root.Digest, err)
	}

	imageService := client.ImageService()
	target := ocispec.Descriptor{
		MediaType: op.Root.MediaType,
		Digest:    ociDigest,
		Size:      op.Root.Size,
	}

	// Print digest once
	fmt.Printf("%s\n", target.Digest)

	var loadedTags []string
	for _, tag := range op.Tags {
		normalizedTag := NormalizeDockerReference(tag)
		img := containerd.Image{
			Name:   normalizedTag,
			Target: target,
		}
		_, err = imageService.Create(ctx, img)
		if err != nil && containerd.IsAlreadyExists(err) {
			_, err = imageService.Update(ctx, img)
		}
		if err != nil {
			return nil, fmt.Errorf("creating/updating image: %w", err)
		}

		// Print tag without digest
		fmt.Printf("%s\n", normalizedTag)
		loadedTags = append(loadedTags, normalizedTag)
	}

	return loadedTags, nil
}

func (l *loader) loadViaDocker(ctx context.Context, op api.IndexedLoadDeployOperation) ([]string, error) {
	// Create a pipe to stream the tar to docker load
	pr, pw := io.Pipe()

	// Start docker load in the background
	errCh := make(chan error, 1)
	go func() {
		err := docker.Load(pr)
		pr.Close()
		errCh <- err
	}()

	// Stream the tar to the pipe writer
	loadedTags, err := l.streamDockerTar(ctx, op, pw)
	pw.Close() // Always close, even on error

	// Wait for docker load to complete
	loadErr := <-errCh

	// Return the first error
	if err != nil {
		return nil, fmt.Errorf("streaming tar to docker load: stream error: %w, load error: %w", err, loadErr)
	}
	if loadErr != nil {
		return nil, loadErr
	}
	return loadedTags, nil
}

func (l *loader) streamDockerTar(ctx context.Context, op api.IndexedLoadDeployOperation, w io.Writer) ([]string, error) {
	tw := docker.NewTarWriter(w)

	if op.RootKind == "index" {
		// For multi-platform images, we need to select a manifest
		manifestIndex, err := l.selectManifestForPlatform(op)
		if err != nil {
			return nil, err
		}
		return l.streamManifestToTar(ctx, op.Manifests[manifestIndex], op.Tags, tw)
	} else if op.RootKind == "manifest" && len(op.Manifests) == 1 {
		// Validate that the single manifest matches requested platform if explicit
		digest, err := registryv1.NewHash(op.Manifests[0].Descriptor.Digest)
		if err != nil {
			return nil, err
		}
		if err := l.validateManifestPlatform(digest); err != nil {
			return nil, fmt.Errorf("single manifest validation failed: %w", err)
		}
		return l.streamManifestToTar(ctx, op.Manifests[0], op.Tags, tw)
	}

	return nil, fmt.Errorf("no manifest or index provided")
}

// selectManifestForPlatform selects the appropriate manifest from an index based on platform criteria
func (l *loader) selectManifestForPlatform(op api.IndexedLoadDeployOperation) (int, error) {
	// Load and parse the index
	digest, err := registryv1.NewHash(op.Root.Digest)
	if err != nil {
		return 0, err
	}
	index, err := l.vfs.ImageIndex(digest)
	if err != nil {
		return 0, err
	}

	mnfst, err := index.IndexManifest()
	if err != nil {
		return 0, err
	}

	// Determine which platforms to match
	platforms := l.platforms
	hasExplicit := l.hasExplicitPlatforms()

	// If "all" is specified, we can't load multi-platform for docker
	// Fall back to current platform
	if contains(platforms, "all") {
		platforms = []string{getCurrentPlatform()}
	} else if !hasExplicit {
		// No explicit platform requested, use defaults
		// If only one manifest, use it
		if len(mnfst.Manifests) == 1 {
			return 0, nil
		}
		// Otherwise use current platform
		platforms = []string{getCurrentPlatform()}
	}

	// Find matching manifest
	for i, manifestDesc := range mnfst.Manifests {
		if manifestDesc.Platform != nil && platformMatches(manifestDesc.Platform, platforms) {
			return i, nil
		}
	}

	return 0, fmt.Errorf("no manifest found for platform(s): %v", platforms)
}

func (l *loader) streamManifestToTar(ctx context.Context, manifestInfo api.ManifestDeployInfo, tags []string, tw *docker.TarWriter) ([]string, error) {
	// Load config
	digest, err := registryv1.NewHash(manifestInfo.Descriptor.Digest)
	if err != nil {
		return nil, err
	}
	img, err := l.vfs.Image(digest)
	if err != nil {
		return nil, err
	}
	rawConfigFile, err := img.RawConfigFile()
	if err != nil {
		return nil, err
	}

	// Write config
	if err := tw.WriteConfig(rawConfigFile); err != nil {
		return nil, fmt.Errorf("writing config: %w", err)
	}

	// Normalize and set tags
	var normalizedTags []string
	for _, tag := range tags {
		normalizedTags = append(normalizedTags, NormalizeDockerReference(tag))
	}
	if len(normalizedTags) > 0 {
		tw.SetTags(normalizedTags)
	}

	// Stream layers
	if err := l.streamLayers(ctx, manifestInfo, tw); err != nil {
		return nil, fmt.Errorf("streaming layers: %w", err)
	}

	// Finalize the tar
	if err := tw.Finalize(); err != nil {
		return nil, fmt.Errorf("finalizing tar: %w", err)
	}

	// Print digest once
	fmt.Printf("%s\n", manifestInfo.Descriptor.Digest)

	// Print each tag without digest
	for _, tag := range normalizedTags {
		fmt.Println(tag)
	}
	return normalizedTags, nil
}

func (l *loader) streamLayers(ctx context.Context, manifestInfo api.ManifestDeployInfo, tw *docker.TarWriter) error {
	for _, layerDesc := range manifestInfo.LayerBlobs {
		digest, err := registryv1.NewHash(layerDesc.Digest)
		if err != nil {
			return err
		}
		layer, err := l.vfs.Layer(digest)
		if err != nil {
			return err
		}
		rc, err := layer.Compressed()
		if err != nil {
			return err
		}
		defer rc.Close()

		if err := tw.WriteLayer(digest, layerDesc.Size, rc); err != nil {
			return err
		}
	}
	return nil
}

func (l *loader) connect(ctx context.Context, daemon string) (*containerd.Client, error) {
	if l.clientConn != nil {
		return l.clientConn, nil
	}
	if l.triedContainerd {
		return nil, fmt.Errorf("containerd connection previously failed")
	}
	client, err := ConnectToContainerd(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Connecting to containerd failed: %v\n", err)
		// Print warning about performance impact and digest differences
		fmt.Fprintln(os.Stderr, "\n\033[33mWARNING: Docker is not using containerd storage backend.\033[0m")
		fmt.Fprintln(os.Stderr, "This will use 'docker load' which is significantly slower than direct containerd loading.")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "\033[33mIMPORTANT: The digest of the image will be different due to the use of 'docker load'.\033[0m")
		fmt.Fprintln(os.Stderr, "Docker load creates a custom Docker manifest that doesn't adhere to OCI spec.")
		fmt.Fprintln(os.Stderr, "If you can load into the containerd backend, you can load the exact OCI image with the expected digest.")
		fmt.Fprintln(os.Stderr, "See: https://github.com/bazel-contrib/rules_img/issues/76")
		fmt.Fprintln(os.Stderr, "")
		if runtime.GOOS == "darwin" {
			fmt.Fprintln(os.Stderr, "\033[33mmacOS note:\033[0m On macOS, containerd runs in a Linux VM, so the containerd socket")
			fmt.Fprintln(os.Stderr, "is never accessible from the host. Docker is working on exposing the content store")
			fmt.Fprintln(os.Stderr, "via the docker socket, which will soon make incremental loading available via the docker socket.")
			fmt.Fprintln(os.Stderr, "")
		} else {
			fmt.Fprintln(os.Stderr, "To improve performance, configure Docker to use containerd:")
			fmt.Fprintln(os.Stderr, "  https://docs.docker.com/storage/containerd/")
			fmt.Fprintln(os.Stderr, "")
		}
		l.haveContainerd = false
		l.triedContainerd = true
		return nil, fmt.Errorf("connecting to containerd: %w", err)
	}
	l.clientConn = client
	l.haveContainerd = true
	l.triedContainerd = true
	return l.clientConn, nil
}

type taskSet struct {
	vfs                 vfs
	platforms           []string
	blobsForDaemon      map[string]map[string]blobWorkItem
	operationsForDaemon map[string][]api.IndexedLoadDeployOperation
}

func newTaskSet(vfs vfs, platforms []string) *taskSet {
	ts := &taskSet{
		vfs:                 vfs,
		platforms:           platforms,
		blobsForDaemon:      map[string]map[string]blobWorkItem{},
		operationsForDaemon: make(map[string][]api.IndexedLoadDeployOperation),
	}
	return ts
}

func (ts *taskSet) addOperation(op api.IndexedLoadDeployOperation) error {
	ts.operationsForDaemon[op.Daemon] = append(ts.operationsForDaemon[op.Daemon], op)
	if _, exists := ts.blobsForDaemon[op.Daemon]; !exists {
		ts.blobsForDaemon[op.Daemon] = make(map[string]blobWorkItem)
	}
	workItems, err := ts.collectBlobs(op)
	if err != nil {
		return fmt.Errorf("collecting blobs for operation: %w", err)
	}
	for _, item := range workItems {
		digest, err := item.layer.Digest()
		if err != nil {
			return fmt.Errorf("getting digest of blob: %w", err)
		}
		ts.blobsForDaemon[op.Daemon][digest.String()] = item
	}
	return nil
}

func (ts *taskSet) daemons() []string {
	daemons := make([]string, 0, len(ts.operationsForDaemon))
	for daemon := range ts.operationsForDaemon {
		daemons = append(daemons, daemon)
	}
	slices.Sort(daemons)
	return daemons
}

func (ts *taskSet) operations(daemon string) []api.IndexedLoadDeployOperation {
	return ts.operationsForDaemon[daemon]
}

func (ts *taskSet) blobs(daemon string) []blobWorkItem {
	blobMap, exists := ts.blobsForDaemon[daemon]
	if !exists {
		return nil
	}
	blobs := make([]blobWorkItem, 0, len(blobMap))
	for _, item := range blobMap {
		blobs = append(blobs, item)
	}
	return blobs
}

func (ts *taskSet) collectBlobs(op api.IndexedLoadDeployOperation) ([]blobWorkItem, error) {
	digest, err := registryv1.NewHash(op.Root.Digest)
	if err != nil {
		return nil, err
	}

	if op.RootKind == "index" {
		return ts.collectBlobsForIndex(digest)
	} else if op.RootKind == "manifest" {
		// Validate that the single manifest matches requested platform if explicit
		if err := ts.validateManifestPlatform(digest); err != nil {
			return nil, fmt.Errorf("single manifest validation failed: %w", err)
		}
		return ts.collectBlobsForManifest(digest)
	}
	return nil, fmt.Errorf("unsupported root kind: %s", op.RootKind)
}

func (ts *taskSet) collectBlobsForIndex(indexDigest registryv1.Hash) ([]blobWorkItem, error) {
	index, err := ts.vfs.ImageIndex(indexDigest)
	if err != nil {
		return nil, fmt.Errorf("getting image for root %s: %w", indexDigest, err)
	}
	indexManifest, err := index.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("getting index manifest for root %s: %w", indexDigest, err)
	}

	// Determine which platforms to load
	platforms := ts.platforms
	loadAllPlatforms := ts.shouldLoadAllPlatforms()

	// If not loading all platforms and no platforms specified, select default
	if !loadAllPlatforms && len(platforms) == 0 {
		// If only one manifest, use it
		if len(indexManifest.Manifests) == 1 {
			platforms = nil // Will match the single manifest
		} else {
			// Use current platform
			platforms = []string{getCurrentPlatform()}
		}
	}

	var allBlobs []blobWorkItem
	indexLabels := make(map[string]string)
	labelIndex := 0
	matchedAny := false
	for _, manifestDesc := range indexManifest.Manifests {
		// Skip manifests that don't match the platform filter (unless loading all)
		if !loadAllPlatforms && !platformMatches(manifestDesc.Platform, platforms) {
			continue
		}

		matchedAny = true
		blobs, err := ts.collectBlobsForManifest(manifestDesc.Digest)
		if err != nil {
			return nil, err
		}
		allBlobs = append(allBlobs, blobs...)
		indexLabels[fmt.Sprintf("containerd.io/gc.ref.content.m.%d", labelIndex)] = manifestDesc.Digest.String()
		labelIndex++
	}

	// If explicit platforms were requested but none matched, fail
	if ts.hasExplicitPlatforms() && !matchedAny {
		return nil, fmt.Errorf("no manifest found matching requested platform(s): %v", ts.platforms)
	}

	// Add the index itself as a blob to upload
	indexLayer, err := ts.vfs.ManifestBlob(indexDigest)
	if err != nil {
		return nil, fmt.Errorf("getting manifest blob for %s: %w", indexDigest.String(), err)
	}

	allBlobs = append(allBlobs, blobWorkItem{
		layer:  indexLayer,
		labels: indexLabels,
	})

	return allBlobs, nil
}

// shouldLoadAllPlatforms returns true if the "all" sentinel value is present in platforms
func (ts *taskSet) shouldLoadAllPlatforms() bool {
	return contains(ts.platforms, "all")
}

// hasExplicitPlatforms returns true if the user explicitly specified platform(s) (excluding "all")
func (ts *taskSet) hasExplicitPlatforms() bool {
	if len(ts.platforms) == 0 {
		return false
	}
	// If only "all" is specified, it's not explicit platform selection
	if len(ts.platforms) == 1 && ts.platforms[0] == "all" {
		return false
	}
	return true
}

// validateManifestPlatform checks if a manifest's platform matches the requested platforms
// Returns an error if explicit platforms were requested but don't match
func (ts *taskSet) validateManifestPlatform(digest registryv1.Hash) error {
	if !ts.hasExplicitPlatforms() {
		return nil // No explicit platforms requested, any platform is OK
	}

	img, err := ts.vfs.Image(digest)
	if err != nil {
		return fmt.Errorf("getting image for validation: %w", err)
	}

	configFile, err := img.ConfigFile()
	if err != nil {
		return fmt.Errorf("getting config file for validation: %w", err)
	}

	manifestPlatform := registryv1.Platform{
		OS:           configFile.OS,
		Architecture: configFile.Architecture,
		Variant:      configFile.Variant,
	}

	if !platformMatches(&manifestPlatform, ts.platforms) {
		return fmt.Errorf("image platform %s/%s does not match requested platform(s): %v",
			manifestPlatform.OS, manifestPlatform.Architecture, ts.platforms)
	}

	return nil
}

// validateManifestPlatform checks if a manifest's platform matches the requested platforms
// Returns an error if explicit platforms were requested but don't match
func (l *loader) validateManifestPlatform(digest registryv1.Hash) error {
	if !l.hasExplicitPlatforms() {
		return nil // No explicit platforms requested, any platform is OK
	}

	img, err := l.vfs.Image(digest)
	if err != nil {
		return fmt.Errorf("getting image for validation: %w", err)
	}

	configFile, err := img.ConfigFile()
	if err != nil {
		return fmt.Errorf("getting config file for validation: %w", err)
	}

	manifestPlatform := registryv1.Platform{
		OS:           configFile.OS,
		Architecture: configFile.Architecture,
		Variant:      configFile.Variant,
	}

	if !platformMatches(&manifestPlatform, l.platforms) {
		return fmt.Errorf("image platform %s/%s does not match requested platform(s): %v",
			manifestPlatform.OS, manifestPlatform.Architecture, l.platforms)
	}

	return nil
}

// hasExplicitPlatforms returns true if the user explicitly specified platform(s) (excluding "all")
func (l *loader) hasExplicitPlatforms() bool {
	if len(l.platforms) == 0 {
		return false
	}
	// If only "all" is specified, it's not explicit platform selection
	if len(l.platforms) == 1 && l.platforms[0] == "all" {
		return false
	}
	return true
}

func (ts *taskSet) collectBlobsForManifest(imageDigest registryv1.Hash) ([]blobWorkItem, error) {
	image, err := ts.vfs.Image(imageDigest)
	if err != nil {
		return nil, fmt.Errorf("getting image for root %s: %w", imageDigest, err)
	}
	imageManifest, err := image.Manifest()
	if err != nil {
		return nil, fmt.Errorf("getting image manifest for root %s: %w", imageDigest, err)
	}

	var blobs []blobWorkItem
	handleLayer := func(entry registryv1.Descriptor) error {
		layer, err := ts.vfs.Layer(entry.Digest)
		if err != nil {
			return fmt.Errorf("getting layer %s: %w", entry.Digest.String(), err)
		}
		blobs = append(blobs, blobWorkItem{
			layer: layer,
		})
		return nil
	}

	if err := handleLayer(imageManifest.Config); err != nil {
		return nil, err
	}

	for _, entry := range imageManifest.Layers {
		if err := handleLayer(entry); err != nil {
			return nil, err
		}
	}

	// Add the manifest itself as a blob to upload
	manifestLayer, err := ts.vfs.ManifestBlob(imageDigest)
	if err != nil {
		return nil, fmt.Errorf("getting manifest blob for %s: %w", imageDigest.String(), err)
	}
	blobs = append(blobs, blobWorkItem{
		layer:  manifestLayer,
		labels: computeManifestGCLabels(imageManifest),
	})

	return blobs, nil
}

type vfs interface {
	ImageIndex(digest registryv1.Hash) (registryv1.ImageIndex, error)
	Image(digest registryv1.Hash) (registryv1.Image, error)
	Layer(digest registryv1.Hash) (registryv1.Layer, error)
	ManifestBlob(digest registryv1.Hash) (registryv1.Layer, error)
	DigestsFromRoot(root registryv1.Hash) ([]registryv1.Hash, error)
	SizeOf(digest registryv1.Hash) (int64, error)
}
