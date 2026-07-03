package layer

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math"

	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compress"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/contentmanifest"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/digestfs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/kvfile"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/metadata"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tarcas"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree/runfiles"
)

func LayerProcess(ctx context.Context, args []string) {
	annotations := make(annotationsFlag)
	var layerHistory string
	var annotationsFile string
	var addFiles addFiles
	var addFromFile addFromFileArgs
	var placeFilesFlags placeFilesArgs
	var importTarFlags importTars
	var runfilesFlags runfilesForExecutables
	var executableFlags executables
	var symlinkFlags symlinks
	var symlinksFromFiles symlinksFromFileArgs
	var symlinkPairsFromFiles symlinkPairsFromFileArgs
	var emptyFilesFromFiles emptyFilesFromFileArgs
	var contentManifestInputFlags contentManifests
	var contentManifestCollection string
	var formatFlag string
	var estargzFlag bool
	var mediaTypeFlag string
	var metadataOutputFlag string
	var contentManifestOutputFlag string
	var defaultMetadataFlag string
	var compressorJobsFlag string
	var compressionLevelFlag int
	var createParentDirectoriesFlag bool
	var treeArtifactHandlingFlag string
	var compactStreamOutputFlag string
	var compactStreamOnlyFlag bool
	var compactStreamInlineThresholdFlag uint64
	fileMetadataFlags := make(fileMetadataFlag)

	flagSet := flag.NewFlagSet("layer", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Creates a compressed tar file which can be used as a container image layer while deduplicating the contents.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img layer [OPTIONS] [output]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"img layer --add /etc/passwd=./passwd --executable /bin/myapp=./myapp layer.tgz",
			"img layer --add-from-file param_file.txt layer.tgz",
			"img layer --add --executable /bin/app=./app --runfiles ./app=runfiles_list.txt layer.tgz",
		}
		fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
		os.Exit(1)
	}
	flagSet.StringVar(&layerHistory, "history", "", `Optional created_by string recorded in the layer's history (e.g. "bazel build //pkg:target"). Defaults to a "history missing" marker.`)
	flagSet.Var(&addFiles, "add", `Add a file to the image layer. The parameter is a string of the form <path_in_image>=<file> where <path_in_image> is the path in the image and <file> is the path in the host filesystem.`)
	flagSet.Var(&addFromFile, "add-from-file", `Add all files listed in the parameter file to the image layer. The parameter file is usually written by Bazel.
The file contains one line per file, where each line contains a path in the image and a path in the host filesystem, separated by a a null byte and a single character indicating the type of the file.
The type is either 'f' for regular files, 'd' for directories. The parameter file is usually written by Bazel.`)
	flagSet.Var(&placeFilesFlags, "place-files", `Add files described by a self-describing placement parameter file. The first line is a header (mode, dest, anchor, skip; null-separated) carrying per-target placement context; each following line is a file in the same encoding as --add-from-file but keyed by rebased short_path. Used to lazily place a target's default outputs relative to its executable or package. The parameter file is usually written by Bazel.`)
	flagSet.Var(&importTarFlags, "import-tar", `Import all files from the given tar file into the image layer while deduplicating the contents.`)
	flagSet.Var(&executableFlags, "executable", `Add the executable file at the specified path in the image. This should be combined with the --runfiles flag to include the runfiles of the executable.`)
	flagSet.Var(&runfilesFlags, "runfiles", `Add the runfiles of an executable file. The runfiles are read from the specified parameter file with the same encoding used by --add-from-file. The parameter file is usually written by Bazel.`)
	flagSet.Var(&symlinkFlags, "symlink", `Add a symlink to the image layer. The parameter is a string of the form <path_in_image>=<target> where <path_in_image> is the path in the image and <target> is the target of the symlink.`)
	flagSet.Var(&symlinksFromFiles, "symlinks-from-file", `Add all symlinks listed in the parameter file to the image layer. The parameter file is usually written by Bazel.`)
	flagSet.Var(&symlinkPairsFromFiles, "symlink-pairs-from-file", `Add symlinks from a parameter file where each line has three null-separated fields: source_prefix, dest_prefix, dir_name. Creates symlink source_prefix/dir_name -> dest_prefix/dir_name.`)
	flagSet.Var(&emptyFilesFromFiles, "empty-files-from-file", `Create zero-size regular files at paths listed in the parameter file (one path per line).`)
	flagSet.Var(&contentManifestInputFlags, "deduplicate", `Path of a content manifest of a previous layer that can be used for deduplication.`)
	flagSet.StringVar(&contentManifestCollection, "deduplicate-collection", "", `Path of a content manifest collection file that can be used for deduplication.`)
	flagSet.StringVar(&formatFlag, "format", "", `The compression format of the output layer. Can be "gzip", "zstd", or "none". Default is to guess the algorithm based on the filename, but fall back to "gzip".`)
	flagSet.BoolVar(&estargzFlag, "estargz", false, `Use estargz format for compression. This creates seekable gzip streams optimized for lazy pulling.`)
	flagSet.StringVar(&mediaTypeFlag, "media-type", "", `Override the layer media type in the metadata output. If empty, auto-detected from the compression format.`)
	flagSet.StringVar(&compressorJobsFlag, "compressor-jobs", "1", `Number of compressor jobs. 1 uses single-threaded stdlib gzip. n>1 uses pgzip. "nproc" uses NumCPU.`)
	flagSet.IntVar(&compressionLevelFlag, "compression-level", -1, `Compression level. For gzip: 0-9. If unset, use library default.`)
	flagSet.Var(&annotations, "annotation", `Add an annotation as key=value. Can be specified multiple times.`)
	flagSet.StringVar(&annotationsFile, "annotations-file", "", `File containing annotations, as JSON ({"key":"value"}, {"key":["v1","v2"]}, or ["key=value"]) or newline-delimited KEY=VALUE text. Annotations from the file are merged with those specified via --annotation, which take precedence.`)
	flagSet.StringVar(&metadataOutputFlag, "metadata", "", `Write the metadata to the specified file. The metadata is a JSON file containing info needed to use the layer as part of an OCI image.`)
	flagSet.StringVar(&contentManifestOutputFlag, "content-manifest", "", `Write a manifest of the contents of the layer to the specified file. The manifest uses a custom binary format listing all blobs, nodes, and trees in the layer after deduplication.`)
	flagSet.StringVar(&defaultMetadataFlag, "default-metadata", "", `JSON-encoded default metadata to apply to all files in the layer. Can include fields like mode, uid, gid, uname, gname, mtime, and pax_records.`)
	flagSet.Var(&fileMetadataFlags, "file-metadata", `Per-file metadata override in the format path=json. Can be specified multiple times. Overrides any defaults from --default-metadata.`)
	flagSet.BoolVar(&createParentDirectoriesFlag, "create-parent-directories", false, `Create parent directory entries in the tar file for all files. Default is false.`)
	flagSet.StringVar(&treeArtifactHandlingFlag, "layer-tree-artifact-handling", "full", `How to handle duplicate tree artifacts. "full" stores each tree at its path. "deduplicate_symlink" replaces duplicates with symlinks.`)
	flagSet.StringVar(&compactStreamOutputFlag, "compact-stream", "", `Write a compact stream representation of the layer alongside the tar output. The compact stream records raw tar headers with content digests in an optionally zstd-compressed format, enabling bit-for-bit tar reconstruction from a content-addressed store.`)
	flagSet.BoolVar(&compactStreamOnlyFlag, "compact-stream-only", false, `Only produce the compact stream and metadata; do not write the tar output file. Requires --compact-stream.`)
	flagSet.Uint64Var(&compactStreamInlineThresholdFlag, "compact-stream-inline-threshold", 0, `Maximum file size (in bytes) to store inline in the compact stream. Files smaller than this threshold have their content stored directly in the byte stream instead of as a CAS reference. 0 disables inlining.`)

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}

	if compactStreamOnlyFlag && compactStreamOutputFlag == "" {
		fmt.Fprintf(os.Stderr, "Error: --compact-stream-only requires --compact-stream\n")
		os.Exit(1)
	}

	if !compactStreamOnlyFlag && flagSet.NArg() != 1 {
		flagSet.Usage()
		os.Exit(1)
	}
	if compactStreamOnlyFlag && flagSet.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "Error: --compact-stream-only does not accept a positional output argument\n")
		os.Exit(1)
	}

	var outputFilePath string
	if !compactStreamOnlyFlag {
		outputFilePath = flagSet.Arg(0)
	}

	// Read annotations from file if provided
	if annotationsFile != "" {
		fileAnnotations, err := readAnnotationsFile(annotationsFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading annotations file: %v\n", err)
			os.Exit(1)
		}
		// Merge file annotations with command-line annotations
		// Command-line annotations take precedence
		for k, v := range fileAnnotations {
			if _, exists := annotations[k]; !exists {
				annotations[k] = v
			}
		}
	}

	var compressionAlgorithm api.CompressionAlgorithm
	switch formatFlag {
	case "":
		if compactStreamOnlyFlag {
			compressionAlgorithm = api.Gzip
		} else if filepath.Ext(outputFilePath) == ".tar" {
			compressionAlgorithm = api.Uncompressed
		} else if filepath.Ext(outputFilePath) == ".tgz" || filepath.Ext(outputFilePath) == ".gz" {
			compressionAlgorithm = api.Gzip
		} else if filepath.Ext(outputFilePath) == ".zst" {
			compressionAlgorithm = api.Zstd
		} else {
			compressionAlgorithm = api.Gzip
		}
	case "gzip":
		compressionAlgorithm = api.Gzip
	case "zstd":
		compressionAlgorithm = api.Zstd
	case "none", "uncompressed", "tar":
		compressionAlgorithm = api.Uncompressed
	default:
		fmt.Fprintf(os.Stderr, "Unknown format %s. Supported formats are gzip, zstd and uncompressed.\n", formatFlag)
		os.Exit(1)
	}

	var outputFile io.Writer
	if compactStreamOnlyFlag {
		outputFile = io.Discard
	} else {
		f, err := os.OpenFile(outputFilePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening output file: %v\n", err)
			os.Exit(1)
		}
		defer func() {
			if err := f.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "Error closing output file: %v\n", err)
				os.Exit(1)
			}
		}()
		outputFile = f
	}

	// Parse layer metadata
	layerMetadata, err := ParseLayerMetadata(defaultMetadataFlag, fileMetadataFlags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing metadata: %v\n", err)
		os.Exit(1)
	}

	// read the addFromFile parameter file and create a list of operations
	for _, paramFile := range addFromFile {
		addFileOpsFromParamFile, err := readParamFile(paramFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading parameter file: %v\n", err)
			os.Exit(1)
		}
		addFiles = append(addFiles, addFileOpsFromParamFile...)
	}

	// read the placeFiles parameter files (self-describing placement specs) and
	// resolve them into add operations.
	for _, paramFile := range placeFilesFlags {
		placedOps, err := readPlaceFilesParamFile(paramFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading placement parameter file: %v\n", err)
			os.Exit(1)
		}
		addFiles = append(addFiles, placedOps...)
	}

	// read the symlinksFromFile parameter file and create a list of operations
	for _, paramFile := range symlinksFromFiles {
		symlinkOpsFromParamFile, err := readSymlinkParamFile(paramFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading symlink parameter file: %v\n", err)
			os.Exit(1)
		}
		symlinkFlags = append(symlinkFlags, symlinkOpsFromParamFile...)
	}

	// read the symlinkPairsFromFile parameter file and create a list of operations
	for _, paramFile := range symlinkPairsFromFiles {
		symlinkOpsFromParamFile, err := readSymlinkPairsParamFile(paramFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading symlink pairs parameter file: %v\n", err)
			os.Exit(1)
		}
		symlinkFlags = append(symlinkFlags, symlinkOpsFromParamFile...)
	}

	// read the emptyFilesFromFile parameter files and collect paths
	var emptyFilePaths []string
	for _, paramFile := range emptyFilesFromFiles {
		paths, err := readEmptyFilesParamFile(paramFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading empty files parameter file: %v\n", err)
			os.Exit(1)
		}
		emptyFilePaths = append(emptyFilePaths, paths...)
	}

	// first, due to the way Bazel attributes work, we need to find out if a pathInImage is used multiple times
	// If so, we add the basename of each file to the pathInImage
	pathsInImageCount := make(map[string]int)
	for _, op := range addFiles {
		pathsInImageCount[op.PathInImage]++
	}
	for _, op := range executableFlags {
		pathsInImageCount[op.PathInImage]++
	}

	// now, we fixup the operations
	for i, op := range addFiles {
		if pathsInImageCount[op.PathInImage] > 1 {
			addFiles[i].PathInImage = fmt.Sprintf("%s/%s", op.PathInImage, filepath.Base(op.File))
		}
	}
	for i, op := range executableFlags {
		if pathsInImageCount[op.PathInImage] > 1 {
			executableFlags[i].PathInImage = fmt.Sprintf("%s/%s", op.PathInImage, filepath.Base(op.Executable))
		}
		// try to match the runfiles parameter file to the executable
		// This is inefficient, but we don't expect a lot of executables
		// to be added.
		for _, runfilesOp := range runfilesFlags {
			if runfilesOp.Executable == op.Executable {
				executableFlags[i].RunfilesParameterFile = runfilesOp.RunfilesFromFile
				break
			}
		}
	}

	casImporter := contentmanifest.NewMultiImporter(contentManifestInputFlags, api.SHA256)
	if len(contentManifestCollection) > 0 {
		casImporter.AddCollection(contentManifestCollection)
	}

	var casExporter api.CASStateExporter
	if len(contentManifestOutputFlag) > 0 {
		casExporter = contentmanifest.New(contentManifestOutputFlag, api.SHA256)
	} else {
		casExporter = contentmanifest.NopExporter()
	}

	compressorState, err := handleLayerState(
		compressionAlgorithm, estargzFlag, addFiles, importTarFlags, executableFlags, symlinkFlags, emptyFilePaths,
		casImporter, casExporter, outputFile, layerMetadata,
		compressorJobsFlag, compressionLevelFlag, createParentDirectoriesFlag,
		treeArtifactHandlingFlag,
		compactStreamOutputFlag, compactStreamInlineThresholdFlag,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Writing layer: %v\n", err)
		os.Exit(1)
	}

	if len(metadataOutputFlag) > 0 {
		metadataOutputFile, err := os.OpenFile(metadataOutputFlag, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening metadata output file: %v\n", err)
			os.Exit(1)
		}
		defer func() {
			if err := metadataOutputFile.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "Error closing metadata output file: %v\n", err)
				os.Exit(1)
			}
		}()

		if err := writeMetadata(layerHistory, compressionAlgorithm, estargzFlag, mediaTypeFlag, annotations, compressorState, metadataOutputFile); err != nil {
			fmt.Fprintf(os.Stderr, "Writing metadata: %v\n", err)
			os.Exit(1)
		}
	}
}

