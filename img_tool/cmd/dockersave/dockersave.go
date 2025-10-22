package dockersave

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	v1 "github.com/malt3/go-containerregistry/pkg/v1"
)

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
"tarball" output group requested with shallow base image. You probably want to add the "layer_handling" attribute to the pull rule of your base image (choose "lazy" or "eager", but NOT "shallow").
If you explicitly want to opt in to Docker save tarballs with missing blobs, use the "--@rules_img//img/settings:shallow_oci_layout=i_know_what_i_am_doing" flag.
`,
			strings.Join(e.MissingBlobs, ", "),
		)
	}
	return fmt.Sprintf("missing blobs: %s", strings.Join(e.MissingBlobs, ", "))
}

// DockerManifest represents the Docker save manifest format
type DockerManifest struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

// readTagFromConfigFile reads the tag field from a configuration file
func readTagFromConfigFile(configPath string) (string, error) {
	if configPath == "" {
		return "", nil
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", fmt.Errorf("reading configuration file: %w", err)
	}

	var config map[string]interface{}
	if err := json.Unmarshal(data, &config); err != nil {
		return "", fmt.Errorf("parsing configuration file: %w", err)
	}

	tag, ok := config["tag"].(string)
	if !ok {
		return "", nil // tag field not present or not a string
	}

	return tag, nil
}

func DockerSaveProcess(ctx context.Context, args []string) {
	var manifestPath string
	var configPath string
	var outputPath string
	var format string
	var layerFlags layerMappingFlag
	var repoTags stringSliceFlag
	var useSymlinks bool
	var allowMissingBlobs bool
	var configurationFilePath string

	flagSet := flag.NewFlagSet("docker-save", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Assembles a Docker save compatible directory or tarball from manifest and layers.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img docker-save [OPTIONS]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"img docker-save --manifest manifest.json --config config.json --layer layer1_meta.json=layer1.tar.gz --repo-tag my/image:latest --output docker-save.tar",
			"img docker-save --manifest manifest.json --config config.json --layer layer1_meta.json=layer1.tar.gz --repo-tag my/image:latest --repo-tag my/image:v1.0 --format directory --output docker-save",
			"img docker-save --manifest manifest.json --config config.json --layer layer1_meta.json=layer1.tar.gz --configuration-file config.json --output docker-save.tar",
		}
		fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
	}

	flagSet.StringVar(&manifestPath, "manifest", "", "Path to the image manifest (required)")
	flagSet.StringVar(&configPath, "config", "", "Path to the image config (required)")
	flagSet.StringVar(&outputPath, "output", "", "Output path for Docker save format (required). Use '-' for stdout")
	flagSet.StringVar(&format, "format", "tar", "Output format: 'directory' or 'tar'")
	flagSet.Var(&layerFlags, "layer", "Layer mapping in format metadata=blob (can be specified multiple times)")
	flagSet.Var(&repoTags, "repo-tag", "Repository tag for the image (can be specified multiple times)")
	flagSet.BoolVar(&useSymlinks, "symlink", false, "Use symlinks instead of copying files")
	flagSet.BoolVar(&allowMissingBlobs, "allow-missing-blobs", false, "Allow missing blobs instead of failing the build")
	flagSet.StringVar(&configurationFilePath, "configuration-file", "", "Path to configuration file containing tag information (optional)")

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}

	// Validate required flags
	if manifestPath == "" {
		fmt.Fprintf(os.Stderr, "Error: --manifest is required\n")
		flagSet.Usage()
		os.Exit(1)
	}
	if configPath == "" {
		fmt.Fprintf(os.Stderr, "Error: --config is required\n")
		flagSet.Usage()
		os.Exit(1)
	}
	if outputPath == "" {
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

	// Read tag from configuration file if provided and no --repo-tag was specified
	if len(repoTags) == 0 && configurationFilePath != "" {
		configTag, err := readTagFromConfigFile(configurationFilePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading configuration file: %v\n", err)
			os.Exit(1)
		}
		if configTag != "" {
			repoTags = []string{configTag}
		}
	}

	// Default repo tag if none provided from either flags or config
	if len(repoTags) == 0 {
		repoTags = []string{"image:latest"}
	}

	err := assembleDockerSave(manifestPath, configPath, outputPath, format, layerFlags, repoTags, useSymlinks, allowMissingBlobs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// createSink creates the appropriate sink based on the format
func createSink(outputPath, format string) (DockerSaveSink, error) {
	switch format {
	case "directory":
		return NewDirectorySink(outputPath), nil
	case "tar":
		return NewTarSink(outputPath)
	default:
		return nil, fmt.Errorf("unsupported format: %s", format)
	}
}

func assembleDockerSave(manifestPath, configPath, outputPath, format string, layers layerMappingFlag, repoTags []string, useSymlinks, allowMissingBlobs bool) error {
	sink, err := createSink(outputPath, format)
	if err != nil {
		return err
	}
	defer sink.Close()

	// Read and parse the manifest
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

	// Create blobs directory
	if err := sink.CreateDir("blobs"); err != nil {
		return fmt.Errorf("creating blobs directory: %w", err)
	}
	if err := sink.CreateDir(filepath.Join("blobs", "sha256")); err != nil {
		return fmt.Errorf("creating blobs/sha256 directory: %w", err)
	}

	blobs := make(blobMap)
	blobs[manifest.Config.Digest.Hex] = configPath

	// Collect layer paths for Docker manifest and check for missing blobs
	var dockerLayers []string
	var missingBlobs []string

	// Add layers to blobs and collect their paths
	for _, layerDesc := range manifest.Layers {
		// Always include the layer path in the Docker manifest (use forward slashes for JSON format)
		dockerLayers = append(dockerLayers, "blobs/sha256/"+layerDesc.Digest.Hex)

		if blobPath, ok := layerBlobsByDigest[layerDesc.Digest.Hex]; ok {
			blobs[layerDesc.Digest.Hex] = blobPath
		} else if !allowMissingBlobs {
			missingBlobs = append(missingBlobs, layerDesc.Digest.String())
		}
	}

	if len(missingBlobs) > 0 {
		return &MissingBlobsError{MissingBlobs: missingBlobs}
	}

	// Copy all blobs
	if err := copyBlobs(sink, blobs, useSymlinks); err != nil {
		return err
	}

	// Create Docker manifest
	dockerManifest := DockerManifest{
		Config:   "blobs/sha256/" + manifest.Config.Digest.Hex,
		RepoTags: repoTags,
		Layers:   dockerLayers,
	}

	// Write Docker manifest as array (Docker load format expects an array)
	dockerManifestArray := []DockerManifest{dockerManifest}
	manifestJSON, err := json.MarshalIndent(dockerManifestArray, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling Docker manifest: %w", err)
	}

	return sink.WriteFile("manifest.json", manifestJSON, 0o644)
}

func copyBlobs(sink DockerSaveSink, blobs blobMap, useSymlinks bool) error {
	// Sort blob digests to ensure deterministic order
	digests := make([]string, 0, len(blobs))
	for digest := range blobs {
		digests = append(digests, digest)
	}
	sort.Strings(digests)

	for _, digest := range digests {
		srcPath := blobs[digest]
		dstPath := filepath.Join("blobs", "sha256", digest)
		if err := sink.CopyFile(dstPath, srcPath, useSymlinks); err != nil {
			return fmt.Errorf("copying blob %s: %w", digest, err)
		}
	}
	return nil
}
