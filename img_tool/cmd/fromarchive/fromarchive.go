package fromarchive

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	specv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/imagearchive"
)

// LayerOutputSpec specifies the desired media type and output paths for a single layer index.
type LayerOutputSpec = imagearchive.LayerOutputSpec

// ManifestOptions configures `img manifest-from-archive`.
type ManifestOptions struct {
	SrcPath          string
	ManifestOutput   string
	ConfigOutput     string
	DescriptorOutput string
	DigestOutput     string
	Platform         imagearchive.Platform
	Layers           []LayerOutputSpec
	Compression      imagearchive.CompressionOptions
}

// ManifestOutputSpec specifies the target platform, manifest/config/descriptor outputs, and layer outputs
// for a single manifest position in a multi-platform index.
type ManifestOutputSpec struct {
	Platform         imagearchive.Platform
	ManifestOutput   string
	ConfigOutput     string
	DescriptorOutput string
	Layers           []LayerOutputSpec
}

// IndexOptions configures `img index-from-archive`.
type IndexOptions struct {
	SrcPath               string
	IndexOutput           string
	DigestOutput          string
	IndexDescriptorOutput string
	Manifests             []ManifestOutputSpec
	Compression           imagearchive.CompressionOptions
}

// ManifestProcess parses CLI flags and runs `img manifest-from-archive`.
func ManifestProcess(_ context.Context, args []string) {
	opts := ManifestOptions{Compression: imagearchive.CompressionOptions{Level: -1}}
	layerMediaTypes := make(indexedStringFlag)
	layerBlobs := make(indexedStringFlag)
	layerMetadataJSON := make(indexedStringFlag)

	fs := flag.NewFlagSet("manifest-from-archive", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Converts an uncompressed Docker save or OCI layout .tar archive to an image manifest.\n\nUsage: img manifest-from-archive [OPTIONS]\n")
		fs.PrintDefaults()
	}
	fs.StringVar(&opts.SrcPath, "src", "", "Path to the uncompressed Docker save or OCI layout .tar archive (required)")
	fs.StringVar(&opts.ManifestOutput, "manifest", "", "Output path for the image manifest (required)")
	fs.StringVar(&opts.ConfigOutput, "config", "", "Output path for the image config (required)")
	fs.StringVar(&opts.DescriptorOutput, "descriptor", "", "Output path for the manifest descriptor (required)")
	fs.StringVar(&opts.DigestOutput, "digest", "", "Output path for the manifest digest (required)")
	fs.StringVar(&opts.Platform.Architecture, "architecture", "", "Target architecture (required)")
	fs.StringVar(&opts.Platform.OS, "os", "", "Target OS (required)")
	fs.StringVar(&opts.Platform.Variant, "variant", "", "Target variant (optional)")
	fs.Var(&layerMediaTypes, "layer_media_type", "Layer media type in format index=mediatype")
	fs.Var(&layerBlobs, "layer_blob", "Output path for layer blob in format index=path")
	fs.Var(&layerMetadataJSON, "layer_metadata_json", "Output path for layer metadata JSON in format index=path")
	fs.StringVar(&opts.Compression.Jobs, "compressor-jobs", "", "Number of concurrent compression jobs ('nproc' or integer)")
	fs.IntVar(&opts.Compression.Level, "compression-level", -1, "Compression level (-1 uses algorithm default)")

	if err := fs.Parse(args); err != nil {
		fs.Usage()
		os.Exit(1)
	}
	for _, req := range []struct{ flag, val string }{
		{"--src", opts.SrcPath},
		{"--manifest", opts.ManifestOutput},
		{"--config", opts.ConfigOutput},
		{"--descriptor", opts.DescriptorOutput},
		{"--digest", opts.DigestOutput},
		{"--architecture", opts.Platform.Architecture},
		{"--os", opts.Platform.OS},
	} {
		if req.val == "" {
			failFlag(fs, "%s is required", req.flag)
		}
	}

	n := len(layerMediaTypes)
	if n == 0 {
		failFlag(fs, "at least one --layer_media_type is required")
	}
	if len(layerBlobs) != n || len(layerMetadataJSON) != n {
		failFlag(fs, "mismatched layer flag counts: have %d --layer_media_type, %d --layer_blob, %d --layer_metadata_json", n, len(layerBlobs), len(layerMetadataJSON))
	}
	opts.Layers = make([]LayerOutputSpec, n)
	for i := range n {
		mt, ok1 := layerMediaTypes[i]
		blob, ok2 := layerBlobs[i]
		meta, ok3 := layerMetadataJSON[i]
		if !ok1 || !ok2 || !ok3 {
			failFlag(fs, "incomplete layer specification at index %d", i)
		}
		opts.Layers[i] = LayerOutputSpec{MediaType: mt, BlobPath: blob, MetadataPath: meta}
	}

	if err := RunManifest(opts); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// RunManifest converts an uncompressed Docker save or OCI layout tar archive into a single-platform manifest.
func RunManifest(opts ManifestOptions) error {
	src, err := imagearchive.OpenArchive(opts.SrcPath, "manifest-from-oci-layout")
	if err != nil {
		return err
	}
	defer src.Close()

	img, err := src.ImageForPlatform(opts.Platform)
	if err != nil {
		return err
	}
	_, err = imagearchive.ConvertImage(
		img,
		opts.Layers,
		opts.Compression,
		opts.ManifestOutput,
		opts.ConfigOutput,
		opts.DescriptorOutput,
		opts.DigestOutput,
		opts.Platform,
	)
	return err
}

// IndexProcess parses CLI flags and runs `img index-from-archive`.
func IndexProcess(_ context.Context, args []string) {
	opts := IndexOptions{Compression: imagearchive.CompressionOptions{Level: -1}}
	manifestPaths := make(indexedStringFlag)
	configPaths := make(indexedStringFlag)
	descriptorPaths := make(indexedStringFlag)
	osList := make(indexedStringFlag)
	archList := make(indexedStringFlag)
	variantList := make(indexedStringFlag)
	layerMediaTypes := make(doubleIndexedStringFlag)
	layerBlobs := make(doubleIndexedStringFlag)
	layerMetadataJSON := make(doubleIndexedStringFlag)

	fs := flag.NewFlagSet("index-from-archive", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Converts an uncompressed multi-platform OCI layout or Docker save .tar archive to an image index.\n\nUsage: img index-from-archive [OPTIONS]\n")
		fs.PrintDefaults()
	}
	fs.StringVar(&opts.SrcPath, "src", "", "Path to the uncompressed multi-platform archive (required)")
	fs.StringVar(&opts.IndexOutput, "index", "", "Output path for the image index JSON (required)")
	fs.StringVar(&opts.DigestOutput, "digest", "", "Output path for the index digest (required)")
	fs.StringVar(&opts.IndexDescriptorOutput, "index-descriptor", "", "Output path for the index descriptor JSON")
	fs.Var(&manifestPaths, "manifest", "Output path for manifest in format index=path")
	fs.Var(&configPaths, "config", "Output path for config in format index=path")
	fs.Var(&descriptorPaths, "descriptor", "Output path for descriptor in format index=path")
	fs.Var(&osList, "os", "Target OS in format index=os")
	fs.Var(&archList, "architecture", "Target architecture in format index=arch")
	fs.Var(&variantList, "variant", "Target variant in format index=variant")
	fs.Var(&layerMediaTypes, "layer_media_type", "Layer media type in format manifest_idx,layer_idx=mediatype")
	fs.Var(&layerBlobs, "layer_blob", "Output path for layer blob in format manifest_idx,layer_idx=path")
	fs.Var(&layerMetadataJSON, "layer_metadata_json", "Output path for layer metadata JSON in format manifest_idx,layer_idx=path")
	fs.StringVar(&opts.Compression.Jobs, "compressor-jobs", "", "Number of concurrent compression jobs ('nproc' or integer)")
	fs.IntVar(&opts.Compression.Level, "compression-level", -1, "Compression level (-1 uses algorithm default)")

	if err := fs.Parse(args); err != nil {
		fs.Usage()
		os.Exit(1)
	}
	for _, req := range []struct{ flag, val string }{
		{"--src", opts.SrcPath},
		{"--index", opts.IndexOutput},
		{"--digest", opts.DigestOutput},
	} {
		if req.val == "" {
			failFlag(fs, "%s is required", req.flag)
		}
	}

	numManifests := len(manifestPaths)
	if numManifests == 0 {
		failFlag(fs, "at least one --manifest is required")
	}
	if len(configPaths) != numManifests || len(descriptorPaths) != numManifests || len(osList) != numManifests || len(archList) != numManifests {
		failFlag(fs, "all per-manifest flags (--manifest, --config, --descriptor, --os, --architecture) must specify %d entries", numManifests)
	}
	for k := range variantList {
		if k >= numManifests {
			failFlag(fs, "--variant index %d out of range (have %d manifests)", k, numManifests)
		}
	}
	if len(layerMediaTypes) != numManifests || len(layerBlobs) != numManifests || len(layerMetadataJSON) != numManifests {
		failFlag(fs, "layer flags must cover all %d manifests", numManifests)
	}

	opts.Manifests = make([]ManifestOutputSpec, numManifests)
	for mIdx := range numManifests {
		mPath, ok1 := manifestPaths[mIdx]
		cPath, ok2 := configPaths[mIdx]
		dPath, ok3 := descriptorPaths[mIdx]
		osVal, ok4 := osList[mIdx]
		archVal, ok5 := archList[mIdx]
		if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
			failFlag(fs, "incomplete manifest specification at index %d", mIdx)
		}
		lCount := len(layerMediaTypes[mIdx])
		if lCount == 0 || len(layerBlobs[mIdx]) != lCount || len(layerMetadataJSON[mIdx]) != lCount {
			failFlag(fs, "invalid or mismatched layer flags for manifest %d", mIdx)
		}
		layers := make([]LayerOutputSpec, lCount)
		for lIdx := range lCount {
			mt, lok1 := layerMediaTypes[mIdx][lIdx]
			blob, lok2 := layerBlobs[mIdx][lIdx]
			meta, lok3 := layerMetadataJSON[mIdx][lIdx]
			if !lok1 || !lok2 || !lok3 {
				failFlag(fs, "incomplete layer specification at %d,%d", mIdx, lIdx)
			}
			layers[lIdx] = LayerOutputSpec{MediaType: mt, BlobPath: blob, MetadataPath: meta}
		}
		opts.Manifests[mIdx] = ManifestOutputSpec{
			Platform:         imagearchive.Platform{OS: osVal, Architecture: archVal, Variant: variantList[mIdx]},
			ManifestOutput:   mPath,
			ConfigOutput:     cPath,
			DescriptorOutput: dPath,
			Layers:           layers,
		}
	}

	if err := RunIndex(opts); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// RunIndex converts an uncompressed multi-platform OCI layout or Docker save tar archive into an image index.
func RunIndex(opts IndexOptions) error {
	if len(opts.Manifests) == 0 {
		return fmt.Errorf("at least one manifest specification is required")
	}
	src, err := imagearchive.OpenArchive(opts.SrcPath, "index-from-oci-layout")
	if err != nil {
		return err
	}
	defer src.Close()

	if src.ManifestCount() != len(opts.Manifests) {
		return fmt.Errorf("manifest count mismatch: archive has %d manifests, but %d were specified", src.ManifestCount(), len(opts.Manifests))
	}

	descriptors := make([]specv1.Descriptor, len(opts.Manifests))
	for mIdx, mSpec := range opts.Manifests {
		img, err := src.ImageAt(mIdx, mSpec.Platform)
		if err != nil {
			return err
		}
		desc, err := imagearchive.ConvertImage(
			img,
			mSpec.Layers,
			opts.Compression,
			mSpec.ManifestOutput,
			mSpec.ConfigOutput,
			mSpec.DescriptorOutput,
			"",
			mSpec.Platform,
		)
		if err != nil {
			return fmt.Errorf("converting manifest %d: %w", mIdx, err)
		}
		descriptors[mIdx] = desc
	}

	return imagearchive.WriteIndex(
		descriptors,
		src.IndexSubject(),
		src.IndexAnnotations(),
		opts.IndexOutput,
		opts.DigestOutput,
		opts.IndexDescriptorOutput,
	)
}

func failFlag(fs *flag.FlagSet, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "Error: "+format+"\n", args...)
	fs.Usage()
	os.Exit(1)
}

