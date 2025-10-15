package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compress"
)

var (
	stateIn              string
	stateOut             string
	compressionLevel     *int
	compressionAlgorithm string
	hashFunction         string
	contentType          string
	inputFilePath        string
	outputFilePath       string
	appendOutput         bool
)

func main() {
	flag.StringVar(&stateIn, "state-in", "", "Path to the state file to resume from")
	flag.StringVar(&stateOut, "state-out", "", "Path to the state file to write to")
	flag.Func("compression-level", "Compression level (0-9)", func(s string) error {
		level, err := strconv.Atoi(s)
		if err != nil {
			return err
		}
		compressionLevel = &level
		return nil
	})
	flag.StringVar(&compressionAlgorithm, "compression-algorithm", "gzip", "Compression algorithm to use")
	flag.StringVar(&hashFunction, "hash-function", "sha256", "Hash function to use")
	flag.StringVar(&contentType, "content-type", "", "Content type of the input file")
	flag.BoolVar(&appendOutput, "append", false, "Append to an existing output file")
	flag.Parse()

	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(1)
	}
	inputFilePath = flag.Arg(0)
	outputFilePath = flag.Arg(1)

	inputFile, err := os.Open(inputFilePath)
	if err != nil {
		fmt.Printf("Error opening input file: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = inputFile.Close() }()

	outputFileFlags := os.O_WRONLY | os.O_CREATE
	if appendOutput {
		outputFileFlags |= os.O_APPEND
	} else {
		outputFileFlags |= os.O_TRUNC
	}
	outputFile, err := os.OpenFile(outputFilePath, outputFileFlags, 0o644)
	if err != nil {
		fmt.Printf("Error opening output file: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = outputFile.Close() }()

	var state api.AppenderState
	if stateIn != "" {
		state, err = readStateFromFile(stateIn)
		if err != nil {
			fmt.Printf("Error reading input state file: %v\n", err)
			os.Exit(1)
		}
	}

	if appendOutput && stateIn != "" {
		fileInfo, err := outputFile.Stat()
		if err == nil && state.CompressedSize != fileInfo.Size() {
			fmt.Printf("Warning: The output file size %d does not match the state file size %d. The state file may be invalid.\n", fileInfo.Size(), state.CompressedSize)
		}
	}

	var opts []compress.Option
	if compressionLevel != nil {
		opts = append(opts, compress.CompressionLevel(*compressionLevel))
	}
	if len(contentType) > 0 {
		opts = append(opts, compress.ContentType(contentType))
	}

	var appender api.Appender
	if stateIn != "" {
		appender, err = compress.ResumeFactory(hashFunction, compressionAlgorithm, state, outputFile, opts...)
	} else {
		appender, err = compress.AppenderFactory(hashFunction, compressionAlgorithm, outputFile, opts...)
	}
	if err != nil {
		fmt.Printf("Error creating appender: %v\n", err)
		os.Exit(1)
	}

	_, err = io.Copy(appender, inputFile)
	if err != nil {
		fmt.Printf("Error copying data: %v\n", err)
		os.Exit(1)
	}

	state, err = appender.Finalize()
	if err != nil {
		fmt.Printf("Error finalizing appender: %v\n", err)
		os.Exit(1)
	}
	err = writeStateToFile(stateOut, state)
	if err != nil {
		fmt.Printf("Error writing output state file: %v\n", err)
		os.Exit(1)
	}
}

func readStateFromFile(filePath string) (api.AppenderState, error) {
	rawFile, err := os.ReadFile(filePath)
	if err != nil {
		return api.AppenderState{}, fmt.Errorf("opening state file: %w", err)
	}

	var state api.AppenderState
	if err := state.UnmarshalBinary(rawFile); err != nil {
		return api.AppenderState{}, fmt.Errorf("unmarshalling state file: %w", err)
	}

	return state, nil
}

func writeStateToFile(filePath string, state api.AppenderState) error {
	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("creating state file: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "Error closing state file: %v\n", closeErr)
		}
	}()

	rawFile, err := state.MarshalBinary()
	if err != nil {
		return fmt.Errorf("marshalling state file: %w", err)
	}
	_, err = file.Write(rawFile)
	return err
}
