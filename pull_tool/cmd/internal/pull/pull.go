package pull

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	reg "github.com/bazel-contrib/rules_img/pull_tool/pkg/auth/registry"
	"github.com/bazel-contrib/rules_img/pull_tool/pkg/blobstore"
	"github.com/bazel-contrib/rules_img/pull_tool/pkg/transport/cachedblob"
	"github.com/google/go-containerregistry/pkg/name"
	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func PullProcess(ctx context.Context, args []string) {
	var reference string
	var repository string
	var outputDir string
	var registries stringSliceFlag
	var layerHandling string
	var concurrency int
	var airgapped bool

	flagSet := flag.NewFlagSet("pull", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Downloads an image from a container registry.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: pull_tool pull [OPTIONS]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"pull_tool pull --reference sha256:abc123... --repository myapp --output ./outdir",
			"pull_tool pull --reference sha256:abc123... --repository myapp --registry docker.io",
		}
		fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
	}

	flagSet.StringVar(&reference, "reference", "", "The reference of the image to download (required)")
	flagSet.StringVar(&repository, "repository", "", "Repository name of the image (required)")
	flagSet.StringVar(&outputDir, "output", ".", "Output directory to save the downloaded image to")
	flagSet.Var(&registries, "registry", "Registry to use (can be specified multiple times, defaults to docker.io)")
	flagSet.StringVar(&layerHandling, "layer-handling", "shallow", "Method used for handling layer data. \"eager\" causes layer data to be materialized.")
	flagSet.IntVar(&concurrency, "j", 10, "Number of concurrent download workers")
	flagSet.BoolVar(&airgapped, "airgapped", false, "Enable airgapped mode (only use local cached blobs, no network access)")

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}

	if reference == "" {
		fmt.Fprintf(os.Stderr, "Error: --reference is required\n")
		flagSet.Usage()
		os.Exit(1)
	}
	if repository == "" {
		fmt.Fprintf(os.Stderr, "Error: --repository is required\n")
		flagSet.Usage()
		os.Exit(1)
	}
	if outputDir == "" {
		fmt.Fprintf(os.Stderr, "Error: --output must be a valid path\n")
		flagSet.Usage()
		os.Exit(1)
	}

	// Initialize blob store
	store := blobstore.New(filepath.Join(outputDir, "blobs"))
	if err := store.Init(); err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing blob store: %v\n", err)
		os.Exit(1)
	}
	// Create a custom HTTP client with cached blob transport
	transport := cachedblob.NewTransport(outputDir, http.DefaultTransport, cachedblob.WithAirgapped(airgapped))

	// Default to docker.io if no registries specified
	if len(registries) == 0 {
		registries = []string{"docker.io"}
	}

	var digest string
	if strings.HasPrefix(reference, "sha256:") {
		digest = reference
	}

	// Try each registry until success
	var lastErr error
	for _, registry := range registries {
		err := pullFromRegistry(ctx, registry, repository, reference, digest, store, transport, layerHandling, concurrency)
		if err == nil {
			return
		}
		lastErr = err
		fmt.Fprintf(os.Stderr, "Failed to download from %s: %v\n", registry, err)
	}

	fmt.Fprintf(os.Stderr, "Error: Failed to download blob from all registries: %v\n", lastErr)
	os.Exit(1)
}

type downloadJob struct {
	layer registryv1.Layer
	store *blobstore.Store
}

type workerPool struct {
	jobs    chan downloadJob
	results chan error
	wg      *sync.WaitGroup
	ctx     context.Context
}

func newWorkerPool(ctx context.Context, numWorkers int) *workerPool {
	return &workerPool{
		jobs:    make(chan downloadJob, numWorkers*2),
		results: make(chan error, numWorkers*2),
		wg:      &sync.WaitGroup{},
		ctx:     ctx,
	}
}

func (wp *workerPool) start(numWorkers int) {
	for i := 0; i < numWorkers; i++ {
		wp.wg.Add(1)
		go wp.worker()
	}
}

