package dockersave

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"

	v1 "github.com/malt3/go-containerregistry/pkg/v1"
	"github.com/malt3/go-containerregistry/pkg/v1/types"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"

	"github.com/malt3/go-containerregistry/pkg/name"
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

const OCILayoutVersion = "1.0.0"

// DockerManifest represents the Docker save manifest format
type DockerManifest struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

func hashBytes(data []byte) v1.Hash {
	h, _, _ := v1.SHA256(bytes.NewReader(data))
	return h
}

func writeJSONWithSink(sink DockerSaveSink, path string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", path, err)
	}
	return sink.WriteFile(path, data, 0o644)
}

// readTagsFromConfigFile reads the tags field from a configuration file
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

	// Convert interface{} slice to string slice
	tags := make([]string, len(tagsInterface))
	for i, tag := range tagsInterface {
		if tagStr, ok := tag.(string); ok {
			tags[i] = tagStr
		} else {
			return nil, fmt.Errorf("tag at index %d is not a string", i)
		}
	}

	return tags, nil
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

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}

	// Validate required flags
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

	// Read tags from configuration file if provided and no --repo-tag was specified
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

	// ociTags are the user-provided tags used for OCI index.json annotations.
	// They may be empty if no tags were specified.
	ociTags := []string(repoTags)

	// Default repo tag if none provided from either flags or config.
	// This default is only used for Docker's manifest.json RepoTags.
	if len(repoTags) == 0 {
		repoTags = []string{"image:latest"}
	}

	var err error
	if indexPath != "" {
		// Index mode: multi-platform images
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
		err = assembleDockerSaveWithIndex(indexPath, outputPath, format, manifestPaths, configPaths, layerFlags, repoTags, ociTags, useSymlinks, allowMissingBlobs)
	} else {
		// Single manifest mode
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
		err = assembleDockerSave(manifestPath, configPath, outputPath, format, layerFlags, repoTags, ociTags, useSymlinks, allowMissingBlobs)
	}
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

// descriptorsForTags generates descriptors for the given image manifest and tags.
// Tooling that ingests OCI layouts (containerd loading, docker image load, container load, ...)
// can recover image names from the descriptors of the root index.json file based on some well-known annotations:
//   - Containerd uses "io.containerd.image.name" to refer to the full image name (<registry>/<repository>:<tag>)
//   - Apple Containerization uses "com.apple.containerization.image.name" to refer to the full image name (<registry>/<repository>:<tag>)
//   - The OCI image spec mentions "org.opencontainers.image.ref.name" to refer to the tag only (i.e. "latest")
//
// Note that the "org.opencontainers.image.ref.name" may not be unique within the index.json file.
// This is surprising, but allowed by the OCI image spec. Other tools also generate duplicate ref.name attributes.
// Tooling that consumes the index and needs to select images based on tags SHOULD select the first matching manifest.
// See also this upstream discussion: https://github.com/opencontainers/image-spec/issues/581
//
// Annotations from the referenced content (data) are copied into the produced descriptors.
// Tag annotations take precedence over content annotations.
func descriptorsForTags(ociTags []string, mediaType types.MediaType, data []byte, digest v1.Hash, artifactType string) []v1.Descriptor {
	size := int64(len(data))

	var parsed struct {
		Annotations map[string]string `json:"annotations,omitempty"`
	}
	json.Unmarshal(data, &parsed)

	if len(ociTags) == 0 {
		desc := v1.Descriptor{MediaType: mediaType, Digest: digest, Size: size, Annotations: parsed.Annotations}
		if artifactType != "" {
			desc.ArtifactType = artifactType
		}
		return []v1.Descriptor{desc}
	}

	descs := make([]v1.Descriptor, 0, len(ociTags))
	for _, repoTag := range ociTags {
		annotations := make(map[string]string)
		maps.Copy(annotations, parsed.Annotations)
		annotations[api.AnnotationContainerdImageName] = repoTag
		annotations[api.AnnotationAppleContainerizationImageName] = repoTag
		if ref, err := name.NewTag(repoTag, name.WithDefaultTag("")); err == nil && ref.TagStr() != "" {
			annotations[api.AnnotationOCIImageRefName] = ref.TagStr()
		}
		desc := v1.Descriptor{
			MediaType:   mediaType,
			Digest:      digest,
			Size:        size,
			Annotations: annotations,
		}
		if artifactType != "" {
			desc.ArtifactType = artifactType
		}
		descs = append(descs, desc)
	}
	return descs
}

