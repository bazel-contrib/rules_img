package load

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/containerd/platforms"
	registryv1 "github.com/malt3/go-containerregistry/pkg/v1"
	ocigodigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/containerd"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/progress"
)

const defaultWorkers = 4

type Request struct {
	Command   string           `json:"command"`
	Daemon    string           `json:"daemon"`
	Strategy  string           `json:"strategy"`
	Tag       string           `json:"tag,omitempty"`
	Manifest  *ManifestRequest `json:"manifest,omitempty"`
	Index     *IndexRequest    `json:"index,omitempty"`
	Platforms []string         `json:"platforms,omitempty"`
}

type ManifestRequest struct {
	ManifestPath string   `json:"manifest"`
	ConfigPath   string   `json:"config"`
	Layers       []string `json:"layers"`
	MissingBlobs []string `json:"missing_blobs,omitempty"`
	api.PullInfo `json:",inline"`
}

type IndexRequest struct {
	IndexPath string            `json:"index"`
	Manifests []ManifestRequest `json:"manifests"`
}

type blobWorkItem struct {
	layer  registryv1.Layer
	labels map[string]string
}

type manifestLoader struct {
	manifest     *ManifestRequest
	manifestData []byte
}

type indexLoader struct {
	index     *IndexRequest
	indexData []byte
	ociIndex  *ocispec.Index
	platforms []string
}

func ConnectToContainerd(ctx context.Context) (*containerd.Client, error) {
	address, err := containerd.FindContainerdSocket()
	if err != nil {
		return nil, fmt.Errorf("finding containerd socket: %w", err)
	}
	return containerd.New(address)
}

func computeManifestGCLabels(manifest *registryv1.Manifest) map[string]string {
	labels := make(map[string]string)
	labels["containerd.io/gc.ref.content.config"] = manifest.Config.Digest.String()
	for i, layer := range manifest.Layers {
		labels[fmt.Sprintf("containerd.io/gc.ref.content.l.%d", i)] = layer.Digest.String()
	}
	return labels
}

func uploadBlobsParallel(ctx context.Context, contentStore containerd.Store, blobs []blobWorkItem, numWorkers int) error {
	if numWorkers <= 0 {
		numWorkers = defaultWorkers
	}

	workCh := make(chan blobWorkItem, len(blobs))
	for _, blob := range blobs {
		workCh <- blob
	}
	close(workCh)

	errCh := make(chan error, len(blobs))
	var wg sync.WaitGroup

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go uploadWorker(ctx, contentStore, workCh, errCh, &wg)
	}

	wg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to upload %d blobs: %v", len(errs), errs)
	}

	return nil
}

func uploadWorker(ctx context.Context, contentStore containerd.Store, workCh <-chan blobWorkItem, errCh chan<- error, wg *sync.WaitGroup) {
	defer wg.Done()

	for blob := range workCh {
		if err := uploadBlob(ctx, contentStore, blob); err != nil {
			errCh <- err
		}
	}
}

func uploadBlob(ctx context.Context, contentStore containerd.Store, blob blobWorkItem) error {
	registryDigest, err := blob.layer.Digest()
	if err != nil {
		return fmt.Errorf("getting digest for upload: %w", err)
	}
	digest := ocigodigest.Digest(registryDigest.String())
	size, err := blob.layer.Size()
	if err != nil {
		return fmt.Errorf("getting size for upload: %w", err)
	}
	mediaType, err := blob.layer.MediaType()
	if err != nil {
		return fmt.Errorf("getting media type for upload: %w", err)
	}
	descriptor := ocispec.Descriptor{
		MediaType: string(mediaType),
		Digest:    digest,
		Size:      size,
	}

	if err := storeBlob(ctx, contentStore, descriptor, blob.layer, blob.labels); err != nil {
		return fmt.Errorf("storing blob in containerd: %w", err)
	}

	return nil
}