// resolveCompressorJobs maps the --compressor-jobs flag to a concrete worker
// count the way the compress factory interprets it: "nproc" and any negative
// value mean NumCPU, a positive integer is used verbatim, and anything else
// (empty or unparseable) means the default single-threaded path.
func resolveCompressorJobs(flag string) int {
	if flag == "nproc" {
		return runtime.NumCPU()
	}
	if n, err := strconv.Atoi(flag); err == nil {
		if n < 0 {
			return runtime.NumCPU()
		}
		return n
	}
	return 0
}

// recordedCompressorJobs is the compressor-jobs value stored in the compact
// stream header. The header field is a single byte and the gzip factory only
// distinguishes parallel (>1, pgzip) from single-threaded (stdlib); pgzip output
// is independent of the exact worker count, so we clamp to 255 to prevent uint8
// truncation from flipping the pgzip/stdlib decision at reconstruction time
// (e.g. NumCPU 256 must not truncate to 0). resolveCompressorJobs mirrors how
// the compress factory interprets the --compressor-jobs flag the build used, so
// the recorded value selects the same gzip implementation at reconstruction.
func recordedCompressorJobs(flag string) uint8 {
	resolved := resolveCompressorJobs(flag)
	switch {
	case resolved <= 1:
		return 1
	case resolved > 255:
		return 255
	default:
		return uint8(resolved)
	}
}

