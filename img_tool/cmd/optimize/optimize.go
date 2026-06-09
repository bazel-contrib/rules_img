package optimize

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	specv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

var (
	sourceManifest        string
	sourceConfig          string
	sourceIndex           string
	sourceDescriptor      string
	layerMetadataArgs     fileList
	manifestDescriptorArgs fileList
	manifestOutput        string
	configOutput          string
	indexOutput           string
	descriptorOutput      string
	digestOutput          string
)

func OptimizeProcess(_ context.Context, args []string) {
	flagSet := flag.NewFlagSet("optimize", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Rewrites an image manifest or index after layer optimization.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img optimize --source-manifest manifest.json --source-config config.json --layer-from-metadata layer.json --manifest out.json --config out.json\n")
		fmt.Fprintf(flagSet.Output(), "   or: img optimize --source-index index.json --manifest-descriptor manifest_descriptor.json --index out.json\n")
		flagSet.PrintDefaults()
		os.Exit(1)
	}
	flagSet.StringVar(&sourceManifest, "source-manifest", "", "Source image manifest to preserve and rewrite.")
	flagSet.StringVar(&sourceConfig, "source-config", "", "Source image config to preserve and rewrite.")
	flagSet.StringVar(&sourceIndex, "source-index", "", "Source image index to preserve and rewrite.")
	flagSet.StringVar(&sourceDescriptor, "source-descriptor", "", "Source descriptor to preserve while updating digest and size.")
	flagSet.Var(&layerMetadataArgs, "layer-from-metadata", `Ordered layer metadata files for a rewritten image manifest.`)
	flagSet.Var(&manifestDescriptorArgs, "manifest-descriptor", `Manifest descriptor files for a rewritten image index.`)
	flagSet.StringVar(&manifestOutput, "manifest", "", "Output image manifest.")
	flagSet.StringVar(&configOutput, "config", "", "Output image config.")
	flagSet.StringVar(&indexOutput, "index", "", "Output image index.")
	flagSet.StringVar(&descriptorOutput, "descriptor", "", "Output descriptor.")
	flagSet.StringVar(&digestOutput, "digest", "", "Output digest.")

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}
	if flagSet.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "Unexpected positional arguments: %s\n", strings.Join(flagSet.Args(), " "))
		flagSet.Usage()
		os.Exit(1)
	}

	manifestMode := sourceManifest != "" || sourceConfig != "" || len(layerMetadataArgs) > 0 || manifestOutput != "" || configOutput != ""
	indexMode := sourceIndex != "" || len(manifestDescriptorArgs) > 0 || indexOutput != ""
	if manifestMode == indexMode {
		fmt.Fprintln(os.Stderr, "Choose exactly one mode: manifest rewrite or index rewrite")
		flagSet.Usage()
		os.Exit(1)
	}

	var err error
	if manifestMode {
		err = rewriteManifest()
	} else {
		err = rewriteIndex()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Optimizing image metadata: %v\n", err)
		os.Exit(1)
	}
}

func rewriteManifest() error {
	if sourceManifest == "" {
		return fmt.Errorf("--source-manifest is required")
	}
	if sourceConfig == "" {
		return fmt.Errorf("--source-config is required")
	}
	if len(layerMetadataArgs) == 0 {
		return fmt.Errorf("at least one --layer-from-metadata is required")
	}

	manifest, err := readJSONObject(sourceManifest)
	if err != nil {
		return fmt.Errorf("reading source manifest: %w", err)
	}
	config, err := readJSONObject(sourceConfig)
	if err != nil {
		return fmt.Errorf("reading source config: %w", err)
	}

	layers, diffIDs, err := readLayerDescriptors(layerMetadataArgs)
	if err != nil {
		return err
	}

	rootFS, _ := config["rootfs"].(map[string]any)
	if rootFS == nil {
		rootFS = make(map[string]any)
	}
	rootFS["type"] = "layers"
	rootFS["diff_ids"] = diffIDs
	config["rootfs"] = rootFS

	configRaw, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("marshaling rewritten config: %w", err)
	}

	configDescriptor := sourceConfigDescriptor(manifest)
	configMediaType := stringField(configDescriptor, "mediaType")
	if configMediaType == "" {
		configMediaType = specv1.MediaTypeImageConfig
	}
	configDescriptor["mediaType"] = configMediaType
	configDescriptor["digest"] = digestString(configRaw)
	configDescriptor["size"] = len(configRaw)
	delete(configDescriptor, "data")

	if _, ok := manifest["schemaVersion"]; !ok {
		manifest["schemaVersion"] = 2
	}
	if stringField(manifest, "mediaType") == "" {
		manifest["mediaType"] = specv1.MediaTypeImageManifest
	}
	manifest["config"] = configDescriptor
	manifest["layers"] = layers

	manifestRaw, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshaling rewritten manifest: %w", err)
	}

	if err := writeIfRequested(configOutput, configRaw); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	if err := writeIfRequested(manifestOutput, manifestRaw); err != nil {
		return fmt.Errorf("writing manifest: %w", err)
	}
	return writeDescriptorAndDigest(sourceDescriptor, stringField(manifest, "mediaType"), manifestRaw)
}

