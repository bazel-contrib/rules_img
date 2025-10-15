// Package syncer implements container image synchronization from CAS to registries.
// It provides efficient blob upload with deduplication and concurrent processing
// using a fixed pool of worker goroutines.
package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"

	"github.com/malt3/go-containerregistry/pkg/name"
	v1 "github.com/malt3/go-containerregistry/pkg/v1"
	"github.com/malt3/go-containerregistry/pkg/v1/remote"
	"github.com/malt3/go-containerregistry/pkg/v1/types"
	"golang.org/x/sync/errgroup"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/registry"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
)

// uploadJob represents a single blob upload task for the worker pool.
// It contains all the necessary context and parameters for uploading
// a blob to a container registry.
type uploadJob struct {
	ctx        context.Context
	ref        name.Repository
	desc       api.Descriptor
	pushOp     api.IndexedPushDeployOperation
	remoteOpts []remote.Option
	result     chan error
}

// makeUploadKey creates a composite key for tracking blob uploads.
// The key combines registry, repository, and digest in container image reference format
// to ensure proper deduplication per destination, as the same blob may be uploaded
// to different registries or repositories.
func makeUploadKey(digest string, ref name.Repository) string {
	// Format: registry/repository@digest (e.g., docker.io/library/ubuntu@sha256:abc123...)
	// This ensures uniqueness across different destinations
	return fmt.Sprintf("%s/%s@%s", ref.RegistryStr(), ref.RepositoryStr(), digest)
}

// makeTagKey creates a composite key for tracking tagged manifests.
// The key combines registry, repository, and tag to identify a specific tag.
func makeTagKey(ref name.Repository, tag string) string {
	// Format: registry/repository:tag (e.g., docker.io/library/ubuntu:latest)
	return fmt.Sprintf("%s/%s:%s", ref.RegistryStr(), ref.RepositoryStr(), tag)
}

// Syncer handles container image synchronization from CAS to registries.
// It processes requests from the Build Event Service (BES) and commits
// container images using efficient upload strategies.
//
// Key features:
//   - In-memory caching of small metadata (manifests, configs) to reduce CAS fetches
//   - Blob deduplication to prevent uploading the same content multiple times
//   - Fixed pool of worker goroutines for concurrent blob uploads
//   - Direct integration with go-containerregistry for registry operations
//
// The syncer maintains thread-safe state across multiple concurrent operations
// and provides graceful shutdown capabilities.
type Syncer struct {
	casClient *cas.CAS

	// Memory cache for small metadata (manifests, configs)
	metadataCache map[string][]byte
	cacheMutex    sync.RWMutex

	// Track ongoing blob transfers to avoid duplicates
	ongoingTransfers map[string]chan error
	transferMutex    sync.Mutex

	// Track uploaded blobs to avoid duplicate uploads
	uploadedBlobs map[string]struct{}
	uploadMutex   sync.RWMutex

	// Track uploaded tags to avoid duplicate tagging
	// Maps registry/repository:tag to digest
	uploadedTags map[string]string
	tagMutex     sync.RWMutex

	// Worker pool for blob uploads
	workQueue   chan *uploadJob
	workerCount int
	shutdown    chan struct{}
	workerWg    sync.WaitGroup
}

// New creates a new Syncer instance with the default worker count of 4.
// This is a convenience function that calls NewWithWorkers with a default
// worker pool size suitable for most use cases.
func New(casClient *cas.CAS) *Syncer {
	return NewWithWorkers(casClient, 4)
}

// NewWithWorkers creates a new Syncer instance with the specified number of workers.
// The worker count determines how many blob uploads can occur concurrently.
// If workerCount is <= 0, it defaults to 4.
//
// The syncer immediately starts all worker goroutines and begins processing
// upload jobs from the work queue. The work queue is buffered to 2x the worker
// count for better throughput.
func NewWithWorkers(casClient *cas.CAS, workerCount int) *Syncer {
	if workerCount <= 0 {
		workerCount = 4
	}

	s := &Syncer{
		casClient:        casClient,
		metadataCache:    make(map[string][]byte),
		ongoingTransfers: make(map[string]chan error),
		uploadedBlobs:    make(map[string]struct{}),
		uploadedTags:     make(map[string]string),
		workQueue:        make(chan *uploadJob, workerCount*2), // Buffer for better performance
		workerCount:      workerCount,
		shutdown:         make(chan struct{}),
	}

	// Start worker goroutines
	for i := 0; i < workerCount; i++ {
		s.workerWg.Add(1)
		go s.worker(i)
	}

	log.Printf("Started syncer with %d worker goroutines", workerCount)
	return s
}

