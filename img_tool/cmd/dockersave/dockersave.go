package dockersave

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/credential"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/protohelper"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/deployvfs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/ocitar"
)

// blobSource describes how to open the complete bytes for one content-addressed blob.
type blobSource struct {
	// path points to either a complete blob or a compact stream that can reconstruct it.
	path string
	// size is the byte size of the complete blob after reconstruction, not the .cstream file.
	size int64
	// compactStream indicates that path is a reconstruction recipe rather than the complete blob.
	compactStream bool
	// store resolves content references encountered while reconstructing a compact stream.
	store compactstream.BlobStore
}

// blobMap indexes complete or reconstructable blob sources by their final digest.
type blobMap map[string]blobSource

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

// readLayerDigestAndSize reads the final compressed layer identity from its metadata.
func readLayerDigestAndSize(metadataPath string) (string, int64, error) {
	metadataData, err := os.ReadFile(metadataPath)
	if err != nil {
		return "", 0, fmt.Errorf("reading layer metadata %s: %w", metadataPath, err)
	}

	var metadata struct {
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	}
	if err := json.Unmarshal(metadataData, &metadata); err != nil {
		return "", 0, fmt.Errorf("unmarshaling layer metadata %s: %w", metadataPath, err)
	}
	if metadata.Digest == "" {
		return "", 0, fmt.Errorf("layer metadata %s does not contain digest", metadataPath)
	}

	return strings.TrimPrefix(metadata.Digest, "sha256:"), metadata.Size, nil
}

// buildLayerCASDirMap indexes optional local compact-stream CAS directories by layer digest.
func buildLayerCASDirMap(layerCASDirFlags layerMappingFlag) (map[string]string, error) {
	result := make(map[string]string)
	for _, layer := range layerCASDirFlags {
		digest, _, err := readLayerDigestAndSize(layer.metadata)
		if err != nil {
			return nil, fmt.Errorf("reading layer CAS directory mapping %s=%s: %w", layer.metadata, layer.blob, err)
		}
		result[digest] = layer.blob
	}
	return result, nil
}

// compactStreamResolver owns the cache sources and remote connection shared by all layers.
type compactStreamResolver struct {
	diskCachePath string
	casReader     deployvfs.CompactStreamCASReader
	close         func() error
}

// newCompactStreamResolverFromEnv configures disk and remote CAS access only when needed.
func newCompactStreamResolverFromEnv(layers layerMappingFlag) (*compactStreamResolver, error) {
	resolver := &compactStreamResolver{diskCachePath: os.Getenv("IMG_DISK_CACHE")}

	// Avoid creating a remote client when every layer is already materialized.
	var hasCompactStream bool
	for _, layer := range layers {
		if strings.HasSuffix(layer.blob, ".cstream") {
			hasCompactStream = true
			break
		}
	}
	if !hasCompactStream {
		return resolver, nil
	}

	// A missing endpoint leaves local CAS directories and the disk cache as fallbacks.
	reapiEndpoint := os.Getenv("IMG_REAPI_ENDPOINT")
	if reapiEndpoint == "" {
		return resolver, nil
	}

	// Use the same optional Bazel credential-helper protocol as lazy deploy.
	credHelper := credential.NopHelper()
	if credentialHelperPath := os.Getenv("IMG_CREDENTIAL_HELPER"); credentialHelperPath != "" {
		credHelper = credential.New(credentialHelperPath, nil)
	}
	// Build the REAPI client used to stream referenced blobs from remote CAS.
	grpcConn, err := protohelper.Client(reapiEndpoint, credHelper)
	if err != nil {
		return nil, fmt.Errorf("creating gRPC client for compact-stream REAPI access: %w", err)
	}
	casReader, err := cas.New(grpcConn, cas.WithInstanceName(os.Getenv("IMG_REAPI_INSTANCE_NAME")))
	if err != nil {
		grpcConn.Close()
		return nil, fmt.Errorf("creating CAS client for compact-stream reconstruction: %w", err)
	}

	resolver.casReader = casReader
	resolver.close = grpcConn.Close
	return resolver, nil
}

// Close releases the shared REAPI connection when one was created.
func (r *compactStreamResolver) Close() error {
	if r.close == nil {
		return nil
	}
	return r.close()
}

// blobStore creates the lazy resolver chain for a single compact-stream layer.
func (r *compactStreamResolver) blobStore(casDirPath string) compactstream.BlobStore {
	return deployvfs.NewCompactStreamBlobStore(casDirPath, r.diskCachePath, r.casReader)
}

