package manifestfromocilayout

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	specv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compress"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/metadata"
)

var (
	srcPath           string
	manifestOutput    string
	configOutput      string
	descriptorOutput  string
	digestOutput      string
	architecture      string
	operatingSystem   string
	variant           string
	flatten           bool
	layerMediaTypes   *indexedStringFlag
	layerBlobs        *indexedStringFlag
	layerMetadataJSON *indexedStringFlag
)

func ManifestFromOCILayoutProcess(_ context.Context, args []string) {
	layerMediaTypes = newIndexedStringFlag()
	layerBlobs = newIndexedStringFlag()
	layerMetadataJSON = newIndexedStringFlag()
	flatten = false

	flagSet := flag.NewFlagSet("manifest-from-oci-layout", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Converts an OCI layout or container image tar archive to an image manifest with specified layer media types.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img manifest-from-oci-layout [OPTIONS]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"img manifest-from-oci-layout --src oci-layout --manifest manifest.json --config config.json --descriptor descriptor.json --digest digest.txt --architecture amd64 --os linux --layer_media_type=0=application/vnd.oci.image.layer.v1.tar+gzip --layer_blob=0=layer0.tgz --layer_metadata_json=0=layer0_metadata.json",
			"img manifest-from-oci-layout --src image.tar --flatten --manifest manifest.json --config config.json --descriptor descriptor.json --digest digest.txt --architecture amd64 --os linux --layer_media_type=0=application/vnd.oci.image.layer.v1.tar+zstd --layer_blob=0=layer0.tzst --layer_metadata_json=0=layer0_metadata.json",
		}
		fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
	}

	flagSet.StringVar(&srcPath, "src", "", "Path to the OCI layout directory, OCI layout tar archive, or docker save tar archive (required)")
	flagSet.StringVar(&manifestOutput, "manifest", "", "Output path for the image manifest (required)")
	flagSet.StringVar(&configOutput, "config", "", "Output path for the image config (required)")
	flagSet.StringVar(&descriptorOutput, "descriptor", "", "Output path for the manifest descriptor (required)")
	flagSet.StringVar(&digestOutput, "digest", "", "Output path for the manifest digest (required)")
	flagSet.StringVar(&architecture, "architecture", "", "Target architecture (required)")
	flagSet.StringVar(&operatingSystem, "os", "", "Target OS (required)")
	flagSet.StringVar(&variant, "variant", "", "Target variant (optional, e.g., v3 for amd64/v3, v8 for arm64/v8)")
	flagSet.BoolVar(&flatten, "flatten", false, "Squash all layers into a single flattened layer while preserving image configuration")
	flagSet.Var(layerMediaTypes, "layer_media_type", "Layer media type in format index=mediatype (can be specified multiple times)")
	flagSet.Var(layerBlobs, "layer_blob", "Output path for layer blob in format index=path (can be specified multiple times)")
	flagSet.Var(layerMetadataJSON, "layer_metadata_json", "Output path for layer metadata JSON in format index=path (can be specified multiple times)")

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}

	// Validate required flags
	if srcPath == "" {
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
	if flatten && len(layerMediaTypes.values) != 1 {
		fmt.Fprintf(os.Stderr, "Error: --flatten requires exactly one --layer_media_type, got %d\n", len(layerMediaTypes.values))
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

	if err := processSource(srcPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func processSource(src string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}
	if fi.IsDir() {
		return processOCILayoutDir(src)
	}
	return processArchive(src)
}

func processArchive(archivePath string) error {
	hasManifestJSON, hasIndexJSON, err := inspectTarArchive(archivePath)
	if err != nil {
		return fmt.Errorf("inspecting archive %s: %w", archivePath, err)
	}

	if hasManifestJSON {
		img, err := tarball.ImageFromPath(archivePath, nil)
		if err == nil {
			return processV1Image(img, nil)
		}
		if !hasIndexJSON {
			return fmt.Errorf("loading docker archive %s: %w", archivePath, err)
		}
	}

	if hasIndexJSON {
		tmpParent := filepath.Dir(manifestOutput)
		if tmpParent == "" {
			tmpParent = "."
		}
		tmpDir, err := os.MkdirTemp(tmpParent, ".oci-layout-*")
		if err != nil {
			return fmt.Errorf("creating temp dir for OCI layout archive: %w", err)
		}
		defer os.RemoveAll(tmpDir)

		if err := extractTarToDir(archivePath, tmpDir); err != nil {
			return fmt.Errorf("extracting OCI layout archive %s: %w", archivePath, err)
		}
		return processOCILayoutDir(tmpDir)
	}

	return fmt.Errorf("unsupported archive %s: neither manifest.json (docker save) nor index.json (OCI layout) found", archivePath)
}

func processOCILayoutDir(dir string) error {
	// Read the OCI layout index
	indexPath := filepath.Join(dir, "index.json")
	indexData, err := os.ReadFile(indexPath)
	if err != nil {
		return fmt.Errorf("reading index.json: %w", err)
	}

	var index specv1.Index
	if err := json.Unmarshal(indexData, &index); err != nil {
		return fmt.Errorf("unmarshaling index.json: %w", err)
	}

	// If index.json points to a nested image index (e.g., produced by `img docker-save --index`),
	// unwrap the nested index so platform lookup finds the platform manifests.
	if len(index.Manifests) == 1 && index.Manifests[0].MediaType == specv1.MediaTypeImageIndex {
		nestedIndexPath := filepath.Join(dir, "blobs", index.Manifests[0].Digest.Algorithm().String(), index.Manifests[0].Digest.Hex())
		nestedIndexData, err := os.ReadFile(nestedIndexPath)
		if err != nil {
			return fmt.Errorf("reading nested index blob: %w", err)
		}
		if err := json.Unmarshal(nestedIndexData, &index); err != nil {
			return fmt.Errorf("unmarshaling nested index: %w", err)
		}
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
		} else if manifestDesc.Platform.Architecture == "arm64" {
			// ARM64 defaults to v8 variant
			// See: https://github.com/containerd/platforms/blob/2e51fd9435bd985e1753954b24f4b0453f4e4767/platforms.go#L290
			manifestVariant = "v8"
		}
		if manifestVariant != variant {
			return fmt.Errorf("manifest descriptor variant mismatch: has %s, but %s was requested", manifestVariant, variant)
		}
	}

	// Read the manifest from blobs
	manifestBlobPath := filepath.Join(dir, "blobs", manifestDesc.Digest.Algorithm().String(), manifestDesc.Digest.Hex())
	manifestData, err := os.ReadFile(manifestBlobPath)
	if err != nil {
		return fmt.Errorf("reading manifest blob: %w", err)
	}

	var manifest specv1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("unmarshaling manifest: %w", err)
	}

	// Read the config from blobs
	configBlobPath := filepath.Join(dir, "blobs", manifest.Config.Digest.Algorithm().String(), manifest.Config.Digest.Hex())
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

	if flatten {
		img, err := partial.CompressedToImage(&ociDirImageCore{
			dir:         dir,
			mediaType:   types.MediaType(manifestDesc.MediaType),
			rawManifest: manifestData,
			rawConfig:   configData,
		})
		if err != nil {
			return fmt.Errorf("loading image from OCI layout for flattening: %w", err)
		}
		return processV1Image(img, manifest.Annotations)
	}

	// Check that the number of layers matches
	if len(manifest.Layers) != len(layerMediaTypes.values) {
		return fmt.Errorf("layer count mismatch: OCI layout has %d layers, but %d layer media types specified", len(manifest.Layers), len(layerMediaTypes.values))
	}
	if len(config.RootFS.DiffIDs) != len(manifest.Layers) {
		return fmt.Errorf("config rootfs.diff_ids count (%d) does not match manifest layer count (%d)", len(config.RootFS.DiffIDs), len(manifest.Layers))
	}

	// Distribute the source config's history across layers so it is preserved
	// when this converted image is used as a base (mirrors the image_import rule).
	perLayerHistory := metadata.SplitHistoryPerLayer(config.History, len(manifest.Layers))

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
		sourceLayerPath := filepath.Join(dir, "blobs", manifest.Layers[i].Digest.Algorithm().String(), manifest.Layers[i].Digest.Hex())
		if err := copyOrHardlink(sourceLayerPath, layerBlobPath); err != nil {
			return fmt.Errorf("copying layer %d blob: %w", i, err)
		}

		if err := writeLayerMetadataFile(
			layerMetadataPath,
			config.RootFS.DiffIDs[i].String(),
			manifest.Layers[i].MediaType,
			manifest.Layers[i].Digest.String(),
			manifest.Layers[i].Size,
			manifest.Layers[i].Annotations,
			perLayerHistory[i],
		); err != nil {
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

	return writeDescriptorAndDigest(manifestDesc.MediaType, manifestDesc.Digest, manifestDesc.Size)
}

type layerStreamSpec struct {
	open    func() (io.ReadCloser, error)
	history []api.History
}

func processV1Image(img v1.Image, manifestAnnotations map[string]string) error {
	rawConfig, err := img.RawConfigFile()
	if err != nil {
		return fmt.Errorf("reading image config: %w", err)
	}

	var config specv1.Image
	if err := json.Unmarshal(rawConfig, &config); err != nil {
		return fmt.Errorf("unmarshaling config: %w", err)
	}

	if config.OS != operatingSystem {
		return fmt.Errorf("OS mismatch: config has %s, but %s was requested", config.OS, operatingSystem)
	}
	if config.Architecture != architecture {
		return fmt.Errorf("architecture mismatch: config has %s, but %s was requested", config.Architecture, architecture)
	}

	var specs []layerStreamSpec
	if flatten {
		flattenedHistory := []api.History{{
			Created:   config.Created,
			CreatedBy: "rules_img flatten",
			Comment:   "flattened image",
		}}
		config.History = []specv1.History{{
			Created:   config.Created,
			CreatedBy: "rules_img flatten",
			Comment:   "flattened image",
		}}
		specs = []layerStreamSpec{{
			open: func() (io.ReadCloser, error) {
				return mutate.Extract(img), nil
			},
			history: flattenedHistory,
		}}
	} else {
		imgLayers, err := img.Layers()
		if err != nil {
			return fmt.Errorf("reading image layers: %w", err)
		}
		if len(imgLayers) != len(layerMediaTypes.values) {
			return fmt.Errorf("layer count mismatch: archive has %d layers, but %d layer media types specified", len(imgLayers), len(layerMediaTypes.values))
		}
		perLayerHistory := metadata.SplitHistoryPerLayer(config.History, len(imgLayers))
		specs = make([]layerStreamSpec, len(imgLayers))
		for i, imgLayer := range imgLayers {
			specs[i] = layerStreamSpec{
				open:    imgLayer.Uncompressed,
				history: perLayerHistory[i],
			}
		}
	}

	diffIDs := make([]digest.Digest, len(specs))
	layerDescriptors := make([]specv1.Descriptor, len(specs))

	for i, spec := range specs {
		targetMediaType, _ := layerMediaTypes.Get(i)
		layerBlobPath, _ := layerBlobs.Get(i)
		layerMetadataPath, _ := layerMetadataJSON.Get(i)

		rc, err := spec.open()
		if err != nil {
			return fmt.Errorf("opening layer %d stream: %w", i, err)
		}
		diffID, blobDigest, blobSize, err := compressStreamToLayerBlob(rc, targetMediaType, layerBlobPath)
		if err != nil {
			return fmt.Errorf("processing layer %d: %w", i, err)
		}

		if err := writeLayerMetadataFile(
			layerMetadataPath,
			diffID.String(),
			targetMediaType,
			blobDigest.String(),
			blobSize,
			nil,
			spec.history,
		); err != nil {
			return fmt.Errorf("writing metadata for layer %d: %w", i, err)
		}

		diffIDs[i] = diffID
		layerDescriptors[i] = specv1.Descriptor{
			MediaType: targetMediaType,
			Digest:    blobDigest,
			Size:      blobSize,
		}
	}

	config.RootFS = specv1.RootFS{
		Type:    "layers",
		DiffIDs: diffIDs,
	}

	return writeGeneratedManifestAndConfig(&config, layerDescriptors, manifestAnnotations)
}

func compressStreamToLayerBlob(rc io.ReadCloser, targetMediaType, layerBlobPath string) (digest.Digest, digest.Digest, int64, error) {
	defer rc.Close()

	compAlg := api.LayerFormat(targetMediaType).CompressionAlgorithm()
	if compAlg == "" {
		return "", "", 0, fmt.Errorf("unsupported layer media type: %s", targetMediaType)
	}

	blobFile, err := os.Create(layerBlobPath)
	if err != nil {
		return "", "", 0, fmt.Errorf("creating layer blob %s: %w", layerBlobPath, err)
	}
	defer blobFile.Close()

	appender, err := compress.AppenderFactory(string(api.SHA256), string(compAlg), blobFile)
	if err != nil {
		return "", "", 0, fmt.Errorf("creating compressor for %s: %w", targetMediaType, err)
	}

	if _, err := io.Copy(appender, rc); err != nil {
		return "", "", 0, fmt.Errorf("streaming layer tar: %w", err)
	}
	if err := rc.Close(); err != nil {
		return "", "", 0, fmt.Errorf("closing layer stream: %w", err)
	}

	state, err := appender.Finalize()
	if err != nil {
		return "", "", 0, fmt.Errorf("finalizing compressed layer: %w", err)
	}
	if err := blobFile.Close(); err != nil {
		return "", "", 0, fmt.Errorf("closing layer blob %s: %w", layerBlobPath, err)
	}

	diffID := digest.NewDigestFromBytes(digest.SHA256, state.ContentHash)
	blobDigest := digest.NewDigestFromBytes(digest.SHA256, state.OuterHash)
	return diffID, blobDigest, state.CompressedSize, nil
}

func writeLayerMetadataFile(
	layerMetadataPath string,
	diffID string,
	mediaType string,
	blobDigest string,
	size int64,
	annotations map[string]string,
	history []api.History,
) error {
	metadataFile, err := os.Create(layerMetadataPath)
	if err != nil {
		return fmt.Errorf("creating metadata file %s: %w", layerMetadataPath, err)
	}
	defer metadataFile.Close()

	if err := metadata.WriteLayerMetadata(
		diffID,
		mediaType,
		blobDigest,
		size,
		annotations,
		history,
		metadataFile,
	); err != nil {
		return err
	}
	return metadataFile.Close()
}

func writeGeneratedManifestAndConfig(
	config *specv1.Image,
	layerDescriptors []specv1.Descriptor,
	manifestAnnotations map[string]string,
) error {
	configRaw, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	if err := os.WriteFile(configOutput, configRaw, 0o644); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	configSHA256 := sha256.Sum256(configRaw)

	manifest := specv1.Manifest{
		Versioned: specs.Versioned{
			SchemaVersion: 2,
		},
		MediaType: specv1.MediaTypeImageManifest,
		Config: specv1.Descriptor{
			MediaType: specv1.MediaTypeImageConfig,
			Digest:    digest.NewDigestFromBytes(digest.SHA256, configSHA256[:]),
			Size:      int64(len(configRaw)),
		},
		Layers:      layerDescriptors,
		Annotations: manifestAnnotations,
	}

	manifestRaw, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshaling manifest: %w", err)
	}
	if err := os.WriteFile(manifestOutput, manifestRaw, 0o644); err != nil {
		return fmt.Errorf("writing manifest: %w", err)
	}
	manifestSHA256 := sha256.Sum256(manifestRaw)
	manifestDigest := digest.NewDigestFromBytes(digest.SHA256, manifestSHA256[:])

	return writeDescriptorAndDigest(specv1.MediaTypeImageManifest, manifestDigest, int64(len(manifestRaw)))
}