// Shutdown gracefully stops the worker pool and waits for all workers to complete.
// It closes the shutdown channel to signal workers to stop, then waits for all
// worker goroutines to finish their current tasks and exit.
//
// This method blocks until all workers have stopped. Any jobs still in the queue
// will not be processed after shutdown begins.
func (s *Syncer) Shutdown() {
	log.Println("Shutting down syncer worker pool...")
	close(s.shutdown)
	s.workerWg.Wait()
	log.Println("Syncer worker pool shutdown complete")
}

// Commit uploads a container image or index to the registry.
// The digest parameter is the SHA256 hash of the push metadata JSON,
// which is produced by the "img deploy-metadata" command and stored in CAS.
//
// This method:
//  1. Retrieves push metadata from CAS using the provided digest
//  2. Parses the metadata to determine if it's an image or multi-platform index
//  3. Orchestrates the upload of all blobs (layers, configs, manifests) using the worker pool
//  4. Writes the final manifest or index to the registry
//
// The upload process uses deduplication to avoid uploading the same blob multiple times
// and leverages the worker pool for concurrent blob uploads.
func (s *Syncer) Commit(ctx context.Context, digest string, sizeBytes int64) error {
	// Parse digest and retrieve push metadata from CAS
	digestBytes, err := hex.DecodeString(digest)
	if err != nil {
		return fmt.Errorf("invalid digest format: %w", err)
	}

	casDigest := cas.SHA256(digestBytes, sizeBytes)
	metadataBytes, err := s.getCachedOrFetch(ctx, casDigest)
	if err != nil {
		return fmt.Errorf("failed to retrieve push metadata from CAS: %w", err)
	}

	var metadata api.DeployManifest
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return fmt.Errorf("failed to parse push metadata: %w", err)
	}

	pushOps, err := metadata.PushOperations()
	if err != nil {
		return fmt.Errorf("failed to get push operations from metadata: %w", err)
	}
	if len(metadata.Operations) == 0 {
		// don't check for len of pushOps, since this may still contain load operations
		return errors.New("no push operations found in metadata")
	}

	for _, op := range pushOps {
		if err := s.commitOne(ctx, op); err != nil {
			return fmt.Errorf("failed to commit image %s: %w", op.Root.Digest, err)
		}
	}

	return nil
}

func (s *Syncer) commitOne(ctx context.Context, pushOp api.IndexedPushDeployOperation) error {
	// Parse base reference without tag for digest-based push
	baseReference := fmt.Sprintf("%s/%s",
		pushOp.Registry,
		pushOp.Repository)

	ref, err := name.NewRepository(baseReference)
	if err != nil {
		return fmt.Errorf("invalid repository %s: %w", baseReference, err)
	}

	remoteOpts := []remote.Option{
		remote.WithContext(ctx),
		registry.WithAuthFromMultiKeychain(),
	}

	rootBlob := pushOp.Root
	mediaType := types.MediaType(rootBlob.MediaType)

	if mediaType.IsIndex() {
		err = s.pushIndex(ctx, ref, pushOp, remoteOpts)
	} else if mediaType.IsImage() {
		err = s.pushImage(ctx, ref, pushOp, remoteOpts)
	} else {
		return fmt.Errorf("unsupported root media type: %s", mediaType)
	}

	if err != nil {
		return err
	}

	needsTagging := false
	for _, tag := range pushOp.Tags {
		tagKey := makeTagKey(ref, tag)
		s.tagMutex.RLock()
		cachedDigest, exists := s.uploadedTags[tagKey]
		s.tagMutex.RUnlock()
		if !exists || cachedDigest != rootBlob.Digest {
			needsTagging = true
			break
		}
	}

	if !needsTagging {
		log.Printf("All tags already point to %s@%s, skipping tagging", ref.Name(), rootBlob.Digest)
		return nil
	}

	digestRef, err := name.ParseReference(ref.String() + "@" + rootBlob.Digest)
	if err != nil {
		return fmt.Errorf("failed to parse digest reference: %w", err)
	}

	desc, err := remote.Get(digestRef, remoteOpts...)
	if err != nil {
		return fmt.Errorf("failed to get descriptor for digest %s: %w", rootBlob.Digest, err)
	}

	for _, tag := range pushOp.Tags {
		tagKey := makeTagKey(ref, tag)

		// Check if tag already points to the correct digest
		s.tagMutex.RLock()
		cachedDigest, exists := s.uploadedTags[tagKey]
		s.tagMutex.RUnlock()
		if exists && cachedDigest == rootBlob.Digest {
			log.Printf("Tag %s already points to %s@%s, skipping", tag, ref.Name(), rootBlob.Digest)
			continue
		}

		tagRef := ref.Tag(tag)

		if err := remote.Tag(tagRef, desc, remoteOpts...); err != nil {
			return fmt.Errorf("failed to tag %s as %s: %w", rootBlob.Digest, tag, err)
		}

		// Update cache with the new digest for this tag
		s.tagMutex.Lock()
		s.uploadedTags[tagKey] = rootBlob.Digest
		s.tagMutex.Unlock()

		log.Printf("Tagged %s as %s", rootBlob.Digest, tagRef.String())
	}

	return nil
}

