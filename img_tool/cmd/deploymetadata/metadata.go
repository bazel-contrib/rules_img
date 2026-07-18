package deploymetadata

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	registryv1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

var (
	command              string
	rootPath             string
	rootKind             string
	configurationPath    string
	strategy             string
	manifestPaths        []string
	originalRegistries   []string
	originalRepository   string
	orginalTag           string
	originalDigest       string
	layerHintsInputPath  string
	layerHintsOutputPath string
	layerSourcesPath     string

	// layerSourcesForManifest holds per-layer upstream sources, parsed from
	// --layer-sources-file. Keyed by manifest index, then aligned with that
	// manifest's layer order: layerSourcesForManifest[manifestIndex][layerIndex]
	// is the list of sources for that layer.
	layerSourcesForManifest map[int][][]api.LayerSource

	// layerCompactStreams maps manifest index -> layer index -> path of that
	// layer's .cstream file. Present for compact-stream layers under the "bes"
	// strategy so the deploy metadata can record the .cstream's CAS digest and the
	// syncer can reconstruct the layer from it.
	layerCompactStreams *doubleIndexedStringFlag

	crossMountStrategy         string
	crossMountFromManifestPath string

	destinationFilePath string

	manifestTagFiles map[int]string

	referrerRootPaths     *indexedStringFlag
	referrerRootKinds     *indexedStringFlag
	referrerManifestPaths *doubleIndexedStringFlag
)