// compactStreamCompressionLevel converts a compression level to the int8 field
// stored in the compact stream header, hard-failing if it does not fit. The
// header reserves a single signed byte; real gzip (0-9) and zstd levels are
// tiny, so this only rejects nonsensical input. Truncating instead would make
// reconstruction recompress at a different level and fail the compressed-stream
// digest check.
func compactStreamCompressionLevel(level int) (int8, error) {
	if level > math.MaxInt8 || level < math.MinInt8 {
		return 0, fmt.Errorf("compression level %d is out of range for the compact stream format (must be between %d and %d)", level, math.MinInt8, math.MaxInt8)
	}
	return int8(level), nil
}

func handleLayerState(
	compressionAlgorithm api.CompressionAlgorithm, useEstargz bool, addFiles addFiles, importTars importTars, addExecutables executables, addSymlinks symlinks, emptyFiles []string,
	casImporter api.CASStateSupplier, casExporter api.CASStateExporter, outputFile io.Writer, layerMetadata *LayerMetadata,
	compressorJobsFlag string, compressionLevelFlag int, createParentDirectories bool,
	treeArtifactHandling string,
	compactStreamPath string, compactStreamInlineThreshold uint64,
) (compressorState api.AppenderState, err error) {
	// Create shared digestfs with precaching
	digestFS := digestfs.New(&tarcas.SHA256Helper{})
	precacher := digestfs.NewPrecacher(digestFS, 4) // 4 workers as requested
	defer precacher.Close()

	// Start precaching files in the background
	startPrecaching(precacher, addFiles, addExecutables)
	var opts []compress.Option
	// compression level
	if compressionLevelFlag >= 0 {
		lvl := compress.CompressionLevel(compressionLevelFlag)
		opts = append(opts, lvl)
	}
	// compressor jobs: accept numeric or "nproc"
	if len(compressorJobsFlag) > 0 {
		if compressorJobsFlag == "nproc" {
			opts = append(opts, compress.CompressorJobs(runtime.NumCPU()))
		} else if n, err := strconv.Atoi(compressorJobsFlag); err == nil {
			opts = append(opts, compress.CompressorJobs(n))
		}
	}

	compressor, err := compress.TarAppenderFactory("sha256", string(compressionAlgorithm), useEstargz, outputFile, opts...)
	if err != nil {
		return compressorState, fmt.Errorf("creating compressor: %w", err)
	}

	var tarcasOpts []tarcas.Option
	tarcasOpts = append(tarcasOpts,
		tarcas.CreateParentDirectories(createParentDirectories),
		tarcas.DeduplicateTreeArtifacts(treeArtifactHandling == "deduplicate_symlink"),
	)

	var csFile *os.File
	var csWriter *compactstream.Writer
	if compactStreamPath != "" {
		// Validate the compression level before creating any file, so an
		// out-of-range level fails fast without leaving a zero-length output.
		csLevel, levelErr := compactStreamCompressionLevel(compressionLevelFlag)
		if levelErr != nil {
			return compressorState, levelErr
		}

		csFile, err = os.OpenFile(compactStreamPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return compressorState, fmt.Errorf("opening compact stream output file: %w", err)
		}

		var origComp uint8
		switch compressionAlgorithm {
		case api.Gzip:
			origComp = compactstream.OriginalCompressionGzip
		case api.Zstd:
			origComp = compactstream.OriginalCompressionZstd
		default:
			origComp = compactstream.OriginalCompressionNone
		}

		var inlineThreshold int64
		if compactStreamInlineThreshold > 0 {
			inlineThreshold = int64(compactStreamInlineThreshold)
		}

		csWriter = compactstream.NewWriter(
			csFile,
			compactstream.HashAlgoSHA256,
			uint16(api.SHA256.Len()),
			compactstream.StreamCompressionZstd,
			compactstream.OriginalCompressionInfo{
				Compression:      origComp,
				Seekable:         useEstargz,
				CompressionLevel: csLevel,
				CompressorJobs:   recordedCompressorJobs(compressorJobsFlag),
			},
			inlineThreshold,
		)
		tarcasOpts = append(tarcasOpts, tarcas.WithCompactStreamWriter{Writer: csWriter})
	}

	tw, err := tarcas.CASFactoryWithDigestFS("sha256", compressor, digestFS, tarcasOpts...)
	if err != nil {
		return compressorState, fmt.Errorf("creating Content-addressable storage inside tar file: %w", err)
	}
	// Finalization runs in this deferred closure so it executes on every exit
	// path, and in a specific order: close the tar writer (flushing all tar data
	// into the compressor and feeding the index observer), then finalize the
	// compressor to obtain the compressed-stream digest and size, then record
	// that information on the index and emit it. The index is written last
	// because its compressed-stream digest is only known once the compressor has
	// been finalized.
	defer func() {
		if err := tw.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Error closing tar writer: %v\n", err)
			os.Exit(1)
		}

		var compressorCloseErr error
		compressorState, compressorCloseErr = compressor.Finalize()
		if compressorCloseErr != nil {
			fmt.Fprintf(os.Stderr, "Error closing compressor: %v\n", compressorCloseErr)
			os.Exit(1)
		}

		if csWriter != nil {
			if err := csWriter.SetCompressedStreamInfo(compressorState.OuterHash, uint64(compressorState.CompressedSize)); err != nil {
				fmt.Fprintf(os.Stderr, "Error recording compressed stream info on compact stream: %v\n", err)
				os.Exit(1)
			}
			if err := csWriter.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "Error writing compact stream: %v\n", err)
				os.Exit(1)
			}
			if err := csFile.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "Error closing index file: %v\n", err)
				os.Exit(1)
			}
		}
	}()
	if err := tw.Import(casImporter); err != nil {
		return compressorState, fmt.Errorf("importing content manifests for deduplication: %w", err)
	}

	recorder := tree.NewRecorder(tw)
	if layerMetadata != nil {
		recorder = recorder.WithMetadata(layerMetadata)
	}
	if err := writeLayer(recorder, addFiles, importTars, addExecutables, addSymlinks, emptyFiles, layerMetadata); err != nil {
		return compressorState, err
	}

	return compressorState, tw.Export(casExporter)
}

