package layer

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compress"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/contentmanifest"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/digestfs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/metadata"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tarcas"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree/runfiles"
)

func LayerProcess(ctx context.Context, args []string) {
	annotations := make(annotationsFlag)
	var layerName string
	var addFiles addFiles
	var addFromFile addFromFileArgs
	var importTarFlags importTars
	var runfilesFlags runfilesForExecutables
	var executableFlags executables
	var symlinkFlags symlinks
	var symlinksFromFiles symlinksFromFileArgs
	var contentManifestInputFlags contentManifests
	var contentManifestCollection string
	var formatFlag string
	var estargzFlag bool
	var metadataOutputFlag string
	var contentManifestOutputFlag string
	var defaultMetadataFlag string
	var compressorJobsFlag string
	var compressionLevelFlag int
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
	flagSet.StringVar(&layerName, "name", "", `Optional name of the layer. Defaults to digest.`)
	flagSet.Var(&addFiles, "add", `Add a file to the image layer. The parameter is a string of the form <path_in_image>=<file> where <path_in_image> is the path in the image and <file> is the path in the host filesystem.`)
	flagSet.Var(&addFromFile, "add-from-file", `Add all files listed in the parameter file to the image layer. The parameter file is usually written by Bazel.
The file contains one line per file, where each line contains a path in the image and a path in the host filesystem, separated by a a null byte and a single character indicating the type of the file.
The type is either 'f' for regular files, 'd' for directories. The parameter file is usually written by Bazel.`)
	flagSet.Var(&importTarFlags, "import-tar", `Import all files from the given tar file into the image layer while deduplicating the contents.`)
	flagSet.Var(&executableFlags, "executable", `Add the executable file at the specified path in the image. This should be combined with the --runfiles flag to include the runfiles of the executable.`)
	flagSet.Var(&runfilesFlags, "runfiles", `Add the runfiles of an executable file. The runfiles are read from the specified parameter file with the same encoding used by --add-from-file. The parameter file is usually written by Bazel.`)
	flagSet.Var(&symlinkFlags, "symlink", `Add a symlink to the image layer. The parameter is a string of the form <path_in_image>=<target> where <path_in_image> is the path in the image and <target> is the target of the symlink.`)
	flagSet.Var(&symlinksFromFiles, "symlinks-from-file", `Add all symlinks listed in the parameter file to the image layer. The parameter file is usually written by Bazel.`)
	flagSet.Var(&contentManifestInputFlags, "deduplicate", `Path of a content manifest of a previous layer that can be used for deduplication.`)
	flagSet.StringVar(&contentManifestCollection, "deduplicate-collection", "", `Path of a content manifest collection file that can be used for deduplication.`)
	flagSet.StringVar(&formatFlag, "format", "", `The compression format of the output layer. Can be "gzip", "zstd", or "none". Default is to guess the algorithm based on the filename, but fall back to "gzip".`)
	flagSet.BoolVar(&estargzFlag, "estargz", false, `Use estargz format for compression. This creates seekable gzip streams optimized for lazy pulling.`)
	flagSet.StringVar(&compressorJobsFlag, "compressor-jobs", "1", `Number of compressor jobs. 1 uses single-threaded stdlib gzip. n>1 uses pgzip. "nproc" uses NumCPU.`)
	flagSet.IntVar(&compressionLevelFlag, "compression-level", -1, `Compression level. For gzip: 0-9. If unset, use library default.`)
	flagSet.Var(&annotations, "annotation", `Add an annotation as key=value. Can be specified multiple times.`)
	flagSet.StringVar(&metadataOutputFlag, "metadata", "", `Write the metadata to the specified file. The metadata is a JSON file containing info needed to use the layer as part of an OCI image.`)
	flagSet.StringVar(&contentManifestOutputFlag, "content-manifest", "", `Write a manifest of the contents of the layer to the specified file. The manifest uses a custom binary format listing all blobs, nodes, and trees in the layer after deduplication.`)
	flagSet.StringVar(&defaultMetadataFlag, "default-metadata", "", `JSON-encoded default metadata to apply to all files in the layer. Can include fields like mode, uid, gid, uname, gname, mtime, and pax_records.`)
	flagSet.Var(&fileMetadataFlags, "file-metadata", `Per-file metadata override in the format path=json. Can be specified multiple times. Overrides any defaults from --default-metadata.`)

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}

	if flagSet.NArg() != 1 {
		flagSet.Usage()
		os.Exit(1)
	}

	outputFilePath := flagSet.Arg(0)

	var compressionAlgorithm api.CompressionAlgorithm
	switch formatFlag {
	case "":
		if filepath.Ext(outputFilePath) == ".tar" {
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

	outputFile, err := os.OpenFile(outputFilePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening output file: %v\n", err)
		os.Exit(1)
	}
	defer outputFile.Close()

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

	// read the symlinksFromFile parameter file and create a list of operations
	for _, paramFile := range symlinksFromFiles {
		symlinkOpsFromParamFile, err := readSymlinkParamFile(paramFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading symlink parameter file: %v\n", err)
			os.Exit(1)
		}
		symlinkFlags = append(symlinkFlags, symlinkOpsFromParamFile...)
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
		compressionAlgorithm, estargzFlag, addFiles, importTarFlags, executableFlags, symlinkFlags,
		casImporter, casExporter, outputFile, layerMetadata,
		compressorJobsFlag, compressionLevelFlag,
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
		defer metadataOutputFile.Close()

		if err := writeMetadata(layerName, compressionAlgorithm, estargzFlag, annotations, compressorState, metadataOutputFile); err != nil {
			fmt.Fprintf(os.Stderr, "Writing metadata: %v\n", err)
			os.Exit(1)
		}
	}
}

func handleLayerState(
	compressionAlgorithm api.CompressionAlgorithm, useEstargz bool, addFiles addFiles, importTars importTars, addExecutables executables, addSymlinks symlinks,
	casImporter api.CASStateSupplier, casExporter api.CASStateExporter, outputFile io.Writer, layerMetadata *LayerMetadata,
	compressorJobsFlag string, compressionLevelFlag int,
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
	defer func() {
		var compressorCloseErr error
		compressorState, compressorCloseErr = compressor.Finalize()
		if compressorCloseErr != nil {
			fmt.Fprintf(os.Stderr, "Error closing compressor: %v\n", compressorCloseErr)
			os.Exit(1)
		}
	}()

	tw, err := tarcas.CASFactoryWithDigestFS("sha256", compressor, digestFS)
	if err != nil {
		return compressorState, fmt.Errorf("creating Content-addressable storage inside tar file: %w", err)
	}
	defer func() {
		if err := tw.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Error closing tar writer: %v\n", err)
			os.Exit(1)
		}
	}()
	if err := tw.Import(casImporter); err != nil {
		return compressorState, fmt.Errorf("importing content manifests for deduplication: %w", err)
	}

	recorder := tree.NewRecorder(tw)
	if layerMetadata != nil {
		recorder = recorder.WithMetadata(layerMetadata)
	}
	if err := writeLayer(recorder, addFiles, importTars, addExecutables, addSymlinks, layerMetadata); err != nil {
		return compressorState, err
	}

	return compressorState, tw.Export(casExporter)
}

func writeLayer(recorder tree.Recorder, addFiles addFiles, importTars importTars, addExecutables executables, addSymlinks symlinks, layerMetadata *LayerMetadata) error {
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

	// Verify that all file metadata entries were used
	if layerMetadata != nil {
		if err := layerMetadata.VerifyAllFileMetadataUsed(); err != nil {
			return err
		}
	}

	return nil
}

func writeMetadata(name string, compressionAlgorithm api.CompressionAlgorithm, useEstargz bool, annotations map[string]string, compressorState api.AppenderState, outputFile io.Writer) error {
	if len(name) == 0 {
		name = fmt.Sprintf("sha256:%x", compressorState.OuterHash)
	}
	var mediaType string
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

	// Merge user annotations with layer annotations from the appender state
	mergedAnnotations := metadata.MergeAnnotations(annotations, compressorState.LayerAnnotations)

	return metadata.WriteLayerMetadata(
		name,
		fmt.Sprintf("sha256:%x", compressorState.ContentHash),
		mediaType,
		fmt.Sprintf("sha256:%x", compressorState.OuterHash),
		compressorState.CompressedSize,
		mergedAnnotations,
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