func DeployMetadataProcess(ctx context.Context, args []string) {
	referrerRootPaths = newIndexedStringFlag()
	referrerRootKinds = newIndexedStringFlag()
	referrerManifestPaths = newDoubleIndexedStringFlag()
	layerCompactStreams = newDoubleIndexedStringFlag()
	manifestTagFiles = nil
	layerSourcesForManifest = nil

	flagSet := flag.NewFlagSet("deploy-metadata", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Writes metadata about a push/load operation.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img deploy-metadata [flags] [output]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"img deploy-metadata --command push --root-path=manifest.json --configuration-file=push_config.json --strategy=eager dispatch.json",
			"img deploy-metadata --command load --root-path=manifest.json --configuration-file=push_config.json --strategy=eager --original-registry=gcr.io --original-registry=docker.io --original-repository=my-repo --original-tag=latest --original-digest=sha256:abcdef1234567890 dispatch.json",
		}
		fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
		os.Exit(1)
	}
	flagSet.StringVar(&command, "command", "", `The kind of operation ("push" or "load")`)
	flagSet.StringVar(&rootPath, "root-path", "", `Path to the root manifest to be deployed (manifest or index).`)
	flagSet.StringVar(&rootKind, "root-kind", "", `Kind of the root manifest ("manifest" or "index").`)
	flagSet.StringVar(&configurationPath, "configuration-file", "", `Path to the configuration file.`)
	flagSet.StringVar(&strategy, "strategy", "eager", `Push strategy to use. One of "eager", "lazy", "cas_registry", or "bes".`)
	flagSet.StringVar(&crossMountStrategy, "cross-mount-strategy", "", `Cross mount strategy.`)
	flagSet.StringVar(&crossMountFromManifestPath, "cross-mount-from-manifest-path", "", `(Optional) deploy manifest of another push, from which layers for this push could be cross mounted.`)
	flagSet.Func("original-registry", `(Optional) original registry that the base of this image was pulled from. Can be specified multiple times.`, func(value string) error {
		originalRegistries = append(originalRegistries, value)
		return nil
	})
	flagSet.StringVar(&originalRepository, "original-repository", "", `(Optional) original repository that the base of this image was pulled from.`)
	flagSet.StringVar(&orginalTag, "original-tag", "", `(Optional) original tag that the base of this image was pulled from.`)
	flagSet.StringVar(&originalDigest, "original-digest", "", `(Optional) original digest that the base of this image was pulled from.`)
	flagSet.StringVar(&destinationFilePath, "destination-file", "", `(Optional) path to a file containing the push destination as "registry/repository". Mutually exclusive with registry/repository in the configuration file.`)
	flagSet.StringVar(&layerHintsInputPath, "layer-hints-paths-file-input", "", `(Optional) path to file containing layer path hints (null-separated blob/metadata pairs).`)
	flagSet.StringVar(&layerHintsOutputPath, "layer-hints-paths-output", "", `(Optional) path to write resolved layer hints output.`)
	flagSet.StringVar(&layerSourcesPath, "layer-sources-file", "", `(Optional) path to a JSON file describing the upstream sources of each layer. Maps manifest index (as string) to a list (aligned with the manifest's layers) of source lists, each source being {"registry":..,"repository":..}.`)
	flagSet.Func("manifest-path", `Path to a manifest file. Format: index=path (e.g., 0=foo.json). Can be specified multiple times.`, func(value string) error {
		parts := strings.SplitN(value, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("manifest-path must be in format index=path")
		}
		index, err := strconv.Atoi(parts[0])
		if err != nil {
			return fmt.Errorf("invalid index in manifest-path: %w", err)
		}
		path := parts[1]
		// Expand slice if necessary
		for len(manifestPaths) <= index {
			manifestPaths = append(manifestPaths, "")
		}
		manifestPaths[index] = path
		return nil
	})
	flagSet.Func("manifest-tag-file", `(Optional) pre-expanded per-platform tags for a child of an image index. Format: manifest_index=path (e.g., 0=tags.json). The file must be a JSON object of the form {"manifest_tags": ["tag1", ...]} and is produced by Bazel's expand_or_write helper, so Go templates are already expanded. Can be specified multiple times. Only valid when --root-kind=index.`, func(value string) error {
		parts := strings.SplitN(value, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("manifest-tag-file must be in format manifest_index=path")
		}
		index, err := strconv.Atoi(parts[0])
		if err != nil {
			return fmt.Errorf("invalid manifest index in manifest-tag-file: %w", err)
		}
		if manifestTagFiles == nil {
			manifestTagFiles = make(map[int]string)
		}
		manifestTagFiles[index] = parts[1]
		return nil
	})
	flagSet.Var(referrerRootPaths, "referrer-root-path", `Path to a referrer root manifest or index file. Format: index=path (e.g., 0=referrer.json). Can be specified multiple times.`)
	flagSet.Var(referrerRootKinds, "referrer-root-kind", `Kind of a referrer root. Format: index=kind (e.g., 0=manifest). Can be specified multiple times.`)
	flagSet.Var(referrerManifestPaths, "referrer-manifest-path", `Path to a referrer child manifest file. Format: referrer_idx,manifest_idx=path (e.g., 0,0=manifest.json). Can be specified multiple times.`)
	flagSet.Var(layerCompactStreams, "layer-compact-stream", `(Optional) compact-stream (.cstream) file for a layer. Format: manifest_idx,layer_idx=path. The file's CAS digest is recorded so the layer can be reconstructed from it (used by the bes strategy). Can be specified multiple times.`)

	if err := flagSet.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		flagSet.Usage()
		os.Exit(1)
	}
	if flagSet.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Error: exactly one output path argument is required")
		flagSet.Usage()
		os.Exit(1)
	}
	outputPath := flagSet.Arg(0)
	if rootPath == "" {
		fmt.Fprintln(os.Stderr, "Error: --root-path is required")
		flagSet.Usage()
		os.Exit(1)
	}
	if rootKind != "manifest" && rootKind != "index" {
		fmt.Fprintln(os.Stderr, "Error: --root-kind must be either 'manifest' or 'index'")
		flagSet.Usage()
		os.Exit(1)
	}
	if len(manifestTagFiles) > 0 && rootKind != "index" {
		fmt.Fprintln(os.Stderr, "Error: --manifest-tag-file can only be used with --root-kind=index")
		flagSet.Usage()
		os.Exit(1)
	}
	if configurationPath == "" {
		fmt.Fprintln(os.Stderr, "Error: --configuration-file is required")
		flagSet.Usage()
		os.Exit(1)
	}
	switch strategy {
	case "eager", "lazy", "cas_registry", "bes":
		// valid strategies
	default:
		fmt.Fprintf(os.Stderr, "Error: invalid strategy %q\n", strategy)
		flagSet.Usage()
		os.Exit(1)
	}
	if (layerHintsInputPath == "" && layerHintsOutputPath != "") || (layerHintsInputPath != "" && layerHintsOutputPath == "") {
		fmt.Fprintln(os.Stderr, "Error: both --layer-hints-paths-file-input= and --layer-hints-paths-output must be specified together")
		flagSet.Usage()
		os.Exit(1)
	}
	if layerHintsInputPath != "" {
		if err := processLayerHints(layerHintsInputPath, layerHintsOutputPath); err != nil {
			fmt.Fprintf(os.Stderr, "Error processing layer hints: %v\n", err)
			os.Exit(1)
		}
	}
	if layerSourcesPath != "" {
		if err := parseLayerSources(layerSourcesPath); err != nil {
			fmt.Fprintf(os.Stderr, "Error parsing layer sources: %v\n", err)
			os.Exit(1)
		}
	}
	if err := WriteMetadata(ctx, outputPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing deploy metadata: %v\n", err)
		os.Exit(1)
	}
}