func rewriteIndex() error {
	if sourceIndex == "" {
		return fmt.Errorf("--source-index is required")
	}
	if len(manifestDescriptorArgs) == 0 {
		return fmt.Errorf("at least one --manifest-descriptor is required")
	}

	index, err := readJSONObject(sourceIndex)
	if err != nil {
		return fmt.Errorf("reading source index: %w", err)
	}

	manifests := make([]any, 0, len(manifestDescriptorArgs))
	for _, manifestDescriptor := range manifestDescriptorArgs {
		descriptor, err := readJSONObject(manifestDescriptor)
		if err != nil {
			return fmt.Errorf("reading manifest descriptor %s: %w", manifestDescriptor, err)
		}
		manifests = append(manifests, descriptor)
	}

	if _, ok := index["schemaVersion"]; !ok {
		index["schemaVersion"] = 2
	}
	if stringField(index, "mediaType") == "" {
		index["mediaType"] = specv1.MediaTypeImageIndex
	}
	index["manifests"] = manifests

	indexRaw, err := json.Marshal(index)
	if err != nil {
		return fmt.Errorf("marshaling rewritten index: %w", err)
	}

	if err := writeIfRequested(indexOutput, indexRaw); err != nil {
		return fmt.Errorf("writing index: %w", err)
	}
	return writeDescriptorAndDigest(sourceDescriptor, stringField(index, "mediaType"), indexRaw)
}

func readLayerDescriptors(paths []string) ([]any, []string, error) {
	layers := make([]any, 0, len(paths))
	diffIDs := make([]string, 0, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("reading layer metadata %s: %w", path, err)
		}
		var layer api.Descriptor
		if err := json.Unmarshal(raw, &layer); err != nil {
			return nil, nil, fmt.Errorf("decoding layer metadata %s: %w", path, err)
		}
		if layer.MediaType == "" {
			return nil, nil, fmt.Errorf("layer metadata %s is missing mediaType", path)
		}
		if layer.Digest == "" {
			return nil, nil, fmt.Errorf("layer metadata %s is missing digest", path)
		}
		if layer.DiffID == "" {
			return nil, nil, fmt.Errorf("layer metadata %s is missing diff_id", path)
		}
		descriptor := map[string]any{
			"mediaType": layer.MediaType,
			"digest":    layer.Digest,
			"size":      layer.Size,
		}
		if len(layer.Annotations) > 0 {
			descriptor["annotations"] = layer.Annotations
		}
		layers = append(layers, descriptor)
		diffIDs = append(diffIDs, layer.DiffID)
	}
	return layers, diffIDs, nil
}

func sourceConfigDescriptor(manifest map[string]any) map[string]any {
	configDescriptor, _ := manifest["config"].(map[string]any)
	if configDescriptor == nil {
		return make(map[string]any)
	}
	clone := make(map[string]any, len(configDescriptor))
	for key, value := range configDescriptor {
		clone[key] = value
	}
	return clone
}

func writeDescriptorAndDigest(sourceDescriptorPath string, mediaType string, content []byte) error {
	contentDigest := digestString(content)
	if digestOutput != "" {
		if err := os.WriteFile(digestOutput, []byte(contentDigest), 0o644); err != nil {
			return fmt.Errorf("writing digest: %w", err)
		}
	}
	if descriptorOutput == "" {
		return nil
	}

	descriptor := make(map[string]any)
	if sourceDescriptorPath != "" {
		sourceDescriptor, err := readJSONObject(sourceDescriptorPath)
		if err != nil {
			return fmt.Errorf("reading source descriptor: %w", err)
		}
		descriptor = sourceDescriptor
	}
	if mediaType == "" {
		mediaType = stringField(descriptor, "mediaType")
	}
	descriptor["mediaType"] = mediaType
	descriptor["digest"] = contentDigest
	descriptor["size"] = len(content)

	rawDescriptor, err := json.Marshal(descriptor)
	if err != nil {
		return fmt.Errorf("marshaling descriptor: %w", err)
	}
	return os.WriteFile(descriptorOutput, rawDescriptor, 0o644)
}

func readJSONObject(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func stringField(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return value
}

func digestString(content []byte) string {
	sum := sha256.Sum256(content)
	return fmt.Sprintf("sha256:%x", sum[:])
}

func writeIfRequested(path string, content []byte) error {
	if path == "" {
		return nil
	}
	return os.WriteFile(path, content, 0o644)
}
