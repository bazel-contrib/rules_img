// Package ocilayoutmetadata implements the `img oci-layout-metadata` subcommand.
//
// Given an OCI image layout directory (a tree artifact, e.g. the output of a
// rules_oci oci_image / oci_image_index), it extracts -- for every platform
// manifest it finds -- the image config JSON and a merged mtree of the image
// filesystem, writing them plus an images.json index into an output directory.
//
// It reads the layer blobs (at build time) to compute the mtree, but the layers
// themselves are never emitted: only the small config + mtree metadata is. The
// image_structure_test aspect uses this so a rules_oci image can be validated
// without shipping any layer to the test runfiles.
package ocilayoutmetadata

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	specv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/mtree"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/structuretest"
)

// index/manifest media types we recognize (OCI and Docker schema 2).
const (
	dockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	dockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
)

// OCILayoutMetadataProcess is the entry point for `img oci-layout-metadata`.
func OCILayoutMetadataProcess(_ context.Context, args []string) {
	var srcDir, outputDir, pathPrefix, options, imageLayout string
	defaults := mtree.DefaultOptions()

	flagSet := flag.NewFlagSet("oci-layout-metadata", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Extracts per-platform config JSON and mtree from an OCI image layout.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img oci-layout-metadata --src <oci-layout-dir> --output <dir>\n")
		flagSet.PrintDefaults()
	}
	flagSet.StringVar(&srcDir, "src", "", "Path to the OCI image layout directory (required).")
	flagSet.StringVar(&outputDir, "output", "", "Output directory for config/mtree/images.json (required).")
	flagSet.StringVar(&pathPrefix, "path-prefix", defaults.PathPrefix, `mtree entry path prefix: "./" or "".`)
	flagSet.StringVar(&options, "options", strings.Join(defaults.Keywords, ","), "Comma-separated mtree fields to emit.")
	flagSet.StringVar(&imageLayout, "image-layout", mtree.LayoutOCIChangeset, "mtree layout: \"tar\" or \"oci_layer_filesystem_applied_changeset\".")

	if err := flagSet.Parse(args); err != nil {
		os.Exit(1)
	}
	if srcDir == "" || outputDir == "" {
		fmt.Fprintln(os.Stderr, "Error: --src and --output are required")
		flagSet.Usage()
		os.Exit(1)
	}

	opts := mtree.Options{PathPrefix: pathPrefix, Keywords: splitOptions(options), Layout: imageLayout}
	if err := run(srcDir, outputDir, opts); err != nil {
		fmt.Fprintf(os.Stderr, "oci-layout-metadata: %v\n", err)
		os.Exit(1)
	}
}

func run(srcDir, outputDir string, opts mtree.Options) error {
	index, err := readIndex(filepath.Join(srcDir, "index.json"))
	if err != nil {
		return err
	}
	manifestDescs, err := collectManifests(srcDir, index, 0)
	if err != nil {
		return err
	}
	if len(manifestDescs) == 0 {
		return fmt.Errorf("no image manifests found in OCI layout %s", srcDir)
	}

	var images []structuretest.ImageSpec
	for i, desc := range manifestDescs {
		image, err := processManifest(srcDir, outputDir, i, desc, opts)
		if err != nil {
			return fmt.Errorf("manifest %d: %w", i, err)
		}
		images = append(images, image)
	}

	spec := structuretest.Spec{Images: images}
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling images.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "images.json"), data, 0o644); err != nil {
		return fmt.Errorf("writing images.json: %w", err)
	}
	return nil
}

// collectManifests walks the index, recursing into nested indexes, and returns
// every image-manifest descriptor (platform preserved from its parent index).
func collectManifests(srcDir string, index *specv1.Index, depth int) ([]specv1.Descriptor, error) {
	if depth > 8 {
		return nil, fmt.Errorf("OCI layout index nesting too deep")
	}
	var out []specv1.Descriptor
	for _, desc := range index.Manifests {
		switch desc.MediaType {
		case specv1.MediaTypeImageIndex, dockerManifestList:
			nested, err := readIndex(blobPath(srcDir, desc))
			if err != nil {
				return nil, err
			}
			children, err := collectManifests(srcDir, nested, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, children...)
		default:
			// Treat anything else as an image manifest (OCI or Docker schema 2).
			out = append(out, desc)
		}
	}
	return out, nil
}