// getCachedOrFetch retrieves blob data from the in-memory cache or fetches it from CAS.
// Small blobs (< 1MB) are automatically cached after fetching to improve performance
// for frequently accessed metadata like manifests and configs.
//
// This method is thread-safe and uses read-write locks to allow concurrent cache reads
// while ensuring exclusive access during cache writes.
func (s *Syncer) getCachedOrFetch(ctx context.Context, digest cas.Digest) ([]byte, error) {
	digestStr := hex.EncodeToString(digest.Hash)

	// Check cache first
	s.cacheMutex.RLock()
	if cached, exists := s.metadataCache[digestStr]; exists {
		s.cacheMutex.RUnlock()
		return cached, nil
	}
	s.cacheMutex.RUnlock()

	// Not in cache, fetch from CAS
	data, err := s.casClient.ReadBlob(ctx, digest)
	if err != nil {
		return nil, fmt.Errorf("failed to read blob from CAS: %w", err)
	}

	// Cache the data if it's small (under 1MB)
	if len(data) < 1024*1024 {
		s.cacheMutex.Lock()
		defer s.cacheMutex.Unlock()
		s.metadataCache[digestStr] = data
	}

	return data, nil
}

// pushImage uploads a single-platform container image to the registry.
// It follows the proper upload order: layers first, then config, then manifest.
// This ensures that all referenced blobs exist before the manifest is written.
//
// The method uses the worker pool for concurrent layer uploads and deduplication
// to avoid uploading the same blob multiple times.
func (s *Syncer) pushImage(ctx context.Context, ref name.Repository, pushOp api.IndexedPushDeployOperation, remoteOpts []remote.Option) error {
	log.Printf("Pushing image to %s", ref.Name())
	manifestBlob := pushOp.Root

	// Get manifest from CAS
	manifestData, err := s.getBlobFromCAS(ctx, manifestBlob)
	if err != nil {
		return fmt.Errorf("failed to get manifest: %w", err)
	}

	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("failed to parse manifest: %w", err)
	}

	// Upload layers first (with deduplication and concurrency)
	if err := s.uploadLayers(ctx, ref, manifest.Layers, pushOp, remoteOpts); err != nil {
		return fmt.Errorf("failed to upload layers: %w", err)
	}

	// Upload config blob
	configResult := s.queueBlobUpload(ctx, ref, apiDescriptorFromV1(manifest.Config), pushOp, remoteOpts)
	if err := <-configResult; err != nil {
		return fmt.Errorf("failed to upload config: %w", err)
	}

	// Create and push manifest
	img := &casImage{
		syncer:       s,
		manifest:     &manifest,
		manifestData: manifestData,
		pushOp:       pushOp,
	}

	// Create a digest reference for pushing
	digestRef := ref.Digest(manifestBlob.Digest)
	if err := remote.Write(digestRef, img, remoteOpts...); err != nil {
		return fmt.Errorf("failed to write manifest: %w", err)
	}

	log.Printf("Pushed image %s@%s", ref.Name(), manifestBlob.Digest)
	return nil
}

