package imagearchive

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	specv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compress"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/metadata"
)

const dockerTarZstdLayer = "application/vnd.docker.image.rootfs.diff.tar.zstd"

// CompressionOptions configures parallel compression jobs and level when encoding layer tar streams.
type CompressionOptions struct {
	Jobs  string
	Level int
	// ZstdWindowLog sets the zstd window to 2^n bytes; 0 leaves it to the level.
	ZstdWindowLog int
}

// LayerOutputSpec specifies the desired media type and output paths for a single layer index.
type LayerOutputSpec struct {
	MediaType    string
	BlobPath     string
	MetadataPath string
}

// WireCompression returns the wire-level compression algorithm of a standard OCI or Docker tar layer media type.
func WireCompression(mediaType string) api.CompressionAlgorithm {
	switch mediaType {
	case api.TarLayer, string(types.DockerUncompressedLayer):
		return api.Uncompressed
	case api.TarGzipLayer, string(types.DockerLayer):
		return api.Gzip
	case api.TarZstdLayer, dockerTarZstdLayer:
		return api.Zstd
	default:
		return ""
	}
}

// ResolveLayerMediaType maps short format names ("zstd", "gzip", "none", "uncompressed")
// or full OCI layer media types to their canonical OCI tar layer media type.
func ResolveLayerMediaType(formatOrMediaType string) (string, error) {
	switch formatOrMediaType {
	case "zstd", api.TarZstdLayer:
		return api.TarZstdLayer, nil
	case "gzip", api.TarGzipLayer:
		return api.TarGzipLayer, nil
	case "none", "uncompressed", api.TarLayer:
		return api.TarLayer, nil
	default:
		return "", fmt.Errorf("unsupported layer format or media type: %s", formatOrMediaType)
	}
}