// parseLayerSources reads the --layer-sources-file JSON and populates
// layerSourcesForManifest. The file maps a manifest index (encoded as a string)
// to a list aligned with that manifest's layers, where each element is the list
// of upstream sources for that layer.
func parseLayerSources(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading layer sources file: %w", err)
	}
	byStringIndex := map[string][][]api.LayerSource{}
	if err := json.Unmarshal(raw, &byStringIndex); err != nil {
		return fmt.Errorf("unmarshalling layer sources file: %w", err)
	}
	layerSourcesForManifest = make(map[int][][]api.LayerSource, len(byStringIndex))
	for key, perLayer := range byStringIndex {
		idx, err := strconv.Atoi(key)
		if err != nil {
			return fmt.Errorf("invalid manifest index %q in layer sources file: %w", key, err)
		}
		layerSourcesForManifest[idx] = perLayer
	}
	return nil
}

// sourcesForLayer returns the upstream sources recorded for the given layer of a
// manifest, or nil if none were provided.
func sourcesForLayer(manifestIndex, layerIndex int) []api.LayerSource {
	perLayer, ok := layerSourcesForManifest[manifestIndex]
	if !ok || layerIndex >= len(perLayer) {
		return nil
	}
	return perLayer[layerIndex]
}

// compactStreamForLayer returns the CAS descriptor of the .cstream for the given
// layer (hashing the file), or nil if the layer is not a compact-stream layer.
// The digest recorded here is the .cstream's own content digest, which is how it
// is addressed in the CAS; the syncer fetches it to reconstruct the layer.
func compactStreamForLayer(manifestIndex, layerIndex int) (*api.Descriptor, error) {
	inner, ok := layerCompactStreams.values[manifestIndex]
	if !ok {
		return nil, nil
	}
	path, ok := inner[layerIndex]
	if !ok || path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading compact stream file %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return &api.Descriptor{
		Digest: fmt.Sprintf("sha256:%x", sum),
		Size:   int64(len(data)),
	}, nil
}