// pushIndex uploads a multi-platform container image index to the registry.
// It coordinates the upload of all platform-specific manifests and their layers
// before writing the index manifest that references them.
//
// The method uses errgroups to upload multiple platform manifests concurrently,
// with each manifest upload handled by uploadManifestAndLayers.
func (s *Syncer) pushIndex(ctx context.Context, ref name.Repository, pushOp api.IndexedPushDeployOperation, remoteOpts []remote.Option) error {
	log.Printf("Pushing index to %s", ref.Name())
	indexBlob := pushOp.Root

	// Get index from CAS
	indexData, err := s.getBlobFromCAS(ctx, indexBlob)
	if err != nil {
		return fmt.Errorf("failed to get index: %w", err)
	}

	var index v1.IndexManifest
	if err := json.Unmarshal(indexData, &index); err != nil {
		return fmt.Errorf("failed to parse index: %w", err)
	}

	// Upload all manifests and their layers concurrently
	eg, egCtx := errgroup.WithContext(ctx)
	for _, manifestDesc := range index.Manifests {
		manifestDesc := manifestDesc // capture loop variable
		eg.Go(func() error {
			return s.uploadManifestAndLayers(egCtx, ref, manifestDesc, pushOp, remoteOpts)
		})
	}

	if err := eg.Wait(); err != nil {
		return fmt.Errorf("failed to upload manifests: %w", err)
	}

	// Create and push index
	idx := &casIndex{
		syncer:    s,
		index:     &index,
		indexData: indexData,
		pushOp:    pushOp,
	}

	// Create a digest reference for pushing
	digestRef := ref.Digest(indexBlob.Digest)
	if err := remote.WriteIndex(digestRef, idx, remoteOpts...); err != nil {
		return fmt.Errorf("failed to write index: %w", err)
	}

	log.Printf("Pushed index %s@%s", ref.Name(), indexBlob.Digest)
	return nil
}

// uploadManifestAndLayers uploads a platform-specific manifest and all its associated layers.
// This method is used when processing multi-platform indexes to upload each platform's
// manifest and layers before writing the index.
//
// It follows the proper upload order: layers, config, then the manifest blob itself.
func (s *Syncer) uploadManifestAndLayers(ctx context.Context, ref name.Repository, manifestDesc v1.Descriptor, pushOp api.IndexedPushDeployOperation, remoteOpts []remote.Option) error {
	// Find the manifest blob in metadata
	var manifestBlob *api.Descriptor
	for _, manifestInfo := range pushOp.Manifests {
		if manifestInfo.Descriptor.Digest == manifestDesc.Digest.String() {
			manifestBlob = &manifestInfo.Descriptor
			break
		}
	}
	if manifestBlob == nil {
		return fmt.Errorf("manifest %s not found in metadata", manifestDesc.Digest)
	}

	// Get manifest from CAS
	manifestData, err := s.getBlobFromCAS(ctx, *manifestBlob)
	if err != nil {
		return fmt.Errorf("failed to get manifest %s: %w", manifestDesc.Digest, err)
	}

	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("failed to parse manifest %s: %w", manifestDesc.Digest, err)
	}

	// Upload layers
	if err := s.uploadLayers(ctx, ref, manifest.Layers, pushOp, remoteOpts); err != nil {
		return fmt.Errorf("failed to upload layers for manifest %s: %w", manifestDesc.Digest, err)
	}

	// Upload config blob
	configResult := s.queueBlobUpload(ctx, ref, apiDescriptorFromV1(manifest.Config), pushOp, remoteOpts)
	if err := <-configResult; err != nil {
		return fmt.Errorf("failed to upload config for manifest %s: %w", manifestDesc.Digest, err)
	}

	// Upload the manifest itself
	manifestResult := s.queueBlobUpload(ctx, ref, apiDescriptorFromV1(manifestDesc), pushOp, remoteOpts)
	if err := <-manifestResult; err != nil {
		return fmt.Errorf("failed to upload manifest %s: %w", manifestDesc.Digest, err)
	}

	return nil
}

// uploadLayers uploads multiple container layers concurrently using the worker pool.
// It queues each layer for upload and waits for all uploads to complete before returning.
// Deduplication is handled automatically by queueBlobUpload.
//
// If any layer fails to upload, the method returns immediately with an error.
func (s *Syncer) uploadLayers(ctx context.Context, ref name.Repository, layers []v1.Descriptor, pushOp api.IndexedPushDeployOperation, remoteOpts []remote.Option) error {
	if len(layers) == 0 {
		return nil
	}

	// Create result channels for each layer
	results := make([]chan error, len(layers))
	for i, layer := range layers {
		results[i] = s.queueBlobUpload(ctx, ref, apiDescriptorFromV1(layer), pushOp, remoteOpts)
	}

	// Wait for all uploads to complete
	for i, result := range results {
		if err := <-result; err != nil {
			return fmt.Errorf("failed to upload layer %d: %w", i, err)
		}
	}

	return nil
}

