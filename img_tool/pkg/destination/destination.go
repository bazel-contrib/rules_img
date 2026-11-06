// Package destination parses push destination files: a single line naming the
// registry and repository to push to, as "registry/repository".
package destination

import (
	"fmt"
	"os"
	"strings"
)

// ParseFile reads the destination file at path and splits its content at the
// first '/' into the registry and the repository.
func ParseFile(path string) (registry string, repository string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("reading destination file: %w", err)
	}

	destination := strings.TrimSpace(string(data))

	if destination == "" {
		return "", "", fmt.Errorf("destination file %q is empty", path)
	}

	if strings.ContainsAny(destination, "\n\r") {
		return "", "", fmt.Errorf("destination file %q must contain exactly one line", path)
	}

	slashIndex := strings.Index(destination, "/")
	if slashIndex < 0 {
		return "", "", fmt.Errorf("destination file %q must contain a '/' separating registry from repository, got %q", path, destination)
	}

	registry = destination[:slashIndex]
	repository = destination[slashIndex+1:]

	if registry == "" {
		return "", "", fmt.Errorf("destination file %q has an empty registry (content: %q)", path, destination)
	}
	if repository == "" {
		return "", "", fmt.Errorf("destination file %q has an empty repository (content: %q)", path, destination)
	}

	return registry, repository, nil
}