func WriteMetadata(ctx context.Context, outputPath string) error {
	rawConfig, err := os.ReadFile(configurationPath)
	if err != nil {
		return fmt.Errorf("reading request file: %w", err)
	}
	var config map[string]any
	if err := json.Unmarshal(rawConfig, &config); err != nil {
		return fmt.Errorf("unmarshalling config file: %w", err)
	}

	// Parse root manifest file to determine kind and calculate digest/size
	rootData, err := os.ReadFile(rootPath)
	if err != nil {
		return fmt.Errorf("reading root manifest file: %w", err)
	}

	rootDigest := sha256.Sum256(rootData)
	rootSize := int64(len(rootData))

	// Try to parse as index first, then as manifest
	var mediaType string

	if rootKind == "index" {
		idx, err := registryv1.ParseIndexManifest(bytes.NewReader(rootData))
		if err != nil {
			return fmt.Errorf("parsing root manifest as index: %w", err)
		}
		mediaType = string(idx.MediaType)
	} else if rootKind == "manifest" {
		manifest, err := registryv1.ParseManifest(bytes.NewReader(rootData))
		if err != nil {
			return fmt.Errorf("parsing root manifest as manifest: %w", err)
		}
		mediaType = string(manifest.MediaType)
	} else {
		return fmt.Errorf("failed to parse root file as either index or manifest")
	}

	rootDescriptor := api.Descriptor{
		MediaType: mediaType,
		Digest:    fmt.Sprintf("sha256:%x", rootDigest),
		Size:      rootSize,
	}

	// Process manifests and missing blobs
	manifests := make([]api.ManifestDeployInfo, len(manifestPaths))
	for i, manifestPath := range manifestPaths {
		if manifestPath == "" {
			continue // Skip empty manifest paths
		}

		manifestData, err := os.ReadFile(manifestPath)
		if err != nil {
			return fmt.Errorf("reading manifest file %s: %w", manifestPath, err)
		}

		manifestDigest := sha256.Sum256(manifestData)
		manifestSize := int64(len(manifestData))

		manifest, err := registryv1.ParseManifest(bytes.NewReader(manifestData))
		if err != nil {
			return fmt.Errorf("parsing manifest file %s: %w", manifestPath, err)
		}

		manifestDescriptor := api.Descriptor{
			MediaType: string(manifest.MediaType),
			Digest:    fmt.Sprintf("sha256:%x", manifestDigest),
			Size:      manifestSize,
		}

		// Extract config descriptor
		configDescriptor := api.Descriptor{
			MediaType: string(manifest.Config.MediaType),
			Digest:    manifest.Config.Digest.String(),
			Size:      manifest.Config.Size,
		}

		// Extract layer descriptors, attaching per-layer upstream sources and, for
		// compact-stream layers, the CAS reference of the .cstream to reconstruct from.
		layerBlobs := make([]api.LayerBlob, len(manifest.Layers))
		for j, layer := range manifest.Layers {
			compactStream, err := compactStreamForLayer(i, j)
			if err != nil {
				return err
			}
			layerBlobs[j] = api.LayerBlob{
				Descriptor: api.Descriptor{
					MediaType: string(layer.MediaType),
					Digest:    layer.Digest.String(),
					Size:      layer.Size,
				},
				Sources:       sourcesForLayer(i, j),
				CompactStream: compactStream,
			}
		}

		manifests[i] = api.ManifestDeployInfo{
			Descriptor: manifestDescriptor,
			Config:     configDescriptor,
			LayerBlobs: layerBlobs,
		}
	}

	baseCommand := api.BaseCommandOperation{
		Command:   command,
		RootKind:  rootKind,
		Root:      rootDescriptor,
		Manifests: manifests,
		PullInfo: api.PullInfo{
			OriginalBaseImageRegistries: originalRegistries,
			OriginalBaseImageRepository: originalRepository,
			OriginalBaseImageTag:        orginalTag,
			OriginalBaseImageDigest:     originalDigest,
		},
	}

	var operationBytes []byte
	var deploySettings api.DeploySettings

	var registryTagOps []json.RawMessage
	if command == "push" {
		deploySettings.PushStrategy = strategy
		operation, err := pushOperation(baseCommand, config)
		if err != nil {
			return err
		}

		operationBytes, err = json.Marshal(operation)
		if err != nil {
			return fmt.Errorf("marshalling push operation: %w", err)
		}

		registryTagOps, err = registryTagOperations(operation.PushTarget, manifests)
		if err != nil {
			return err
		}
	} else if command == "load" {
		deploySettings.LoadStrategy = strategy
		operation, err := loadOperation(baseCommand, config)
		if err != nil {
			return err
		}
		operationBytes, err = json.Marshal(operation)
		if err != nil {
			return fmt.Errorf("marshalling load operation: %w", err)
		}
	} else {
		return fmt.Errorf("invalid command %q", command)
	}

	deployManifest := api.DeployManifest{
		Operations: []json.RawMessage{operationBytes},
		Settings:   deploySettings,
	}

	if len(registryTagOps) > 0 {
		deployManifest.Operations = append(deployManifest.Operations, registryTagOps...)
	}

	// Process referrers (only for push operations)
	if command == "push" && len(referrerRootPaths.values) > 0 {
		// Collect all known digests from the main image for subject validation
		knownDigests := collectKnownDigests(rootDescriptor, manifests)

		referrerOps, err := processReferrers(knownDigests, config)
		if err != nil {
			return fmt.Errorf("processing referrers: %w", err)
		}
		deployManifest.Operations = append(deployManifest.Operations, referrerOps...)
	}

	manifestBytes, err := json.Marshal(deployManifest)
	if err != nil {
		return fmt.Errorf("marshalling metadata: %w", err)
	}
	if err := os.WriteFile(outputPath, manifestBytes, 0o644); err != nil {
		return fmt.Errorf("writing metadata file: %w", err)
	}
	return nil
}

