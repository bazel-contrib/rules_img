package syncocirefgraph

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	reg "github.com/bazel-contrib/rules_img/pull_tool/pkg/auth/registry"
)

// Facts represents the cached OCI reference graph from previous runs
type Facts map[string]interface{}

// ImageInfo represents an image to pull with its sources
type ImageInfo struct {
	Repository    string              `json:"repository"`
	Registries    []string            `json:"registries"`
	Digest        string              `json:"digest"`
	Tag           string              `json:"tag,omitempty"`
	LayerHandling string              `json:"layer_handling"`
	Sources       map[string][]string `json:"sources"`
}

// RefGraphEntry represents a manifest or index in the OCI reference graph
type RefGraphEntry struct {
	Kind      string   `json:"kind"`
	Config    string   `json:"config,omitempty"`
	Layers    []string `json:"layers,omitempty"`
	Manifests []string `json:"manifests,omitempty"`
}

// ManifestDownloadJob represents a job to download a manifest
type ManifestDownloadJob struct {
	Digest string
	Img    ImageInfo
}

// ManifestDownloadResult represents the result of downloading a manifest
type ManifestDownloadResult struct {
	Digest        string
	RefGraphEntry RefGraphEntry
	ManifestData  []byte
	Error         error
}

func SyncOCIRefGraphProcess(ctx context.Context, args []string) {
	var factsPath string
	var imagesPath string
	var outputPath string

	flagSet := flag.NewFlagSet("sync-oci-ref-graph", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Syncs the OCI reference graph by downloading manifests in parallel.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: pull_tool sync-oci-ref-graph [OPTIONS]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"pull_tool sync-oci-ref-graph --facts facts.json --images images.json --output updated_facts.json",
		}
		fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
	}

	flagSet.StringVar(&factsPath, "facts", "", "Path to the facts JSON file from previous run")
	flagSet.StringVar(&imagesPath, "images", "", "Path to the images JSON file (images_by_digest)")
	flagSet.StringVar(&outputPath, "output", "", "Path to write updated facts JSON")

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}

	if factsPath == "" || imagesPath == "" || outputPath == "" {
		fmt.Fprintf(os.Stderr, "Error: --facts, --images, and --output are all required\n")
		flagSet.Usage()
		os.Exit(1)
	}

	// Load facts from file
	facts := make(Facts)
	if factsData, err := os.ReadFile(factsPath); err == nil && len(factsData) > 0 {
		if err := json.Unmarshal(factsData, &facts); err != nil {
			fmt.Fprintf(os.Stderr, "Error: Failed to parse facts JSON: %v\n", err)
			os.Exit(1)
		}
	}

	// Load images from file
	imagesByDigest := make(map[string]ImageInfo)
	imagesData, err := os.ReadFile(imagesPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to read images file: %v\n", err)
		os.Exit(1)
	}
	if err := json.Unmarshal(imagesData, &imagesByDigest); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to parse images JSON: %v\n", err)
		os.Exit(1)
	}

	// Download manifests and build ref graph
	ociRefGraph := make(map[string]RefGraphEntry)

	// Phase 1: Download top-level manifests/indexes
	topLevelJobs := make([]ManifestDownloadJob, 0, len(imagesByDigest))
	for digest, img := range imagesByDigest {
		topLevelJobs = append(topLevelJobs, ManifestDownloadJob{
			Digest: digest,
			Img:    img,
		})
	}

	topLevelResults := downloadManifestsParallel(topLevelJobs, facts)
	for _, result := range topLevelResults {
		if result.Error != nil {
			fmt.Fprintf(os.Stderr, "Error: Failed to download manifest %s: %v\n", result.Digest, result.Error)
			os.Exit(1)
		}
		ociRefGraph[result.Digest] = result.RefGraphEntry
	}

	// Phase 2: Download child manifests referenced by indexes
	childJobs := make([]ManifestDownloadJob, 0)
	for parentDigest, refGraphEntry := range ociRefGraph {
		if refGraphEntry.Kind == "index" {
			parentImg := imagesByDigest[parentDigest]
			for _, childDigest := range refGraphEntry.Manifests {
				if _, exists := ociRefGraph[childDigest]; !exists {
					childJobs = append(childJobs, ManifestDownloadJob{
						Digest: childDigest,
						Img:    parentImg, // Use parent image's sources
					})
				}
			}
		}
	}

	if len(childJobs) > 0 {
		childResults := downloadManifestsParallel(childJobs, facts)
		for _, result := range childResults {
			if result.Error != nil {
				fmt.Fprintf(os.Stderr, "Error: Failed to download child manifest %s: %v\n", result.Digest, result.Error)
				os.Exit(1)
			}
			if result.RefGraphEntry.Kind != "manifest" {
				fmt.Fprintf(os.Stderr, "Error: Expected manifest for digest %s but got %s\n", result.Digest, result.RefGraphEntry.Kind)
				os.Exit(1)
			}
			ociRefGraph[result.Digest] = result.RefGraphEntry
		}
	}

	// Build updated facts with oci_ref_graph entries
	updatedFacts := make(Facts)
	for digest, refGraphEntry := range ociRefGraph {
		key := fmt.Sprintf("oci_ref_graph@%s", digest)
		updatedFacts[key] = refGraphEntry
	}

	// Write updated facts to output file
	outputData, err := json.MarshalIndent(updatedFacts, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to marshal updated facts: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(outputPath, outputData, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to write output file: %v\n", err)
		os.Exit(1)
	}
}