type indexedStringFlag map[int]string

func (f *indexedStringFlag) String() string { return "" }

func (f *indexedStringFlag) Set(value string) error {
	key, val, ok := strings.Cut(value, "=")
	if !ok {
		return fmt.Errorf("expected index=value, got %s", value)
	}
	idx, err := strconv.Atoi(key)
	if err != nil || idx < 0 {
		return fmt.Errorf("invalid non-negative index %q", key)
	}
	(*f)[idx] = val
	return nil
}

type doubleIndexedStringFlag map[int]map[int]string

func (f *doubleIndexedStringFlag) String() string { return "" }

func (f *doubleIndexedStringFlag) Set(value string) error {
	key, val, ok := strings.Cut(value, "=")
	if !ok {
		return fmt.Errorf("expected manifest_idx,layer_idx=value, got %s", value)
	}
	k1, k2, ok := strings.Cut(key, ",")
	if !ok {
		return fmt.Errorf("expected two comma-separated indices, got %s", key)
	}
	idx1, err1 := strconv.Atoi(k1)
	idx2, err2 := strconv.Atoi(k2)
	if err1 != nil || idx1 < 0 || err2 != nil || idx2 < 0 {
		return fmt.Errorf("invalid non-negative indices %q", key)
	}
	if (*f)[idx1] == nil {
		(*f)[idx1] = make(map[int]string)
	}
	(*f)[idx1][idx2] = val
	return nil
}
