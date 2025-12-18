package deploymetadata

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

func processLayerHints(inputPath, outputPath string) error {
	// 1. Setup - open input and output files
	inputFile, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("opening input file: %w", err)
	}
	defer inputFile.Close()

	outputFile, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("creating output file: %w", err)
	}
	defer outputFile.Close()

	// 2. Process input file and build digest map
	digestMap, err := processInputFile(inputFile)
	if err != nil {
		return fmt.Errorf("processing input file: %w", err)
	}

	// 3. Write output in sorted format
	if err := writeOutputFile(outputFile, digestMap); err != nil {
		return fmt.Errorf("writing output file: %w", err)
	}

	return nil
}

// processInputFile reads the input file line by line and builds a map of digest -> blob paths
func processInputFile(inputFile *os.File) (map[string][]string, error) {
	digestMap := make(map[string][]string)
	scanner := bufio.NewScanner(inputFile)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue // skip empty lines
		}

		// Split by null byte to get blob path and metadata path
		parts := strings.Split(line, "\x00")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid line format: expected 2 parts separated by null byte, got %d", len(parts))
		}

		blobPath := parts[0]
		metadataPath := parts[1]

		// Get digest from metadata file
		digest, err := getDigestFromMetadata(metadataPath)
		if err != nil {
			return nil, fmt.Errorf("getting digest from metadata file %s: %w", metadataPath, err)
		}

		// Add blob path to the list for this digest
		digestMap[digest] = append(digestMap[digest], blobPath)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading input file: %w", err)
	}

	return digestMap, nil
}

// getDigestFromMetadata reads a metadata file and extracts the digest
func getDigestFromMetadata(metadataPath string) (string, error) {
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return "", fmt.Errorf("reading metadata file: %w", err)
	}

	var descriptor api.Descriptor
	if err := json.Unmarshal(data, &descriptor); err != nil {
		return "", fmt.Errorf("unmarshaling metadata JSON: %w", err)
	}

	if descriptor.Digest == "" {
		return "", fmt.Errorf("digest field is empty in metadata")
	}

	return descriptor.Digest, nil
}

// writeOutputFile writes the digest map to the output file in sorted format
func writeOutputFile(outputFile *os.File, digestMap map[string][]string) error {
	// Get digests in sorted order
	digests := make([]string, 0, len(digestMap))
	for digest := range digestMap {
		digests = append(digests, digest)
	}
	slices.Sort(digests)

	// Write each digest with its blob paths
	for _, digest := range digests {
		blobPaths := digestMap[digest]

		// Build line: digest followed by null-byte-separated blob paths
		var buf bytes.Buffer
		buf.WriteString(digest)
		slices.Sort(blobPaths)
		blobPaths = slices.Compact(blobPaths)
		for _, blobPath := range blobPaths {
			buf.WriteByte('\x00')
			buf.WriteString(blobPath)
		}
		buf.WriteByte('\n')

		if _, err := outputFile.Write(buf.Bytes()); err != nil {
			return fmt.Errorf("writing output line: %w", err)
		}
	}

	return nil
}

// mergeLayerHintsFiles merges multiple layer hints output files into a single file.
// Each input file has format: digest\0blobpath1\0blobpath2\0...\n
// The output file will have the same format with all unique blob paths per digest.
func mergeLayerHintsFiles(inputPaths []string, outputPath string) error {
	digestMap := make(map[string][]string)

	// Read all input files and merge into digestMap
	for _, inputPath := range inputPaths {
		if err := readLayerHintsOutput(inputPath, digestMap); err != nil {
			return fmt.Errorf("reading layer hints file %s: %w", inputPath, err)
		}
	}

	// Write merged output
	outputFile, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("creating output file: %w", err)
	}
	defer outputFile.Close()

	if err := writeOutputFile(outputFile, digestMap); err != nil {
		return fmt.Errorf("writing output file: %w", err)
	}

	return nil
}

// readLayerHintsOutput reads a layer hints output file and adds entries to digestMap
func readLayerHintsOutput(inputPath string, digestMap map[string][]string) error {
	inputFile, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("opening input file: %w", err)
	}
	defer inputFile.Close()

	scanner := bufio.NewScanner(inputFile)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue // skip empty lines
		}

		// Split by null byte to get digest and blob paths
		parts := strings.Split(line, "\x00")
		if len(parts) < 1 {
			return fmt.Errorf("invalid line format: expected at least 1 part")
		}

		digest := parts[0]
		blobPaths := parts[1:]

		// Add blob paths to the map for this digest
		digestMap[digest] = append(digestMap[digest], blobPaths...)
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading input file: %w", err)
	}

	return nil
}