func (wp *workerPool) worker() {
	defer wp.wg.Done()
	for job := range wp.jobs {
		select {
		case <-wp.ctx.Done():
			wp.results <- wp.ctx.Err()
			return
		default:
			err := downloadLayer(job.layer, job.store)
			wp.results <- err
		}
	}
}

func (wp *workerPool) submit(job downloadJob) {
	wp.jobs <- job
}

func (wp *workerPool) close() {
	close(wp.jobs)
}

func (wp *workerPool) wait() {
	wp.wg.Wait()
	close(wp.results)
}

func pullFromRegistry(ctx context.Context, registry, repository, tag, digest string, store *blobstore.Store, transport http.RoundTripper, layerHandling string, concurrency int) error {
	desc, err := downloadManifest(registry, repository, tag, digest, store, transport)
	if err != nil {
		return fmt.Errorf("downloading manifest: %w", err)
	}
	var layers []registryv1.Layer
	if desc.Descriptor.MediaType.IsImage() {
		image, err := desc.Image()
		if err != nil {
			return fmt.Errorf("getting image from descriptor: %w", err)
		}
		layers, err = downloadImage(image, store)
		if err != nil {
			return fmt.Errorf("downloading image: %w", err)
		}
	} else if desc.Descriptor.MediaType.IsIndex() {
		index, err := desc.ImageIndex()
		if err != nil {
			return fmt.Errorf("getting index from descriptor: %w", err)
		}
		layers, err = downloadIndex(ctx, index, store, concurrency)
		if err != nil {
			return fmt.Errorf("downloading index: %w", err)
		}
	} else {
		return fmt.Errorf("unsupported media type: %s", desc.Descriptor.MediaType)
	}
	if layerHandling != "eager" {
		return nil
	}

	pool := newWorkerPool(ctx, concurrency)
	pool.start(concurrency)

	var errors []error
	var errorsMu sync.Mutex
	var resultsWg sync.WaitGroup
	resultsWg.Add(1)

	go func() {
		defer resultsWg.Done()
		for err := range pool.results {
			if err != nil {
				errorsMu.Lock()
				errors = append(errors, err)
				errorsMu.Unlock()
			}
		}
	}()

	for _, layer := range layers {
		pool.submit(downloadJob{layer: layer, store: store})
	}

	pool.close()
	pool.wait()
	resultsWg.Wait()

	if len(errors) > 0 {
		return fmt.Errorf("failed to download %d layers: %v", len(errors), errors[0])
	}
	return nil
}

type manifestJob struct {
	index registryv1.ImageIndex
	desc  registryv1.Descriptor
	store *blobstore.Store
	i     int
}

type manifestResult struct {
	layers []registryv1.Layer
	err    error
}

func downloadIndex(ctx context.Context, index registryv1.ImageIndex, store *blobstore.Store, concurrency int) ([]registryv1.Layer, error) {
	manifests, err := index.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("getting index manifest: %w", err)
	}

	jobs := make(chan manifestJob, len(manifests.Manifests))
	results := make(chan manifestResult, len(manifests.Manifests))

	numWorkers := concurrency
	if numWorkers > len(manifests.Manifests) {
		numWorkers = len(manifests.Manifests)
	}

	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				select {
				case <-ctx.Done():
					results <- manifestResult{err: ctx.Err()}
					return
				default:
					layers, err := processManifest(job.index, job.desc, job.store, job.i)
					results <- manifestResult{layers: layers, err: err}
				}
			}
		}()
	}

	for i, desc := range manifests.Manifests {
		jobs <- manifestJob{index: index, desc: desc, store: store, i: i}
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	var allLayers []registryv1.Layer
	for result := range results {
		if result.err != nil {
			return nil, result.err
		}
		allLayers = append(allLayers, result.layers...)
	}

	return allLayers, nil
}

