package compress

import (
    "bytes"
    "context"
    "encoding/json"
    "flag"
    "fmt"
    "io"
    "os"
    "slices"
    "strconv"
    "runtime"

	"github.com/tweag/rules_img/src/pkg/api"
	"github.com/tweag/rules_img/src/pkg/compress"
	"github.com/tweag/rules_img/src/pkg/fileopener"
)

var (
	layerName          string
	sourceFormat       string
	format             string
	estargzFlag        bool
	metadataOutputFile string
)

func CompressProcess(ctx context.Context, args []string) {
	annotations := make(annotationsFlag)
	var compressorJobsFlag string
	var compressionLevelFlag int
	flagSet := flag.NewFlagSet("compress", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "(Re-)compresses a layer to the chosen format.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img compress [--name name] [--source-format format] [--format format] [--metadata=metadata_output_file] [input] [output]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"img compress --format gzip layer.tar layer.tgz",
			"img compress --source-format gzip --format none --metadata layer.json layer.tgz layer.tar",
		}
		fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
		os.Exit(1)
	}
	flagSet.StringVar(&layerName, "name", "", `Optional name of the layer. Defaults to digest.`)
	flagSet.StringVar(&sourceFormat, "source-format", "", `The format of the source layer. Can be "tar" or "gzip".`)
	flagSet.StringVar(&format, "format", "", `The format of the output layer. Can be "tar" or "gzip".`)
	flagSet.BoolVar(&estargzFlag, "estargz", false, `Use estargz format for compression. This creates seekable gzip streams optimized for lazy pulling.`)
    flagSet.StringVar(&compressorJobsFlag, "compressor-jobs", "1", `Number of compressor jobs. 1 uses single-threaded stdlib gzip. n>1 uses pgzip. "nproc" uses NumCPU.`)
	flagSet.IntVar(&compressionLevelFlag, "compression-level", -1, `Compression level. For gzip: 0-9. If unset, use library default.`)
	flagSet.Var(&annotations, "annotation", `Add an annotation as key=value. Can be specified multiple times.`)
	flagSet.StringVar(&metadataOutputFile, "metadata", "", `Write the metadata to the specified file. The metadata is a JSON file containing info needed to use the layer as part of an OCI image.`)

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

	inputHandle, err := os.Open(layerFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening input layer: %v\n", err)
		os.Exit(1)
	}
	defer inputHandle.Close()

	var reader io.Reader
	var openErr error
	if sourceFormat == "" {
		reader, openErr = fileopener.CompressionReader(inputHandle)
	} else {
		reader, openErr = fileopener.CompressionReaderWithFormat(inputHandle, api.CompressionAlgorithm(sourceFormat))
	}
	if openErr != nil {
		fmt.Fprintf(os.Stderr, "Error opening output layer: %v\n", openErr)
		os.Exit(1)
	}

	var outputFormat api.LayerFormat
	switch format {
	case "tar", "none", "uncompressed":
		outputFormat = api.TarLayer
	case "gzip":
		outputFormat = api.TarGzipLayer
	case "zstd":
		outputFormat = api.TarZstdLayer
	case "":
		fmt.Println("--format flag is required")
		flagSet.Usage()
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "Unsupported output format: %s\n", format)
		os.Exit(1)
	}

	outputHandle, err := os.OpenFile(outputFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening output file: %v\n", err)
		os.Exit(1)
	}
	defer outputHandle.Close()

	compressorState, mediaType, err := recompress(reader, outputHandle, outputFormat, estargzFlag, compressorJobsFlag, compressionLevelFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Recompressing layer: %v\n", err)
		os.Exit(1)
	}

	if len(metadataOutputFile) > 0 {
		metadataOutputHandle, err := os.OpenFile(metadataOutputFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening metadata output file: %v\n", err)
			os.Exit(1)
		}
		defer metadataOutputHandle.Close()
		if err := writeMetadata(compressorState, annotations, mediaType, metadataOutputHandle); err != nil {
			fmt.Fprintf(os.Stderr, "Writing metadata: %v\n", err)
			os.Exit(1)
		}
	}
}