// collectKnownDigests gathers all content-addressed digests from the main image.
// This includes the root descriptor, all manifest descriptors, config descriptors,
// and layer blob descriptors.
func collectKnownDigests(root api.Descriptor, manifests []api.ManifestDeployInfo) map[string]bool {
	digests := map[string]bool{root.Digest: true}
	for _, m := range manifests {
		digests[m.Descriptor.Digest] = true
		digests[m.Config.Digest] = true
		for _, layer := range m.LayerBlobs {
			digests[layer.Digest] = true
		}
	}
	return digests
}

// processReferrers creates push operations for each referrer.
// Each referrer is validated to have a subject whose digest matches a known digest
// of the main image.
func processReferrers(knownDigests map[string]bool, config map[string]any) ([]json.RawMessage, error) {
	// Sort referrer indices for deterministic output
	indices := make([]int, 0, len(referrerRootPaths.values))
	for idx := range referrerRootPaths.values {
		indices = append(indices, idx)
	}
	sort.Ints(indices)

	var ops []json.RawMessage
	for _, refIdx := range indices {
		refRootPath := referrerRootPaths.values[refIdx]
		refRootKind, ok := referrerRootKinds.values[refIdx]
		if !ok {
			return nil, fmt.Errorf("referrer %d: --referrer-root-kind is required", refIdx)
		}
		if refRootKind != "manifest" && refRootKind != "index" {
			return nil, fmt.Errorf("referrer %d: --referrer-root-kind must be 'manifest' or 'index', got %q", refIdx, refRootKind)
		}

		refRootData, err := os.ReadFile(refRootPath)
		if err != nil {
			return nil, fmt.Errorf("referrer %d: reading root file %s: %w", refIdx, refRootPath, err)
		}

		// Validate subject field
		if err := validateSubject(refIdx, refRootData, knownDigests); err != nil {
			return nil, err
		}

		// Compute root descriptor
		refRootDigest := sha256.Sum256(refRootData)
		refRootSize := int64(len(refRootData))

		var refMediaType string
		if refRootKind == "index" {
			idx, err := registryv1.ParseIndexManifest(bytes.NewReader(refRootData))
			if err != nil {
				return nil, fmt.Errorf("referrer %d: parsing root as index: %w", refIdx, err)
			}
			refMediaType = string(idx.MediaType)
		} else {
			manifest, err := registryv1.ParseManifest(bytes.NewReader(refRootData))
			if err != nil {
				return nil, fmt.Errorf("referrer %d: parsing root as manifest: %w", refIdx, err)
			}
			refMediaType = string(manifest.MediaType)
		}

		refRootDescriptor := api.Descriptor{
			MediaType: refMediaType,
			Digest:    fmt.Sprintf("sha256:%x", refRootDigest),
			Size:      refRootSize,
		}

		// Process referrer manifests
		refManifestPathsForIdx := referrerManifestPaths.values[refIdx]
		refManifestIndices := make([]int, 0, len(refManifestPathsForIdx))
		for mIdx := range refManifestPathsForIdx {
			refManifestIndices = append(refManifestIndices, mIdx)
		}
		sort.Ints(refManifestIndices)

		refManifests := make([]api.ManifestDeployInfo, len(refManifestIndices))
		for i, mIdx := range refManifestIndices {
			manifestPath := refManifestPathsForIdx[mIdx]
			manifestData, err := os.ReadFile(manifestPath)
			if err != nil {
				return nil, fmt.Errorf("referrer %d: reading manifest %d from %s: %w", refIdx, mIdx, manifestPath, err)
			}

			manifestDigest := sha256.Sum256(manifestData)
			manifestSize := int64(len(manifestData))

			manifest, err := registryv1.ParseManifest(bytes.NewReader(manifestData))
			if err != nil {
				return nil, fmt.Errorf("referrer %d: parsing manifest %d from %s: %w", refIdx, mIdx, manifestPath, err)
			}

			manifestDescriptor := api.Descriptor{
				MediaType: string(manifest.MediaType),
				Digest:    fmt.Sprintf("sha256:%x", manifestDigest),
				Size:      manifestSize,
			}

			configDescriptor := api.Descriptor{
				MediaType: string(manifest.Config.MediaType),
				Digest:    manifest.Config.Digest.String(),
				Size:      manifest.Config.Size,
			}

			layerBlobs := make([]api.LayerBlob, len(manifest.Layers))
			for j, layer := range manifest.Layers {
				layerBlobs[j] = api.LayerBlob{
					Descriptor: api.Descriptor{
						MediaType: string(layer.MediaType),
						Digest:    layer.Digest.String(),
						Size:      layer.Size,
					},
				}
			}

			refManifests[i] = api.ManifestDeployInfo{
				Descriptor: manifestDescriptor,
				Config:     configDescriptor,
				LayerBlobs: layerBlobs,
			}
		}

		refBaseCommand := api.BaseCommandOperation{
			Command:   "push",
			RootKind:  refRootKind,
			Root:      refRootDescriptor,
			Manifests: refManifests,
		}

		// Create push operation with same registry/repository but no tags
		refOp, err := referrerPushOperation(refBaseCommand, config)
		if err != nil {
			return nil, fmt.Errorf("referrer %d: creating push operation: %w", refIdx, err)
		}

		opBytes, err := json.Marshal(refOp)
		if err != nil {
			return nil, fmt.Errorf("referrer %d: marshalling push operation: %w", refIdx, err)
		}
		ops = append(ops, opBytes)
	}
	return ops, nil
}

