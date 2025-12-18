package deploymetadata

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	registryv1 "github.com/malt3/go-containerregistry/pkg/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

var (
	command                 string
	rootPath                string
	rootKind                string
	configurationPath       string
	strategy                string
	manifestPaths           []string
	missingBlobsForManifest [][]string
	originalRegistries      []string
	originalRepository      string
	orginalTag              string
	originalDigest          string
	layerHintsInputPath     string
	layerHintsOutputPath    string
)

func DeployMetadataProcess(ctx context.Context, args []string) {
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
	flagSet.Func("original-registry", `(Optional) original registry that the base of this image was pulled from. Can be specified multiple times.`, func(value string) error {
		originalRegistries = append(originalRegistries, value)
		return nil
	})
	flagSet.StringVar(&originalRepository, "original-repository", "", `(Optional) original repository that the base of this image was pulled from.`)
	flagSet.StringVar(&orginalTag, "original-tag", "", `(Optional) original tag that the base of this image was pulled from.`)
	flagSet.StringVar(&originalDigest, "original-digest", "", `(Optional) original digest that the base of this image was pulled from.`)
	flagSet.StringVar(&layerHintsInputPath, "layer-hints-paths-file-input", "", `(Optional) path to file containing layer path hints (null-separated blob/metadata pairs).`)
	flagSet.StringVar(&layerHintsOutputPath, "layer-hints-paths-output", "", `(Optional) path to write resolved layer hints output.`)
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
	flagSet.Func("missing-blobs-for-manifest", `Missing blobs for a manifest. Format: index=blob1,blob2,... (e.g., 0=sha256:abc,sha256:def). Can be specified multiple times.`, func(value string) error {
		parts := strings.SplitN(value, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("missing-blobs-for-manifest must be in format index=blob1,blob2,...")
		}
		index, err := strconv.Atoi(parts[0])
		if err != nil {
			return fmt.Errorf("invalid index in missing-blobs-for-manifest: %w", err)
		}
		blobs := strings.Split(parts[1], ",")
		if len(blobs) == 1 && blobs[0] == "" {
			blobs = nil // Handle empty case
		}
		// Expand slice if necessary
		for len(missingBlobsForManifest) <= index {
			missingBlobsForManifest = append(missingBlobsForManifest, nil)
		}
		missingBlobsForManifest[index] = blobs
		return nil
	})

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
	if err := WriteMetadata(ctx, outputPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing deploy metadata: %v\n", err)
		os.Exit(1)
	}
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

		// Extract layer descriptors
		layerBlobs := make([]api.Descriptor, len(manifest.Layers))
		for j, layer := range manifest.Layers {
			layerBlobs[j] = api.Descriptor{
				MediaType: string(layer.MediaType),
				Digest:    layer.Digest.String(),
				Size:      layer.Size,
			}
		}

		// Get missing blobs for this manifest
		var missingBlobs []string
		if i < len(missingBlobsForManifest) && missingBlobsForManifest[i] != nil {
			missingBlobs = missingBlobsForManifest[i]
		}

		manifests[i] = api.ManifestDeployInfo{
			Descriptor:   manifestDescriptor,
			Config:       configDescriptor,
			LayerBlobs:   layerBlobs,
			MissingBlobs: missingBlobs,
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
		return fmt.Errorf("invalid command " + command)
	}

	deployManifest := api.DeployManifest{
		Operations: []json.RawMessage{operationBytes},
		Settings:   deploySettings,
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

func pushOperation(baseCommand api.BaseCommandOperation, config map[string]any) (api.PushDeployOperation, error) {
	registry, ok := config["registry"].(string)
	if !ok || registry == "" {
		return api.PushDeployOperation{}, fmt.Errorf("configuration file must contain a non-empty 'registry' field")
	}
	repository, ok := config["repository"].(string)
	if !ok || repository == "" {
		return api.PushDeployOperation{}, fmt.Errorf("configuration file must contain a non-empty 'repository' field")
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

	return api.PushDeployOperation{
		BaseCommandOperation: baseCommand,
		PushTarget: api.PushTarget{
			Registry:   registry,
			Repository: repository,
			Tags:       tags,
		},
	}, nil
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

	return api.LoadDeployOperation{
		BaseCommandOperation: baseCommand,
		Tags:                 tags,
		Daemon:               daemon,
	}, nil
}