// queueBlobUpload queues a blob upload job with the worker pool and returns a result channel.
// This method handles deduplication by checking if the blob is already uploaded or
// currently being uploaded by another goroutine.
//
// The returned channel will receive exactly one value: nil on success or an error.
// Callers should read from this channel to wait for upload completion.
//
// Deduplication behavior:
//   - If blob is already uploaded: returns immediately with nil
//   - If blob upload is in progress: waits for the ongoing upload to complete
//   - If blob is not uploaded: queues a new upload job
func (s *Syncer) queueBlobUpload(ctx context.Context, ref name.Repository, desc api.Descriptor, pushOp api.IndexedPushDeployOperation, remoteOpts []remote.Option) chan error {
	digest := desc.Digest
	uploadKey := makeUploadKey(digest, ref)
	result := make(chan error, 1)

	// Check if already uploaded (deduplication)
	s.uploadMutex.RLock()
	if _, exists := s.uploadedBlobs[uploadKey]; exists {
		s.uploadMutex.RUnlock()
		result <- nil
		return result
	}
	s.uploadMutex.RUnlock()

	// Check if upload is in progress
	s.transferMutex.Lock()
	if ongoing, exists := s.ongoingTransfers[uploadKey]; exists {
		s.transferMutex.Unlock()
		// Wait for the ongoing transfer to complete and return its result
		go func() {
			result <- <-ongoing
		}()
		return result
	}

	// Mark as in progress
	s.ongoingTransfers[uploadKey] = result
	s.transferMutex.Unlock()

	// Queue the job
	job := &uploadJob{
		ctx:        ctx,
		ref:        ref,
		desc:       desc,
		pushOp:     pushOp,
		remoteOpts: remoteOpts,
		result:     result,
	}

	select {
	case s.workQueue <- job:
		// Job queued successfully
	case <-ctx.Done():
		// Context canceled, clean up and return error
		s.transferMutex.Lock()
		delete(s.ongoingTransfers, uploadKey)
		s.transferMutex.Unlock()
		result <- ctx.Err()
	}

	return result
}

// uploadBlob performs the actual blob upload to the registry.
// This method is called by worker goroutines to process queued upload jobs.
// It creates a layer wrapper and uploads it to the registry using go-containerregistry.
func (s *Syncer) uploadBlob(ctx context.Context, ref name.Repository, desc api.Descriptor, pushOp api.IndexedPushDeployOperation, remoteOpts []remote.Option) error {
	digest := desc.Digest
	digestAsMissingBlob := strings.TrimPrefix(digest, "sha256:")
	uploadKey := makeUploadKey(digest, ref)

	var layer v1.Layer

	// Check if this is a missing blob from shallow base image pull
	isMissing := false

	allMissingBlobs := []string{}
	for _, manifestInfo := range pushOp.Manifests {
		allMissingBlobs = append(allMissingBlobs, manifestInfo.MissingBlobs...)
	}
	for _, missingDigest := range allMissingBlobs {
		if missingDigest == digestAsMissingBlob {
			isMissing = true
			break
		}
	}

	if isMissing {
		// Layer is from base image and not in CAS, stream from original registry
		layer = &remoteStreamingLayer{
			digest:    digest,
			diffID:    desc.DiffID,
			size:      desc.Size,
			mediaType: desc.MediaType,
			desc:      desc,
			pullInfo:  pushOp.PullInfo,
		}
	} else {
		// Layer is in CAS
		layer = &casStreamingLayer{
			syncer:    s,
			digest:    digest,
			diffID:    desc.DiffID,
			size:      desc.Size,
			mediaType: desc.MediaType,
			desc:      desc,
		}
	}

	// Upload to registry
	if err := remote.WriteLayer(ref, layer, remoteOpts...); err != nil {
		return fmt.Errorf("failed to upload blob %s: %w", digest, err)
	}

	// Mark as uploaded
	s.uploadMutex.Lock()
	defer s.uploadMutex.Unlock()
	s.uploadedBlobs[uploadKey] = struct{}{}

	return nil
}

// worker is the main goroutine function for processing blob upload jobs.
// Each worker runs in its own goroutine and continuously processes jobs from
// the work queue until the shutdown signal is received.
//
// The worker handles:
//   - Final deduplication check before upload
//   - Actual blob upload via uploadBlob
//   - Cleanup of ongoing transfer tracking
//   - Graceful shutdown when signaled
func (s *Syncer) worker(id int) {
	defer s.workerWg.Done()

	for {
		select {
		case <-s.shutdown:
			return
		case job := <-s.workQueue:
			func() {
				digest := job.desc.Digest
				uploadKey := makeUploadKey(digest, job.ref)

				// Clean up ongoing transfer tracking when done
				defer func() {
					s.transferMutex.Lock()
					defer s.transferMutex.Unlock()
					delete(s.ongoingTransfers, uploadKey)
				}()

				// Double-check if already uploaded (race condition protection)
				s.uploadMutex.RLock()
				alreadyUploaded := false
				if _, exists := s.uploadedBlobs[uploadKey]; exists {
					alreadyUploaded = true
				}
				s.uploadMutex.RUnlock()

				if alreadyUploaded {
					job.result <- nil
					return
				}

				// Perform the upload
				err := s.uploadBlob(job.ctx, job.ref, job.desc, job.pushOp, job.remoteOpts)

				// Send result
				job.result <- err
			}()
		}
	}
}