func writeDescriptorAndDigest(mediaType string, manifestDigest digest.Digest, size int64) error {
	descriptor := specv1.Descriptor{
		MediaType: mediaType,
		Digest:    manifestDigest,
		Size:      size,
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
	if err := os.WriteFile(digestOutput, []byte(manifestDigest.String()), 0o644); err != nil {
		return fmt.Errorf("writing digest: %w", err)
	}
	return nil
}

func inspectTarArchive(tarPath string) (hasManifestJSON bool, hasIndexJSON bool, err error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return false, false, err
	}
	defer f.Close()

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false, false, err
		}
		switch path.Clean(hdr.Name) {
		case "manifest.json":
			hasManifestJSON = true
		case "index.json":
			hasIndexJSON = true
		}
		if hasManifestJSON && hasIndexJSON {
			break
		}
	}
	return hasManifestJSON, hasIndexJSON, nil
}

func extractTarToDir(tarPath, dstDir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		cleanRel := filepath.Clean(hdr.Name)
		if cleanRel == "." {
			continue
		}
		if !filepath.IsLocal(cleanRel) {
			return fmt.Errorf("unsafe tar entry path: %s", hdr.Name)
		}
		targetPath := filepath.Join(dstDir, cleanRel)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(hdr.Linkname) || !filepath.IsLocal(filepath.Join(filepath.Dir(cleanRel), hdr.Linkname)) {
				return fmt.Errorf("unsafe tar symlink target: %s", hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, targetPath); err != nil {
				return err
			}
		case tar.TypeLink:
			linkClean := filepath.Clean(hdr.Linkname)
			if !filepath.IsLocal(linkClean) {
				return fmt.Errorf("unsafe tar hardlink target: %s", hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
				return err
			}
			if err := os.Link(filepath.Join(dstDir, linkClean), targetPath); err != nil {
				return err
			}
		}
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
			} else if desc.Platform.Architecture == "arm64" {
				// ARM64 defaults to v8 variant
				// See: https://github.com/containerd/platforms/blob/2e51fd9435bd985e1753954b24f4b0453f4e4767/platforms.go#L290
				descVariant = "v8"
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

// copyOrHardlink resolves symlinks on src and attempts to hardlink the regular
// file to dst, falling back to a file copy if hardlinking fails.
func copyOrHardlink(src, dst string) error {
	resolvedSrc := src
	if resolved, err := filepath.EvalSymlinks(src); err == nil {
		resolvedSrc = resolved
	}

	// Try hardlink first
	if err := os.Link(resolvedSrc, dst); err == nil {
		return nil
	}

	// Fallback to copy
	srcFile, err := os.Open(resolvedSrc)
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

	return dstFile.Close()
}

type ociDirImageCore struct {
	dir         string
	mediaType   types.MediaType
	rawManifest []byte
	rawConfig   []byte
}

func (c *ociDirImageCore) MediaType() (types.MediaType, error) {
	return c.mediaType, nil
}

func (c *ociDirImageCore) RawManifest() ([]byte, error) {
	return c.rawManifest, nil
}

func (c *ociDirImageCore) RawConfigFile() ([]byte, error) {
	return c.rawConfig, nil
}

func (c *ociDirImageCore) LayerByDigest(h v1.Hash) (partial.CompressedLayer, error) {
	m, err := partial.Manifest(c)
	if err != nil {
		return nil, err
	}
	for _, desc := range m.Layers {
		if desc.Digest == h {
			return &ociDirBlob{dir: c.dir, desc: desc}, nil
		}
	}
	return nil, fmt.Errorf("layer %s not found in manifest", h)
}

type ociDirBlob struct {
	dir  string
	desc v1.Descriptor
}

func (b *ociDirBlob) Digest() (v1.Hash, error)            { return b.desc.Digest, nil }
func (b *ociDirBlob) Size() (int64, error)                { return b.desc.Size, nil }
func (b *ociDirBlob) MediaType() (types.MediaType, error) { return b.desc.MediaType, nil }
func (b *ociDirBlob) Compressed() (io.ReadCloser, error) {
	return os.Open(filepath.Join(b.dir, "blobs", b.desc.Digest.Algorithm, b.desc.Digest.Hex))
}