// downloadManifestsParallel downloads manifests in parallel using a worker pool
func downloadManifestsParallel(jobs []ManifestDownloadJob, facts Facts) []ManifestDownloadResult {
	const numWorkers = 10

	jobsChan := make(chan ManifestDownloadJob, len(jobs))
	resultsChan := make(chan ManifestDownloadResult, len(jobs))

	var wg sync.WaitGroup

	// Start workers
	for range numWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobsChan {
				result := downloadAndParseManifest(job.Digest, job.Img, facts)
				resultsChan <- result
			}
		}()
	}

	// Send jobs
	for _, job := range jobs {
		jobsChan <- job
	}
	close(jobsChan)

	// Wait for all workers to finish
	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	// Collect results
	results := make([]ManifestDownloadResult, 0, len(jobs))
	for result := range resultsChan {
		results = append(results, result)
	}

	return results
}

// downloadAndParseManifest downloads a manifest and parses it into a ref graph entry
func downloadAndParseManifest(digest string, img ImageInfo, facts Facts) ManifestDownloadResult {
	result := ManifestDownloadResult{
		Digest: digest,
	}

	// Check if structure is cached in facts
	factKey := fmt.Sprintf("oci_ref_graph@%s", digest)
	if cachedEntry, ok := facts[factKey]; ok {
		// Try to convert cached entry to RefGraphEntry
		cachedJSON, err := json.Marshal(cachedEntry)
		if err == nil {
			var refGraphEntry RefGraphEntry
			if err := json.Unmarshal(cachedJSON, &refGraphEntry); err == nil {
				result.RefGraphEntry = refGraphEntry
				return result
			}
		}
	}

	// Download manifest from sources
	manifestData, err := downloadManifestFromSources(digest, img.Sources)
	if err != nil {
		result.Error = fmt.Errorf("downloading manifest: %w", err)
		return result
	}
	result.ManifestData = manifestData

	// Parse manifest
	var manifest map[string]interface{}
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		result.Error = fmt.Errorf("parsing manifest JSON: %w", err)
		return result
	}

	// Determine kind from media type
	mediaType := getMediaType(manifest)
	kind := manifestKind(mediaType)

	if kind != "manifest" && kind != "index" {
		result.Error = fmt.Errorf("unknown manifest kind: %s", kind)
		return result
	}

	// Build ref graph entry
	refGraphEntry := RefGraphEntry{Kind: kind}
	if kind == "manifest" {
		if config, ok := manifest["config"].(map[string]interface{}); ok {
			if configDigest, ok := config["digest"].(string); ok {
				refGraphEntry.Config = configDigest
			}
		}
		if layers, ok := manifest["layers"].([]interface{}); ok {
			for _, layer := range layers {
				if layerMap, ok := layer.(map[string]interface{}); ok {
					if layerDigest, ok := layerMap["digest"].(string); ok {
						refGraphEntry.Layers = append(refGraphEntry.Layers, layerDigest)
					}
				}
			}
		}
	} else if kind == "index" {
		if manifests, ok := manifest["manifests"].([]interface{}); ok {
			for _, m := range manifests {
				if mMap, ok := m.(map[string]interface{}); ok {
					if mDigest, ok := mMap["digest"].(string); ok {
						refGraphEntry.Manifests = append(refGraphEntry.Manifests, mDigest)
					}
				}
			}
		}
	}

	result.RefGraphEntry = refGraphEntry
	return result
}

// downloadManifestFromSources downloads a manifest trying each source in order
func downloadManifestFromSources(digest string, sources map[string][]string) ([]byte, error) {
	var lastErr error

	// Try each repository/registry combination
	for repository, registries := range sources {
		for _, registry := range registries {
			data, err := downloadManifest(registry, repository, digest)
			if err == nil {
				return data, nil
			}
			lastErr = err
			fmt.Fprintf(os.Stderr, "Warning: Failed to download from %s/%s: %v\n", registry, repository, err)
		}
	}

	if lastErr != nil {
		return nil, fmt.Errorf("failed to download from all sources: %w", lastErr)
	}
	return nil, fmt.Errorf("no sources available")
}

// downloadManifest downloads a manifest from a specific registry/repository
func downloadManifest(registry, repository, digest string) ([]byte, error) {
	ref, err := name.NewDigest(fmt.Sprintf("%s/%s@%s", registry, repository, digest))
	if err != nil {
		return nil, fmt.Errorf("creating manifest reference: %w", err)
	}

	descriptor, err := remote.Get(ref, reg.WithAuthFromMultiKeychain())
	if err != nil {
		return nil, fmt.Errorf("getting manifest: %w", err)
	}

	return descriptor.Manifest, nil
}

// getMediaType extracts the media type from a manifest
func getMediaType(manifest map[string]any) string {
	if mediaType, ok := manifest["mediaType"].(string); ok {
		return mediaType
	}
	// Default for Docker v2 schema 1 manifests
	if schemaVersion, ok := manifest["schemaVersion"].(float64); ok && schemaVersion == 1 {
		return "application/vnd.docker.distribution.manifest.v1+json"
	}
	return ""
}

// manifestKind determines the kind (manifest or index) from media type
func manifestKind(mediaType string) string {
	switch {
	case strings.Contains(mediaType, "manifest.list"),
		strings.Contains(mediaType, "image.index"):
		return "index"
	case strings.Contains(mediaType, "manifest.v1"),
		strings.Contains(mediaType, "manifest.v2"):
		return "manifest"
	default:
		return "unknown"
	}
}