// getBlobFromCAS retrieves blob data from CAS using the provided descriptor.
// It converts the descriptor's SHA256 digest to the CAS digest format and
// uses getCachedOrFetch to retrieve the data, benefiting from caching.
//
// Only SHA256 digests are supported. The method expects descriptors with
// digests in the format "sha256:hex_string".
func (s *Syncer) getBlobFromCAS(ctx context.Context, desc api.Descriptor) ([]byte, error) {
	if !strings.HasPrefix(desc.Digest, "sha256:") {
		return nil, fmt.Errorf("unsupported digest algorithm in %s", desc.Digest)
	}

	hashHex := desc.Digest[7:] // Remove "sha256:" prefix
	hashBytes, err := hex.DecodeString(hashHex)
	if err != nil {
		return nil, fmt.Errorf("invalid digest hex: %w", err)
	}

	casDigest := cas.SHA256(hashBytes, desc.Size)
	return s.getCachedOrFetch(ctx, casDigest)
}

// apiDescriptorFromV1 converts a go-containerregistry v1.Descriptor to an api.Descriptor.
// This helper function bridges the type systems between go-containerregistry
// and the internal API types used by the syncer.
func apiDescriptorFromV1(desc v1.Descriptor) api.Descriptor {
	return api.Descriptor{
		MediaType: string(desc.MediaType),
		Digest:    desc.Digest.String(),
		Size:      desc.Size,
	}
}

// casImage implements the go-containerregistry v1.Image interface with CAS backing.
// It provides lazy access to image data stored in CAS, allowing go-containerregistry
// to work with images without requiring all data to be loaded into memory.
//
// This type is used when writing single-platform images to registries.
type casImage struct {
	syncer       *Syncer
	manifest     *v1.Manifest
	manifestData []byte
	pushOp       api.IndexedPushDeployOperation
	config       *v1.ConfigFile // cached config
	configOnce   sync.Once
	configErr    error
}

func (i *casImage) MediaType() (types.MediaType, error) {
	return i.manifest.MediaType, nil
}

func (i *casImage) Size() (int64, error) {
	return int64(len(i.manifestData)), nil
}

func (i *casImage) ConfigName() (v1.Hash, error) {
	return i.manifest.Config.Digest, nil
}

func (i *casImage) ConfigFile() (*v1.ConfigFile, error) {
	i.configOnce.Do(func() {
		configData, err := i.syncer.getBlobFromCAS(context.Background(), apiDescriptorFromV1(i.manifest.Config))
		if err != nil {
			i.configErr = err
			return
		}

		var config v1.ConfigFile
		if err := json.Unmarshal(configData, &config); err != nil {
			i.configErr = err
			return
		}
		i.config = &config
	})
	return i.config, i.configErr
}

func (i *casImage) RawConfigFile() ([]byte, error) {
	return i.syncer.getBlobFromCAS(context.Background(), apiDescriptorFromV1(i.manifest.Config))
}

func (i *casImage) Digest() (v1.Hash, error) {
	// Calculate digest of manifest
	hash := sha256.Sum256(i.manifestData)
	h := v1.Hash{
		Algorithm: "sha256",
		Hex:       fmt.Sprintf("%x", hash),
	}
	return h, nil
}

func (i *casImage) Manifest() (*v1.Manifest, error) {
	return i.manifest, nil
}

func (i *casImage) RawManifest() ([]byte, error) {
	return i.manifestData, nil
}

func (i *casImage) LayerByDigest(hash v1.Hash) (v1.Layer, error) {
	// Get config to access DiffIDs
	config, err := i.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("failed to get config: %w", err)
	}

	for idx, layer := range i.manifest.Layers {
		if layer.Digest == hash {
			diffID := ""
			if idx < len(config.RootFS.DiffIDs) {
				diffID = config.RootFS.DiffIDs[idx].String()
			}
			return &casLayer{
				digest:    layer.Digest.String(),
				diffID:    diffID,
				size:      layer.Size,
				mediaType: string(layer.MediaType),
			}, nil
		}
	}
	return nil, fmt.Errorf("layer %s not found", hash)
}

