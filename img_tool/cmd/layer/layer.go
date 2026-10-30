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
	"github.com/bazel-contrib/rules_img/img_tool/pkg/bytesize"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compress"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/contentmanifest"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/digestfs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/erofscas"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/kvfile"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/metadata"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tarcas"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree/runfiles"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/ztoc"
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
	var baseMetadataFlags baseMetadataArgs
	var baseMetadataFromFiles baseMetadataFromFileArgs
	var formatFlag string
	var layerFormatFlag string
	var erofsBlockSizeFlag int
	var erofsInlineThresholdFlag uint64
	var erofsSyntheticDirectoriesFlag string
	var erofsNoVerifyCASRefsFlag bool
	var cpioDeduplicateFlag bool
	var ext4DeduplicateFlag bool
	var ext4BlockSizeFlag int
	var ext4SizeFlag string
	var ext4BytesPerInodeFlag string
	var ext4ExtraInodesFlag uint
	var ext4LabelFlag string
	var ext4UUIDFlag string
	var ext4OmitLostAndFoundFlag bool
	var estargzFlag bool
	var zstdChunkedFlag bool
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
	var ztocOutputFlag string
	var ztocSpanSizeFlag int64
	var ztocBuildToolIdentifierFlag string
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
	flagSet.StringVar(&layerHistory, "history", api.MissingHistoryMarker, `Optional created_by string recorded in the layer's history (e.g. "bazel build //pkg:target"). Omitting the flag is equivalent to passing the missing-history marker; passing it with an empty value ("--history=") omits the history entry entirely.`)
	flagSet.Var(&addFiles, "add", `Add a file to the image layer. The parameter is a string of the form <path_in_image>=<file> where <path_in_image> is the path in the image and <file> is the path in the host filesystem.`)
	flagSet.Var(&addFromFile, "add-from-file", `Add all files listed in the parameter file to the image layer. The parameter file is usually written by Bazel.
The file contains one line per file, where each line contains a path in the image and a path in the host filesystem, separated by a a null byte and a single character indicating the type of the file.
The type is either 'f' for regular files, 'd' for directories. The parameter file is usually written by Bazel.`)
	flagSet.Var(&placeFilesFlags, "place-files", `Add files described by a self-describing placement parameter file. The first line is a header (mode, dest, anchor, skip, optional on_escape; null-separated) carrying per-target placement context; each following line is a file in the same encoding as --add-from-file but keyed by rebased short_path. Used to lazily place a target's default outputs relative to its executable or package. The parameter file is usually written by Bazel.`)
	flagSet.Var(&importTarFlags, "import-tar", `Import all files from the given tar file into the image layer while deduplicating the contents.`)
	flagSet.Var(&executableFlags, "executable", `Add the executable file at the specified path in the image. This should be combined with the --runfiles flag to include the runfiles of the executable.`)
	flagSet.Var(&runfilesFlags, "runfiles", `Add the runfiles of an executable file. The runfiles are read from the specified parameter file with the same encoding used by --add-from-file. The parameter file is usually written by Bazel.`)
	flagSet.Var(&symlinkFlags, "symlink", `Add a symlink to the image layer. The parameter is a string of the form <path_in_image>=<target> where <path_in_image> is the path in the image and <target> is the target of the symlink.`)
	flagSet.Var(&symlinksFromFiles, "symlinks-from-file", `Add all symlinks listed in the parameter file to the image layer. The parameter file is usually written by Bazel.`)
	flagSet.Var(&symlinkPairsFromFiles, "symlink-pairs-from-file", `Add symlinks from a parameter file where each line has three null-separated fields: source_prefix, dest_prefix, dir_name. Creates symlink source_prefix/dir_name -> dest_prefix/dir_name.`)
	flagSet.Var(&emptyFilesFromFiles, "empty-files-from-file", `Create zero-size regular files at paths listed in the parameter file (one path per line).`)
	flagSet.Var(&baseMetadataFlags, "base-metadata", `Add every tar entry described by a base metadata stream (as written by "img base"). Can be specified multiple times; for a path described by several streams, the last one wins.`)
	flagSet.Var(&baseMetadataFromFiles, "base-metadata-from-file", `Add the base metadata streams listed in the parameter file, one path per line, in order. The parameter file is usually written by Bazel.`)
	flagSet.Var(&contentManifestInputFlags, "deduplicate", `Path of a content manifest of a previous layer that can be used for deduplication.`)
	flagSet.StringVar(&contentManifestCollection, "deduplicate-collection", "", `Path of a content manifest collection file that can be used for deduplication.`)
	flagSet.StringVar(&formatFlag, "format", "", `The compression format of the output layer. Can be "gzip", "zstd", or "none". Default is to guess the algorithm based on the filename, but fall back to "gzip".`)
	flagSet.StringVar(&layerFormatFlag, "layer-format", "tar", `The container format of the output layer. "tar" writes an OCI tar changeset. "erofs" writes a mountable EROFS filesystem image, which accepts only --format none or zstd and no --estargz or --ztoc. "ext4" writes an ext4 filesystem image instead of a tar file. It is not an OCI layer: the options that only apply to tar layers (--format, --estargz, --zstd-chunked, --ztoc and the compressor flags) are ignored, while --compact-stream writes a compact stream of the image. "cpio" writes a cpio archive in the newc format, which is what the Linux kernel unpacks as an initramfs. It is not an OCI layer either. It is not compressed unless --format says gzip or zstd (the kernel unpacks both), and --estargz, --zstd-chunked and --ztoc are ignored.`)
	flagSet.BoolVar(&cpioDeduplicateFlag, "cpio-deduplicate", true, `Store regular files with the same content and metadata of a cpio layer once, as hard links of each other. Turning this off avoids reading file content just to find duplicates. Only used with --layer-format cpio.`)
	flagSet.BoolVar(&ext4DeduplicateFlag, "ext4-deduplicate", true, `Store regular files with the same content and metadata of an ext4 layer once, as hard links of each other. Turning this off avoids reading file content just to find duplicates. Only used with --layer-format ext4.`)
	flagSet.IntVar(&ext4BlockSizeFlag, "ext4-block-size", 0, `Filesystem block size of an ext4 layer, in bytes: a power of two between 1024 and 65536. 0 uses 4096. Linux can only mount block sizes up to the page size of the machine. Only used with --layer-format ext4.`)
	flagSet.StringVar(&ext4SizeFlag, "ext4-min-size", "", `Minimum size of an ext4 layer, with an optional binary unit ("512MiB"). The filesystem is otherwise exactly as large as its content needs; a larger size leaves the extra space free. Only used with --layer-format ext4.`)
	flagSet.StringVar(&ext4BytesPerInodeFlag, "ext4-bytes-per-inode", "", `Reserve one inode of an ext4 layer for every this many bytes of --ext4-min-size, in addition to the inodes the content needs. Empty leaves only the inodes the content needs plus --ext4-extra-inodes. Only used with --layer-format ext4.`)
	flagSet.UintVar(&ext4ExtraInodesFlag, "ext4-extra-inodes", 0, `Number of free inodes to leave in an ext4 layer beyond those its content uses. Only used with --layer-format ext4.`)
	flagSet.StringVar(&ext4LabelFlag, "ext4-label", "", `Volume label of an ext4 layer (at most 16 bytes). Only used with --layer-format ext4.`)
	flagSet.StringVar(&ext4UUIDFlag, "ext4-uuid", "", `Filesystem UUID of an ext4 layer. Empty derives a UUID from the metadata of the content, so that equal inputs give equal images. Only used with --layer-format ext4.`)
	flagSet.BoolVar(&ext4OmitLostAndFoundFlag, "ext4-omit-lost-and-found", false, `Leave out the lost+found directory of an ext4 layer, which fsck uses to reattach orphaned files. Without it, fsck reports the directory as missing. Only used with --layer-format ext4.`)
	flagSet.IntVar(&erofsBlockSizeFlag, "erofs-block-size", 0, `Filesystem block size of an EROFS layer, in bytes. 0 uses 4096, which matches the common page size. Only used with --layer-format erofs.`)
	flagSet.Uint64Var(&erofsInlineThresholdFlag, "erofs-inline-threshold", 0, `Maximum size (in bytes) of a file payload an EROFS layer may tail-pack into its inode. A file at least this large gets a data extent of its own, and therefore becomes a content-addressed reference in the compact stream rather than inline bytes. 0 packs whatever fits in the inode's block. Only used with --layer-format erofs.`)
	flagSet.StringVar(&erofsSyntheticDirectoriesFlag, "erofs-synthetic-directories", "inherit", `Attributes for directories an EROFS layer has to invent because nothing describes them. "inherit" copies the nearest described ancestor's ownership (what mkfs.erofs does), "default" uses 0755 root:root, "strict" fails the layer instead of guessing. Only used with --layer-format erofs.`)
	flagSet.BoolVar(&erofsNoVerifyCASRefsFlag, "erofs-no-verify-cas-refs", false, `Skip re-hashing each region an EROFS layer's compact stream replaces with a content-addressed reference. Verification is cheap and turns a wrong layout plan into a build failure, so only disable it to work around a bug.`)
	flagSet.BoolVar(&estargzFlag, "estargz", false, `Use estargz format for compression. This creates seekable gzip streams optimized for lazy pulling.`)
	flagSet.BoolVar(&zstdChunkedFlag, "zstd-chunked", false, `Use zstd:chunked format for compression. Requires --format zstd. Files are split into content-defined chunks that can be fetched and deduplicated individually.`)
	flagSet.StringVar(&mediaTypeFlag, "media-type", "", `Override the layer media type in the metadata output. If empty, auto-detected from the compression format.`)
	flagSet.StringVar(&compressorJobsFlag, "compressor-jobs", "", `Number of compressor jobs. For gzip, 1 uses single-threaded stdlib gzip and n>1 uses pgzip. For zstd, it is the number of parallel encoder goroutines, which is always at least 2. "nproc" uses the available cores. If unset, gzip is single-threaded and zstd uses 2 jobs.`)
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
	flagSet.StringVar(&ztocOutputFlag, "ztoc", "", `Write a ztoc (SOCI table of contents) for the compressed layer to the specified file. Only supported for gzip-compressed layers, and incompatible with --compact-stream-only.`)
	flagSet.Int64Var(&ztocSpanSizeFlag, "ztoc-span-size", ztoc.DefaultSpanSize, `Minimum number of uncompressed bytes between ztoc checkpoints (only used with --ztoc).`)
	flagSet.StringVar(&ztocBuildToolIdentifierFlag, "ztoc-build-tool-identifier", ztoc.DefaultBuildToolIdentifier, `Recorded in the ztoc's build_tool_identifier field (only used with --ztoc).`)

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}

	var containerFormat api.ContainerFormat
	switch layerFormatFlag {
	case "", "tar":
		containerFormat = api.TarFormat
	case "erofs":
		containerFormat = api.ErofsFormat
	case "ext4":
		containerFormat = api.Ext4Format
	case "cpio":
		containerFormat = api.CpioFormat
	default:
		fmt.Fprintf(os.Stderr, "Unknown layer format %s. Supported layer formats are tar, erofs, ext4 and cpio.\n", layerFormatFlag)
		os.Exit(1)
	}
	if containerFormat.IsImageFormat() {
		// A filesystem image is written directly. Options that only apply to
		// tar layers are ignored. A compact stream is written next to the
		// image, or instead of it with --compact-stream-only. An ext4 image
		// is not compressed, while a cpio archive is as asked.
		if containerFormat != api.CpioFormat {
			formatFlag = "none"
		}
		estargzFlag = false
		zstdChunkedFlag = false
		ztocOutputFlag = ""
	}

	ext4Settings := ext4Settings{
		blockSize:        ext4BlockSizeFlag,
		label:            ext4LabelFlag,
		omitLostAndFound: ext4OmitLostAndFoundFlag,
		deduplicate:      ext4DeduplicateFlag,
		deduplicateTrees: treeArtifactHandlingFlag == "deduplicate_symlink",
	}
	if containerFormat == api.Ext4Format {
		var err error
		if ext4SizeFlag != "" {
			if ext4Settings.size, err = bytesize.Parse(ext4SizeFlag); err != nil {
				fmt.Fprintf(os.Stderr, "Error: --ext4-min-size %s: %v\n", ext4SizeFlag, err)
				os.Exit(1)
			}
		}
		if ext4BytesPerInodeFlag != "" {
			if ext4Settings.bytesPerInode, err = bytesize.Parse(ext4BytesPerInodeFlag); err != nil {
				fmt.Fprintf(os.Stderr, "Error: --ext4-bytes-per-inode %s: %v\n", ext4BytesPerInodeFlag, err)
				os.Exit(1)
			}
		}
		if ext4ExtraInodesFlag > math.MaxUint32 {
			fmt.Fprintf(os.Stderr, "Error: --ext4-extra-inodes %d is out of range (maximum %d)\n", ext4ExtraInodesFlag, uint32(math.MaxUint32))
			os.Exit(1)
		}
		ext4Settings.extraInodes = uint32(ext4ExtraInodesFlag)
		if ext4UUIDFlag != "" {
			if ext4Settings.uuid, err = parseUUID(ext4UUIDFlag); err != nil {
				fmt.Fprintf(os.Stderr, "Error: --ext4-uuid: %v\n", err)
				os.Exit(1)
			}
		}
	}

	if compactStreamOnlyFlag && compactStreamOutputFlag == "" {
		fmt.Fprintf(os.Stderr, "Error: --compact-stream-only requires --compact-stream\n")
		os.Exit(1)
	}

	if ztocOutputFlag != "" && compactStreamOnlyFlag {
		fmt.Fprintf(os.Stderr, "Error: --ztoc cannot be combined with --compact-stream-only (there is no materialized layer blob to index)\n")
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
		if containerFormat == api.ErofsFormat || containerFormat == api.CpioFormat {
			// An EROFS image is mountable as it is, so leaving it uncompressed
			// is the useful default; gzip does not exist for this family. An
			// initramfs is uncompressed unless asked otherwise.
			compressionAlgorithm = api.Uncompressed
		} else if compactStreamOnlyFlag {
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

	if erofsInlineThresholdFlag > math.MaxInt32 {
		fmt.Fprintf(os.Stderr, "Error: --erofs-inline-threshold %d is out of range (maximum %d)\n", erofsInlineThresholdFlag, math.MaxInt32)
		os.Exit(1)
	}

	erofsSettings := erofsSettings{
		blockSize:       erofsBlockSizeFlag,
		inlineThreshold: erofsInlineThresholdFlag,
		verifyCASRefs:   !erofsNoVerifyCASRefsFlag,
	}
	if containerFormat == api.ErofsFormat {
		if compressionAlgorithm == api.Gzip {
			fmt.Fprintf(os.Stderr, "Error: --layer-format erofs cannot be gzip-compressed; there is no application/vnd.erofs.layer.v1+gzip media type. Use --format none or --format zstd\n")
			os.Exit(1)
		}
		if estargzFlag {
			fmt.Fprintf(os.Stderr, "Error: --estargz is tar-specific and cannot be combined with --layer-format erofs\n")
			os.Exit(1)
		}
		if zstdChunkedFlag {
			fmt.Fprintf(os.Stderr, "Error: --zstd-chunked is tar-specific and cannot be combined with --layer-format erofs\n")
			os.Exit(1)
		}
		if ztocOutputFlag != "" {
			fmt.Fprintf(os.Stderr, "Error: --ztoc indexes a gzip tar and cannot be combined with --layer-format erofs\n")
			os.Exit(1)
		}
		if compactStreamInlineThresholdFlag != 0 {
			fmt.Fprintf(os.Stderr, "Error: --compact-stream-inline-threshold is tar-specific and cannot be combined with --layer-format erofs; use --erofs-inline-threshold\n")
			os.Exit(1)
		}
		switch erofsSyntheticDirectoriesFlag {
		case "", "inherit":
			erofsSettings.syntheticDirectories = erofscas.SyntheticDirectoriesInherit
		case "default":
			erofsSettings.syntheticDirectories = erofscas.SyntheticDirectoriesDefault
		case "strict":
			erofsSettings.syntheticDirectories = erofscas.SyntheticDirectoriesStrict
		default:
			fmt.Fprintf(os.Stderr, "Unknown --erofs-synthetic-directories value %s. Supported values are inherit, default and strict.\n", erofsSyntheticDirectoriesFlag)
			os.Exit(1)
		}
	}

	if ztocOutputFlag != "" && compressionAlgorithm != api.Gzip {
		fmt.Fprintf(os.Stderr, "Error: --ztoc is only supported for gzip-compressed layers, got %s\n", compressionAlgorithm)
		os.Exit(1)
	}

	tarFormat, err := compress.ResolveTarFormat(estargzFlag, zstdChunkedFlag, string(compressionAlgorithm))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	var outputFile io.Writer
	var outputOSFile *os.File
	if compactStreamOnlyFlag {
		outputFile = io.Discard
	} else {
		openFlags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		if containerFormat.IsImageFormat() {
			openFlags = os.O_RDWR | os.O_CREATE | os.O_TRUNC // read back for the digest
		}
		f, err := os.OpenFile(outputFilePath, openFlags, 0o644)
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
		outputOSFile = f
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

	// read the symlinkPairsFromFile parameter files and create a list of
	// operations, dropping the repeats that dedupeSymlinks explains.
	var symlinkPairOps symlinks
	for _, paramFile := range symlinkPairsFromFiles {
		symlinkOpsFromParamFile, err := readSymlinkPairsParamFile(paramFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading symlink pairs parameter file: %v\n", err)
			os.Exit(1)
		}
		symlinkPairOps = append(symlinkPairOps, symlinkOpsFromParamFile...)
	}
	symlinkFlags = append(symlinkFlags, dedupeSymlinks(symlinkPairOps)...)

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

	// read the baseMetadataFromFile parameter files and collect stream paths.
	// Streams named directly on the command line come first, then those listed
	// in parameter files, in the order given.
	baseMetadataPaths := []string(baseMetadataFlags)
	for _, paramFile := range baseMetadataFromFiles {
		paths, err := readBaseMetadataParamFile(paramFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading base metadata parameter file: %v\n", err)
			os.Exit(1)
		}
		baseMetadataPaths = append(baseMetadataPaths, paths...)
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

	if containerFormat.IsImageFormat() {
		cpioSettings := cpioSettings{
			deduplicate:      cpioDeduplicateFlag,
			deduplicateTrees: treeArtifactHandlingFlag == "deduplicate_symlink",
			compression:      compressionAlgorithm,
			compressorJobs:   compressorJobsFlag,
			compressionLevel: compressionLevelFlag,
		}
		if err := handleImageLayer(
			containerFormat, ext4Settings, cpioSettings, outputOSFile, addFiles, importTarFlags, executableFlags, symlinkFlags, emptyFilePaths, baseMetadataPaths,
			createParentDirectoriesFlag, layerMetadata, layerHistory, mediaTypeFlag, annotations, metadataOutputFlag,
			compactStreamOutputFlag, compactStreamInlineThresholdFlag,
		); err != nil {
			fmt.Fprintf(os.Stderr, "Writing %s layer: %v\n", containerFormat, err)
			os.Exit(1)
		}
		return
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
		containerFormat, erofsSettings,
		compressionAlgorithm, tarFormat, addFiles, importTarFlags, executableFlags, symlinkFlags, emptyFilePaths,
		baseMetadataPaths,
		casImporter, casExporter, outputFile, layerMetadata,
		compressorJobsFlag, compressionLevelFlag, createParentDirectoriesFlag,
		treeArtifactHandlingFlag,
		compactStreamOutputFlag, compactStreamInlineThresholdFlag,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Writing layer: %v\n", err)
		os.Exit(1)
	}

	// Generate the ztoc from the finalized gzip blob. handleLayerState has
	// returned, so its deferred compressor.Finalize() has flushed every byte to
	// outputFilePath (validation above guarantees gzip and a materialized blob).
	if ztocOutputFlag != "" {
		if err := generateLayerZtoc(outputFilePath, ztocOutputFlag, ztocSpanSizeFlag, ztocBuildToolIdentifierFlag); err != nil {
			fmt.Fprintf(os.Stderr, "Writing ztoc: %v\n", err)
			os.Exit(1)
		}
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

		if err := writeMetadata(layerHistory, containerFormat, compressionAlgorithm, mediaTypeFlag, annotations, compressorState, metadataOutputFile); err != nil {
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

// maxRecordedCompressorJobs bounds the worker count written to the compact
// stream header.
//
// Neither compressor's output depends on the exact count - gzip only
// distinguishes parallel (>1, pgzip) from single-threaded (stdlib), and zstd
// lays its parallel jobs out independently of the worker count - so
// reconstruction reproduces the same bytes from a bounded value. Bounding it
// matters because the build machine's core count is not the reconstructing
// machine's: a value taken verbatim from a machine with hundreds of cores would
// have every reconstruction allocate hundreds of zstd job buffers, at roughly
// 64 MiB of peak memory each. The build itself is free to use every core, since
// there the count was asked for.
const maxRecordedCompressorJobs = 16

// recordedCompressorJobs is the compressor-jobs value stored in the compact
// stream header. resolveCompressorJobs mirrors how the compress factory
// interprets the --compressor-jobs flag the build used, so the recorded value
// selects the same gzip implementation, and parallel zstd encoding, at
// reconstruction time.
func recordedCompressorJobs(flag string) uint8 {
	resolved := resolveCompressorJobs(flag)
	switch {
	case resolved <= 1:
		return 1
	case resolved > maxRecordedCompressorJobs:
		return maxRecordedCompressorJobs
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

// erofsSettings are the EROFS-only knobs of a layer action.
type erofsSettings struct {
	blockSize            int
	inlineThreshold      uint64
	verifyCASRefs        bool
	syntheticDirectories erofscas.SyntheticDirectories
}

// finalizer is the part of a compressor the layer action needs once every byte
// has been written: the digests and sizes of the blob and its uncompressed
// content.
type finalizer interface {
	Finalize() (api.AppenderState, error)
}

// compressOptions turns the --compression-level and --compressor-jobs flags
// into compressor options.
func compressOptions(compressionLevelFlag int, compressorJobsFlag string) []compress.Option {
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
	return opts
}

func handleLayerState(
	containerFormat api.ContainerFormat, erofs erofsSettings,
	compressionAlgorithm api.CompressionAlgorithm, tarFormat compress.TarFormat, addFiles addFiles, importTars importTars, addExecutables executables, addSymlinks symlinks, emptyFiles []string,
	baseMetadataPaths []string,
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
	opts := compressOptions(compressionLevelFlag, compressorJobsFlag)

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

		// An EROFS layer decides what is carried inline while it plans its
		// layout -- a tail-packed payload lands in the metadata area -- so the
		// compact stream itself makes no inlining decisions. That decision also
		// shapes the image, so it comes from --erofs-inline-threshold rather
		// than from a compact-stream flag: the blob must not depend on whether a
		// compact stream was requested.
		var inlineThreshold int64
		if compactStreamInlineThreshold > 0 && containerFormat != api.ErofsFormat {
			inlineThreshold = int64(compactStreamInlineThreshold)
		}

		csWriter = compactstream.NewWriter(
			csFile,
			compactstream.HashAlgoSHA256,
			uint16(api.SHA256.Len()),
			compactstream.StreamCompressionZstd,
			compactstream.OriginalCompressionInfo{
				Compression:      origComp,
				Format:           tarFormat,
				CompressionLevel: csLevel,
				CompressorJobs:   recordedCompressorJobs(compressorJobsFlag),
			},
			inlineThreshold,
		)
	}

	var tw api.TarCAS
	var compressor finalizer
	if containerFormat == api.ErofsFormat {
		appender, appenderErr := compress.AppenderFactory("sha256", string(compressionAlgorithm), outputFile, opts...)
		if appenderErr != nil {
			return compressorState, fmt.Errorf("creating compressor: %w", appenderErr)
		}
		compressor = appender

		erofsOpts := []erofscas.Option{
			erofscas.VerifyCASRefs(erofs.verifyCASRefs),
			erofscas.WithSyntheticDirectories(erofs.syntheticDirectories),
			erofscas.DeduplicateTreeArtifacts(treeArtifactHandling == "deduplicate_symlink"),
			erofscas.InlineThreshold(erofs.inlineThreshold),
		}
		if erofs.blockSize > 0 {
			erofsOpts = append(erofsOpts, erofscas.BlockSize(erofs.blockSize))
		}
		if layerMetadata != nil && (layerMetadata.Defaults != nil || len(layerMetadata.FileOverrides) > 0) {
			erofsOpts = append(erofsOpts, erofscas.GeneratedMetadata(layerMetadata.applyToGeneratedHeader))
		}
		if csWriter != nil {
			erofsOpts = append(erofsOpts, erofscas.WithCompactStreamWriter{Writer: csWriter})
		}
		tw, err = erofscas.CASFactoryWithDigestFS("sha256", appender, digestFS, erofsOpts...)
		if err != nil {
			return compressorState, fmt.Errorf("creating EROFS layer writer: %w", err)
		}
	} else {
		appender, appenderErr := compress.TarAppenderFactory("sha256", string(compressionAlgorithm), tarFormat, outputFile, opts...)
		if appenderErr != nil {
			return compressorState, fmt.Errorf("creating compressor: %w", appenderErr)
		}
		compressor = appender

		tarcasOpts := []tarcas.Option{
			tarcas.CreateParentDirectories(createParentDirectories),
			tarcas.DeduplicateTreeArtifacts(treeArtifactHandling == "deduplicate_symlink"),
		}
		if layerMetadata != nil && (layerMetadata.Defaults != nil || len(layerMetadata.FileOverrides) > 0) {
			tarcasOpts = append(tarcasOpts, tarcas.GeneratedMetadata(layerMetadata.applyToGeneratedHeader))
		}
		if csWriter != nil {
			tarcasOpts = append(tarcasOpts, tarcas.WithCompactStreamWriter{Writer: csWriter})
		}
		tw, err = tarcas.CASFactoryWithDigestFS("sha256", appender, digestFS, tarcasOpts...)
		if err != nil {
			return compressorState, fmt.Errorf("creating Content-addressable storage inside tar file: %w", err)
		}
	}
	// Finalization runs in this deferred closure so it executes on every exit
	// path, and in a specific order: close the layer writer (flushing all layer
	// data into the compressor and feeding the index observer), then finalize
	// the compressor to obtain the compressed-stream digest and size, then
	// record that information on the index and emit it. The index is written
	// last because its compressed-stream digest is only known once the
	// compressor has been finalized.
	defer func() {
		if err := tw.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Error closing layer writer: %v\n", err)
			os.Exit(1)
		}
		// Closing writes deferred symlinks and their parent directories.
		if err == nil && layerMetadata != nil {
			err = layerMetadata.VerifyAllFileMetadataUsed()
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
	if err := writeLayer(recorder, addFiles, importTars, addExecutables, addSymlinks, emptyFiles, baseMetadataPaths, createParentDirectories, layerMetadata); err != nil {
		return compressorState, err
	}

	return compressorState, tw.Export(casExporter)
}

func writeLayer(recorder tree.Recorder, addFiles addFiles, importTars importTars, addExecutables executables, addSymlinks symlinks, emptyFiles []string, baseMetadataPaths []string, createParentDirectories bool, layerMetadata *LayerMetadata) error {
	// Base metadata comes first: it describes the scaffolding of the image (the
	// directory skeleton, /etc, the trust store), and writing it ahead of
	// everything else keeps parent directories in front of the files placed
	// into them.
	if len(baseMetadataPaths) > 0 {
		if err := writeBaseEntries(recorder, baseMetadataPaths, createParentDirectories, layerMetadata); err != nil {
			return err
		}
	}

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

	return nil
}

func writeMetadata(history string, containerFormat api.ContainerFormat, compressionAlgorithm api.CompressionAlgorithm, mediaTypeOverride string, annotations map[string]string, compressorState api.AppenderState, outputFile io.Writer) error {
	// An empty --history asks for a layer with no history entries at all;
	// omitting the flag defaults to MissingHistoryMarker, which falls
	// through to the non-empty case below like any other value.
	var layerHistory []api.History
	if history == "" {
		layerHistory = []api.History{}
	} else {
		layerHistory = api.LayerHistory(history)
	}
	var mediaType string
	if mediaTypeOverride != "" {
		mediaType = mediaTypeOverride
	} else if containerFormat == api.ErofsFormat {
		switch compressionAlgorithm {
		case api.Uncompressed:
			mediaType = api.ErofsLayer
		case api.Zstd:
			mediaType = api.ErofsZstdLayer
		default:
			return fmt.Errorf("unsupported compression algorithm for an EROFS layer: %s", compressionAlgorithm)
		}
	} else {
		switch compressionAlgorithm {
		case api.Uncompressed:
			mediaType = api.TarLayer
		case api.Gzip:
			mediaType = api.TarGzipLayer
		case api.Zstd:
			mediaType = api.TarZstdLayer
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

// generateLayerZtoc builds a ztoc (SOCI table of contents) for the finalized
// gzip layer blob at blobPath and writes it to ztocPath. It must be called after
// the layer's compressor has been finalized so blobPath holds the complete
// compressed stream.
func generateLayerZtoc(blobPath, ztocPath string, spanSize int64, buildToolIdentifier string) error {
	z, err := ztoc.BuildFromFile(blobPath, ztoc.WithSpanSize(spanSize), ztoc.WithBuildToolIdentifier(buildToolIdentifier))
	if err != nil {
		return fmt.Errorf("building ztoc: %w", err)
	}
	data, err := ztoc.Marshal(z)
	if err != nil {
		return fmt.Errorf("marshaling ztoc: %w", err)
	}
	if err := os.WriteFile(ztocPath, data, 0o644); err != nil {
		return fmt.Errorf("writing ztoc: %w", err)
	}
	return nil
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