func writeLayer(recorder tree.Recorder, addFiles addFiles, importTars importTars, addExecutables executables, addSymlinks symlinks, emptyFiles []string, layerMetadata *LayerMetadata) error {
	for _, tarFile := range importTars {
		if err := recorder.ImportTar(tarFile); err != nil {
			return fmt.Errorf("importing tar file: %w", err)
		}
	}

	for _, op := range addFiles {
		switch op.FileType {
		case api.RegularFile:
			if err := recorder.RegularFileFromPath(op.File, op.PathInImage); err != nil {
				return fmt.Errorf("writing regular file: %w", err)
			}
		case api.Directory:
			if err := recorder.TreeFromPath(op.File, op.PathInImage); err != nil {
				return fmt.Errorf("writing directory: %w", err)
			}
		case api.Symlink:
			link, err := op.Readlink()
			if err != nil {
				return fmt.Errorf("reading symlink: %w", err)
			}
			if err := recorder.Symlink(link, op.PathInImage); err != nil {
				return fmt.Errorf("writing symlink: %w", err)
			}
		default:
			return fmt.Errorf("unknown type %s for file %s", op.FileType.String(), op.File)
		}
	}

	for _, op := range addExecutables {
		runfilesList, err := readParamFile(op.RunfilesParameterFile)
		if err != nil {
			return fmt.Errorf("reading runfiles parameter file: %w", err)
		}
		accessor := runfiles.NewRunfilesFS()
		for _, f := range runfilesList {
			accessor.Add(f.PathInImage, f)
		}
		if err := recorder.Executable(op.Executable, op.PathInImage, accessor); err != nil {
			return fmt.Errorf("writing executable: %w", err)
		}
	}

	for _, op := range addSymlinks {
		if err := recorder.Symlink(op.Target, op.LinkName); err != nil {
			return fmt.Errorf("writing symlink: %w", err)
		}
	}

	for _, path := range emptyFiles {
		if err := recorder.EmptyFile(path); err != nil {
			return fmt.Errorf("writing empty file: %w", err)
		}
	}

	// Verify that all file metadata entries were used
	if layerMetadata != nil {
		if err := layerMetadata.VerifyAllFileMetadataUsed(); err != nil {
			return err
		}
	}

	return nil
}