func storeBlob(ctx context.Context, store containerd.Store, desc ocispec.Descriptor, layer registryv1.Layer, labels map[string]string) error {
	// Check if the blob already exists
	info, err := store.Info(ctx, desc.Digest)
	if err == nil && info.Digest == desc.Digest && info.Size == desc.Size {
		// Blob already exists with correct size, nothing to do
		return nil
	}

	writer, err := store.Writer(ctx,
		containerd.WithDescriptor(desc))
	if err != nil {
		return fmt.Errorf("creating writer: %w", err)
	}
	defer writer.Close()

	bufferedWriter := bufio.NewWriter(writer)

	reader, err := layer.Compressed()
	if err != nil {
		return fmt.Errorf("getting reader for upload: %w", err)
	}
	defer reader.Close()

	pw := progress.Writer(desc.Size, desc.Digest.Hex()[:12])
	if _, err := io.Copy(io.MultiWriter(bufferedWriter, pw), reader); err != nil {
		return fmt.Errorf("copying data to writer: %w", err)
	}

	if err := bufferedWriter.Flush(); err != nil {
		return fmt.Errorf("flushing buffered writer: %w", err)
	}

	// Prepare commit options
	commitOpts := []containerd.Opt{}
	if len(labels) > 0 {
		commitOpts = append(commitOpts, containerd.WithLabels(labels))
	}

	if err := writer.Commit(ctx, desc.Size, desc.Digest, commitOpts...); err != nil {
		if containerd.IsAlreadyExists(err) {
			// Blob was written by another process, that's ok
			return nil
		}
		return fmt.Errorf("committing data: %w", err)
	}

	return nil
}

func digest(data []byte) ocigodigest.Digest {
	return ocigodigest.FromBytes(data)
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func NormalizeDockerReference(ref string) string {
	if ref == "" {
		return ""
	}

	parts := strings.SplitN(ref, ":", 2)
	name := parts[0]
	tag := ""
	if len(parts) > 1 {
		tag = parts[1]
	}

	hasHostname := false

	if strings.HasPrefix(name, "localhost/") {
		hasHostname = true
	} else {
		firstSlash := strings.Index(name, "/")
		if firstSlash > 0 {
			possibleHost := name[:firstSlash]
			if strings.Contains(possibleHost, ".") || strings.Contains(possibleHost, ":") {
				hasHostname = true
			}
		}
	}

	if !hasHostname {
		if !strings.Contains(name, "/") {
			name = "docker.io/library/" + name
		} else {
			name = "docker.io/" + name
		}
	}

	if tag != "" {
		return name + ":" + tag
	}
	return name
}

// platformMatches checks if a manifest platform matches any of the requested platforms.
// Uses containerd's platform matching logic with variant fallback.
func platformMatches(manifestPlatform *registryv1.Platform, requestedPlatforms []string) bool {
	if len(requestedPlatforms) == 0 {
		return true // No filter, all platforms match
	}

	// Convert registryv1.Platform to ocispec.Platform for containerd/platforms
	manifestOCI := ocispec.Platform{
		OS:           manifestPlatform.OS,
		Architecture: manifestPlatform.Architecture,
		Variant:      manifestPlatform.Variant,
	}

	for _, reqPlatStr := range requestedPlatforms {
		// Parse and normalize the requested platform
		reqPlat, err := platforms.Parse(reqPlatStr)
		if err != nil {
			continue
		}

		// Use containerd's Only() matcher which implements platformVector logic
		// This handles variant fallback (e.g., amd64/v3 -> v2 -> v1)
		matcher := platforms.Only(reqPlat)
		if matcher.Match(manifestOCI) {
			return true
		}
	}

	return false
}

// getCurrentPlatform returns the current platform string
// It checks the DOCKER_DEFAULT_PLATFORM environment variable first
// If not set, it defaults to "linux/$(GOARCH)".
func getCurrentPlatform() string {
	if plt, ok := os.LookupEnv("DOCKER_DEFAULT_PLATFORM"); ok {
		return plt
	}
	return "linux/" + runtime.GOARCH
}
