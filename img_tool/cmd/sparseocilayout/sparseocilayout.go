package sparseocilayout

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	v1 "github.com/malt3/go-containerregistry/pkg/v1"

	"github.com/bazel-contrib/rules_img/img_tool/cmd/ocilayout"
)

const SparseOCILayoutVersion = "1.0.0"

func SparseOCILayoutProcess(ctx context.Context, args []string) {
	var manifestPath string
	var indexPath string
	var outputDir string
	var configPath string
	var layerFlags layerMappingFlag
	var manifestPaths stringSliceFlag
	var configPaths stringSliceFlag
	var format string

	flagSet := flag.NewFlagSet("sparse-oci-layout", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Assembles a sparse OCI layout directory from manifest/index and layers.\n\n")
		fmt.Fprintf(flagSet.Output(), "Unlike oci-layout, layer blobs are NOT included. Instead, layer descriptor\n")
		fmt.Fprintf(flagSet.Output(), "metadata files are written for each layer.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img sparse-oci-layout [OPTIONS]\n")
		flagSet.PrintDefaults()
	}

	flagSet.StringVar(&manifestPath, "manifest", "", "Path to the image manifest (for single manifest)")
	flagSet.StringVar(&indexPath, "index", "", "Path to the image index (for multi-platform)")
	flagSet.StringVar(&configPath, "config", "", "Path to the image config (for single manifest)")
	flagSet.StringVar(&outputDir, "output", "", "Output path for sparse OCI layout (required)")
	flagSet.StringVar(&format, "format", "directory", "Output format: 'directory' or 'tar'")
	flagSet.Var(&layerFlags, "layer", "Layer metadata path (can be specified multiple times)")
	flagSet.Var(&manifestPaths, "manifest-path", "Path to manifest file (for index, can be specified multiple times)")
	flagSet.Var(&configPaths, "config-path", "Path to config file (for index, can be specified multiple times)")

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}

	if outputDir == "" {
		fmt.Fprintf(os.Stderr, "Error: --output is required\n")
		flagSet.Usage()
		os.Exit(1)
	}

	if format != "directory" && format != "tar" {
		fmt.Fprintf(os.Stderr, "Error: --format must be 'directory' or 'tar', got '%s'\n", format)
		flagSet.Usage()
		os.Exit(1)
	}

	var err error
	if indexPath != "" {
		if manifestPath != "" || configPath != "" {
			fmt.Fprintf(os.Stderr, "Error: cannot use --manifest or --config with --index\n")
			os.Exit(1)
		}
		if len(manifestPaths) != len(configPaths) {
			fmt.Fprintf(os.Stderr, "Error: number of --manifest-path must match --config-path\n")
			os.Exit(1)
		}
		if len(manifestPaths) == 0 {
			fmt.Fprintf(os.Stderr, "Error: --index requires at least one --manifest-path and --config-path\n")
			os.Exit(1)
		}
		err = assembleSparseLayoutWithIndex(indexPath, outputDir, format, manifestPaths, configPaths, layerFlags)
	} else {
		if manifestPath == "" {
			fmt.Fprintf(os.Stderr, "Error: either --manifest or --index is required\n")
			flagSet.Usage()
			os.Exit(1)
		}
		if configPath == "" {
			fmt.Fprintf(os.Stderr, "Error: --config is required when using --manifest\n")
			flagSet.Usage()
			os.Exit(1)
		}
		if len(manifestPaths) > 0 || len(configPaths) > 0 {
			fmt.Fprintf(os.Stderr, "Error: cannot use --manifest-path or --config-path without --index\n")
			os.Exit(1)
		}
		err = assembleSparseLayout(manifestPath, configPath, outputDir, format, layerFlags)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

type blobMap = map[string]string

func createSink(outputPath, format string) (ocilayout.OCILayoutSink, error) {
	switch format {
	case "directory":
		return ocilayout.NewDirectorySink(outputPath), nil
	case "tar":
		return ocilayout.NewTarSink(outputPath)
	default:
		return nil, fmt.Errorf("unsupported format: %s", format)
	}
}

func setupSparseLayout(sink ocilayout.OCILayoutSink) error {
	if err := sink.CreateDir("blobs"); err != nil {
		return fmt.Errorf("creating blobs directory: %w", err)
	}
	if err := sink.CreateDir(filepath.Join("blobs", "sha256")); err != nil {
		return fmt.Errorf("creating blobs/sha256 directory: %w", err)
	}

	layoutMarker := map[string]string{
		"imageLayoutVersion": SparseOCILayoutVersion,
	}
	return writeJSON(sink, "sparse-oci-layout", layoutMarker)
}

func hashBytes(data []byte) v1.Hash {
	h, _, _ := v1.SHA256(bytes.NewReader(data))
	return h
}

func writeJSON(sink ocilayout.OCILayoutSink, path string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", path, err)
	}
	return sink.WriteFile(path, data, 0o644)
}

func copyBlobs(sink ocilayout.OCILayoutSink, blobs blobMap) error {
	blobKeys := make([]string, 0, len(blobs))
	for k := range blobs {
		blobKeys = append(blobKeys, k)
	}
	slices.Sort(blobKeys)
	for _, digest := range blobKeys {
		srcPath := blobs[digest]
		dstPath := filepath.Join("blobs", "sha256", digest)
		if err := sink.CopyFile(dstPath, srcPath, false); err != nil {
			return fmt.Errorf("copying blob %s: %w", digest, err)
		}
	}
	return nil
}

type layerDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

