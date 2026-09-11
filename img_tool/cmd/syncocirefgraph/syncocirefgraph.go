package syncocirefgraph

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/registryopts"
)

// Facts represents the cached OCI reference graph from previous runs
type Facts map[string]interface{}

// graphFactPrefix keys a reference graph entry in the facts of the images module extension.
//
// Facts persist in MODULE.bazel.lock across changes to this code, so the key encodes the schema:
// v2 entries additionally carry the descriptors of an index, which is what lets the extension
// route a build to one manifest without reading the index blob again.
const graphFactPrefix = "oci_ref_graph_v2@"

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
	// Descriptors holds the "manifests" array of an index verbatim, one entry per digest in
	// Manifests. The extension imports each child manifest with the descriptor referring to it,
	// which is the only place its platform, its annotations and its media type are recorded.
	Descriptors []map[string]interface{} `json:"descriptors,omitempty"`
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
		fmt.Fprintf(flagSet.Output(), "Usage: img sync-oci-ref-graph [OPTIONS]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"img sync-oci-ref-graph --facts facts.json --images images.json --output updated_facts.json",
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

	// Phase 2: Download child manifests referenced by indexes. A child listed by several
	// indexes is downloaded once, from any of the locations serving it. The parents are visited
	// in a fixed order so that the merged mirror list does not depend on Go map iteration:
	// downloads try the locations in order, and an air-gapped mirror has to stay first.
	childSources := make(map[string]map[string][]string)
	for _, parentDigest := range slices.Sorted(maps.Keys(ociRefGraph)) {
		refGraphEntry := ociRefGraph[parentDigest]
		if refGraphEntry.Kind != "index" {
			continue
		}
		parentImg := imagesByDigest[parentDigest]
		for _, childDigest := range refGraphEntry.Manifests {
			if _, exists := ociRefGraph[childDigest]; exists {
				continue
			}
			childSources[childDigest] = mergeSourceMaps(childSources[childDigest], parentImg.Sources)
		}
	}

	childJobs := make([]ManifestDownloadJob, 0, len(childSources))
	for _, childDigest := range slices.Sorted(maps.Keys(childSources)) {
		childJobs = append(childJobs, ManifestDownloadJob{
			Digest: childDigest,
			Img:    ImageInfo{Sources: childSources[childDigest]},
		})
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
		key := graphFactPrefix + digest
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
	if cachedEntry, ok := facts[graphFactPrefix+digest]; ok {
		// Try to convert cached entry to RefGraphEntry
		cachedJSON, err := json.Marshal(cachedEntry)
		if err == nil {
			var refGraphEntry RefGraphEntry
			if err := json.Unmarshal(cachedJSON, &refGraphEntry); err == nil && validRefGraphEntry(refGraphEntry) {
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

	refGraphEntry, err := parseRefGraphEntry(digest, manifestData)
	if err != nil {
		result.Error = err
		return result
	}
	result.RefGraphEntry = refGraphEntry
	return result
}

// parseRefGraphEntry turns a downloaded manifest or index blob into a reference graph entry:
// structure and index descriptors, never blob contents.
func parseRefGraphEntry(digest string, manifestData []byte) (RefGraphEntry, error) {
	var manifest map[string]interface{}
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return RefGraphEntry{}, fmt.Errorf("parsing manifest JSON: %w", err)
	}

	kind := manifestKind(getMediaType(manifest))
	refGraphEntry := RefGraphEntry{Kind: kind}
	switch kind {
	case "manifest":
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
	case "index":
		manifests, ok := manifest["manifests"].([]any)
		if !ok && manifest["manifests"] != nil {
			return RefGraphEntry{}, fmt.Errorf("index %s has a manifests field that is not an array", digest)
		}
		refGraphEntry.Manifests = make([]string, 0, len(manifests))
		refGraphEntry.Descriptors = make([]map[string]interface{}, 0, len(manifests))
		for i, m := range manifests {
			mMap, ok := m.(map[string]interface{})
			if !ok {
				return RefGraphEntry{}, fmt.Errorf("child %d of index %s is not an object", i, digest)
			}
			mDigest, ok := mMap["digest"].(string)
			if !ok || mDigest == "" {
				return RefGraphEntry{}, fmt.Errorf("child %d of index %s has no digest", i, digest)
			}
			// Descriptors and Manifests stay in lockstep: the extension pairs each child with
			// the descriptor at the same position.
			refGraphEntry.Descriptors = append(refGraphEntry.Descriptors, mMap)
			refGraphEntry.Manifests = append(refGraphEntry.Manifests, mDigest)
		}
	default:
		return RefGraphEntry{}, fmt.Errorf("unknown manifest kind: %s", kind)
	}
	return refGraphEntry, nil
}

// validRefGraphEntry rejects facts an older version of this tool wrote without the descriptors of
// an index, so that they are rediscovered instead of silently degrading platform selection. The
// digests have to line up, not just the counts: the extension pairs the manifest at a position
// with the descriptor at the same position.
func validRefGraphEntry(entry RefGraphEntry) bool {
	switch entry.Kind {
	case "manifest":
		return true
	case "index":
		if len(entry.Manifests) != len(entry.Descriptors) {
			return false
		}
		for i, descriptor := range entry.Descriptors {
			if digest, _ := descriptor["digest"].(string); digest != entry.Manifests[i] {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// mergeSourceMaps combines every location serving a shared child manifest, without mutating the
// source map of the image a location came from.
func mergeSourceMaps(target, other map[string][]string) map[string][]string {
	merged := make(map[string][]string, len(target)+len(other))
	for _, sources := range []map[string][]string{target, other} {
		for repository, registries := range sources {
			for _, registry := range registries {
				if !slices.Contains(merged[repository], registry) {
					merged[repository] = append(merged[repository], registry)
				}
			}
		}
	}
	return merged
}

// downloadManifestFromSources downloads a manifest trying each source in order
func downloadManifestFromSources(digest string, sources map[string][]string) ([]byte, error) {
	var lastErr error

	// Try each repository/registry combination. Repositories are visited in a fixed order so
	// that "in order" means something: the registries within one were merged in declaration
	// order, and Go map iteration would throw that away.
	for _, repository := range slices.Sorted(maps.Keys(sources)) {
		for _, registry := range sources[repository] {
			data, err := downloadManifest(registry, repository, digest)
			lastErr = err
			if err == nil {
				return data, nil
			}
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
	ref, err := name.NewDigest(fmt.Sprintf("%s/%s@%s", registry, repository, digest), registryopts.NameOptions()...)
	if err != nil {
		return nil, fmt.Errorf("creating manifest reference: %w", err)
	}

	descriptor, err := remote.Get(ref, registryopts.Default().WithTransport(registryopts.DirectTransport()).Remote()...)
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