// validateSubject checks that a referrer's root manifest/index has a subject field
// whose digest matches a known digest of the main image.
func validateSubject(refIdx int, rootData []byte, knownDigests map[string]bool) error {
	// Parse as generic JSON to extract the subject field
	var root struct {
		Subject *struct {
			Digest string `json:"digest"`
		} `json:"subject"`
	}
	if err := json.Unmarshal(rootData, &root); err != nil {
		return fmt.Errorf("referrer %d: parsing root JSON for subject validation: %w", refIdx, err)
	}
	if root.Subject == nil {
		return fmt.Errorf("referrer %d: manifest/index does not contain a 'subject' field", refIdx)
	}
	if root.Subject.Digest == "" {
		return fmt.Errorf("referrer %d: subject descriptor has no digest", refIdx)
	}
	if !knownDigests[root.Subject.Digest] {
		return fmt.Errorf("referrer %d: subject digest %s does not match any blob of the main image", refIdx, root.Subject.Digest)
	}
	return nil
}

// referrerPushOperation creates a push operation for a referrer using the same
// registry and repository as the main image, but with no tags.
func referrerPushOperation(baseCommand api.BaseCommandOperation, config map[string]any) (api.PushDeployOperation, error) {
	var registry, repository string

	if destinationFilePath != "" {
		reg, repo, err := parseDestinationFile(destinationFilePath)
		if err != nil {
			return api.PushDeployOperation{}, err
		}
		registry = reg
		repository = repo
	} else {
		var ok bool
		registry, ok = config["registry"].(string)
		if !ok || registry == "" {
			return api.PushDeployOperation{}, fmt.Errorf("configuration file must contain a non-empty 'registry' field")
		}
		repository, ok = config["repository"].(string)
		if !ok || repository == "" {
			return api.PushDeployOperation{}, fmt.Errorf("configuration file must contain a non-empty 'repository' field")
		}
	}

	return api.PushDeployOperation{
		BaseCommandOperation: baseCommand,
		PushTarget: api.PushTarget{
			Registry:   registry,
			Repository: repository,
			// No tags for referrers — they are discovered via the referrers API
		},
	}, nil
}