func assembleDockerSave(manifestPath, configPath, outputPath, format string, layers layerMappingFlag, repoTags, ociTags []string, useSymlinks, allowMissingBlobs bool) error {
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

	blobs := make(blobMap)
	blobs[manifest.Config.Digest.Hex] = configPath

	// Add manifest itself as a blob for OCI layout
	manifestDigest := hashBytes(manifestData)
	blobs[manifestDigest.Hex] = manifestPath

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

	// Write metadata files first so consumers can read them without scanning the full tar.
	// Order: oci-layout, index.json, manifest.json, then blobs.
	if err := sink.CreateDir("."); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	// Write OCI layout file
	ociLayout := map[string]string{"imageLayoutVersion": OCILayoutVersion}
	if err := writeJSONWithSink(sink, "oci-layout", ociLayout); err != nil {
		return fmt.Errorf("writing oci-layout: %w", err)
	}

	// Write OCI index.json pointing to the manifest
	var artifactType string
	if manifest.Config.MediaType != "" && !manifest.Config.MediaType.IsConfig() {
		artifactType = string(manifest.Config.MediaType)
	}
	manifests := descriptorsForTags(ociTags, manifest.MediaType, manifestData, manifestDigest, artifactType)
	ociIndex := v1.IndexManifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests:     manifests,
	}
	if err := writeJSONWithSink(sink, "index.json", ociIndex); err != nil {
		return fmt.Errorf("writing index.json: %w", err)
	}

	// Write Docker manifest.json
	dockerManifest := DockerManifest{
		Config:   "blobs/sha256/" + manifest.Config.Digest.Hex,
		RepoTags: repoTags,
		Layers:   dockerLayers,
	}
	dockerManifestArray := []DockerManifest{dockerManifest}
	manifestJSON, err := json.MarshalIndent(dockerManifestArray, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling Docker manifest: %w", err)
	}
	if err := sink.WriteFile("manifest.json", manifestJSON, 0o644); err != nil {
		return fmt.Errorf("writing manifest.json: %w", err)
	}

	// Write blobs last
	if err := sink.CreateDir("blobs"); err != nil {
		return fmt.Errorf("creating blobs directory: %w", err)
	}
	if err := sink.CreateDir(filepath.Join("blobs", "sha256")); err != nil {
		return fmt.Errorf("creating blobs/sha256 directory: %w", err)
	}
	if err := copyBlobs(sink, blobs, useSymlinks); err != nil {
		return err
	}

	return nil
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

func assembleDockerSaveWithIndex(indexPath, outputPath, format string, manifestPaths, configPaths []string, layers layerMappingFlag, repoTags, ociTags []string, useSymlinks, allowMissingBlobs bool) error {
	sink, err := createSink(outputPath, format)
	if err != nil {
		return err
	}
	defer sink.Close()

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

		digest := strings.TrimPrefix(metadata.Digest, "sha256:")
		layerBlobsByDigest[digest] = layer.blob
	}

	blobs := make(blobMap)
	var allMissingBlobs []string

	// Track the first manifest's data for the Docker manifest.json
	var firstManifest *v1.Manifest
	var firstConfigDigestHex string

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

		// Check for missing layer blobs
		for _, layerDesc := range manifest.Layers {
			if blobPath, ok := layerBlobsByDigest[layerDesc.Digest.Hex]; ok {
				blobs[layerDesc.Digest.Hex] = blobPath
			} else if !allowMissingBlobs {
				allMissingBlobs = append(allMissingBlobs, layerDesc.Digest.String())
			}
		}

		// Remember first manifest for Docker manifest.json
		if i == 0 {
			firstManifest = &manifest
			firstConfigDigestHex = manifest.Config.Digest.Hex
		}
	}

	if len(allMissingBlobs) > 0 {
		return &MissingBlobsError{MissingBlobs: allMissingBlobs}
	}

	// Write metadata files first so consumers can read them without scanning the full tar.
	// Order: oci-layout, index.json, manifest.json, then blobs.
	if err := sink.CreateDir("."); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	// Write OCI layout file
	ociLayout := map[string]string{"imageLayoutVersion": OCILayoutVersion}
	if err := writeJSONWithSink(sink, "oci-layout", ociLayout); err != nil {
		return fmt.Errorf("writing oci-layout: %w", err)
	}

	// Read the pre-built index to store as a blob and wrap in a new root index
	indexData, err := os.ReadFile(indexPath)
	if err != nil {
		return fmt.Errorf("reading index file: %w", err)
	}
	indexDigest := hashBytes(indexData)
	blobs[indexDigest.Hex] = indexPath

	// Write new root index.json referencing the pre-built index blob
	manifests := descriptorsForTags(ociTags, types.OCIImageIndex, indexData, indexDigest, "")
	rootIndex := v1.IndexManifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests:     manifests,
	}
	if err := writeJSONWithSink(sink, "index.json", rootIndex); err != nil {
		return fmt.Errorf("writing index.json: %w", err)
	}

	// Build Docker manifest.json from the FIRST manifest (the "default" for docker load)
	var dockerLayers []string
	for _, layerDesc := range firstManifest.Layers {
		dockerLayers = append(dockerLayers, "blobs/sha256/"+layerDesc.Digest.Hex)
	}

	dockerManifest := DockerManifest{
		Config:   "blobs/sha256/" + firstConfigDigestHex,
		RepoTags: repoTags,
		Layers:   dockerLayers,
	}
	dockerManifestArray := []DockerManifest{dockerManifest}
	manifestJSON, err := json.MarshalIndent(dockerManifestArray, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling Docker manifest: %w", err)
	}
	if err := sink.WriteFile("manifest.json", manifestJSON, 0o644); err != nil {
		return fmt.Errorf("writing manifest.json: %w", err)
	}

	// Write blobs last
	if err := sink.CreateDir("blobs"); err != nil {
		return fmt.Errorf("creating blobs directory: %w", err)
	}
	if err := sink.CreateDir(filepath.Join("blobs", "sha256")); err != nil {
		return fmt.Errorf("creating blobs/sha256 directory: %w", err)
	}
	if err := copyBlobs(sink, blobs, useSymlinks); err != nil {
		return err
	}

	return nil
}
