package syncocirefgraph

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/name"
	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/bazel-contrib/rules_img/img_tool/cmd/pull"
	imgreferrers "github.com/bazel-contrib/rules_img/img_tool/pkg/referrers"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/registryopts"
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
	// Subject is the digest in the manifest's subject descriptor, if any. It
	// is what attributes a referrer to the manifest it applies to.
	Subject string `json:"subject,omitempty"`
}

// ReferrerRequest asks for a referrer manifest that must apply to the tree of
// the anchor manifest: the anchor itself or, for an index, one of its children.
// It is fetched from the sources of every image whose tree contains the anchor.
type ReferrerRequest struct {
	Digest string `json:"digest"`
	Anchor string `json:"anchor"`
}

// DiscoveryRequest asks for the referrers of the given artifact types of every
// manifest in the tree of the anchor manifest.
type DiscoveryRequest struct {
	Anchor        string   `json:"anchor"`
	ArtifactTypes []string `json:"artifact_types"`
}

// ReferrersInput is the format of the --referrers file.
type ReferrersInput struct {
	Referrers []ReferrerRequest  `json:"referrers"`
	Discover  []DiscoveryRequest `json:"discover"`
}

// discoveryFactPrefix keys the referrers discovered for a manifest, as an
// object mapping each requested artifact type to the referrer digests found.
// Recording them makes later evaluations reuse the discovered set instead of
// querying the registries again.
const discoveryFactPrefix = "referrer_discovery@"