func (i *casImage) Layers() ([]v1.Layer, error) {
	// Get config to access DiffIDs
	config, err := i.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("failed to get config: %w", err)
	}

	layers := make([]v1.Layer, len(i.manifest.Layers))
	for j, layer := range i.manifest.Layers {
		diffID := ""
		if j < len(config.RootFS.DiffIDs) {
			diffID = config.RootFS.DiffIDs[j].String()
		}
		layers[j] = &casLayer{
			digest:    layer.Digest.String(),
			diffID:    diffID,
			size:      layer.Size,
			mediaType: string(layer.MediaType),
		}
	}
	return layers, nil
}

func (i *casImage) LayerByDiffID(hash v1.Hash) (v1.Layer, error) {
	// For simplicity, assume DiffID matches Digest in our implementation
	// In practice, this should handle compressed vs uncompressed hashes
	return i.LayerByDigest(hash)
}

// casIndex implements v1.ImageIndex interface backed by CAS
// casIndex implements the go-containerregistry v1.ImageIndex interface with CAS backing.
// It provides lazy access to multi-platform image index data stored in CAS.
//
// This type is used when writing multi-platform image indexes to registries.
type casIndex struct {
	syncer    *Syncer
	index     *v1.IndexManifest
	indexData []byte
	pushOp    api.IndexedPushDeployOperation
}

func (idx *casIndex) MediaType() (types.MediaType, error) {
	return idx.index.MediaType, nil
}

func (idx *casIndex) Size() (int64, error) {
	return int64(len(idx.indexData)), nil
}

func (idx *casIndex) Digest() (v1.Hash, error) {
	hash := sha256.Sum256(idx.indexData)
	h := v1.Hash{
		Algorithm: "sha256",
		Hex:       fmt.Sprintf("%x", hash),
	}
	return h, nil
}

func (idx *casIndex) IndexManifest() (*v1.IndexManifest, error) {
	return idx.index, nil
}

func (idx *casIndex) RawManifest() ([]byte, error) {
	return idx.indexData, nil
}

func (idx *casIndex) Image(hash v1.Hash) (v1.Image, error) {
	for _, manifest := range idx.index.Manifests {
		if manifest.Digest == hash {
			manifestData, err := idx.syncer.getBlobFromCAS(context.Background(), apiDescriptorFromV1(manifest))
			if err != nil {
				return nil, err
			}

			var manifestObj v1.Manifest
			if err := json.Unmarshal(manifestData, &manifestObj); err != nil {
				return nil, err
			}

			return &casImage{
				syncer:       idx.syncer,
				manifest:     &manifestObj,
				manifestData: manifestData,
				pushOp:       idx.pushOp,
			}, nil
		}
	}
	return nil, fmt.Errorf("image %s not found in index", hash)
}

func (idx *casIndex) ImageIndex(hash v1.Hash) (v1.ImageIndex, error) {
	// This would be for nested indexes, which is less common
	return nil, fmt.Errorf("nested indexes not implemented")
}

// casLayer implements v1.Layer interface backed by CAS
// casLayer implements the go-containerregistry v1.Layer interface without actual data.
// Since blob uploads are handled in advance, this layer serves only as metadata
// holder and returns errors for any data access methods, as the data should
// already be present in the target registry.
type casLayer struct {
	digest    string
	diffID    string
	size      int64
	mediaType string
}

func (l *casLayer) Digest() (v1.Hash, error) {
	if !strings.HasPrefix(l.digest, "sha256:") {
		return v1.Hash{}, fmt.Errorf("unsupported digest algorithm: %s", l.digest)
	}
	return v1.Hash{
		Algorithm: "sha256",
		Hex:       l.digest[7:],
	}, nil
}

func (l *casLayer) DiffID() (v1.Hash, error) {
	if !strings.HasPrefix(l.diffID, "sha256:") {
		return v1.Hash{}, fmt.Errorf("unsupported diff id algorithm: %s", l.diffID)
	}
	return v1.Hash{
		Algorithm: "sha256",
		Hex:       l.diffID[7:],
	}, nil
}

func (l *casLayer) Size() (int64, error) {
	return l.size, nil
}

func (l *casLayer) MediaType() (types.MediaType, error) {
	return types.MediaType(l.mediaType), nil
}

func (l *casLayer) Compressed() (io.ReadCloser, error) {
	return nil, errors.New("layers should never be requested here, we already uploaded them")
}

func (l *casLayer) Uncompressed() (io.ReadCloser, error) {
	return nil, errors.New("layer data should not be accessed - blobs are already uploaded to registry")
}

