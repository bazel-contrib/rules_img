package persistentworker

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// ParseArgs processes command arguments by expanding argfiles and extracting
// the persistent_worker flag. It returns the processed argument slice and
// a boolean indicating whether the persistent_worker flag was set.
func ParseArgs(args []string) ([]string, bool, error) {
	// Expand argfile if present
	expandedArgs, err := expandArgfile(args)
	if err != nil {
		return nil, false, err
	}

	// Extract persistent_worker flag
	processedArgs, isPersistentWorker := extractPersistentWorkerFlag(expandedArgs)

	return processedArgs, isPersistentWorker, nil
}

// extractPersistentWorkerFlag searches for and removes the --persistent_worker flag.
// Returns the remaining args and whether the flag was found.
func extractPersistentWorkerFlag(args []string) ([]string, bool) {
	isPersistentWorker := false
	result := make([]string, 0, len(args))

	for _, arg := range args {
		if arg == "--persistent_worker" {
			isPersistentWorker = true
			// Skip this arg - don't add it to result
		} else {
			result = append(result, arg)
		}
	}

	return result, isPersistentWorker
}

// expandArgfile expands a single optional argfile argument (@path/to/file).
// If an argfile is found, it reads the file and returns the arguments from it.
// Only one argfile is supported.
func expandArgfile(args []string) ([]string, error) {
	argfileIndex := -1
	for i, arg := range args {
		if strings.HasPrefix(arg, "@") {
			if argfileIndex != -1 {
				return nil, fmt.Errorf("multiple argfiles not supported")
			}
			argfileIndex = i
		}
	}

	// No argfile found
	if argfileIndex == -1 {
		return args, nil
	}

	argfilePath := args[argfileIndex][1:] // Remove @ prefix
	fileArgs, err := readArgfile(argfilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read argfile %s: %w", argfilePath, err)
	}

	// Build new args slice: args before argfile + fileArgs + args after argfile
	result := make([]string, 0, len(args)-1+len(fileArgs))
	result = append(result, args[:argfileIndex]...)
	result = append(result, fileArgs...)
	result = append(result, args[argfileIndex+1:]...)

	return result, nil
}

// readArgfile reads arguments from a file, one per line.
// Empty lines and lines starting with # are ignored.
func readArgfile(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var args []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		args = append(args, line)
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return args, nil
}