// ConvertImage converts a LoadedImage into rules_img manifest, config, descriptor, optional digest,
// and per-layer blob and metadata files. Layers whose wire compression already matches the target
// media type are copied directly without recompression.
func ConvertImage(
	img *LoadedImage,
	layers []LayerOutputSpec,
	compOpts CompressionOptions,
	manifestOutput string,
	configOutput string,
	descriptorOutput string,
	digestOutput string,
	platform Platform,
) (specv1.Descriptor, error) {
	v1Layers, err := img.Image.Layers()
	if err != nil {
		return specv1.Descriptor{}, fmt.Errorf("reading source layers: %w", err)
	}
	if len(v1Layers) != len(layers) {
		return specv1.Descriptor{}, fmt.Errorf("layer count mismatch: archive has %d layers, but %d layer media types were specified", len(v1Layers), len(layers))
	}

	perLayerHistory := metadata.SplitHistoryPerLayer(img.Config.History, len(v1Layers))

	verbatimManifest := img.ManifestDesc != nil &&
		len(img.RawManifest) > 0 &&
		len(img.RawConfig) > 0 &&
		(img.ManifestDesc.MediaType == "" || img.ManifestDesc.MediaType == specv1.MediaTypeImageManifest) &&
		(img.Manifest.MediaType == "" || img.Manifest.MediaType == specv1.MediaTypeImageManifest)

	layerDescs := make([]specv1.Descriptor, len(layers))

	for i, v1Layer := range v1Layers {
		spec := layers[i]
		srcDiffID := img.Config.RootFS.DiffIDs[i]
		var srcDesc *specv1.Descriptor
		if img.ManifestDesc != nil {
			srcDesc = &img.Manifest.Layers[i]
		}
		if srcDesc == nil || srcDesc.MediaType != spec.MediaType {
			verbatimManifest = false
		}

		srcWire := ""
		if srcDesc != nil {
			srcWire = string(WireCompression(srcDesc.MediaType))
		}
		dstWire := string(WireCompression(spec.MediaType))

		if srcDesc != nil && srcWire != "" && srcWire == dstWire {
			rc, err := v1Layer.Compressed()
			if err != nil {
				return specv1.Descriptor{}, fmt.Errorf("opening layer %d blob: %w", i, err)
			}
			written, err := CopyStreamToFile(rc, spec.BlobPath)
			if err != nil {
				return specv1.Descriptor{}, fmt.Errorf("writing layer %d blob: %w", i, err)
			}
			if written != srcDesc.Size {
				return specv1.Descriptor{}, fmt.Errorf("layer %d size mismatch: manifest declares %d bytes, archive entry has %d bytes", i, srcDesc.Size, written)
			}
			layerDescs[i] = specv1.Descriptor{
				MediaType:   spec.MediaType,
				Digest:      srcDesc.Digest,
				Size:        srcDesc.Size,
				Annotations: srcDesc.Annotations,
			}
			if err := WriteLayerMetadataFile(
				spec.MetadataPath,
				srcDiffID.String(),
				spec.MediaType,
				srcDesc.Digest.String(),
				srcDesc.Size,
				srcDesc.Annotations,
				perLayerHistory[i],
			); err != nil {
				return specv1.Descriptor{}, fmt.Errorf("writing metadata for layer %d: %w", i, err)
			}
			continue
		}

		rc, err := v1Layer.Uncompressed()
		if err != nil {
			return specv1.Descriptor{}, fmt.Errorf("opening layer %d tar stream: %w", i, err)
		}
		diffID, blobDigest, blobSize, err := CompressStreamToLayerBlob(rc, spec.MediaType, spec.BlobPath, compOpts)
		if err != nil {
			return specv1.Descriptor{}, fmt.Errorf("converting layer %d: %w", i, err)
		}
		if srcDiffID != "" && diffID != srcDiffID {
			return specv1.Descriptor{}, fmt.Errorf("layer %d diff_id mismatch: config declares %s, tar content hashed to %s", i, srcDiffID, diffID)
		}

		var annotations map[string]string
		if srcDesc != nil {
			annotations = filterRecompressedAnnotations(srcDesc.Annotations)
		}
		if err := WriteLayerMetadataFile(
			spec.MetadataPath,
			diffID.String(),
			spec.MediaType,
			blobDigest.String(),
			blobSize,
			annotations,
			perLayerHistory[i],
		); err != nil {
			return specv1.Descriptor{}, fmt.Errorf("writing metadata for layer %d: %w", i, err)
		}
		layerDescs[i] = specv1.Descriptor{
			MediaType:   spec.MediaType,
			Digest:      blobDigest,
			Size:        blobSize,
			Annotations: annotations,
		}
	}

	if verbatimManifest {
		manifestDigest := digest.FromBytes(img.RawManifest)
		manifestSize := int64(len(img.RawManifest))
		if img.ManifestDesc.Digest != "" && img.ManifestDesc.Digest != manifestDigest {
			return specv1.Descriptor{}, fmt.Errorf("manifest digest mismatch: source declares %s, manifest blob hashed to %s", img.ManifestDesc.Digest, manifestDigest)
		}
		if img.ManifestDesc.Size != 0 && img.ManifestDesc.Size != manifestSize {
			return specv1.Descriptor{}, fmt.Errorf("manifest size mismatch: source declares %d bytes, manifest blob has %d bytes", img.ManifestDesc.Size, manifestSize)
		}
		if err := os.WriteFile(manifestOutput, img.RawManifest, 0o644); err != nil {
			return specv1.Descriptor{}, fmt.Errorf("writing manifest: %w", err)
		}
		if err := os.WriteFile(configOutput, img.RawConfig, 0o644); err != nil {
			return specv1.Descriptor{}, fmt.Errorf("writing config: %w", err)
		}
		return WriteDescriptorAndDigest(
			api.ImageManifestMediaType(img.ManifestDesc.MediaType),
			manifestDigest,
			manifestSize,
			platform,
			descriptorOutput,
			digestOutput,
		)
	}

	// Recompression never changes diff_ids (verified above), so the source config stays byte-for-byte identical.
	return WriteGeneratedManifestAndConfig(
		img.RawConfig,
		layerDescs,
		img.Manifest.Subject,
		img.Manifest.Annotations,
		manifestOutput,
		configOutput,
		descriptorOutput,
		digestOutput,
		platform,
	)
}