// ManifestDownloadJob represents a job to download a manifest
type ManifestDownloadJob struct {
	Digest string
	Img    ImageInfo
	// RequireSubject rejects a cached entry without a subject (as written by
	// versions that did not record one), so a referrer gets re-fetched.
	RequireSubject bool
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
	var referrersPath string

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
	flagSet.StringVar(&referrersPath, "referrers", "", "Path to a JSON object with referrer requests ({\"referrers\": [{\"digest\", \"anchor\"}], \"discover\": [{\"anchor\", \"artifact_types\"}]}) to add to the graph")

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

	// Phase 3: Download the referrer manifests. A referrer lives in the
	// repository of its subject, so it is fetched from the sources of the images
	// containing its anchor. Requests whose anchor is not part of any image are
	// skipped; the module extension decides whether that is an error.
	discovered := map[string]map[string][]string{}
	if referrersPath != "" {
		referrersData, err := os.ReadFile(referrersPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: Failed to read referrers file: %v\n", err)
			os.Exit(1)
		}
		var input ReferrersInput
		if err := json.Unmarshal(referrersData, &input); err != nil {
			fmt.Fprintf(os.Stderr, "Error: Failed to parse referrers JSON: %v\n", err)
			os.Exit(1)
		}
		discovered, err = discoverReferrers(input.Discover, imagesByDigest, ociRefGraph, facts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: Failed to discover referrers: %v\n", err)
			os.Exit(1)
		}
		requests := input.Referrers
		for _, discovery := range input.Discover {
			for _, manifest := range treeOf(discovery.Anchor, ociRefGraph) {
				for _, artifactType := range discovery.ArtifactTypes {
					for _, referrer := range discovered[manifest][artifactType] {
						requests = append(requests, ReferrerRequest{Digest: referrer, Anchor: discovery.Anchor})
					}
				}
			}
		}
		referrerJobs := referrerJobs(requests, imagesByDigest, ociRefGraph)
		manifests := map[string][]byte{}
		for _, result := range downloadManifestsParallel(referrerJobs, facts) {
			if result.Error != nil {
				fmt.Fprintf(os.Stderr, "Error: Failed to download referrer manifest %s: %v\n", result.Digest, result.Error)
				os.Exit(1)
			}
			ociRefGraph[result.Digest] = result.RefGraphEntry
			manifests[result.Digest] = result.ManifestData
		}
		attributeCosignArtifacts(input.Discover, discovered, ociRefGraph)
		if err := attributePinnedCosignArtifacts(input.Referrers, manifests, imagesByDigest, ociRefGraph); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	}

	// Build updated facts with oci_ref_graph entries
	updatedFacts := make(Facts)
	for digest, refGraphEntry := range ociRefGraph {
		key := fmt.Sprintf("oci_ref_graph@%s", digest)
		updatedFacts[key] = refGraphEntry
	}
	for manifest, found := range discovered {
		updatedFacts[discoveryFactPrefix+manifest] = found
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
				result := downloadAndParseManifest(job, facts)
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

// referrerJobs turns referrer requests into download jobs, one per referrer
// digest, with the merged sources of every image whose tree contains an anchor.
func referrerJobs(requests []ReferrerRequest, imagesByDigest map[string]ImageInfo, ociRefGraph map[string]RefGraphEntry) []ManifestDownloadJob {
	sourcesByReferrer := map[string]map[string][]string{}
	var order []string
	for _, request := range requests {
		anchorSources := mergedSources(request.Anchor, imagesByDigest, ociRefGraph)
		if anchorSources == nil {
			continue
		}
		sources, ok := sourcesByReferrer[request.Digest]
		if !ok {
			sources = map[string][]string{}
			sourcesByReferrer[request.Digest] = sources
			order = append(order, request.Digest)
		}
		addSources(sources, anchorSources)
	}
	jobs := make([]ManifestDownloadJob, 0, len(order))
	for _, digest := range order {
		jobs = append(jobs, ManifestDownloadJob{
			Digest:         digest,
			Img:            ImageInfo{Sources: sourcesByReferrer[digest]},
			RequireSubject: true,
		})
	}
	return jobs
}

// attributeCosignArtifacts records the subject of the artifacts discovered by
// a pseudo artifact type, whose manifests may have no subject field to record:
// cosign's tag-scheme artifacts, about the manifest whose cosign tag they were
// found under (which their signed payloads were checked to name), and BuildKit
// attestation manifests, about the manifest the image index lists them for.
func attributeCosignArtifacts(requests []DiscoveryRequest, discovered map[string]map[string][]string, ociRefGraph map[string]RefGraphEntry) {
	for _, discovery := range requests {
		for _, manifest := range treeOf(discovery.Anchor, ociRefGraph) {
			for _, artifactType := range discovery.ArtifactTypes {
				if !imgreferrers.IsCosignTagType(artifactType) && artifactType != imgreferrers.BuildKitAttestationType {
					continue
				}
				for _, referrer := range discovered[manifest][artifactType] {
					if entry, ok := ociRefGraph[referrer]; ok && entry.Subject == "" {
						entry.Subject = manifest
						ociRefGraph[referrer] = entry
					}
				}
			}
		}
	}
}

// attributePinnedCosignArtifacts records the subject of the pinned referrers
// without a subject field: cosign's tag-scheme artifacts, about the manifest of
// their anchor's tree that their signed payloads name. Their layers are read
// from the sources of the images containing the anchor.
func attributePinnedCosignArtifacts(requests []ReferrerRequest, manifests map[string][]byte, imagesByDigest map[string]ImageInfo, ociRefGraph map[string]RefGraphEntry) error {
	indexes := newIndexCache()
	options := pull.ReferrersOptions(registryopts.DirectTransport())
	for _, request := range requests {
		entry, ok := ociRefGraph[request.Digest]
		raw := manifests[request.Digest]
		if !ok || entry.Subject != "" || entry.Kind != "manifest" || raw == nil {
			continue
		}
		sources := orderedSources(mergedSources(request.Anchor, imagesByDigest, ociRefGraph))
		if len(sources) == 0 {
			continue
		}
		// A BuildKit attestation manifest (oci-artifact=false) of the anchor index:
		// the index names its subject.
		if anchor, ok := ociRefGraph[request.Anchor]; ok && anchor.Kind == "index" {
			subject, found, err := buildKitAttestationSubject(indexes, request.Anchor, request.Digest, sources)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: %v (not attributing %s as a BuildKit attestation)\n", err, request.Digest)
			}
			if found {
				entry.Subject = subject
				ociRefGraph[request.Digest] = entry
				continue
			}
		}
		read := func(layer registryv1.Descriptor) ([]byte, error) {
			var lastErr error
			for _, src := range sources {
				repo, err := src.repository()
				if err != nil {
					return nil, err
				}
				data, err := imgreferrers.RemoteLayers(context.Background(), repo, options)(layer)
				if err == nil {
					return data, nil
				}
				lastErr = err
			}
			return nil, lastErr
		}
		_, subject, err := imgreferrers.AttributeCosignArtifact(raw, read, treeOf(request.Anchor, ociRefGraph))
		if err != nil {
			return fmt.Errorf("referrer %s has no subject, and is not one of cosign's signatures or attestations of %s: %w", request.Digest, request.Anchor, err)
		}
		entry.Subject = subject
		ociRefGraph[request.Digest] = entry
	}
	return nil
}

// buildKitAttestationSubject returns the manifest that the image index lists
// the BuildKit attestation manifest digest for.
func buildKitAttestationSubject(indexes *indexCache, index, digest string, sources []source) (string, bool, error) {
	raw, err := indexes.get(index, sources)
	if err != nil {
		return "", false, err
	}
	return imgreferrers.BuildKitAttestationSubject(raw, digest)
}

// source is a registry serving a repository.
type source struct {
	repo     string
	registry string
}

func (s source) String() string { return s.registry + "/" + s.repo }

func (s source) repository() (name.Repository, error) {
	repo, err := name.NewRepository(s.String(), registryopts.NameOptions()...)
	if err != nil {
		return name.Repository{}, fmt.Errorf("creating repository reference: %w", err)
	}
	return repo, nil
}

// orderedSources lists the sources in the order to try them: repositories
// sorted (a map has no order of its own), each one's registries in the given
// order. That keeps what a fetch finds independent of map iteration order.
func orderedSources(sources map[string][]string) []source {
	repositories := make([]string, 0, len(sources))
	for repository := range sources {
		repositories = append(repositories, repository)
	}
	slices.Sort(repositories)
	var ordered []source
	for _, repository := range repositories {
		for _, registry := range sources[repository] {
			ordered = append(ordered, source{repo: repository, registry: registry})
		}
	}
	return ordered
}

// indexCache fetches every image index once, however many manifests of its
// tree need it.
type indexCache struct {
	// fetch gets the manifest digest from src.
	fetch func(src source, digest string) ([]byte, error)

	mu      sync.Mutex
	entries map[string]*cachedIndex
}

type cachedIndex struct {
	once sync.Once
	raw  []byte
	err  error
}

func newIndexCache() *indexCache {
	options := pull.ReferrersOptions(registryopts.DirectTransport())
	return &indexCache{
		fetch: func(src source, digest string) ([]byte, error) {
			repo, err := src.repository()
			if err != nil {
				return nil, err
			}
			descriptor, err := remote.Get(repo.Digest(digest), options.Remote...)
			if err != nil {
				return nil, err
			}
			return descriptor.Manifest, nil
		},
		entries: map[string]*cachedIndex{},
	}
}

// get returns the image index digest, from the first of sources that has it.
// go-containerregistry checks its content against the digest, so it doesn't
// matter which source that is.
func (c *indexCache) get(digest string, sources []source) ([]byte, error) {
	c.mu.Lock()
	entry, ok := c.entries[digest]
	if !ok {
		entry = &cachedIndex{}
		c.entries[digest] = entry
	}
	c.mu.Unlock()
	entry.once.Do(func() {
		var lastErr error
		for _, src := range sources {
			raw, err := c.fetch(src, digest)
			if err == nil {
				entry.raw = raw
				return
			}
			lastErr = err
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("no sources available")
		}
		entry.err = fmt.Errorf("fetching the image index %s: %w", digest, lastErr)
	})
	return entry.raw, entry.err
}

// treeOf returns the anchor and, if it is an index, its children.
func treeOf(anchor string, ociRefGraph map[string]RefGraphEntry) []string {
	tree := []string{anchor}
	if entry, ok := ociRefGraph[anchor]; ok && entry.Kind == "index" {
		tree = append(tree, entry.Manifests...)
	}
	return tree
}

// mergedSources returns the merged sources of every image whose tree contains
// digest, or nil if none does. The images are visited in digest order, so the
// registries of a repository are in the same order every time.
func mergedSources(digest string, imagesByDigest map[string]ImageInfo, ociRefGraph map[string]RefGraphEntry) map[string][]string {
	roots := make([]string, 0, len(imagesByDigest))
	for rootDigest := range imagesByDigest {
		roots = append(roots, rootDigest)
	}
	slices.Sort(roots)
	var sources map[string][]string
	for _, rootDigest := range roots {
		if !treeContains(rootDigest, digest, ociRefGraph) {
			continue
		}
		if sources == nil {
			sources = map[string][]string{}
		}
		addSources(sources, imagesByDigest[rootDigest].Sources)
	}
	return sources
}

// addSources adds the registries of from to those of into, keeping their order
// and leaving out the ones into has already.
func addSources(into, from map[string][]string) {
	for repository, registries := range from {
		for _, registry := range registries {
			if !slices.Contains(into[repository], registry) {
				into[repository] = append(into[repository], registry)
			}
		}
	}
}

type discoveryJob struct {
	manifest      string
	artifactTypes []string
	sources       []source
	// index is the image index the manifest belongs to (the anchor, if it is
	// an index), whose entries name its BuildKit attestations.
	index string
}

// discoverReferrers resolves the discovery requests to the referrers of each
// requested artifact type, per manifest. Artifact types recorded in the facts
// for a manifest are reused; only the others are queried, with one query per
// manifest. Requests whose anchor is not part of any image are skipped.
func discoverReferrers(requests []DiscoveryRequest, imagesByDigest map[string]ImageInfo, ociRefGraph map[string]RefGraphEntry, facts Facts) (map[string]map[string][]string, error) {
	discovered := map[string]map[string][]string{}
	missing := map[string][]string{}
	sources := map[string]map[string][]string{}
	var order []string
	indexes := map[string]string{}
	for _, request := range requests {
		anchorSources := mergedSources(request.Anchor, imagesByDigest, ociRefGraph)
		if anchorSources == nil {
			continue
		}
		for _, manifest := range treeOf(request.Anchor, ociRefGraph) {
			if _, ok := discovered[manifest]; !ok {
				discovered[manifest] = map[string][]string{}
				order = append(order, manifest)
			}
			sources[manifest] = anchorSources
			if entry, ok := ociRefGraph[request.Anchor]; ok && entry.Kind == "index" && manifest != request.Anchor {
				indexes[manifest] = request.Anchor
			}
			cached := cachedDiscovery(facts, manifest)
			for _, artifactType := range request.ArtifactTypes {
				if _, ok := discovered[manifest][artifactType]; ok {
					continue
				}
				if digests, ok := cached[artifactType]; ok {
					discovered[manifest][artifactType] = digests
				} else if !slices.Contains(missing[manifest], artifactType) {
					missing[manifest] = append(missing[manifest], artifactType)
				}
			}
		}
	}

	var jobs []discoveryJob
	for _, manifest := range order {
		if len(missing[manifest]) > 0 {
			jobs = append(jobs, discoveryJob{manifest: manifest, artifactTypes: missing[manifest], sources: orderedSources(sources[manifest]), index: indexes[manifest]})
		}
	}
	results := make([]map[string][]string, len(jobs))
	errs := make([]error, len(jobs))
	indexCache := newIndexCache()
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 10)
	for i, job := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			results[i], errs[i] = discoverFromSources(job, indexCache)
		}()
	}
	wg.Wait()
	for i, job := range jobs {
		if errs[i] != nil {
			return nil, fmt.Errorf("manifest %s: %w", job.manifest, errs[i])
		}
		for artifactType, digests := range results[i] {
			discovered[job.manifest][artifactType] = digests
		}
	}
	return discovered, nil
}