func checkCrossMountSource(targetRegistry string, sourceRegistry string, sourceRepository string) *api.CrossMountSource {
	if targetRegistry == sourceRegistry && (crossMountStrategy == "same_registry" || crossMountStrategy == "cross_registry") {
		return &api.CrossMountSource{Repository: sourceRepository}
	}

	if crossMountStrategy == "cross_registry" {
		return &api.CrossMountSource{Registry: sourceRegistry, Repository: sourceRepository}
	}

	return nil
}

func pickCrossMountSource(targetRegistry string) (*api.CrossMountSource, error) {
	if crossMountFromManifestPath != "" {
		manifestData, err := os.ReadFile(crossMountFromManifestPath)
		if err != nil {
			return nil, fmt.Errorf("reading manifest file %s: %w", crossMountFromManifestPath, err)
		}

		var deployManifest api.DeployManifest
		if err := json.Unmarshal(manifestData, &deployManifest); err != nil {
			return nil, fmt.Errorf("parsing manifest file %s: %w", crossMountFromManifestPath, err)
		}

		pushOps, err := deployManifest.PushOperations()
		if err != nil {
			return nil, fmt.Errorf("parsing manifest file %s: %w", crossMountFromManifestPath, err)
		}

		for _, sourceOperation := range pushOps {
			if source := checkCrossMountSource(targetRegistry, sourceOperation.Registry, sourceOperation.Repository); source != nil {
				return source, nil
			}
		}
	}

	for _, originalRegistry := range originalRegistries {
		if source := checkCrossMountSource(targetRegistry, originalRegistry, originalRepository); source != nil {
			return source, nil
		}
	}

	return nil, nil
}

func parseDestinationFile(path string) (registry string, repository string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("reading destination file: %w", err)
	}

	destination := strings.TrimSpace(string(data))

	if destination == "" {
		return "", "", fmt.Errorf("destination file %q is empty", path)
	}

	if strings.ContainsAny(destination, "\n\r") {
		return "", "", fmt.Errorf("destination file %q must contain exactly one line", path)
	}

	slashIndex := strings.Index(destination, "/")
	if slashIndex < 0 {
		return "", "", fmt.Errorf("destination file %q must contain a '/' separating registry from repository, got %q", path, destination)
	}

	registry = destination[:slashIndex]
	repository = destination[slashIndex+1:]

	if registry == "" {
		return "", "", fmt.Errorf("destination file %q has an empty registry (content: %q)", path, destination)
	}
	if repository == "" {
		return "", "", fmt.Errorf("destination file %q has an empty repository (content: %q)", path, destination)
	}

	return registry, repository, nil
}

func pushOperation(baseCommand api.BaseCommandOperation, config map[string]any) (api.PushDeployOperation, error) {
	var registry, repository string

	if destinationFilePath != "" {
		reg, repo, err := parseDestinationFile(destinationFilePath)
		if err != nil {
			return api.PushDeployOperation{}, err
		}
		registry = reg
		repository = repo
	} else {
		var ok bool
		registry, ok = config["registry"].(string)
		if !ok || registry == "" {
			return api.PushDeployOperation{}, fmt.Errorf("configuration file must contain a non-empty 'registry' field")
		}
		repository, ok = config["repository"].(string)
		if !ok || repository == "" {
			return api.PushDeployOperation{}, fmt.Errorf("configuration file must contain a non-empty 'repository' field")
		}
	}

	tagsInterface, ok := config["tags"].([]interface{})
	if !ok {
		tagsInterface = []interface{}{}
	}

	// Convert interface{} slice to string slice
	tags := make([]string, len(tagsInterface))
	for i, tag := range tagsInterface {
		if tagStr, ok := tag.(string); ok {
			tags[i] = tagStr
		} else {
			return api.PushDeployOperation{}, fmt.Errorf("tag at index %d is not a string", i)
		}
	}

	var err error
	baseCommand.CrossMountHint, err = pickCrossMountSource(registry)
	if err != nil {
		return api.PushDeployOperation{}, err
	}

	return api.PushDeployOperation{
		BaseCommandOperation: baseCommand,
		PushTarget: api.PushTarget{
			Registry:   registry,
			Repository: repository,
			Tags:       tags,
		},
	}, nil
}