// CompressStreamToLayerBlob streams an uncompressed tar reader through rules_img's compressor
// into layerBlobPath and returns (diffID, compressedBlobDigest, compressedSize).
func CompressStreamToLayerBlob(
	rc io.ReadCloser,
	targetMediaType string,
	layerBlobPath string,
	compOpts CompressionOptions,
) (digest.Digest, digest.Digest, int64, error) {
	defer func() {
		if rc != nil {
			_ = rc.Close()
		}
	}()

	compAlg := api.LayerFormat(targetMediaType).CompressionAlgorithm()
	if compAlg == "" {
		return "", "", 0, fmt.Errorf("unsupported layer media type: %s", targetMediaType)
	}

	var opts []compress.Option
	if compOpts.Level >= 0 {
		opts = append(opts, compress.CompressionLevel(compOpts.Level))
	}
	if compOpts.Jobs != "" {
		if compOpts.Jobs == "nproc" {
			opts = append(opts, compress.CompressorJobs(-1))
		} else if n, err := strconv.Atoi(compOpts.Jobs); err == nil {
			opts = append(opts, compress.CompressorJobs(n))
		} else {
			return "", "", 0, fmt.Errorf("invalid --compressor-jobs value %q: %w", compOpts.Jobs, err)
		}
	}
	if compOpts.ZstdWindowLog > 0 {
		opts = append(opts, compress.ZstdWindowLog(compOpts.ZstdWindowLog))
	}

	blobFile, err := os.Create(layerBlobPath)
	if err != nil {
		return "", "", 0, fmt.Errorf("creating layer blob %s: %w", layerBlobPath, err)
	}
	defer func() {
		if blobFile != nil {
			_ = blobFile.Close()
		}
	}()

	appender, err := compress.AppenderFactory(string(api.SHA256), string(compAlg), blobFile, opts...)
	if err != nil {
		return "", "", 0, fmt.Errorf("creating compressor for %s: %w", targetMediaType, err)
	}

	if _, err := io.Copy(appender, rc); err != nil {
		return "", "", 0, fmt.Errorf("streaming layer tar: %w", err)
	}
	src := rc
	rc = nil
	if err := src.Close(); err != nil {
		return "", "", 0, fmt.Errorf("closing layer stream: %w", err)
	}

	state, err := appender.Finalize()
	if err != nil {
		return "", "", 0, fmt.Errorf("finalizing compressed layer: %w", err)
	}
	out := blobFile
	blobFile = nil
	if err := out.Close(); err != nil {
		return "", "", 0, fmt.Errorf("closing layer blob %s: %w", layerBlobPath, err)
	}

	diffID := digest.NewDigestFromBytes(digest.SHA256, state.ContentHash)
	blobDigest := digest.NewDigestFromBytes(digest.SHA256, state.OuterHash)
	return diffID, blobDigest, state.CompressedSize, nil
}

// CopyStreamToFile writes rc directly to dstPath and returns the number of bytes written.
func CopyStreamToFile(rc io.ReadCloser, dstPath string) (int64, error) {
	defer func() {
		if rc != nil {
			_ = rc.Close()
		}
	}()

	dstFile, err := os.Create(dstPath)
	if err != nil {
		return 0, fmt.Errorf("creating %s: %w", dstPath, err)
	}
	defer func() {
		if dstFile != nil {
			_ = dstFile.Close()
		}
	}()

	n, err := io.Copy(dstFile, rc)
	if err != nil {
		return 0, fmt.Errorf("writing %s: %w", dstPath, err)
	}
	src := rc
	rc = nil
	if err := src.Close(); err != nil {
		return 0, fmt.Errorf("closing source stream for %s: %w", dstPath, err)
	}
	out := dstFile
	dstFile = nil
	if err := out.Close(); err != nil {
		return 0, fmt.Errorf("closing %s: %w", dstPath, err)
	}
	return n, nil
}

