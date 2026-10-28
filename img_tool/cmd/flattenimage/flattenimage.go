package flattenimage

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/opencontainers/go-digest"
	specv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/imagearchive"
)

type stringSliceFlag []string

func (s *stringSliceFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error { *s = append(*s, v); return nil }

// Options configures `img flatten-image`.
type Options struct {
	SrcPath             string
	SourceManifest      string
	SourceConfig        string
	Layers              []string
	Platform            imagearchive.Platform
	Format              string
	History             string
	OmitHistory         bool
	Compression         imagearchive.CompressionOptions
	ManifestOutput      string
	ConfigOutput        string
	DescriptorOutput    string
	DigestOutput        string
	LayerBlobOutput     string
	LayerMetadataOutput string
}

// FlattenImageProcess parses CLI flags and runs `img flatten-image`.
func FlattenImageProcess(_ context.Context, args []string) {
	opts := Options{Compression: imagearchive.CompressionOptions{Level: -1}}
	var layers stringSliceFlag

	fs := flag.NewFlagSet("flatten-image", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Squashes all layers of a container image into a single layer while preserving image configuration.\n\nUsage: img flatten-image [OPTIONS]\n")
		fs.PrintDefaults()
	}
	fs.StringVar(&opts.SrcPath, "src", "", "Path to an uncompressed Docker save .tar, OCI layout .tar, or OCI layout directory")
	fs.StringVar(&opts.SourceManifest, "source-manifest", "", "Path to source OCI manifest JSON (mutually exclusive with --src)")
	fs.StringVar(&opts.SourceConfig, "source-config", "", "Path to source OCI config JSON (used with --source-manifest)")
	fs.Var(&layers, "layer", "Path to source layer blob in manifest order (repeatable; used with --source-manifest)")
	fs.StringVar(&opts.Platform.Architecture, "architecture", "", "Target architecture (required with --src)")
	fs.StringVar(&opts.Platform.OS, "os", "", "Target OS (required with --src)")
	fs.StringVar(&opts.Platform.Variant, "variant", "", "Target platform variant (optional)")
	fs.StringVar(&opts.Format, "format", "zstd", "Compression format or layer media type for the flattened layer ('zstd', 'gzip', 'none')")
	fs.StringVar(&opts.History, "history", "rules_img flatten", "History created_by entry recorded for the flattened layer ('--history=' omits history)")
	fs.StringVar(&opts.Compression.Jobs, "compressor-jobs", "", "Number of concurrent compression jobs ('nproc' or integer)")
	fs.IntVar(&opts.Compression.Level, "compression-level", -1, "Compression level (-1 uses algorithm default)")
	fs.StringVar(&opts.ManifestOutput, "manifest", "", "Output path for the flattened image manifest (required)")
	fs.StringVar(&opts.ConfigOutput, "config", "", "Output path for the flattened image config (required)")
	fs.StringVar(&opts.DescriptorOutput, "descriptor", "", "Output path for the flattened image descriptor (required)")
	fs.StringVar(&opts.DigestOutput, "digest", "", "Output path for the flattened image digest (required)")
	fs.StringVar(&opts.LayerBlobOutput, "layer-blob", "", "Output path for the flattened layer blob (required)")
	fs.StringVar(&opts.LayerMetadataOutput, "layer-metadata", "", "Output path for the flattened layer metadata JSON (required)")

	if err := fs.Parse(args); err != nil {
		fs.Usage()
		os.Exit(1)
	}
	opts.Layers = layers
	if opts.History == "" {
		opts.OmitHistory = true
	}

	if err := validateOptions(opts); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		fs.Usage()
		os.Exit(1)
	}
	if err := Run(opts); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func validateOptions(opts Options) error {
	hasSrc := opts.SrcPath != ""
	hasManifest := opts.SourceManifest != "" || opts.SourceConfig != "" || len(opts.Layers) > 0
	if hasSrc == hasManifest {
		return fmt.Errorf("specify either --src or (--source-manifest and --source-config), but not both")
	}
	if hasSrc {
		if opts.Platform.Architecture == "" || opts.Platform.OS == "" {
			return fmt.Errorf("--architecture and --os are required when --src is set")
		}
	} else if opts.SourceManifest == "" || opts.SourceConfig == "" {
		return fmt.Errorf("both --source-manifest and --source-config are required")
	}
	for _, req := range []struct{ flag, val string }{
		{"--manifest", opts.ManifestOutput},
		{"--config", opts.ConfigOutput},
		{"--descriptor", opts.DescriptorOutput},
		{"--digest", opts.DigestOutput},
		{"--layer-blob", opts.LayerBlobOutput},
		{"--layer-metadata", opts.LayerMetadataOutput},
	} {
		if req.val == "" {
			return fmt.Errorf("%s is required", req.flag)
		}
	}
	return nil
}

// Run squashes all layers of the input image into a single compressed layer and rewrites config.rootfs.diff_ids.
func Run(opts Options) error {
	if err := validateOptions(opts); err != nil {
		return err
	}
	targetMediaType, err := imagearchive.ResolveLayerMediaType(cmp.Or(opts.Format, "zstd"))
	if err != nil {
		return err
	}

	var (
		loaded   *imagearchive.LoadedImage
		platform = opts.Platform
	)
	if opts.SrcPath != "" {
		src, err := imagearchive.OpenSource(opts.SrcPath)
		if err != nil {
			return err
		}
		defer src.Close()
		loaded, err = src.ImageForPlatform(platform)
		if err != nil {
			return err
		}
	} else {
		loaded, err = imagearchive.LoadFromManifestAndBlobs(opts.SourceManifest, opts.SourceConfig, opts.Layers)
		if err != nil {
			return err
		}
		if err := imagearchive.ValidateConfigPlatform(&loaded.Config, platform); err != nil {
			return err
		}
		if platform.Architecture == "" {
			platform.Architecture = loaded.Config.Architecture
		}
		if platform.OS == "" {
			platform.OS = loaded.Config.OS
		}
		if platform.Variant == "" {
			platform.Variant = loaded.Config.Variant
		}
	}

	if err := imagearchive.ValidateTarLayers(loaded); err != nil {
		return fmt.Errorf("flattening layers: %w", err)
	}

	layerHistory := []api.History{}
	var configHistory []specv1.History
	if !opts.OmitHistory {
		createdBy := cmp.Or(opts.History, "rules_img flatten")
		created := loaded.Config.Created
		layerHistory = []api.History{{Created: created, CreatedBy: createdBy, Comment: "flattened image"}}
		configHistory = []specv1.History{{Created: created, CreatedBy: createdBy, Comment: "flattened image"}}
	}

	diffID, blobDigest, blobSize, err := imagearchive.CompressStreamToLayerBlob(
		mutate.Extract(loaded.Image),
		targetMediaType,
		opts.LayerBlobOutput,
		opts.Compression,
	)
	if err != nil {
		return fmt.Errorf("flattening layers: %w", err)
	}

	if err := imagearchive.WriteLayerMetadataFile(
		opts.LayerMetadataOutput,
		diffID.String(),
		targetMediaType,
		blobDigest.String(),
		blobSize,
		nil,
		layerHistory,
	); err != nil {
		return fmt.Errorf("writing flattened layer metadata: %w", err)
	}

	rawConfig, err := flattenedConfig(loaded.RawConfig, diffID, configHistory)
	if err != nil {
		return err
	}
	_, err = imagearchive.WriteGeneratedManifestAndConfig(
		rawConfig,
		[]specv1.Descriptor{{MediaType: targetMediaType, Digest: blobDigest, Size: blobSize}},
		loaded.Manifest.Subject,
		loaded.Manifest.Annotations,
		opts.ManifestOutput,
		opts.ConfigOutput,
		opts.DescriptorOutput,
		opts.DigestOutput,
		platform,
	)
	return err
}

// flattenedConfig replaces rootfs and history in rawConfig and keeps every other field verbatim,
// including Docker-only fields such as Healthcheck, OnBuild, and Shell that specv1.Image drops.
func flattenedConfig(rawConfig []byte, diffID digest.Digest, history []specv1.History) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rawConfig, &fields); err != nil {
		return nil, fmt.Errorf("parsing source config: %w", err)
	}
	rootFS, err := json.Marshal(specv1.RootFS{Type: "layers", DiffIDs: []digest.Digest{diffID}})
	if err != nil {
		return nil, err
	}
	fields["rootfs"] = rootFS
	delete(fields, "history")
	if len(history) > 0 {
		if fields["history"], err = json.Marshal(history); err != nil {
			return nil, err
		}
	}
	return json.Marshal(fields)
}