func cachedDiscovery(facts Facts, manifest string) map[string][]string {
	cached, ok := facts[discoveryFactPrefix+manifest]
	if !ok {
		return nil
	}
	data, err := json.Marshal(cached)
	if err != nil {
		return nil
	}
	var found map[string][]string
	if err := json.Unmarshal(data, &found); err != nil {
		return nil
	}
	return found
}

// discoverFromSources queries the sources in turn until one answers.
func discoverFromSources(job discoveryJob, indexes *indexCache) (map[string][]string, error) {
	options := pull.ReferrersOptions(registryopts.DirectTransport())
	if job.index != "" && slices.Contains(job.artifactTypes, imgreferrers.BuildKitAttestationType) {
		index, err := indexes.get(job.index, job.sources)
		if err != nil {
			return nil, err
		}
		options.Index = index
	}
	var lastErr error
	for _, src := range job.sources {
		repo, err := src.repository()
		if err != nil {
			return nil, err
		}
		found, err := imgreferrers.Discover(context.Background(), repo, job.manifest, job.artifactTypes, options)
		if err == nil {
			return found, nil
		}
		lastErr = err
		fmt.Fprintf(os.Stderr, "Warning: Failed to discover referrers from %s: %v\n", src, err)
	}
	if lastErr != nil {
		return nil, fmt.Errorf("failed to discover referrers from all sources: %w", lastErr)
	}
	return nil, fmt.Errorf("no sources available")
}

// treeContains reports whether digest is the image rooted at rootDigest, or one
// of its children if that is an index.
func treeContains(rootDigest, digest string, ociRefGraph map[string]RefGraphEntry) bool {
	if rootDigest == digest {
		return true
	}
	entry, ok := ociRefGraph[rootDigest]
	return ok && entry.Kind == "index" && slices.Contains(entry.Manifests, digest)
}

// downloadAndParseManifest downloads a manifest and parses it into a ref graph entry
func downloadAndParseManifest(job ManifestDownloadJob, facts Facts) ManifestDownloadResult {
	digest := job.Digest
	img := job.Img
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
			if err := json.Unmarshal(cachedJSON, &refGraphEntry); err == nil && (refGraphEntry.Subject != "" || !job.RequireSubject) {
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

	if subject, ok := manifest["subject"].(map[string]interface{}); ok {
		if subjectDigest, ok := subject["digest"].(string); ok {
			refGraphEntry.Subject = subjectDigest
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
