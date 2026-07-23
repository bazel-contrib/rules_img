package soci

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	specv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/ztoc"
)

// sociIndexResult holds the serialized outputs of building a SOCI index.
type sociIndexResult struct {
	manifest   []byte
	config     []byte
	descriptor []byte
	digest     digest.Digest
}

// SociIndexProcess builds a SOCI Index Manifest v2 from a set of per-layer ztoc
// blobs. The SOCI index is itself an OCI image manifest whose config is the
// 2-byte "{}" blob (with the v2 artifact media type) and whose layers are the
// ztoc blobs. It links to its image via a com.amazon.soci.index-digest annotation
// stamped onto the image manifest (see `img manifest --soci-index-descriptor`),
// not via the referrers API.
func SociIndexProcess(_ context.Context, args []string) {
	var (
		layers              layerZtocPairs
		spanSize            int64
		minLayerSize        int64
		buildToolIdentifier string
		operatingSystem     string
		architecture        string
		variant             string
		manifestOutput      string
		configOutput        string
		descriptorOutput    string
		digestOutput        string
	)

	flagSet := flag.NewFlagSet("soci-index", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Creates a SOCI Index Manifest v2 from per-layer ztoc blobs.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img soci-index --layer layer-metadata.json=layer.ztoc [--layer ...] --manifest soci.json --config config.json --descriptor descriptor.json\n")
		flagSet.PrintDefaults()
		examples := []string{
			"img soci-index --layer l0.metadata.json=l0.ztoc --layer l1.metadata.json=l1.ztoc --span-size 4194304 --min-layer-size 10485760 --manifest soci.json --config soci-config.json --descriptor soci-descriptor.json",
		}
		fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
		os.Exit(1)
	}
	flagSet.Var(&layers, "layer", `An image layer's metadata file paired with its ztoc blob, as <layer-metadata.json>=<ztoc-path> (repeatable, in image-layer order).`)
	flagSet.Int64Var(&spanSize, "span-size", ztoc.DefaultSpanSize, `The span size (in bytes) the ztocs were generated with. Recorded in the com.amazon.soci.span-size annotation.`)
	flagSet.Int64Var(&minLayerSize, "min-layer-size", 0, `Layers smaller than this many bytes get no ztoc entry and are omitted from the SOCI index. 0 includes every layer with a ztoc.`)
	flagSet.StringVar(&buildToolIdentifier, "build-tool-identifier", ztoc.DefaultBuildToolIdentifier, `Recorded in the com.amazon.soci.build-tool-identifier annotation on the index.`)
	flagSet.StringVar(&operatingSystem, "os", "", `Operating system recorded on the SOCI index descriptor's platform.`)
	flagSet.StringVar(&architecture, "architecture", "", `Architecture recorded on the SOCI index descriptor's platform.`)
	flagSet.StringVar(&variant, "variant", "", `Platform variant recorded on the SOCI index descriptor's platform.`)
	flagSet.StringVar(&manifestOutput, "manifest", "", `Output file for the SOCI index manifest JSON.`)
	flagSet.StringVar(&configOutput, "config", "", `Output file for the SOCI index config blob (the 2-byte "{}").`)
	flagSet.StringVar(&descriptorOutput, "descriptor", "", `Output file for the SOCI index descriptor JSON (mediaType image manifest, artifactType v2, platform).`)
	flagSet.StringVar(&digestOutput, "digest", "", `Optional output file for the SOCI index digest.`)

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}
	if flagSet.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "Unexpected positional arguments: %v\n", flagSet.Args())
		flagSet.Usage()
		os.Exit(1)
	}
	if spanSize <= 0 {
		fmt.Fprintf(os.Stderr, "--span-size must be positive, got %d\n", spanSize)
		os.Exit(1)
	}

	var platform *specv1.Platform
	if operatingSystem != "" || architecture != "" || variant != "" {
		platform = &specv1.Platform{OS: operatingSystem, Architecture: architecture, Variant: variant}
	}

	result, err := buildSociIndex(layers, spanSize, minLayerSize, buildToolIdentifier, platform)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to build SOCI index: %v\n", err)
		os.Exit(1)
	}

	writeOutput := func(path string, data []byte) {
		if path == "" {
			return
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to write %s: %v\n", path, err)
			os.Exit(1)
		}
	}
	writeOutput(manifestOutput, result.manifest)
	writeOutput(configOutput, result.config)
	writeOutput(descriptorOutput, result.descriptor)
	writeOutput(digestOutput, []byte(result.digest.String()))
}