// buildLayerBlobSourceMap converts CLI mappings into sources keyed by final layer digest.
func buildLayerBlobSourceMap(layers layerMappingFlag, layerCASDirFlags layerMappingFlag, resolver *compactStreamResolver) (map[string]blobSource, error) {
	layerCASDirsByDigest, err := buildLayerCASDirMap(layerCASDirFlags)
	if err != nil {
		return nil, err
	}

	result := make(map[string]blobSource)
	for _, layer := range layers {
		digest, size, err := readLayerDigestAndSize(layer.metadata)
		if err != nil {
			return nil, err
		}
		isCompactStream := strings.HasSuffix(layer.blob, ".cstream")
		source := blobSource{
			path:          layer.blob,
			size:          size,
			compactStream: isCompactStream,
		}
		// Compact sources resolve referenced bytes lazily instead of opening a complete blob.
		if isCompactStream {
			source.store = resolver.blobStore(layerCASDirsByDigest[digest])
		}
		result[digest] = source
	}
	return result, nil
}

func DockerSaveProcess(ctx context.Context, args []string) {
	var manifestPath string
	var configPath string
	var outputPath string
	var format string
	var layerFlags layerMappingFlag
	var layerCASDirFlags layerMappingFlag
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
			"IMG_REAPI_ENDPOINT=grpcs://cache.example.com img docker-save --manifest manifest.json --config config.json --layer layer1_meta.json=layer1.tar.gz.cstream --repo-tag my/image:latest --output docker-save.tar",
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
	flagSet.Var(&layerFlags, "layer", "Layer mapping in format metadata=blob (can be specified multiple times). The blob may be a materialized layer or a .cstream compact stream.")
	flagSet.Var(&layerCASDirFlags, "layer-cas-dir", "Optional local CAS directory mapping in format metadata=cas_dir for compact streams; disk and remote CAS are used as fallbacks (can be specified multiple times)")
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
		err = assembleDockerSaveWithIndex(ctx, indexPath, outputPath, format, manifestPaths, configPaths, layerFlags, layerCASDirFlags, repoTags, ociTags, useSymlinks, allowMissingBlobs)
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
		err = assembleDockerSave(ctx, manifestPath, configPath, outputPath, format, layerFlags, layerCASDirFlags, repoTags, ociTags, useSymlinks, allowMissingBlobs)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func assembleDockerSave(ctx context.Context, manifestPath, configPath, outputPath, format string, layers layerMappingFlag, layerCASDirFlags layerMappingFlag, repoTags, ociTags []string, useSymlinks, allowMissingBlobs bool) error {
	// Read and parse the manifest
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}

	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("unmarshaling manifest: %w", err)
	}

	// Share one disk/remote CAS resolver across every compact layer in the image.
	resolver, err := newCompactStreamResolverFromEnv(layers)
	if err != nil {
		return err
	}
	defer resolver.Close()

	// Build a map of available layers by their digest.
	layerBlobsByDigest, err := buildLayerBlobSourceMap(layers, layerCASDirFlags, resolver)
	if err != nil {
		return err
	}

	blobs := make(blobMap)
	blobs[manifest.Config.Digest.Hex] = blobSource{path: configPath}

	manifestDigest := hashBytes(manifestData)
	blobs[manifestDigest.Hex] = blobSource{path: manifestPath}

	// Check for missing blobs
	var missingBlobs []string
	for _, layerDesc := range manifest.Layers {
		if blob, ok := layerBlobsByDigest[layerDesc.Digest.Hex]; ok {
			if blob.size == 0 {
				blob.size = layerDesc.Size
			}
			blobs[layerDesc.Digest.Hex] = blob
		} else if !allowMissingBlobs {
			missingBlobs = append(missingBlobs, layerDesc.Digest.String())
		}
	}

	if len(missingBlobs) > 0 {
		return &MissingBlobsError{MissingBlobs: missingBlobs}
	}

	if format == "tar" {
		return assembleDockerSaveTar(ctx, outputPath, &manifest, manifestData, blobs, repoTags, ociTags)
	}
	return assembleDockerSaveDirectory(ctx, outputPath, &manifest, manifestData, blobs, repoTags, ociTags, useSymlinks)
}

func assembleDockerSaveTar(ctx context.Context, outputPath string, manifest *v1.Manifest, manifestData []byte, blobs blobMap, repoTags, ociTags []string) error {
	var w *os.File
	var err error
	if outputPath == "-" {
		w = os.Stdout
	} else {
		w, err = os.Create(outputPath)
		if err != nil {
			return fmt.Errorf("creating output file: %w", err)
		}
		defer w.Close()
	}

	source := &fileBlobSource{blobs: blobs}
	opts := ocitar.Options{
		Tags:    repoTags,
		OCITags: ociTags,
	}
	return ocitar.WriteSingleManifest(ctx, w, manifest, manifestData, source, opts)
}