func writeMetadata(history string, compressionAlgorithm api.CompressionAlgorithm, useEstargz bool, mediaTypeOverride string, annotations map[string]string, compressorState api.AppenderState, outputFile io.Writer) error {
	// Record the created_by history from the user-provided --history; a missing
	// value becomes "history missing" (LayerHistory).
	layerHistory := api.LayerHistory(history)
	var mediaType string
	if mediaTypeOverride != "" {
		mediaType = mediaTypeOverride
	} else {
		switch compressionAlgorithm {
		case api.Uncompressed:
			mediaType = "application/vnd.oci.image.layer.v1.tar"
		case api.Gzip:
			mediaType = "application/vnd.oci.image.layer.v1.tar+gzip"
		case api.Zstd:
			mediaType = "application/vnd.oci.image.layer.v1.tar+zstd"
		default:
			return fmt.Errorf("unsupported compression algorithm: %s", compressionAlgorithm)
		}
	}

	// Merge user annotations with layer annotations from the appender state
	mergedAnnotations := metadata.MergeAnnotations(annotations, compressorState.LayerAnnotations)

	// Replace sentinel annotation values with computed diff ID
	diffID := fmt.Sprintf("sha256:%x", compressorState.ContentHash)
	for _, key := range annotationKeysWithDerivableDiffID {
		if v, ok := mergedAnnotations[key]; ok && v == "DERIVE_FROM_DIFF_ID" {
			mergedAnnotations[key] = diffID
		}
	}

	return metadata.WriteLayerMetadata(
		fmt.Sprintf("sha256:%x", compressorState.ContentHash),
		mediaType,
		fmt.Sprintf("sha256:%x", compressorState.OuterHash),
		compressorState.CompressedSize,
		mergedAnnotations,
		layerHistory,
		outputFile,
	)
}

