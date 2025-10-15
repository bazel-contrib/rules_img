package layermeta

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fileopener"
)

var (
	layerName   string
	annotations annotationsFlag
)

func LayerMetadataProcess(ctx context.Context, args []string) {
	annotations = make(annotationsFlag)
	flagSet := flag.NewFlagSet("layer-metadata", flag.ExitOnError)
	flagSet.Usage = func() {
		_, _ = fmt.Fprintf(flagSet.Output(), "Calculates metadata about an existing layer file.\n\n")
		_, _ = fmt.Fprintf(flagSet.Output(), "Usage: img layer-metadata [--name=name] [--annotation=key=value] [layer] [output]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"img layer-metadata layer.tgz layer.json",
			"img layer-metadata --annotation=foo=bar --annotation=version=1.0 layer.tgz layer.json",
		}
		_, _ = fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			_, _ = fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
		os.Exit(1)
	}
	flagSet.StringVar(&layerName, "name", "", `Optional name of the layer. Defaults to digest.`)
	flagSet.Var(&annotations, "annotation", `Add an annotation as key=value. Can be specified multiple times.`)
	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}

	if flagSet.NArg() != 2 {
		flagSet.Usage()
		os.Exit(1)
	}

	layerFile := flagSet.Arg(0)
	outputFile := flagSet.Arg(1)

	layerFileHandle, err := os.Open(layerFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening layer file: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if err := layerFileHandle.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to close layer file: %v\n", err)
		}
	}()

	hasher := sha256.New()
	compressedSize, err := io.Copy(hasher, layerFileHandle)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading layer file: %v\n", err)
		os.Exit(1)
	}
	_, err = layerFileHandle.Seek(0, io.SeekStart)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error seeking to start of layer file: %v\n", err)
		os.Exit(1)
	}
	digest := hasher.Sum(nil)

	layerFormat, err := fileopener.LearnLayerFormat(layerFileHandle)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error determining layer format: %v\n", err)
		os.Exit(1)
	}

	reader, err := fileopener.CompressionReaderWithFormat(layerFileHandle, layerFormat.CompressionAlgorithm())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening layer file with compression: %v\n", err)
		os.Exit(1)
	}

	layerMetadata, err := calculateLayerMetadata(reader, digest, compressedSize, layerFormat, annotations)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	outputFileHandle, err := os.OpenFile(outputFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening output file: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if err := outputFileHandle.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to close output file: %v\n", err)
		}
	}()

	json.NewEncoder(outputFileHandle).SetIndent("", "  ")
	if err := json.NewEncoder(outputFileHandle).Encode(layerMetadata); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing output file: %v\n", err)
		os.Exit(1)
	}
}

func calculateLayerMetadata(layerFile io.Reader, digest []byte, compressedSize int64, layerFormat api.LayerFormat, annotations map[string]string) (api.Descriptor, error) {
	if len(layerName) == 0 {
		layerName = fmt.Sprintf("sha256:%x", digest)
	}
	if layerFormat == api.TarLayer {
		return api.Descriptor{
			Name:        layerName,
			DiffID:      fmt.Sprintf("sha256:%x", digest),
			MediaType:   api.TarLayer,
			Digest:      fmt.Sprintf("sha256:%x", digest),
			Size:        compressedSize,
			Annotations: annotations,
		}, nil
	}

	hasher := sha256.New()
	_, err := io.Copy(hasher, layerFile)
	if err != nil {
		return api.Descriptor{}, fmt.Errorf("reading layer file: %w", err)
	}
	return api.Descriptor{
		Name:        layerName,
		DiffID:      fmt.Sprintf("sha256:%x", hasher.Sum(nil)),
		MediaType:   string(layerFormat),
		Digest:      fmt.Sprintf("sha256:%x", digest),
		Size:        compressedSize,
		Annotations: annotations,
	}, nil
}