func assembleDockerSaveDirectory(ctx context.Context, outputPath string, manifest *v1.Manifest, manifestData []byte, blobs blobMap, repoTags, ociTags []string, useSymlinks bool) error {
	sink := NewDirectorySink(outputPath)
	defer sink.Close()

	manifestDigest := hashBytes(manifestData)

	if err := sink.CreateDir("."); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	// Write OCI layout file
	ociLayout := map[string]string{"imageLayoutVersion": OCILayoutVersion}
	if err := writeJSONWithSink(sink, "oci-layout", ociLayout); err != nil {
		return fmt.Errorf("writing oci-layout: %w", err)
	}

	// Write OCI index.json
	var artifactType string
	if manifest.Config.MediaType != "" && !manifest.Config.MediaType.IsConfig() {
		artifactType = string(manifest.Config.MediaType)
	}
	manifests := ocitar.DescriptorsForTags(ociTags, manifest.MediaType, manifestData, manifestDigest, artifactType)
	ociIndex := v1.IndexManifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests:     manifests,
	}
	if err := writeJSONWithSink(sink, "index.json", ociIndex); err != nil {
		return fmt.Errorf("writing index.json: %w", err)
	}

	// Write Docker manifest.json
	var dockerLayers []string
	for _, layerDesc := range manifest.Layers {
		dockerLayers = append(dockerLayers, "blobs/sha256/"+layerDesc.Digest.Hex)
	}
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

	// Write blobs
	if err := sink.CreateDir("blobs"); err != nil {
		return fmt.Errorf("creating blobs directory: %w", err)
	}
	if err := sink.CreateDir(filepath.Join("blobs", "sha256")); err != nil {
		return fmt.Errorf("creating blobs/sha256 directory: %w", err)
	}
	if err := copyBlobs(ctx, sink, blobs, useSymlinks); err != nil {
		return err
	}

	return nil
}

// copyBlobs materializes every digest into a directory-format Docker save output.
func copyBlobs(ctx context.Context, sink DockerSaveSink, blobs blobMap, useSymlinks bool) error {
	// Sort blob digests to ensure deterministic order
	digests := make([]string, 0, len(blobs))
	for digest := range blobs {
		digests = append(digests, digest)
	}
	sort.Strings(digests)

	for _, digest := range digests {
		src := blobs[digest]
		dstPath := filepath.Join("blobs", "sha256", digest)
		// Complete blobs retain the existing copy-or-symlink behavior.
		if !src.compactStream {
			if err := sink.CopyFile(dstPath, src.path, useSymlinks); err != nil {
				return fmt.Errorf("copying blob %s: %w", digest, err)
			}
			continue
		}

		// Compact blobs are reconstructed directly into their final digest path.
		rc, _, err := openBlobSource(ctx, digest, src)
		if err != nil {
			return fmt.Errorf("opening compact-stream blob %s: %w", digest, err)
		}
		if err := sink.WriteStream(dstPath, rc, 0o644); err != nil {
			rc.Close()
			return fmt.Errorf("copying blob %s: %w", digest, err)
		}
		if err := rc.Close(); err != nil {
			return fmt.Errorf("closing compact-stream blob %s: %w", digest, err)
		}
	}
	return nil
}

// fileBlobSource adapts the digest map to ocitar's streaming BlobSource interface.
type fileBlobSource struct {
	blobs blobMap
}

// OpenBlob returns a reader over the complete bytes expected for the requested digest.
func (f *fileBlobSource) OpenBlob(ctx context.Context, hexDigest string) (io.ReadCloser, int64, error) {
	return openBlobSource(ctx, hexDigest, f.blobs[hexDigest])
}

// openBlobSource opens materialized blobs directly and compact blobs through reconstruction.
func openBlobSource(ctx context.Context, hexDigest string, source blobSource) (io.ReadCloser, int64, error) {
	if source.path == "" {
		return nil, 0, fmt.Errorf("blob %s not found", hexDigest)
	}
	// Feed reconstruction through a pipe so the complete layer never lands on disk.
	if source.compactStream {
		file, err := os.Open(source.path)
		if err != nil {
			return nil, 0, err
		}

		pr, pw := io.Pipe()
		go func() {
			err := compactstream.Reconstruct(ctx, file, source.store, pw)
			file.Close()
			pw.CloseWithError(err)
		}()
		return pr, source.size, nil
	}

	// Preserve the direct-file path for existing materialized layer inputs.
	file, err := os.Open(source.path)
	if err != nil {
		return nil, 0, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, 0, err
	}
	return file, info.Size(), nil
}