// startPrecaching begins background digest calculation for files that will be processed
func startPrecaching(precacher *digestfs.Precacher, addFiles addFiles, addExecutables executables) {
	// Collect all files that will need digest calculation
	var filesToPrecache []string

	// Add files from addFiles operations
	for _, op := range addFiles {
		if op.FileType == api.RegularFile {
			filesToPrecache = append(filesToPrecache, op.File)
		}
	}

	// Add executable files and their runfiles
	for _, op := range addExecutables {
		filesToPrecache = append(filesToPrecache, op.Executable)

		// Add runfiles if available
		if op.RunfilesParameterFile != "" {
			runfilesList, err := readParamFile(op.RunfilesParameterFile)
			if err == nil {
				for _, f := range runfilesList {
					if f.FileType == api.RegularFile {
						filesToPrecache = append(filesToPrecache, f.File)
					}
				}
			}
		}
	}

	// Start precaching in the background
	precacher.PrecacheFiles(filesToPrecache)
}

// readAnnotationsFile reads a file containing annotations in JSON or
// newline-delimited KEY=VALUE form (see the kvfile package) and flattens it to
// a map, keeping the last value for each key.
func readAnnotationsFile(path string) (map[string]string, error) {
	pairs, err := kvfile.ParseFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading annotations file: %w", err)
	}
	return kvfile.Flatten(pairs), nil
}

// any annotation keys in this list can be set to the magic sentinel
// "DERIVE_FROM_DIFF_ID" to inject the diff_id as a layer annotation.
var annotationKeysWithDerivableDiffID = []string{
	"io.deis.oras.content.digest",
}
