package dockersave

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/ocilayout"
)

// readTagsFromConfigFile reads registry/repository/tags from a load
// configuration file and reconstructs the full image references. When registry
// and repository are both set, each tag becomes "<registry>/<repository>:<tag>";
// otherwise the tags are returned verbatim (backwards-compatible full
// references). See api.QualifyLoadTags.
func readTagsFromConfigFile(configPath string) ([]string, error) {
	if configPath == "" {
		return nil, nil
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("reading configuration file: %w", err)
	}

	var config map[string]interface{}
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parsing configuration file: %w", err)
	}

	tagsInterface, ok := config["tags"].([]interface{})
	if !ok {
		return nil, nil // tags field not present or not a list
	}

	tags := make([]string, len(tagsInterface))
	for i, tag := range tagsInterface {
		if tagStr, ok := tag.(string); ok {
			tags[i] = tagStr
		} else {
			return nil, fmt.Errorf("tag at index %d is not a string", i)
		}
	}

	registry, _ := config["registry"].(string)
	repository, _ := config["repository"].(string)
	if err := api.ValidateLoadDestination(registry, repository); err != nil {
		return nil, err
	}
	return api.QualifyLoadTags(registry, repository, tags), nil
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
	var indexPath string
	var manifestPaths stringSliceFlag
	var configPaths stringSliceFlag
	var ociRefNameTagOnly bool

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
	flagSet.StringVar(&indexPath, "index", "", "Path to the image index (for multi-platform, mutually exclusive with --manifest and --config)")
	flagSet.Var(&manifestPaths, "manifest-path", "Path to manifest file (for index, can be specified multiple times)")
	flagSet.Var(&configPaths, "config-path", "Path to config file (for index, can be specified multiple times)")
	flagSet.BoolVar(&ociRefNameTagOnly, "oci-ref-name-tag-only", false, "Set org.opencontainers.image.ref.name to just the tag (OCI spec); default uses the full reference (compatible with skopeo and rules_oci)")

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}

	if outputPath == "" {
		fmt.Fprintf(os.Stderr, "Error: --output is required\n")
		flagSet.Usage()
		os.Exit(1)
	}

	if format != "directory" && format != "tar" {
		fmt.Fprintf(os.Stderr, "Error: --format must be 'directory' or 'tar', got '%s'\n", format)
		flagSet.Usage()
		os.Exit(1)
	}

	// Read tags from configuration file if provided and no --repo-tag was specified.
	if len(repoTags) == 0 && configurationFilePath != "" {
		configTags, err := readTagsFromConfigFile(configurationFilePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading configuration file: %v\n", err)
			os.Exit(1)
		}
		if len(configTags) > 0 {
			repoTags = configTags
		}
	}

	// ociTags are the user-provided tags used for OCI index.json annotations;
	// they may be empty.
	ociTags := []string(repoTags)

	// Default repo tag for Docker's manifest.json RepoTags if none provided.
	if len(repoTags) == 0 {
		repoTags = []string{"image:latest"}
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
		err = assembleDockerSaveWithIndex(ctx, indexPath, outputPath, format, manifestPaths, configPaths, layerFlags, repoTags, ociTags, useSymlinks, allowMissingBlobs, ociRefNameTagOnly)
	} else {
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
		if len(manifestPaths) > 0 || len(configPaths) > 0 {
			fmt.Fprintf(os.Stderr, "Error: cannot use --manifest-path or --config-path without --index\n")
			os.Exit(1)
		}
		err = assembleDockerSave(ctx, manifestPath, configPath, outputPath, format, layerFlags, repoTags, ociTags, useSymlinks, allowMissingBlobs, ociRefNameTagOnly)
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

// manifestInput builds a ManifestInput from a parsed manifest, its raw bytes,
// the config path and a digest->layer-path map.
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

func applyDockerSaveOptions(b *ocilayout.Builder, repoTags, ociTags []string, useSymlinks, allowMissingBlobs, ociRefNameTagOnly bool) {
	b.WithTags(repoTags).
		WithOCITags(ociTags).
		WithMissingBlobsHint(ocilayout.OutputGroupTarball).
		WithLinkStrategy(useSymlinks, false)
	if allowMissingBlobs {
		b.AllowMissingBlobs()
	}
	if ociRefNameTagOnly {
		b.WithAnnotationMode(ocilayout.AnnotateTagOnly)
	}
}

func writeLayout(ctx context.Context, b *ocilayout.Builder, format, outputPath string) error {
	if format == "tar" {
		return b.WriteTar(ctx, outputPath)
	}
	return b.WriteDir(ctx, outputPath)
}

func assembleDockerSave(ctx context.Context, manifestPath, configPath, outputPath, format string, layers layerMappingFlag, repoTags, ociTags []string, useSymlinks, allowMissingBlobs, ociRefNameTagOnly bool) error {
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

	b := ocilayout.New(ocilayout.DockerSave())
	applyDockerSaveOptions(b, repoTags, ociTags, useSymlinks, allowMissingBlobs, ociRefNameTagOnly)
	b.AddManifest(manifestInput(&manifest, manifestData, configPath, layerPaths))
	return writeLayout(ctx, b, format, outputPath)
}

func assembleDockerSaveWithIndex(ctx context.Context, indexPath, outputPath, format string, manifestPaths, configPaths []string, layers layerMappingFlag, repoTags, ociTags []string, useSymlinks, allowMissingBlobs, ociRefNameTagOnly bool) error {
	indexData, err := os.ReadFile(indexPath)
	if err != nil {
		return fmt.Errorf("reading index file: %w", err)
	}

	layerPaths, err := layerPathsByDigest(layers)
	if err != nil {
		return err
	}

	b := ocilayout.New(ocilayout.DockerSave().WithIndexStyle(ocilayout.IndexWrapping))
	applyDockerSaveOptions(b, repoTags, ociTags, useSymlinks, allowMissingBlobs, ociRefNameTagOnly)
	b.SetRootIndex(ocilayout.BlobFromBytes(indexData))

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