func processManifest(index registryv1.ImageIndex, desc registryv1.Descriptor, store *blobstore.Store, i int) ([]registryv1.Layer, error) {
	image, err := index.Image(desc.Digest)
	if err != nil {
		return nil, fmt.Errorf("getting image %d from index: %w", i, err)
	}

	manifestBytes, err := image.RawManifest()
	if err != nil {
		return nil, fmt.Errorf("getting image %d from index: %w", i, err)
	}

	// Store manifest blob using the digest
	digestStr := desc.Digest.String()
	if err := store.WriteSmallWithDigest(digestStr, manifestBytes); err != nil {
		return nil, fmt.Errorf("writing image manifest %d: %w", i, err)
	}

	imageLayers, err := downloadImage(image, store)
	if err != nil {
		return nil, fmt.Errorf("downloading image %d: %w", i, err)
	}
	return imageLayers, nil
}

func downloadImage(image registryv1.Image, store *blobstore.Store) ([]registryv1.Layer, error) {
	// download the config
	configHash, err := image.ConfigName()
	if err != nil {
		return nil, fmt.Errorf("getting config digest: %w", err)
	}

	rawConfig, err := image.RawConfigFile()
	if err != nil {
		return nil, fmt.Errorf("getting config file: %w", err)
	}

	// Store config blob using the digest
	digestStr := configHash.String()
	if err := store.WriteSmallWithDigest(digestStr, rawConfig); err != nil {
		return nil, fmt.Errorf("writing config file: %w", err)
	}

	return image.Layers()
}

func downloadLayer(layer registryv1.Layer, store *blobstore.Store) error {
	digest, err := layer.Digest()
	if err != nil {
		return fmt.Errorf("getting layer digest: %w", err)
	}

	// Get the compressed layer reader
	rc, err := layer.Compressed()
	if err != nil {
		return fmt.Errorf("getting compressed layer: %w", err)
	}
	defer rc.Close()

	// Store the layer using WriteLarge since layers can be big
	digestStr := digest.String()
	if err := store.WriteLarge(digestStr, rc); err != nil {
		return fmt.Errorf("writing layer %s: %w", digestStr, err)
	}

	return nil
}

func downloadManifest(registry, repository, tag, digest string, store *blobstore.Store, transport http.RoundTripper) (*remote.Descriptor, error) {
	var ref name.Reference
	if len(digest) > 0 {
		var err error
		ref, err = name.NewDigest(fmt.Sprintf("%s/%s@%s", registry, repository, digest))
		if err != nil {
			return nil, fmt.Errorf("creating manifest reference with digest: %w", err)
		}
	} else {
		var err error
		ref, err = name.NewTag(fmt.Sprintf("%s/%s:%s", registry, repository, tag))
		if err != nil {
			return nil, fmt.Errorf("creating manifest reference: %w", err)
		}
	}

	// Use the custom client for remote operations
	desc, err := remote.Get(ref, reg.WithAuthFromMultiKeychain(), remote.WithTransport(transport))
	if err != nil {
		return nil, fmt.Errorf("getting manifest: %w", err)
	}

	if len(digest) == 0 {
		digest = desc.Descriptor.Digest.String()
		fmt.Fprintf(os.Stderr, "Missing valid image digest. Observed the following digest when pulling manifest for %s:\n    %s\n", ref.String(), digest)
		return nil, fmt.Errorf("missing valid digest, please specify the digest explicitly")
	}

	if fmt.Sprintf("sha256:%x", sha256.Sum256(desc.Manifest)) != digest {
		return nil, fmt.Errorf("manifest digest mismatch: expected %s, got sha256:%x", digest, sha256.Sum256(desc.Manifest))
	}

	// Store the manifest in the blob store
	if err := store.WriteSmallWithDigest(digest, desc.Manifest); err != nil {
		return nil, fmt.Errorf("writing manifest to blob store: %w", err)
	}

	return desc, nil
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSliceFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}