// buildSociIndex assembles the SOCI index manifest, its "{}" config blob, and the
// SOCI index descriptor. Layers whose recorded size is below minLayerSize are
// omitted (matching soci-snapshotter's min-layer-size behavior). The manifest is
// built to match soci-snapshotter's MarshalIndex byte-for-byte: no top-level
// artifactType on the manifest (the type is conveyed by the config media type).
func buildSociIndex(pairs layerZtocPairs, spanSize, minLayerSize int64, buildToolIdentifier string, platform *specv1.Platform) (sociIndexResult, error) {
	ztocDescriptors := make([]specv1.Descriptor, 0, len(pairs))
	for _, pair := range pairs {
		layerDesc, err := readLayerMetadata(pair.metadataPath)
		if err != nil {
			return sociIndexResult{}, fmt.Errorf("reading layer metadata %s: %w", pair.metadataPath, err)
		}
		// Layers below the minimum size get no ztoc entry (matches
		// soci-snapshotter's defaultMinLayerSize behavior).
		if layerDesc.Size < minLayerSize {
			continue
		}
		ztocDesc, err := ztocDescriptor(pair.ztocPath, layerDesc, spanSize)
		if err != nil {
			return sociIndexResult{}, fmt.Errorf("describing ztoc %s: %w", pair.ztocPath, err)
		}
		ztocDescriptors = append(ztocDescriptors, ztocDesc)
	}

	// The SOCI index config is the 2-byte "{}" blob. Its descriptor's media type
	// is the v2 artifact type; this is how the manifest signals it is a SOCI index
	// (soci's MarshalIndex does not set a top-level artifactType on the manifest).
	configRaw := []byte("{}")
	configSHA := sha256.Sum256(configRaw)
	configDescriptor := specv1.Descriptor{
		MediaType: api.SociIndexArtifactTypeV2,
		Digest:    digest.NewDigestFromBytes(digest.SHA256, configSHA[:]),
		Size:      int64(len(configRaw)),
	}

	manifest := specv1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: specv1.MediaTypeImageManifest,
		Config:    configDescriptor,
		Layers:    ztocDescriptors,
		Annotations: map[string]string{
			api.SociBuildToolIdentifierAnnotation: buildToolIdentifier,
		},
	}

	manifestRaw, err := json.Marshal(manifest)
	if err != nil {
		return sociIndexResult{}, fmt.Errorf("marshaling SOCI index manifest: %w", err)
	}
	manifestSHA := sha256.Sum256(manifestRaw)
	manifestDigest := digest.NewDigestFromBytes(digest.SHA256, manifestSHA[:])

	descriptor := specv1.Descriptor{
		MediaType:    specv1.MediaTypeImageManifest,
		ArtifactType: api.SociIndexArtifactTypeV2,
		Digest:       manifestDigest,
		Size:         int64(len(manifestRaw)),
		Platform:     platform,
	}
	descriptorRaw, err := json.Marshal(descriptor)
	if err != nil {
		return sociIndexResult{}, fmt.Errorf("marshaling SOCI index descriptor: %w", err)
	}

	return sociIndexResult{
		manifest:   manifestRaw,
		config:     configRaw,
		descriptor: descriptorRaw,
		digest:     manifestDigest,
	}, nil
}

// ztocDescriptor builds the SOCI index layer descriptor for a single ztoc blob.
func ztocDescriptor(ztocPath string, layerDesc api.Descriptor, spanSize int64) (specv1.Descriptor, error) {
	f, err := os.Open(ztocPath)
	if err != nil {
		return specv1.Descriptor{}, fmt.Errorf("opening ztoc: %w", err)
	}
	defer f.Close()

	hasher := sha256.New()
	size, err := io.Copy(hasher, f)
	if err != nil {
		return specv1.Descriptor{}, fmt.Errorf("hashing ztoc: %w", err)
	}

	return specv1.Descriptor{
		MediaType: api.SociLayerMediaType,
		Digest:    digest.NewDigestFromBytes(digest.SHA256, hasher.Sum(nil)),
		Size:      size,
		Annotations: map[string]string{
			api.SociImageLayerMediaTypeAnnotation: layerDesc.MediaType,
			api.SociImageLayerDigestAnnotation:    layerDesc.Digest,
			api.SociSpanSizeAnnotation:            strconv.FormatInt(spanSize, 10),
		},
	}, nil
}

// readLayerMetadata decodes an image layer's metadata file (as produced by
// `img layer --metadata`).
func readLayerMetadata(filePath string) (api.Descriptor, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return api.Descriptor{}, fmt.Errorf("reading layer metadata file: %w", err)
	}
	var layer api.Descriptor
	if err := json.Unmarshal(data, &layer); err != nil {
		return api.Descriptor{}, fmt.Errorf("decoding layer metadata file: %w", err)
	}
	return layer, nil
}