// casStreamingLayer implements the go-containerregistry v1.Layer interface with streaming CAS access.
// It provides blob data by streaming directly from CAS to avoid loading large blobs into memory.
// This is used for large blobs (> 1MB) to minimize memory usage during registry uploads.
type casStreamingLayer struct {
	syncer    *Syncer
	digest    string
	diffID    string
	size      int64
	mediaType string
	desc      api.Descriptor
}

// remoteStreamingLayer implements the go-containerregistry v1.Layer interface for layers from shallow base pulls.
// It streams blob data directly from the original registry when the blob is not available in CAS.
// This is used for base image layers that were not downloaded during the shallow pull.
type remoteStreamingLayer struct {
	digest    string
	diffID    string
	size      int64
	mediaType string
	desc      api.Descriptor
	pullInfo  api.PullInfo
}

func (l *casStreamingLayer) Digest() (v1.Hash, error) {
	if !strings.HasPrefix(l.digest, "sha256:") {
		return v1.Hash{}, fmt.Errorf("unsupported digest algorithm: %s", l.digest)
	}
	return v1.Hash{
		Algorithm: "sha256",
		Hex:       l.digest[7:],
	}, nil
}

func (l *casStreamingLayer) DiffID() (v1.Hash, error) {
	if !strings.HasPrefix(l.diffID, "sha256:") {
		return v1.Hash{}, fmt.Errorf("unsupported diff id algorithm: %s", l.diffID)
	}
	return v1.Hash{
		Algorithm: "sha256",
		Hex:       l.diffID[7:],
	}, nil
}

func (l *casStreamingLayer) Size() (int64, error) {
	return l.size, nil
}

func (l *casStreamingLayer) MediaType() (types.MediaType, error) {
	return types.MediaType(l.mediaType), nil
}

func (l *casStreamingLayer) Compressed() (io.ReadCloser, error) {
	// Convert API descriptor to CAS digest format
	if !strings.HasPrefix(l.desc.Digest, "sha256:") {
		return nil, fmt.Errorf("unsupported digest algorithm in %s", l.desc.Digest)
	}

	hashHex := l.desc.Digest[7:] // Remove "sha256:" prefix
	hashBytes, err := hex.DecodeString(hashHex)
	if err != nil {
		return nil, fmt.Errorf("invalid digest hex: %w", err)
	}

	casDigest := cas.SHA256(hashBytes, l.desc.Size)

	// Get streaming reader from CAS
	return l.syncer.casClient.ReaderForBlob(context.Background(), casDigest)
}

func (l *casStreamingLayer) Uncompressed() (io.ReadCloser, error) {
	return nil, errors.New("layer data should not be accessed - blobs are already uploaded to registry")
}

func (l *remoteStreamingLayer) Digest() (v1.Hash, error) {
	if !strings.HasPrefix(l.digest, "sha256:") {
		return v1.Hash{}, fmt.Errorf("unsupported digest algorithm: %s", l.digest)
	}
	return v1.Hash{
		Algorithm: "sha256",
		Hex:       l.digest[7:],
	}, nil
}

func (l *remoteStreamingLayer) DiffID() (v1.Hash, error) {
	if !strings.HasPrefix(l.diffID, "sha256:") {
		return v1.Hash{}, fmt.Errorf("unsupported diff id algorithm: %s", l.diffID)
	}
	return v1.Hash{
		Algorithm: "sha256",
		Hex:       l.diffID[7:],
	}, nil
}

func (l *remoteStreamingLayer) Size() (int64, error) {
	return l.size, nil
}

func (l *remoteStreamingLayer) MediaType() (types.MediaType, error) {
	return types.MediaType(l.mediaType), nil
}

func (l *remoteStreamingLayer) Compressed() (io.ReadCloser, error) {
	// Stream from the original registry
	if len(l.pullInfo.OriginalBaseImageRegistries) == 0 {
		return nil, fmt.Errorf("no original registries provided for remote layer %s", l.digest)
	}

	// Construct the reference to the blob in the original registry
	ref, err := name.NewDigest(fmt.Sprintf("%s/%s@%s",
		l.pullInfo.OriginalBaseImageRegistries[0],
		l.pullInfo.OriginalBaseImageRepository,
		l.digest))
	if err != nil {
		return nil, fmt.Errorf("creating blob reference: %w", err)
	}

	// Fetch the layer from the original registry
	layer, err := remote.Layer(ref, registry.WithAuthFromMultiKeychain())
	if err != nil {
		return nil, fmt.Errorf("getting layer from original registry: %w", err)
	}

	return layer.Compressed()
}

func (l *remoteStreamingLayer) Uncompressed() (io.ReadCloser, error) {
	return nil, errors.New("layer data should not be accessed - blobs are already uploaded to registry")
}
