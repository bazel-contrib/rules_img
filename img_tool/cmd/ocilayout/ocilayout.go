package ocilayout

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	v1 "github.com/malt3/go-containerregistry/pkg/v1"
)

const OCILayoutVersion = "1.0.0"

type blobMap map[string]string // digest -> source path

// MissingBlobsError represents an error when one or more blobs are missing
type MissingBlobsError struct {
	MissingBlobs []string
}

func (e *MissingBlobsError) Error() string {
	if os.Getenv("RULES_IMG") == "1" {
		// invoked by rules_img
		return fmt.Sprintf(
			`Missing layer blobs %s
"oci_layout" output group requested with shallow base image. You probably want to add the "layer_handling" attribute to the pull rule of your base image (choose "lazy" or "eager", but NOT "shallow").
If you explicitly want to opt in to OCI image layouts with missing blobs, use the "--@rules_img//img/settings:shallow_oci_layout=i_know_what_i_am_doing" flag.
`,
			strings.Join(e.MissingBlobs, ", "),
		)
	}
	return fmt.Sprintf("missing blobs: %s", strings.Join(e.MissingBlobs, ", "))
}

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

	// Validate required flags
	if outputDir == "" {
		fmt.Fprintf(os.Stderr, "Error: --output is required\n")
		flagSet.Usage()
		os.Exit(1)
	}

	// Validate format parameter
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
		err = assembleOCILayoutWithIndex(indexPath, outputDir, format, manifestPaths, configPaths, layerFlags, useSymlinks, allowMissingBlobs)
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
		err = assembleOCILayout(manifestPath, configPath, outputDir, format, layerFlags, useSymlinks, allowMissingBlobs)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// createSink creates the appropriate sink based on the format
func createSink(outputPath, format string) (OCILayoutSink, error) {
	switch format {
	case "directory":
		return NewDirectorySink(outputPath), nil
	case "tar":
		return NewTarSink(outputPath)
	default:
		return nil, fmt.Errorf("unsupported format: %s", format)
	}
}

func assembleOCILayout(manifestPath, configPath, outputPath, format string, layers layerMappingFlag, useSymlinks, allowMissingBlobs bool) error {
	sink, err := createSink(outputPath, format)
	if err != nil {
		return err
	}
	defer sink.Close()

	if err := setupOCILayoutWithSink(sink); err != nil {
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

	// Build a map of available layers by their digest
	layerBlobsByDigest := make(map[string]string)
	for _, layer := range layers {
		metadataData, err := os.ReadFile(layer.metadata)
		if err != nil {
			return fmt.Errorf("reading layer metadata %s: %w", layer.metadata, err)
		}

		var metadata struct {
			Digest string `json:"digest"`
		}
		if err := json.Unmarshal(metadataData, &metadata); err != nil {
			return fmt.Errorf("unmarshaling layer metadata %s: %w", layer.metadata, err)
		}

		// Extract hex digest from sha256:xxxx format
		digest := strings.TrimPrefix(metadata.Digest, "sha256:")
		layerBlobsByDigest[digest] = layer.blob
	}

	blobs := make(blobMap)
	blobs[manifest.Config.Digest.Hex] = configPath

	// Check for missing blobs
	var missingBlobs []string
	for _, layerDesc := range manifest.Layers {
		if blobPath, ok := layerBlobsByDigest[layerDesc.Digest.Hex]; ok {
			blobs[layerDesc.Digest.Hex] = blobPath
		} else if !allowMissingBlobs {
			missingBlobs = append(missingBlobs, layerDesc.Digest.String())
		}
	}

	if len(missingBlobs) > 0 {
		return &MissingBlobsError{MissingBlobs: missingBlobs}
	}

	// Copy manifest to blobs directory
	manifestDigest := hashBytes(manifestData)
	blobs[manifestDigest.Hex] = manifestPath

	index := v1.IndexManifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests: []v1.Descriptor{
			{
				MediaType: manifest.MediaType,
				Digest:    manifestDigest,
				Size:      int64(len(manifestData)),
			},
		},
	}

	if err := writeJSONWithSink(sink, "index.json", index); err != nil {
		return fmt.Errorf("writing index.json: %w", err)
	}

	if err := copyBlobsWithSink(sink, blobs, useSymlinks); err != nil {
		return fmt.Errorf("copying blobs: %w", err)
	}

	return nil
}

