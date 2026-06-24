package argfile

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Expand expands a single optional argfile argument (@path/to/file).
// Argfiles use Bazel's multiline format: one argument per line.
// Empty lines and lines starting with # are ignored.
func Expand(args []string) ([]string, error) {
	argfileIndex := -1
	for i, arg := range args {
		if strings.HasPrefix(arg, "@") {
			if argfileIndex != -1 {
				return nil, fmt.Errorf("multiple argfiles not supported")
			}
			argfileIndex = i
		}
	}

	if argfileIndex == -1 {
		return args, nil
	}

	argfilePath := args[argfileIndex][1:]
	fileArgs, err := read(argfilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read argfile %s: %w", argfilePath, err)
	}

	result := make([]string, 0, len(args)-1+len(fileArgs))
	result = append(result, args[:argfileIndex]...)
	result = append(result, fileArgs...)
	result = append(result, args[argfileIndex+1:]...)

	return result, nil
}

func read(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var args []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
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
