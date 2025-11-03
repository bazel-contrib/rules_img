package manifestfromocilayout

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	specv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

var (
	srcDir            string
	manifestOutput    string
	configOutput      string
	descriptorOutput  string
	digestOutput      string
	architecture      string
	operatingSystem   string
	variant           string
	layerMediaTypes   *indexedStringFlag
	layerBlobs        *indexedStringFlag
	layerMetadataJSON *indexedStringFlag
)

func ManifestFromOCILayoutProcess(_ context.Context, args []string) {
	layerMediaTypes = newIndexedStringFlag()
	layerBlobs = newIndexedStringFlag()
	layerMetadataJSON = newIndexedStringFlag()

	flagSet := flag.NewFlagSet("manifest-from-oci-layout", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Converts an OCI layout to an image manifest with specified layer media types.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img manifest-from-oci-layout [OPTIONS]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"img manifest-from-oci-layout --src oci-layout --manifest manifest.json --config config.json --descriptor descriptor.json --digest digest.txt --architecture amd64 --os linux --layer_media_type=0=application/vnd.oci.image.layer.v1.tar+gzip --layer_blob=0=layer0.tgz --layer_metadata_json=0=layer0_metadata.json",
		}
		fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
	}

	flagSet.StringVar(&srcDir, "src", "", "Path to the OCI layout directory (required)")
	flagSet.StringVar(&manifestOutput, "manifest", "", "Output path for the image manifest (required)")
	flagSet.StringVar(&configOutput, "config", "", "Output path for the image config (required)")
	flagSet.StringVar(&descriptorOutput, "descriptor", "", "Output path for the manifest descriptor (required)")
	flagSet.StringVar(&digestOutput, "digest", "", "Output path for the manifest digest (required)")
	flagSet.StringVar(&architecture, "architecture", "", "Target architecture (required)")
	flagSet.StringVar(&operatingSystem, "os", "", "Target OS (required)")
	flagSet.StringVar(&variant, "variant", "", "Target variant (optional, e.g., v3 for amd64/v3, v8 for arm64/v8)")
	flagSet.Var(layerMediaTypes, "layer_media_type", "Layer media type in format index=mediatype (can be specified multiple times)")
	flagSet.Var(layerBlobs, "layer_blob", "Output path for layer blob in format index=path (can be specified multiple times)")
	flagSet.Var(layerMetadataJSON, "layer_metadata_json", "Output path for layer metadata JSON in format index=path (can be specified multiple times)")

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}

	// Validate required flags
	if srcDir == "" {
		fmt.Fprintf(os.Stderr, "Error: --src is required\n")
		flagSet.Usage()
		os.Exit(1)
	}
	if manifestOutput == "" {
		fmt.Fprintf(os.Stderr, "Error: --manifest is required\n")
		flagSet.Usage()
		os.Exit(1)
	}
	if configOutput == "" {
		fmt.Fprintf(os.Stderr, "Error: --config is required\n")
		flagSet.Usage()
		os.Exit(1)
	}
	if descriptorOutput == "" {
		fmt.Fprintf(os.Stderr, "Error: --descriptor is required\n")
		flagSet.Usage()
		os.Exit(1)
	}
	if digestOutput == "" {
		fmt.Fprintf(os.Stderr, "Error: --digest is required\n")
		flagSet.Usage()
		os.Exit(1)
	}
	if architecture == "" {
		fmt.Fprintf(os.Stderr, "Error: --architecture is required\n")
		flagSet.Usage()
		os.Exit(1)
	}
	if operatingSystem == "" {
		fmt.Fprintf(os.Stderr, "Error: --os is required\n")
		flagSet.Usage()
		os.Exit(1)
	}

	// ARM64 defaults to v8 variant
	// See: https://github.com/containerd/platforms/blob/2e51fd9435bd985e1753954b24f4b0453f4e4767/platforms.go#L290
	if architecture == "arm64" && variant == "" {
		variant = "v8"
	}

	if len(layerMediaTypes.values) == 0 {
		fmt.Fprintf(os.Stderr, "Error: at least one --layer_media_type is required\n")
		flagSet.Usage()
		os.Exit(1)
	}

	// Validate that layer specifications are complete
	for i := range len(layerMediaTypes.values) {
		if _, ok := layerMediaTypes.Get(i); !ok {
			fmt.Fprintf(os.Stderr, "Error: missing --layer_media_type=%d\n", i)
			os.Exit(1)
		}
		if _, ok := layerBlobs.Get(i); !ok {
			fmt.Fprintf(os.Stderr, "Error: missing --layer_blob=%d\n", i)
			os.Exit(1)
		}
		if _, ok := layerMetadataJSON.Get(i); !ok {
			fmt.Fprintf(os.Stderr, "Error: missing --layer_metadata_json=%d\n", i)
			os.Exit(1)
		}
	}

	if err := processOCILayout(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func processOCILayout() error {
	// Read the OCI layout index
	indexPath := filepath.Join(srcDir, "index.json")
	indexData, err := os.ReadFile(indexPath)
	if err != nil {
		return fmt.Errorf("reading index.json: %w", err)
	}

	var index specv1.Index
	if err := json.Unmarshal(indexData, &index); err != nil {
		return fmt.Errorf("unmarshaling index.json: %w", err)
	}

	// Find the manifest for the target platform
	manifestDesc, err := findManifestForPlatform(&index, architecture, operatingSystem, variant)
	if err != nil {
		return fmt.Errorf("finding manifest for platform: %w", err)
	}

	// Validate that the manifest descriptor platform matches if it has platform information
	// (single-platform OCI layouts from image_manifest may not have platform in the descriptor)
	if manifestDesc.Platform != nil {
		if manifestDesc.Platform.OS != operatingSystem {
			return fmt.Errorf("manifest descriptor OS mismatch: has %s, but %s was requested", manifestDesc.Platform.OS, operatingSystem)
		}
		if manifestDesc.Platform.Architecture != architecture {
			return fmt.Errorf("manifest descriptor architecture mismatch: has %s, but %s was requested", manifestDesc.Platform.Architecture, architecture)
		}
		// Normalize variant for comparison (empty string vs unset)
		manifestVariant := ""
		if manifestDesc.Platform.Variant != "" {
			manifestVariant = manifestDesc.Platform.Variant
		}
		if manifestVariant != variant {
			return fmt.Errorf("manifest descriptor variant mismatch: has %s, but %s was requested", manifestVariant, variant)
		}
	}

	// Read the manifest from blobs
	manifestBlobPath := filepath.Join(srcDir, "blobs", manifestDesc.Digest.Algorithm().String(), manifestDesc.Digest.Hex())
	manifestData, err := os.ReadFile(manifestBlobPath)
	if err != nil {
		return fmt.Errorf("reading manifest blob: %w", err)
	}

	var manifest specv1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("unmarshaling manifest: %w", err)
	}

	// Read the config from blobs
	configBlobPath := filepath.Join(srcDir, "blobs", manifest.Config.Digest.Algorithm().String(), manifest.Config.Digest.Hex())
	configData, err := os.ReadFile(configBlobPath)
	if err != nil {
		return fmt.Errorf("reading config blob: %w", err)
	}

	var config specv1.Image
	if err := json.Unmarshal(configData, &config); err != nil {
		return fmt.Errorf("unmarshaling config: %w", err)
	}

	// Validate that the config matches the requested platform
	if config.OS != operatingSystem {
		return fmt.Errorf("OS mismatch: config has %s, but %s was requested", config.OS, operatingSystem)
	}
	if config.Architecture != architecture {
		return fmt.Errorf("architecture mismatch: config has %s, but %s was requested", config.Architecture, architecture)
	}

	// Check that the number of layers matches
	if len(manifest.Layers) != len(layerMediaTypes.values) {
		return fmt.Errorf("layer count mismatch: OCI layout has %d layers, but %d layer media types specified", len(manifest.Layers), len(layerMediaTypes.values))
	}

	// Copy/hardlink each layer blob and create metadata JSONs
	for i := range manifest.Layers {
		targetMediaType, _ := layerMediaTypes.Get(i)
		layerBlobPath, _ := layerBlobs.Get(i)
		layerMetadataPath, _ := layerMetadataJSON.Get(i)

		// Verify media type matches
		if manifest.Layers[i].MediaType != targetMediaType {
			return fmt.Errorf("layer %d media type mismatch: OCI layout has %s, but %s was requested", i, manifest.Layers[i].MediaType, targetMediaType)
		}

		// Copy the layer blob
		sourceLayerPath := filepath.Join(srcDir, "blobs", manifest.Layers[i].Digest.Algorithm().String(), manifest.Layers[i].Digest.Hex())
		if err := copyOrHardlink(sourceLayerPath, layerBlobPath); err != nil {
			return fmt.Errorf("copying layer %d blob: %w", i, err)
		}

		// Create the metadata JSON from the existing descriptor
		metadata := api.Descriptor{
			DiffID:      config.RootFS.DiffIDs[i].String(),
			MediaType:   manifest.Layers[i].MediaType,
			Digest:      manifest.Layers[i].Digest.String(),
			Size:        manifest.Layers[i].Size,
			Annotations: manifest.Layers[i].Annotations,
		}

		metadataJSON, err := json.MarshalIndent(metadata, "", "  ")
		if err != nil {
			return fmt.Errorf("marshaling metadata for layer %d: %w", i, err)
		}

		if err := os.WriteFile(layerMetadataPath, metadataJSON, 0o644); err != nil {
			return fmt.Errorf("writing metadata for layer %d: %w", i, err)
		}
	}

	// Copy manifest blob to output
	if err := copyOrHardlink(manifestBlobPath, manifestOutput); err != nil {
		return fmt.Errorf("copying manifest: %w", err)
	}

	// Copy config blob to output
	if err := copyOrHardlink(configBlobPath, configOutput); err != nil {
		return fmt.Errorf("copying config: %w", err)
	}

	// Create the descriptor
	descriptor := specv1.Descriptor{
		MediaType: manifestDesc.MediaType,
		Digest:    manifestDesc.Digest,
		Size:      manifestDesc.Size,
		Platform: &specv1.Platform{
			Architecture: architecture,
			OS:           operatingSystem,
			Variant:      variant,
		},
	}
	descriptorJSON, err := json.Marshal(descriptor)
	if err != nil {
		return fmt.Errorf("marshaling descriptor: %w", err)
	}

	if err := os.WriteFile(descriptorOutput, descriptorJSON, 0o644); err != nil {
		return fmt.Errorf("writing descriptor: %w", err)
	}

	// Write digest
	if err := os.WriteFile(digestOutput, []byte(manifestDesc.Digest.String()), 0o644); err != nil {
		return fmt.Errorf("writing digest: %w", err)
	}

	return nil
}