func copyFile(src, dst string, useSymlinks bool) error {
	if useSymlinks {
		absSrc, err := filepath.Abs(src)
		if err != nil {
			return err
		}
		return os.Symlink(absSrc, dst)
	}

	if err := os.Link(src, dst); err == nil {
		return nil
	}

	if err := tryReflink(src, dst); err == nil {
		return nil
	}

	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	return err
}

func hashBytes(data []byte) v1.Hash {
	h, _, _ := v1.SHA256(bytes.NewReader(data))
	return h
}

func assembleOCILayoutWithIndex(indexPath, outputPath, format string, manifestPaths, configPaths []string, layers layerMappingFlag, useSymlinks, allowMissingBlobs bool) error {
	sink, err := createSink(outputPath, format)
	if err != nil {
		return err
	}
	defer sink.Close()

	if err := setupOCILayoutWithSink(sink); err != nil {
		return err
	}

	// Copy the index file unmodified
	if err := sink.CopyFile("index.json", indexPath, false); err != nil {
		return fmt.Errorf("copying index file: %w", err)
	}

	// Build a map of available layers by their digest
	layerBlobsByDigest := make(map[string]string)
	for _, layer := range layers {
		metadataData, err := os.ReadFile(layer.metadata)
		if err != nil {
			return fmt.Errorf("reading layer metadata %s: %w", layer.metadata, err)
		}

		var metadata struct {
			Digest string `json:"digest"`
		}
		if err := json.Unmarshal(metadataData, &metadata); err != nil {
			return fmt.Errorf("unmarshaling layer metadata %s: %w", layer.metadata, err)
		}

		// Extract hex digest from sha256:xxxx format
		digest := strings.TrimPrefix(metadata.Digest, "sha256:")
		layerBlobsByDigest[digest] = layer.blob
	}

	blobs := make(blobMap)
	var allMissingBlobs []string

	for i := range manifestPaths {
		manifestData, err := os.ReadFile(manifestPaths[i])
		if err != nil {
			return fmt.Errorf("reading manifest %d: %w", i, err)
		}

		var manifest v1.Manifest
		if err := json.Unmarshal(manifestData, &manifest); err != nil {
			return fmt.Errorf("unmarshaling manifest %d: %w", i, err)
		}

		// Add manifest to blobs
		manifestDigest := hashBytes(manifestData)
		blobs[manifestDigest.Hex] = manifestPaths[i]

		// Add config to blobs
		blobs[manifest.Config.Digest.Hex] = configPaths[i]

		// Check for missing blobs in this manifest
		for _, layerDesc := range manifest.Layers {
			if blobPath, ok := layerBlobsByDigest[layerDesc.Digest.Hex]; ok {
				blobs[layerDesc.Digest.Hex] = blobPath
			} else if !allowMissingBlobs {
				allMissingBlobs = append(allMissingBlobs, layerDesc.Digest.String())
			}
		}
	}

	if len(allMissingBlobs) > 0 {
		return &MissingBlobsError{MissingBlobs: allMissingBlobs}
	}

	if err := copyBlobsWithSink(sink, blobs, useSymlinks); err != nil {
		return err
	}

	return nil
}

func setupOCILayoutWithSink(sink OCILayoutSink) error {
	if err := sink.CreateDir("blobs"); err != nil {
		return fmt.Errorf("creating blobs directory: %w", err)
	}
	if err := sink.CreateDir(filepath.Join("blobs", "sha256")); err != nil {
		return fmt.Errorf("creating blobs/sha256 directory: %w", err)
	}

	ociLayout := map[string]string{
		"imageLayoutVersion": OCILayoutVersion,
	}
	return writeJSONWithSink(sink, "oci-layout", ociLayout)
}

func writeJSONWithSink(sink OCILayoutSink, path string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", path, err)
	}
	return sink.WriteFile(path, data, 0o644)
}

func copyBlobsWithSink(sink OCILayoutSink, blobs blobMap, useSymlinks bool) error {
	blobKeys := make([]string, 0, len(blobs))
	for k := range blobs {
		blobKeys = append(blobKeys, k)
	}
	slices.Sort(blobKeys)
	for _, digest := range blobKeys {
		srcPath := blobs[digest]
		dstPath := filepath.Join("blobs", "sha256", digest)
		if err := sink.CopyFile(dstPath, srcPath, useSymlinks); err != nil {
			return fmt.Errorf("copying blob %s: %w", digest, err)
		}
	}
	return nil
}