// processManifest writes <output>/<i>/config.json and <output>/<i>/image.mtree
// for a single manifest and returns its ImageSpec (with paths relative to the
// output tree).
func processManifest(srcDir, outputDir string, i int, desc specv1.Descriptor, opts mtree.Options) (structuretest.ImageSpec, error) {
	var manifest specv1.Manifest
	if err := readJSONBlob(blobPath(srcDir, desc), &manifest); err != nil {
		return structuretest.ImageSpec{}, fmt.Errorf("reading manifest: %w", err)
	}

	subdir := strconv.Itoa(i)
	if err := os.MkdirAll(filepath.Join(outputDir, subdir), 0o755); err != nil {
		return structuretest.ImageSpec{}, err
	}

	// Copy the config blob and read it for the platform fallback.
	configRel := filepath.Join(subdir, "config.json")
	configSrc := blobPath(srcDir, manifest.Config)
	if err := copyFile(configSrc, filepath.Join(outputDir, configRel)); err != nil {
		return structuretest.ImageSpec{}, fmt.Errorf("copying config: %w", err)
	}
	var config specv1.Image
	if err := readJSONBlob(configSrc, &config); err != nil {
		return structuretest.ImageSpec{}, fmt.Errorf("reading config: %w", err)
	}

	// Build the merged mtree from the tar layers.
	mtreeRel := filepath.Join(subdir, "image.mtree")
	complete, err := writeLayersMtree(srcDir, manifest.Layers, filepath.Join(outputDir, mtreeRel), opts)
	if err != nil {
		return structuretest.ImageSpec{}, fmt.Errorf("building mtree: %w", err)
	}

	return structuretest.ImageSpec{
		Platform: platformFor(desc, config),
		Config:   configRel,
		Mtree:    mtreeRel,
		Complete: complete,
	}, nil
}

// writeLayersMtree renders the merged mtree of the tar layers to outPath and
// reports whether every layer was a tar (a skipped non-tar layer makes the mtree
// an incomplete filesystem view).
func writeLayersMtree(srcDir string, layers []specv1.Descriptor, outPath string, opts mtree.Options) (complete bool, err error) {
	var closers []io.Closer
	defer func() {
		for i := len(closers) - 1; i >= 0; i-- {
			if cerr := closers[i].Close(); cerr != nil && err == nil {
				err = cerr
			}
		}
	}()

	complete = true
	var inputs []mtree.Input
	for _, layer := range layers {
		if !strings.Contains(layer.MediaType, "tar") {
			complete = false
			continue
		}
		f, oerr := os.Open(blobPath(srcDir, layer))
		if oerr != nil {
			return false, fmt.Errorf("opening layer %s: %w", layer.Digest, oerr)
		}
		closers = append(closers, f)
		reader, derr := mtree.Decompress(f)
		if derr != nil {
			return false, fmt.Errorf("decompressing layer %s: %w", layer.Digest, derr)
		}
		inputs = append(inputs, mtree.Input{Kind: mtree.TarInput, Reader: reader, Digester: mtree.HashContent})
	}

	out, cerr := os.Create(outPath)
	if cerr != nil {
		return false, cerr
	}
	closers = append(closers, out)
	if werr := mtree.WriteMulti(out, opts, inputs); werr != nil {
		return false, werr
	}
	return complete, nil
}

// platformFor resolves an image's platform from the manifest descriptor, falling
// back to the config for a single-image layout whose descriptor has no platform.
func platformFor(desc specv1.Descriptor, config specv1.Image) structuretest.Platform {
	os_, arch, variant := config.OS, config.Architecture, config.Variant
	if desc.Platform != nil {
		if desc.Platform.OS != "" {
			os_ = desc.Platform.OS
		}
		if desc.Platform.Architecture != "" {
			arch = desc.Platform.Architecture
		}
		if desc.Platform.Variant != "" {
			variant = desc.Platform.Variant
		}
	}
	return structuretest.Platform{OS: os_, Architecture: arch, Variant: variant}
}

func blobPath(srcDir string, desc specv1.Descriptor) string {
	return filepath.Join(srcDir, "blobs", desc.Digest.Algorithm().String(), desc.Digest.Hex())
}

func readIndex(path string) (*specv1.Index, error) {
	var index specv1.Index
	if err := readJSON(path, &index); err != nil {
		return nil, fmt.Errorf("reading index %s: %w", path, err)
	}
	return &index, nil
}

func readJSONBlob(path string, v any) error { return readJSON(path, v) }

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}

func splitOptions(options string) []string {
	var out []string
	for _, part := range strings.Split(options, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