// WriteLayerMetadataFile writes a SingleLayerInfo metadata JSON file.
func WriteLayerMetadataFile(
	layerMetadataPath string,
	diffID string,
	mediaType string,
	blobDigest string,
	size int64,
	annotations map[string]string,
	history []api.History,
) error {
	f, err := os.Create(layerMetadataPath)
	if err != nil {
		return fmt.Errorf("creating metadata file %s: %w", layerMetadataPath, err)
	}
	if err := metadata.WriteLayerMetadata(diffID, mediaType, blobDigest, size, annotations, history, f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func writeJSON(dstPath string, v any) ([]byte, digest.Digest, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, "", err
	}
	return raw, digest.FromBytes(raw), os.WriteFile(dstPath, raw, 0o644)
}

// WriteGeneratedManifestAndConfig writes rawConfig verbatim, generates the manifest JSON, and writes the manifest descriptor and optional digest.
func WriteGeneratedManifestAndConfig(
	rawConfig []byte,
	layerDescriptors []specv1.Descriptor,
	subject *specv1.Descriptor,
	manifestAnnotations map[string]string,
	manifestOutput string,
	configOutput string,
	descriptorOutput string,
	digestOutput string,
	platform Platform,
) (specv1.Descriptor, error) {
	if err := os.WriteFile(configOutput, rawConfig, 0o644); err != nil {
		return specv1.Descriptor{}, fmt.Errorf("writing config: %w", err)
	}

	manifest := specv1.Manifest{
		Versioned:   specs.Versioned{SchemaVersion: 2},
		MediaType:   specv1.MediaTypeImageManifest,
		Config:      specv1.Descriptor{MediaType: specv1.MediaTypeImageConfig, Digest: digest.FromBytes(rawConfig), Size: int64(len(rawConfig))},
		Layers:      layerDescriptors,
		Subject:     subject,
		Annotations: manifestAnnotations,
	}
	manifestRaw, manifestDigest, err := writeJSON(manifestOutput, manifest)
	if err != nil {
		return specv1.Descriptor{}, fmt.Errorf("writing manifest: %w", err)
	}

	return WriteDescriptorAndDigest(specv1.MediaTypeImageManifest, manifestDigest, int64(len(manifestRaw)), platform, descriptorOutput, digestOutput)
}

// WriteDescriptorAndDigest writes the manifest descriptor JSON and optional plain manifest digest file.
func WriteDescriptorAndDigest(
	mediaType string,
	manifestDigest digest.Digest,
	size int64,
	platform Platform,
	descriptorOutput string,
	digestOutput string,
) (specv1.Descriptor, error) {
	dp := platform.DescriptorPlatform()
	descriptor := specv1.Descriptor{
		MediaType: mediaType,
		Digest:    manifestDigest,
		Size:      size,
		Platform:  &specv1.Platform{Architecture: dp.Architecture, OS: dp.OS, Variant: dp.Variant},
	}
	if _, _, err := writeJSON(descriptorOutput, descriptor); err != nil {
		return specv1.Descriptor{}, fmt.Errorf("writing descriptor: %w", err)
	}
	if digestOutput != "" {
		if err := os.WriteFile(digestOutput, []byte(manifestDigest.String()), 0o644); err != nil {
			return specv1.Descriptor{}, fmt.Errorf("writing digest: %w", err)
		}
	}
	return descriptor, nil
}

// WriteIndex writes an OCI image index JSON, plain digest file, and optional index descriptor JSON.
func WriteIndex(
	manifests []specv1.Descriptor,
	subject *specv1.Descriptor,
	annotations map[string]string,
	indexOutput string,
	digestOutput string,
	descriptorOutput string,
) error {
	idx := specv1.Index{
		Versioned:   specs.Versioned{SchemaVersion: 2},
		MediaType:   specv1.MediaTypeImageIndex,
		Manifests:   manifests,
		Subject:     subject,
		Annotations: annotations,
	}
	raw, dgst, err := writeJSON(indexOutput, idx)
	if err != nil {
		return fmt.Errorf("writing index: %w", err)
	}
	if err := os.WriteFile(digestOutput, []byte(dgst.String()), 0o644); err != nil {
		return fmt.Errorf("writing digest: %w", err)
	}
	if descriptorOutput != "" {
		desc := specv1.Descriptor{MediaType: specv1.MediaTypeImageIndex, Digest: dgst, Size: int64(len(raw))}
		if _, _, err := writeJSON(descriptorOutput, desc); err != nil {
			return fmt.Errorf("writing index descriptor: %w", err)
		}
	}
	return nil
}

// filterRecompressedAnnotations drops the annotations that describe the source blob's byte
// layout -- the estargz TOC and uncompressed size, and the zstd:chunked TOC and tar-split --
// because recompression invalidates them. Mirrors writeMetadata in cmd/compress.
func filterRecompressedAnnotations(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		if k == api.TocDigestAnnotation || k == api.UncompressedSizeAnnotation || strings.HasPrefix(k, api.ZstdChunkedAnnotationPrefix) {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