func registryTagOperations(pushTarget api.PushTarget, manifests []api.ManifestDeployInfo) ([]json.RawMessage, error) {
	if len(manifestTagFiles) == 0 {
		return nil, nil
	}
	indices := make([]int, 0, len(manifestTagFiles))
	for idx := range manifestTagFiles {
		indices = append(indices, idx)
	}
	sort.Ints(indices)

	// tag -> child digest; collisions across distinct children are a build error.
	tagOwner := make(map[string]string)

	var ops []json.RawMessage
	for _, manifestIdx := range indices {
		filePath := manifestTagFiles[manifestIdx]
		if manifestIdx < 0 || manifestIdx >= len(manifests) {
			return nil, fmt.Errorf("--manifest-tag-file references manifest index %d, but only %d manifests are present", manifestIdx, len(manifests))
		}
		child := manifests[manifestIdx]
		if child.Descriptor.Digest == "" {
			return nil, fmt.Errorf("--manifest-tag-file references manifest index %d, but that manifest has no descriptor (empty manifest-path?)", manifestIdx)
		}

		raw, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("reading manifest-tag-file for manifest index %d: %w", manifestIdx, err)
		}
		var payload struct {
			ManifestTags []string `json:"manifest_tags"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, fmt.Errorf("parsing manifest-tag-file for manifest index %d (%s): %w", manifestIdx, filePath, err)
		}

		tags := dedupeNonEmpty(payload.ManifestTags)
		if len(tags) == 0 {
			continue
		}

		for _, t := range tags {
			if existingDigest, ok := tagOwner[t]; ok && existingDigest != child.Descriptor.Digest {
				return nil, fmt.Errorf(
					"manifest_tag %q would be written to two different child manifests (%s and %s); "+
						"template must discriminate between all child platforms (e.g. by including {{.variant}})",
					t, existingDigest, child.Descriptor.Digest,
				)
			}
			tagOwner[t] = child.Descriptor.Digest
		}

		op := api.RegistryTagDeployOperation{
			BaseCommandOperation: api.BaseCommandOperation{
				Command:  "registry_tag",
				RootKind: "manifest",
				Root:     child.Descriptor,
			},
			PushTarget: api.PushTarget{
				Registry:   pushTarget.Registry,
				Repository: pushTarget.Repository,
				Tags:       tags,
			},
		}
		encoded, err := json.Marshal(op)
		if err != nil {
			return nil, fmt.Errorf("marshalling registry_tag operation for manifest index %d: %w", manifestIdx, err)
		}
		ops = append(ops, encoded)
	}
	return ops, nil
}

func dedupeNonEmpty(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func loadOperation(baseCommand api.BaseCommandOperation, config map[string]any) (api.LoadDeployOperation, error) {
	tagsInterface, ok := config["tags"].([]interface{})
	if !ok {
		tagsInterface = []interface{}{}
	}

	// Convert interface{} slice to string slice
	tags := make([]string, len(tagsInterface))
	for i, tag := range tagsInterface {
		if tagStr, ok := tag.(string); ok {
			tags[i] = tagStr
		} else {
			return api.LoadDeployOperation{}, fmt.Errorf("tag at index %d is not a string", i)
		}
	}

	daemon, ok := config["daemon"].(string)
	if !ok || daemon == "" {
		return api.LoadDeployOperation{}, fmt.Errorf("configuration file must contain a non-empty 'daemon' field")
	}

	// registry and repository are optional. When both are set, the loader and
	// docker-save reconstruct full image names as "<registry>/<repository>:<tag>".
	// When absent (the rules_oci-compatible mode) the tags are already full
	// references and only the 'tags' field is emitted (registry/repository are
	// omitempty on LoadDeployOperation). Exactly one being set (e.g. a template
	// that expanded to empty) is a hard error, mirroring the push path.
	registry, _ := config["registry"].(string)
	repository, _ := config["repository"].(string)
	if err := api.ValidateLoadDestination(registry, repository); err != nil {
		return api.LoadDeployOperation{}, err
	}

	return api.LoadDeployOperation{
		BaseCommandOperation: baseCommand,
		Registry:             registry,
		Repository:           repository,
		Tags:                 tags,
		Daemon:               daemon,
	}, nil
}
