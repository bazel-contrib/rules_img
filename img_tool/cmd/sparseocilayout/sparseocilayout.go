package sparseocilayout

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/ocilayout"
)

func SparseOCILayoutProcess(ctx context.Context, args []string) {
	var manifestPath string
	var indexPath string
	var outputDir string
	var configPath string
	var layerFlags layerMappingFlag
	var layerCompactStreamFlags layerMappingFlag
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
	flagSet.Var(&layerCompactStreamFlags, "layer-compact-stream", "Layer compact stream as <metadata_path>=<cstream_path> (can be specified multiple times)")
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
		err = assembleSparseLayoutWithIndex(ctx, indexPath, outputDir, format, manifestPaths, configPaths, layerFlags, layerCompactStreamFlags)
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
		err = assembleSparseLayout(ctx, manifestPath, configPath, outputDir, format, layerFlags, layerCompactStreamFlags)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// buildLayerMetadataMap reads each --layer metadata file into a map from digest
// hex to a sparse layer descriptor override.
func buildLayerMetadataMap(layers []string) (map[string]ocilayout.SparseLayerDescriptor, error) {
	result := make(map[string]ocilayout.SparseLayerDescriptor)
	for _, path := range layers {
		meta, err := ocilayout.ReadLayerMetadata(path)
		if err != nil {
			return nil, err
		}
		result[meta.HexDigest()] = ocilayout.SparseLayerDescriptor{
			MediaType:   meta.MediaType,
			Digest:      meta.Digest,
			Size:        meta.Size,
			Annotations: meta.Annotations,
		}
	}
	return result, nil
}

// buildLayerCompactStreamMap parses --layer-compact-stream flags of the form
// <metadata_path>=<cstream_path> into a map from digest hex to cstream path.
func buildLayerCompactStreamMap(entries []string) (map[string]string, error) {
	result := make(map[string]string)
	for _, entry := range entries {
		sep := strings.Index(entry, "=")
		if sep < 0 {
			return nil, fmt.Errorf("invalid --layer-compact-stream value %q: expected <metadata_path>=<cstream_path>", entry)
		}
		metadataPath := entry[:sep]
		compactStreamPath := entry[sep+1:]
		meta, err := ocilayout.ReadLayerMetadata(metadataPath)
		if err != nil {
			return nil, fmt.Errorf("reading metadata for --layer-compact-stream %q: %w", entry, err)
		}
		result[meta.HexDigest()] = compactStreamPath
	}
	return result, nil
}

func sparseManifestInput(manifest *v1.Manifest, manifestData []byte, configPath string, metaByDigest map[string]ocilayout.SparseLayerDescriptor, cstreamByDigest map[string]string) ocilayout.ManifestInput {
	mi := ocilayout.ManifestInput{
		Manifest:     manifest,
		ManifestData: manifestData,
		Config:       ocilayout.BlobFromPath(configPath),
	}
	for _, ld := range manifest.Layers {
		li := ocilayout.LayerInput{Descriptor: ld}
		if meta, ok := metaByDigest[ld.Digest.Hex]; ok {
			m := meta
			li.SparseMeta = &m
		}
		if csPath, ok := cstreamByDigest[ld.Digest.Hex]; ok {
			cs := ocilayout.BlobFromPath(csPath)
			li.CompactStream = &cs
		}
		mi.Layers = append(mi.Layers, li)
	}
	return mi
}

func writeLayout(ctx context.Context, b *ocilayout.Builder, format, outputPath string) error {
	if format == "tar" {
		return b.WriteTar(ctx, outputPath)
	}
	return b.WriteDir(ctx, outputPath)
}

func assembleSparseLayout(ctx context.Context, manifestPath, configPath, outputPath, format string, layers, layerCompactStreamFlags layerMappingFlag) error {
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("unmarshaling manifest: %w", err)
	}

	metaByDigest, err := buildLayerMetadataMap(layers)
	if err != nil {
		return err
	}
	cstreamByDigest, err := buildLayerCompactStreamMap(layerCompactStreamFlags)
	if err != nil {
		return err
	}

	b := ocilayout.New(ocilayout.SparseOCILayout()).
		AddManifest(sparseManifestInput(&manifest, manifestData, configPath, metaByDigest, cstreamByDigest))
	return writeLayout(ctx, b, format, outputPath)
}

func assembleSparseLayoutWithIndex(ctx context.Context, indexPath, outputPath, format string, manifestPaths, configPaths []string, layers, layerCompactStreamFlags layerMappingFlag) error {
	indexData, err := os.ReadFile(indexPath)
	if err != nil {
		return fmt.Errorf("reading index: %w", err)
	}

	metaByDigest, err := buildLayerMetadataMap(layers)
	if err != nil {
		return err
	}
	cstreamByDigest, err := buildLayerCompactStreamMap(layerCompactStreamFlags)
	if err != nil {
		return err
	}

	b := ocilayout.New(ocilayout.SparseOCILayout()).
		SetRootIndex(ocilayout.BlobFromBytes(indexData))

	for i := range manifestPaths {
		manifestData, err := os.ReadFile(manifestPaths[i])
		if err != nil {
			return fmt.Errorf("reading manifest %d: %w", i, err)
		}
		var manifest v1.Manifest
		if err := json.Unmarshal(manifestData, &manifest); err != nil {
			return fmt.Errorf("unmarshaling manifest %d: %w", i, err)
		}
		b.AddManifest(sparseManifestInput(&manifest, manifestData, configPaths[i], metaByDigest, cstreamByDigest))
	}

	return writeLayout(ctx, b, format, outputPath)
}
