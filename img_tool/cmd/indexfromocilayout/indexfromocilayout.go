package indexfromocilayout

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	specv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/metadata"
)

var (
	srcDir            string
	indexOutput       string
	digestOutput      string
	manifestPaths     *indexedStringFlag
	configPaths       *indexedStringFlag
	descriptorPaths   *indexedStringFlag
	osList            *indexedStringFlag
	architectureList  *indexedStringFlag
	variantList       *indexedStringFlag
	layerMediaTypes   *doubleIndexedStringFlag
	layerBlobs        *doubleIndexedStringFlag
	layerMetadataJSON *doubleIndexedStringFlag
)

func IndexFromOCILayoutProcess(_ context.Context, args []string) {
	manifestPaths = newIndexedStringFlag()
	configPaths = newIndexedStringFlag()
	descriptorPaths = newIndexedStringFlag()
	osList = newIndexedStringFlag()
	architectureList = newIndexedStringFlag()
	variantList = newIndexedStringFlag()
	layerMediaTypes = newDoubleIndexedStringFlag()
	layerBlobs = newDoubleIndexedStringFlag()
	layerMetadataJSON = newDoubleIndexedStringFlag()

	flagSet := flag.NewFlagSet("index-from-oci-layout", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Converts an OCI layout to an image index with specified manifests.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img index-from-oci-layout [OPTIONS]\n")
		flagSet.PrintDefaults()
	}

	flagSet.StringVar(&srcDir, "src", "", "Path to the OCI layout directory (required)")
	flagSet.StringVar(&indexOutput, "index", "", "Output path for the image index (required)")
	flagSet.StringVar(&digestOutput, "digest", "", "Output path for the index digest (required)")
	flagSet.Var(manifestPaths, "manifest", "Output path for manifest in format index=path")
	flagSet.Var(configPaths, "config", "Output path for config in format index=path")
	flagSet.Var(descriptorPaths, "descriptor", "Output path for descriptor in format index=path")
	flagSet.Var(osList, "os", "Target OS in format index=os")
	flagSet.Var(architectureList, "architecture", "Target architecture in format index=arch")
	flagSet.Var(variantList, "variant", "Target variant in format index=variant (optional, e.g., 'v3' for amd64/v3, 'v8' for arm64/v8)")
	flagSet.Var(layerMediaTypes, "layer_media_type", "Layer media type in format manifest_idx,layer_idx=mediatype")
	flagSet.Var(layerBlobs, "layer_blob", "Output path for layer blob in format manifest_idx,layer_idx=path")
	flagSet.Var(layerMetadataJSON, "layer_metadata_json", "Output path for layer metadata JSON in format manifest_idx,layer_idx=path")

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
	if indexOutput == "" {
		fmt.Fprintf(os.Stderr, "Error: --index is required\n")
		flagSet.Usage()
		os.Exit(1)
	}
	if digestOutput == "" {
		fmt.Fprintf(os.Stderr, "Error: --digest is required\n")
		flagSet.Usage()
		os.Exit(1)
	}

	if err := processOCILayoutIndex(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func processOCILayoutIndex() error {
	// Read the OCI layout index
	indexPath := filepath.Join(srcDir, "index.json")
	indexData, err := os.ReadFile(indexPath)
	if err != nil {
		return fmt.Errorf("reading index.json: %w", err)
	}

	var sourceIndex specv1.Index
	if err := json.Unmarshal(indexData, &sourceIndex); err != nil {
		return fmt.Errorf("unmarshaling index.json: %w", err)
	}

	// Check if this is a nested index structure (index -> index -> manifests)
	// Look for manifest descriptors that are actually image indexes
	if len(sourceIndex.Manifests) > 0 {
		// Check if the first descriptor is an image index
		firstDesc := &sourceIndex.Manifests[0]
		if firstDesc.MediaType == specv1.MediaTypeImageIndex {
			// This is a nested index, follow the pointer
			nestedIndexBlobPath := filepath.Join(srcDir, "blobs", firstDesc.Digest.Algorithm().String(), firstDesc.Digest.Hex())
			nestedIndexData, err := os.ReadFile(nestedIndexBlobPath)
			if err != nil {
				return fmt.Errorf("reading nested index blob: %w", err)
			}

			var nestedIndex specv1.Index
			if err := json.Unmarshal(nestedIndexData, &nestedIndex); err != nil {
				return fmt.Errorf("unmarshaling nested index: %w", err)
			}

			// Use the nested index's manifests
			sourceIndex = nestedIndex
		}
	}

	// Validate that the number of specified manifests matches the source index
	numSpecifiedManifests := len(manifestPaths.values)
	numSourceManifests := len(sourceIndex.Manifests)
	if numSpecifiedManifests != numSourceManifests {
		return fmt.Errorf("manifest count mismatch: OCI layout has %d manifests, but %d were specified", numSourceManifests, numSpecifiedManifests)
	}

	// Validate that each specified manifest matches the platform in the source index (in order)
	for i := range numSourceManifests {
		sourceManifest := &sourceIndex.Manifests[i]

		// Get the specified OS and architecture for this index
		specifiedOS, osOk := osList.Get(i)
		specifiedArch, archOk := architectureList.Get(i)

		if !osOk || !archOk {
			return fmt.Errorf("missing platform specification for manifest index %d", i)
		}

		// Get the specified variant (may be empty)
		specifiedVariant, _ := variantList.Get(i)

		// ARM64 defaults to v8 variant
		// See: https://github.com/containerd/platforms/blob/2e51fd9435bd985e1753954b24f4b0453f4e4767/platforms.go#L290
		if specifiedArch == "arm64" && specifiedVariant == "" {
			specifiedVariant = "v8"
		}

		// Check if the source manifest has platform information
		if sourceManifest.Platform == nil {
			return fmt.Errorf("manifest at index %d in OCI layout has no platform information", i)
		}

		// Normalize source variant for comparison
		sourceVariant := ""
		if sourceManifest.Platform.Variant != "" {
			sourceVariant = sourceManifest.Platform.Variant
		}

		// Validate that the platform matches
		if sourceManifest.Platform.OS != specifiedOS {
			return fmt.Errorf("manifest index %d: platform OS mismatch - OCI layout has %s/%s/%s, but %s/%s/%s was specified",
				i, sourceManifest.Platform.OS, sourceManifest.Platform.Architecture, sourceVariant, specifiedOS, specifiedArch, specifiedVariant)
		}
		if sourceManifest.Platform.Architecture != specifiedArch {
			return fmt.Errorf("manifest index %d: platform architecture mismatch - OCI layout has %s/%s/%s, but %s/%s/%s was specified",
				i, sourceManifest.Platform.OS, sourceManifest.Platform.Architecture, sourceVariant, specifiedOS, specifiedArch, specifiedVariant)
		}
		if sourceVariant != specifiedVariant {
			return fmt.Errorf("manifest index %d: platform variant mismatch - OCI layout has %s/%s/%s, but %s/%s/%s was specified",
				i, sourceManifest.Platform.OS, sourceManifest.Platform.Architecture, sourceVariant, specifiedOS, specifiedArch, specifiedVariant)
		}
	}

	// Get the list of manifest indices we need to process
	manifestIndices := make([]int, 0)
	for idx := range manifestPaths.values {
		manifestIndices = append(manifestIndices, idx)
	}

	// Process each manifest
	newManifestDescriptors := make([]specv1.Descriptor, len(manifestIndices))

	for i, manifestIdx := range manifestIndices {
		os, _ := osList.Get(manifestIdx)
		arch, _ := architectureList.Get(manifestIdx)
		variant, _ := variantList.Get(manifestIdx)
		manifestPath, _ := manifestPaths.Get(manifestIdx)
		configPath, _ := configPaths.Get(manifestIdx)
		descriptorPath, _ := descriptorPaths.Get(manifestIdx)

		// ARM64 defaults to v8 variant
		// See: https://github.com/containerd/platforms/blob/2e51fd9435bd985e1753954b24f4b0453f4e4767/platforms.go#L290
		if arch == "arm64" && variant == "" {
			variant = "v8"
		}

		// Convert the manifest at this index
		// We use the manifest descriptor directly from the source index since we've already validated
		// that the platforms match in order
		sourceManifestDesc := &sourceIndex.Manifests[manifestIdx]
		desc, err := convertManifest(sourceManifestDesc, manifestIdx, arch, os, variant, manifestPath, configPath, descriptorPath)
		if err != nil {
			return fmt.Errorf("converting manifest %d: %w", manifestIdx, err)
		}

		newManifestDescriptors[i] = desc
	}

	// Create the new index
	newIndex := specv1.Index{
		Versioned: specs.Versioned{
			SchemaVersion: 2,
		},
		MediaType: specv1.MediaTypeImageIndex,
		Manifests: newManifestDescriptors,
	}

	// Serialize and write the index
	indexJSON, err := json.Marshal(newIndex)
	if err != nil {
		return fmt.Errorf("marshaling index: %w", err)
	}

	if err := os.WriteFile(indexOutput, indexJSON, 0o644); err != nil {
		return fmt.Errorf("writing index: %w", err)
	}

	// Write digest
	indexSHA256 := sha256.Sum256(indexJSON)
	indexDigest := digest.NewDigestFromBytes(digest.SHA256, indexSHA256[:])
	if err := os.WriteFile(digestOutput, []byte(indexDigest.String()), 0o644); err != nil {
		return fmt.Errorf("writing digest: %w", err)
	}

	return nil
}

func convertManifest(manifestDesc *specv1.Descriptor, manifestIdx int, arch, operatingSystem, variant, manifestOutput, configOutput, descriptorOutput string) (specv1.Descriptor, error) {
	// Validate that the manifest descriptor platform matches if it has platform information
	// (This is a sanity check since we already validated in the caller)
	if manifestDesc.Platform != nil {
		if manifestDesc.Platform.OS != operatingSystem {
			return specv1.Descriptor{}, fmt.Errorf("manifest descriptor OS mismatch: has %s, but %s was requested", manifestDesc.Platform.OS, operatingSystem)
		}
		if manifestDesc.Platform.Architecture != arch {
			return specv1.Descriptor{}, fmt.Errorf("manifest descriptor architecture mismatch: has %s, but %s was requested", manifestDesc.Platform.Architecture, arch)
		}
		// Normalize variant for comparison (empty string vs unset)
		manifestVariant := ""
		if manifestDesc.Platform.Variant != "" {
			manifestVariant = manifestDesc.Platform.Variant
		}
		if manifestVariant != variant {
			return specv1.Descriptor{}, fmt.Errorf("manifest descriptor variant mismatch: has %s, but %s was requested", manifestVariant, variant)
		}
	}

	// Read the manifest from blobs
	manifestBlobPath := filepath.Join(srcDir, "blobs", manifestDesc.Digest.Algorithm().String(), manifestDesc.Digest.Hex())
	manifestData, err := os.ReadFile(manifestBlobPath)
	if err != nil {
		return specv1.Descriptor{}, fmt.Errorf("reading manifest blob: %w", err)
	}

	var manifest specv1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return specv1.Descriptor{}, fmt.Errorf("unmarshaling manifest: %w", err)
	}

	// Read the config from blobs
	configBlobPath := filepath.Join(srcDir, "blobs", manifest.Config.Digest.Algorithm().String(), manifest.Config.Digest.Hex())
	configData, err := os.ReadFile(configBlobPath)
	if err != nil {
		return specv1.Descriptor{}, fmt.Errorf("reading config blob: %w", err)
	}

	var config specv1.Image
	if err := json.Unmarshal(configData, &config); err != nil {
		return specv1.Descriptor{}, fmt.Errorf("unmarshaling config: %w", err)
	}

	// Validate that the config matches the requested platform
	if config.OS != operatingSystem {
		return specv1.Descriptor{}, fmt.Errorf("OS mismatch: config has %s, but %s was requested", config.OS, operatingSystem)
	}
	if config.Architecture != arch {
		return specv1.Descriptor{}, fmt.Errorf("architecture mismatch: config has %s, but %s was requested", config.Architecture, arch)
	}

	// Check layer count
	layerCount := layerMediaTypes.GetLayerCount(manifestIdx)
	if len(manifest.Layers) != layerCount {
		return specv1.Descriptor{}, fmt.Errorf("layer count mismatch: OCI layout has %d layers, but %d layer media types specified", len(manifest.Layers), layerCount)
	}

	// Copy/hardlink each layer blob and create metadata JSONs
	for i := range manifest.Layers {
		targetMediaType, ok := layerMediaTypes.Get(manifestIdx, i)
		if !ok {
			return specv1.Descriptor{}, fmt.Errorf("missing layer media type for manifest %d, layer %d", manifestIdx, i)
		}

		layerBlobPath, ok := layerBlobs.Get(manifestIdx, i)
		if !ok {
			return specv1.Descriptor{}, fmt.Errorf("missing layer blob path for manifest %d, layer %d", manifestIdx, i)
		}

		layerMetadataPath, ok := layerMetadataJSON.Get(manifestIdx, i)
		if !ok {
			return specv1.Descriptor{}, fmt.Errorf("missing layer metadata path for manifest %d, layer %d", manifestIdx, i)
		}

		// Verify media type matches
		if manifest.Layers[i].MediaType != targetMediaType {
			return specv1.Descriptor{}, fmt.Errorf("layer %d media type mismatch: OCI layout has %s, but %s was requested", i, manifest.Layers[i].MediaType, targetMediaType)
		}

		// Copy the layer blob
		sourceLayerPath := filepath.Join(srcDir, "blobs", manifest.Layers[i].Digest.Algorithm().String(), manifest.Layers[i].Digest.Hex())
		if err := copyOrHardlink(sourceLayerPath, layerBlobPath); err != nil {
			return specv1.Descriptor{}, fmt.Errorf("copying layer %d blob: %w", i, err)
		}

		// Create the metadata JSON from the existing descriptor
		// Use digest as the name for anonymous layers from OCI layout
		layerName := manifest.Layers[i].Digest.String()

		metadataFile, err := os.Create(layerMetadataPath)
		if err != nil {
			return specv1.Descriptor{}, fmt.Errorf("creating metadata file for layer %d: %w", i, err)
		}
		defer metadataFile.Close()

		if err := metadata.WriteLayerMetadata(
			layerName,
			config.RootFS.DiffIDs[i].String(),
			manifest.Layers[i].MediaType,
			manifest.Layers[i].Digest.String(),
			manifest.Layers[i].Size,
			manifest.Layers[i].Annotations,
			metadataFile,
		); err != nil {
			return specv1.Descriptor{}, fmt.Errorf("writing metadata for layer %d: %w", i, err)
		}
	}

	// Copy manifest blob to output
	if err := copyOrHardlink(manifestBlobPath, manifestOutput); err != nil {
		return specv1.Descriptor{}, fmt.Errorf("copying manifest: %w", err)
	}

	// Copy config blob to output
	if err := copyOrHardlink(configBlobPath, configOutput); err != nil {
		return specv1.Descriptor{}, fmt.Errorf("copying config: %w", err)
	}

	// Create the descriptor
	descriptor := specv1.Descriptor{
		MediaType: manifestDesc.MediaType,
		Digest:    manifestDesc.Digest,
		Size:      manifestDesc.Size,
		Platform: &specv1.Platform{
			Architecture: arch,
			OS:           operatingSystem,
			Variant:      variant,
		},
	}
	descriptorJSON, err := json.Marshal(descriptor)
	if err != nil {
		return specv1.Descriptor{}, fmt.Errorf("marshaling descriptor: %w", err)
	}

	if err := os.WriteFile(descriptorOutput, descriptorJSON, 0o644); err != nil {
		return specv1.Descriptor{}, fmt.Errorf("writing descriptor: %w", err)
	}

	return descriptor, nil
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