func assembleDockerSaveWithIndex(ctx context.Context, indexPath, outputPath, format string, manifestPaths, configPaths []string, layers layerMappingFlag, layerCASDirFlags layerMappingFlag, repoTags, ociTags []string, useSymlinks, allowMissingBlobs bool) error {
	// Share one disk/remote CAS resolver across every compact layer in the index.
	resolver, err := newCompactStreamResolverFromEnv(layers)
	if err != nil {
		return err
	}
	defer resolver.Close()

	// Build a map of available layers by their digest
	layerBlobsByDigest, err := buildLayerBlobSourceMap(layers, layerCASDirFlags, resolver)
	if err != nil {
		return err
	}

	blobs := make(blobMap)
	var allMissingBlobs []string
	var manifestInfos []ocitar.ManifestInfo

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
		blobs[manifestDigest.Hex] = blobSource{path: manifestPaths[i]}

		// Add config blob
		blobs[manifest.Config.Digest.Hex] = blobSource{path: configPaths[i]}

		// Build ManifestInfo
		info := ocitar.ManifestInfo{
			ManifestData: manifestData,
			ConfigDigest: manifest.Config.Digest.Hex,
			MediaType:    manifest.MediaType,
		}

		// Check for missing layer blobs
		for _, layerDesc := range manifest.Layers {
			if blob, ok := layerBlobsByDigest[layerDesc.Digest.Hex]; ok {
				if blob.size == 0 {
					blob.size = layerDesc.Size
				}
				blobs[layerDesc.Digest.Hex] = blob
			} else if !allowMissingBlobs {
				allMissingBlobs = append(allMissingBlobs, layerDesc.Digest.String())
			}
			info.LayerDigests = append(info.LayerDigests, layerDesc.Digest.Hex)
		}

		manifestInfos = append(manifestInfos, info)
	}

	if len(allMissingBlobs) > 0 {
		return &MissingBlobsError{MissingBlobs: allMissingBlobs}
	}

	// Read the pre-built index
	indexData, err := os.ReadFile(indexPath)
	if err != nil {
		return fmt.Errorf("reading index file: %w", err)
	}
	indexDigest := hashBytes(indexData)
	blobs[indexDigest.Hex] = blobSource{path: indexPath}

	if format == "tar" {
		return assembleDockerSaveWithIndexTar(ctx, outputPath, indexData, manifestInfos, blobs, repoTags, ociTags)
	}
	return assembleDockerSaveWithIndexDirectory(ctx, outputPath, indexData, indexDigest, manifestInfos, blobs, repoTags, ociTags, useSymlinks)
}

func assembleDockerSaveWithIndexTar(ctx context.Context, outputPath string, indexData []byte, manifestInfos []ocitar.ManifestInfo, blobs blobMap, repoTags, ociTags []string) error {
	var w *os.File
	var err error
	if outputPath == "-" {
		w = os.Stdout
	} else {
		w, err = os.Create(outputPath)
		if err != nil {
			return fmt.Errorf("creating output file: %w", err)
		}
		defer w.Close()
	}

	source := &fileBlobSource{blobs: blobs}
	opts := ocitar.Options{
		Tags:    repoTags,
		OCITags: ociTags,
	}
	return ocitar.WriteIndex(ctx, w, indexData, manifestInfos, source, opts)
}

func assembleDockerSaveWithIndexDirectory(ctx context.Context, outputPath string, indexData []byte, indexDigest v1.Hash, manifestInfos []ocitar.ManifestInfo, blobs blobMap, repoTags, ociTags []string, useSymlinks bool) error {
	sink := NewDirectorySink(outputPath)
	defer sink.Close()

	if err := sink.CreateDir("."); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	// Write OCI layout file
	ociLayout := map[string]string{"imageLayoutVersion": OCILayoutVersion}
	if err := writeJSONWithSink(sink, "oci-layout", ociLayout); err != nil {
		return fmt.Errorf("writing oci-layout: %w", err)
	}

	// Write new root index.json referencing the pre-built index blob
	manifests := ocitar.DescriptorsForTags(ociTags, types.OCIImageIndex, indexData, indexDigest, "")
	rootIndex := v1.IndexManifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests:     manifests,
	}
	if err := writeJSONWithSink(sink, "index.json", rootIndex); err != nil {
		return fmt.Errorf("writing index.json: %w", err)
	}

	// Build Docker manifest.json from the first manifest
	firstInfo := manifestInfos[0]
	var dockerLayers []string
	for _, layerHex := range firstInfo.LayerDigests {
		dockerLayers = append(dockerLayers, "blobs/sha256/"+layerHex)
	}
	dockerManifest := DockerManifest{
		Config:   "blobs/sha256/" + firstInfo.ConfigDigest,
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

	// Write blobs
	if err := sink.CreateDir("blobs"); err != nil {
		return fmt.Errorf("creating blobs directory: %w", err)
	}
	if err := sink.CreateDir(filepath.Join("blobs", "sha256")); err != nil {
		return fmt.Errorf("creating blobs/sha256 directory: %w", err)
	}
	if err := copyBlobs(ctx, sink, blobs, useSymlinks); err != nil {
		return err
	}

	return nil
}