func findManifestForPlatform(index *specv1.Index, arch, os, variant string) (*specv1.Descriptor, error) {
	// First try to find a manifest with matching platform information
	for i := range index.Manifests {
		desc := &index.Manifests[i]
		if desc.Platform != nil && desc.Platform.Architecture == arch && desc.Platform.OS == os {
			// Normalize variant for comparison (empty string vs unset)
			descVariant := ""
			if desc.Platform.Variant != "" {
				descVariant = desc.Platform.Variant
			}
			if descVariant == variant {
				return desc, nil
			}
		}
	}

	// If no platform-specific manifest found, and there's exactly one manifest,
	// use it (this is typical for single-platform OCI layouts from image_manifest)
	if len(index.Manifests) == 1 {
		return &index.Manifests[0], nil
	}

	variantMsg := ""
	if variant != "" {
		variantMsg = "/" + variant
	}
	return nil, fmt.Errorf("no manifest found for platform %s/%s%s", os, arch, variantMsg)
}

// copyOrHardlink attempts to hardlink the file, falling back to copy if hardlink fails
func copyOrHardlink(src, dst string) error {
	// Try hardlink first
	if err := os.Link(src, dst); err == nil {
		return nil
	}

	// Fallback to copy
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening source file: %w", err)
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("creating destination file: %w", err)
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return fmt.Errorf("copying file: %w", err)
	}

	return nil
}