func writeLayerDescriptors(sink ocilayout.OCILayoutSink, manifest v1.Manifest, layerMetadataByDigest map[string]layerDescriptor) error {
	for _, layerDesc := range manifest.Layers {
		desc := layerDescriptor{
			MediaType:   string(layerDesc.MediaType),
			Digest:      layerDesc.Digest.String(),
			Size:        layerDesc.Size,
			Annotations: layerDesc.Annotations,
		}
		if meta, ok := layerMetadataByDigest[layerDesc.Digest.Hex]; ok {
			desc = meta
		}
		descPath := filepath.Join("blobs", "sha256", layerDesc.Digest.Hex+".descriptor.json")
		if err := writeJSON(sink, descPath, desc); err != nil {
			return fmt.Errorf("writing layer descriptor for %s: %w", layerDesc.Digest.Hex, err)
		}
	}
	return nil
}

func readLayerMetadata(path string) (layerDescriptor, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return layerDescriptor{}, fmt.Errorf("reading layer metadata %s: %w", path, err)
	}
	var meta struct {
		MediaType   string            `json:"mediaType"`
		Digest      string            `json:"digest"`
		Size        int64             `json:"size"`
		Annotations map[string]string `json:"annotations,omitempty"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return layerDescriptor{}, fmt.Errorf("unmarshaling layer metadata %s: %w", path, err)
	}
	return layerDescriptor{
		MediaType:   meta.MediaType,
		Digest:      meta.Digest,
		Size:        meta.Size,
		Annotations: meta.Annotations,
	}, nil
}

func buildLayerMetadataMap(layers []string) (map[string]layerDescriptor, error) {
	result := make(map[string]layerDescriptor)
	for _, path := range layers {
		meta, err := readLayerMetadata(path)
		if err != nil {
			return nil, err
		}
		hex := strings.TrimPrefix(meta.Digest, "sha256:")
		result[hex] = meta
	}
	return result, nil
}

func assembleSparseLayout(manifestPath, configPath, outputPath, format string, layers layerMappingFlag) error {
	sink, err := createSink(outputPath, format)
	if err != nil {
		return err
	}
	defer sink.Close()

	if err := setupSparseLayout(sink); err != nil {
		return err
	}

	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}

	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("unmarshaling manifest: %w", err)
	}

	layerMetadataByDigest, err := buildLayerMetadataMap(layers)
	if err != nil {
		return err
	}

	blobs := make(blobMap)

	// Add config blob
	blobs[manifest.Config.Digest.Hex] = configPath

	// Add manifest blob
	manifestDigest := hashBytes(manifestData)
	blobs[manifestDigest.Hex] = manifestPath

	// Write root descriptor
	rootDesc := v1.Descriptor{
		MediaType: manifest.MediaType,
		Digest:    manifestDigest,
		Size:      int64(len(manifestData)),
	}
	if err := writeJSON(sink, "root.descriptor.json", rootDesc); err != nil {
		return fmt.Errorf("writing root.descriptor.json: %w", err)
	}

	// Copy non-layer blobs (manifest + config)
	if err := copyBlobs(sink, blobs); err != nil {
		return fmt.Errorf("copying blobs: %w", err)
	}

	// Write layer descriptor files
	if err := writeLayerDescriptors(sink, manifest, layerMetadataByDigest); err != nil {
		return err
	}

	return nil
}

func assembleSparseLayoutWithIndex(indexPath, outputPath, format string, manifestPaths, configPaths []string, layers layerMappingFlag) error {
	sink, err := createSink(outputPath, format)
	if err != nil {
		return err
	}
	defer sink.Close()

	if err := setupSparseLayout(sink); err != nil {
		return err
	}

	indexData, err := os.ReadFile(indexPath)
	if err != nil {
		return fmt.Errorf("reading index: %w", err)
	}

	var idx v1.IndexManifest
	if err := json.Unmarshal(indexData, &idx); err != nil {
		return fmt.Errorf("unmarshaling index: %w", err)
	}

	layerMetadataByDigest, err := buildLayerMetadataMap(layers)
	if err != nil {
		return err
	}

	blobs := make(blobMap)

	// Add index blob
	indexDigest := hashBytes(indexData)
	blobs[indexDigest.Hex] = indexPath

	// Write root descriptor pointing to the index
	rootDesc := v1.Descriptor{
		MediaType: idx.MediaType,
		Digest:    indexDigest,
		Size:      int64(len(indexData)),
	}
	if err := writeJSON(sink, "root.descriptor.json", rootDesc); err != nil {
		return fmt.Errorf("writing root.descriptor.json: %w", err)
	}

	for i := range manifestPaths {
		manifestData, err := os.ReadFile(manifestPaths[i])
		if err != nil {
			return fmt.Errorf("reading manifest %d: %w", i, err)
		}

		var manifest v1.Manifest
		if err := json.Unmarshal(manifestData, &manifest); err != nil {
			return fmt.Errorf("unmarshaling manifest %d: %w", i, err)
		}

		// Add manifest blob
		manifestDigest := hashBytes(manifestData)
		blobs[manifestDigest.Hex] = manifestPaths[i]

		// Add config blob
		blobs[manifest.Config.Digest.Hex] = configPaths[i]

		// Write layer descriptor files for this manifest
		if err := writeLayerDescriptors(sink, manifest, layerMetadataByDigest); err != nil {
			return err
		}
	}

	// Copy all non-layer blobs
	if err := copyBlobs(sink, blobs); err != nil {
		return err
	}

	return nil
}
