package ocilayoutcmd

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/ocilayout"
)

func OCILayoutProcess(ctx context.Context, args []string) {
	var manifestPath string
	var indexPath string
	var outputDir string
	var configPath string
	var layerFlags layerMappingFlag
	var manifestPaths stringSliceFlag
	var configPaths stringSliceFlag
	var useSymlinks bool
	var allowMissingBlobs bool
	var format string

	flagSet := flag.NewFlagSet("oci-layout", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Assembles an OCI layout directory from manifest/index and layers.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img oci-layout [OPTIONS]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"img oci-layout --manifest manifest.json --config config.json --layer layer1_meta.json=layer1.tar.gz --output oci-layout",
			"img oci-layout --index index.json --manifest-path m1.json --config-path c1.json --layer l1_meta.json=l1.tar.gz --output oci-layout",
		}
		fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
	}

	flagSet.StringVar(&manifestPath, "manifest", "", "Path to the image manifest (for single manifest)")
	flagSet.StringVar(&indexPath, "index", "", "Path to the image index (for multi-platform)")
	flagSet.StringVar(&configPath, "config", "", "Path to the image config (for single manifest)")
	flagSet.StringVar(&outputDir, "output", "", "Output path for OCI layout (required)")
	flagSet.StringVar(&format, "format", "directory", "Output format: 'directory' or 'tar'")
	flagSet.Var(&layerFlags, "layer", "Layer mapping in format metadata=blob (can be specified multiple times)")
	flagSet.Var(&manifestPaths, "manifest-path", "Path to manifest file (for index, can be specified multiple times)")
	flagSet.Var(&configPaths, "config-path", "Path to config file (for index, can be specified multiple times)")
	flagSet.BoolVar(&useSymlinks, "symlink", false, "Use symlinks instead of copying files")
	flagSet.BoolVar(&allowMissingBlobs, "allow-missing-blobs", false, "Allow missing blobs instead of failing the build")

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
		err = assembleOCILayoutWithIndex(ctx, indexPath, outputDir, format, manifestPaths, configPaths, layerFlags, useSymlinks, allowMissingBlobs)
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
		err = assembleOCILayout(ctx, manifestPath, configPath, outputDir, format, layerFlags, useSymlinks, allowMissingBlobs)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// layerPathsByDigest reads the --layer metadata=blob mappings into a digest->path map.
func layerPathsByDigest(layers layerMappingFlag) (map[string]string, error) {
	result := make(map[string]string)
	for _, layer := range layers {
		meta, err := ocilayout.ReadLayerMetadata(layer.metadata)
		if err != nil {
			return nil, err
		}
		result[meta.HexDigest()] = layer.blob
	}
	return result, nil
}

func manifestInput(manifest *v1.Manifest, manifestData []byte, configPath string, layerPaths map[string]string) ocilayout.ManifestInput {
	mi := ocilayout.ManifestInput{
		Manifest:     manifest,
		ManifestData: manifestData,
		Config:       ocilayout.BlobFromPath(configPath),
	}
	for _, ld := range manifest.Layers {
		path, ok := layerPaths[ld.Digest.Hex]
		mi.Layers = append(mi.Layers, ocilayout.LayerInput{
			Descriptor: ld,
			Blob:       ocilayout.BlobFromPath(path),
			Present:    ok,
		})
	}
	return mi
}

func writeLayout(ctx context.Context, b *ocilayout.Builder, format, outputPath string) error {
	if format == "tar" {
		return b.WriteTar(ctx, outputPath)
	}
	return b.WriteDir(ctx, outputPath)
}

func assembleOCILayout(ctx context.Context, manifestPath, configPath, outputPath, format string, layers layerMappingFlag, useSymlinks, allowMissingBlobs bool) error {
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("unmarshaling manifest: %w", err)
	}

	layerPaths, err := layerPathsByDigest(layers)
	if err != nil {
		return err
	}

	b := ocilayout.New(ocilayout.OCILayout()).
		WithMissingBlobsHint(ocilayout.OutputGroupOCILayout).
		WithLinkStrategy(useSymlinks, false)
	if allowMissingBlobs {
		b.AllowMissingBlobs()
	}
	b.AddManifest(manifestInput(&manifest, manifestData, configPath, layerPaths))
	return writeLayout(ctx, b, format, outputPath)
}

func assembleOCILayoutWithIndex(ctx context.Context, indexPath, outputPath, format string, manifestPaths, configPaths []string, layers layerMappingFlag, useSymlinks, allowMissingBlobs bool) error {
	indexData, err := os.ReadFile(indexPath)
	if err != nil {
		return fmt.Errorf("reading index: %w", err)
	}

	layerPaths, err := layerPathsByDigest(layers)
	if err != nil {
		return err
	}

	b := ocilayout.New(ocilayout.OCILayoutFromIndex()).
		WithMissingBlobsHint(ocilayout.OutputGroupOCILayout).
		WithLinkStrategy(useSymlinks, false).
		SetRootIndex(ocilayout.BlobFromBytes(indexData))
	if allowMissingBlobs {
		b.AllowMissingBlobs()
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
		b.AddManifest(manifestInput(&manifest, manifestData, configPaths[i], layerPaths))
	}

	return writeLayout(ctx, b, format, outputPath)
}