func recompress(input io.Reader, output io.Writer, format api.LayerFormat, estargz bool, compressorJobsFlag string, compressionLevelFlag int) (compressorState api.AppenderState, mediaType string, err error) {
	var CompressionAlgorithm api.CompressionAlgorithm
	switch format {
	case api.TarLayer:
		CompressionAlgorithm = api.Uncompressed
	case api.TarGzipLayer:
		CompressionAlgorithm = api.Gzip
	case api.TarZstdLayer:
		CompressionAlgorithm = api.Zstd
	default:
		return compressorState, "", fmt.Errorf("unsupported compression format: %s", format)
	}
	mediaType = string(format)
	var opts []compress.Option
    if compressionLevelFlag >= 0 {
        opts = append(opts, compress.CompressionLevel(compressionLevelFlag))
    }
    if len(compressorJobsFlag) > 0 {
        if compressorJobsFlag == "nproc" {
            opts = append(opts, compress.CompressorJobs(runtime.NumCPU()))
        } else if n, err := strconv.Atoi(compressorJobsFlag); err == nil {
            opts = append(opts, compress.CompressorJobs(n))
        }
    }
	compressor, err := compress.TarAppenderFactory(string(api.SHA256), string(CompressionAlgorithm), estargz, output, append(opts, compress.ContentType("tar"))...)
	if err != nil {
		return compressorState, "", fmt.Errorf("creating compressor: %w", err)
	}
	defer func() {
		var compressorCloseErr error
		compressorState, compressorCloseErr = compressor.Finalize()
		if compressorCloseErr != nil {
			fmt.Fprintf(os.Stderr, "Error closing compressor: %v\n", compressorCloseErr)
			os.Exit(1)
		}
	}()

	return compressorState, mediaType, compressor.AppendTar(input)
}

func writeMetadata(compressorState api.AppenderState, annotations map[string]string, mediaType string, outputFile io.Writer) error {
	if len(layerName) == 0 {
		layerName = fmt.Sprintf("sha256:%x", compressorState.OuterHash)
	}

	// Merge user annotations with layer annotations from the appender state
	mergedAnnotations := make(map[string]string)
	// First add user annotations in sorted order to ensure determinism
	keys := make([]string, 0, len(annotations))
	for k := range annotations {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		mergedAnnotations[k] = annotations[k]
	}
	// Then add layer annotations from AppenderState (e.g., estargz annotations)
	for k, v := range compressorState.LayerAnnotations {
		mergedAnnotations[k] = v
	}

	metadata := api.Descriptor{
		Name:        layerName,
		DiffID:      fmt.Sprintf("sha256:%x", compressorState.ContentHash),
		MediaType:   mediaType,
		Digest:      fmt.Sprintf("sha256:%x", compressorState.OuterHash),
		Size:        compressorState.CompressedSize,
		Annotations: mergedAnnotations,
	}

	json.NewEncoder(outputFile).SetIndent("", "  ")
	if err := json.NewEncoder(outputFile).Encode(metadata); err != nil {
		return fmt.Errorf("encoding metadata: %w", err)
	}
	return nil
}

func learnFileType(r io.ReaderAt) (api.LayerFormat, error) {
	// poke the first few bytes to see if it is a compressed
	// file or a uncompressed tar file.

	var startMagic [4]byte
	if _, err := r.ReadAt(startMagic[:], 0); err != nil {
		return "", err
	}
	if bytes.Compare(startMagic[:2], gzipMagic[:]) == 0 {
		return api.TarGzipLayer, nil
	}
	if bytes.Compare(startMagic[:4], zstdMagic[:]) == 0 {
		return api.TarZstdLayer, nil
	}

	var tarMagic [8]byte
	if _, err := r.ReadAt(tarMagic[:], 257); err != nil {
		return "", err
	}
	if bytes.Compare(tarMagic[:], tarMagicA[:]) == 0 || bytes.Compare(tarMagic[:], tarMagicB[:]) == 0 {
		return api.TarLayer, nil
	}
	return "", fmt.Errorf("unknown file type")
}

var (
	gzipMagic = [2]byte{0x1f, 0x8b}
	zstdMagic = [4]byte{0x28, 0xb5, 0x2f, 0xfd}
	tarMagicA = [8]byte{0x75, 0x73, 0x74, 0x61, 0x72, 0x00, 0x30, 0x30}
	tarMagicB = [8]byte{0x75, 0x73, 0x74, 0x61, 0x72, 0x20, 0x20, 0x00}
)
