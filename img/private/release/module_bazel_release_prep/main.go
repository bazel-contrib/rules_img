package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

const (
	startMarker = "### REMOVE_BEFORE_RELEASE_START"
	endMarker   = "### REMOVE_BEFORE_RELEASE_END"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "Usage: %s <input-MODULE.bazel> <output-MODULE.bazel>\n", os.Args[0])
		os.Exit(1)
	}

	inputPath := os.Args[1]
	outputPath := os.Args[2]

	if err := processFile(inputPath, outputPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func processFile(inputPath, outputPath string) error {
	inputFile, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("failed to open input file: %w", err)
	}
	defer inputFile.Close()

	outputFile, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer outputFile.Close()

	scanner := bufio.NewScanner(inputFile)
	writer := bufio.NewWriter(outputFile)
	defer writer.Flush()

	inRemoveSection := false
	var lines []string

	for scanner.Scan() {
		line := scanner.Text()

		if strings.TrimSpace(line) == startMarker {
			inRemoveSection = true
			continue
		}

		if strings.TrimSpace(line) == endMarker {
			inRemoveSection = false
			continue
		}

		if !inRemoveSection {
			lines = append(lines, line)
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("failed to read input file: %w", err)
	}

	// Trim trailing blank lines to ensure exactly one newline at the end
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	// Write all lines and ensure exactly one newline at the end
	for _, line := range lines {
		if _, err := writer.WriteString(line + "\n"); err != nil {
			return fmt.Errorf("failed to write output: %w", err)
		}
	}

	return nil
}
