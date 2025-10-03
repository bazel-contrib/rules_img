package docker

import (
	"fmt"
	"io"
	"os"
	"os/exec"
)

// Load pipes the tar stream to docker load
func Load(tarReader io.Reader) error {
	loaderBinary, ok := os.LookupEnv("LOADER_BINARY")
	if !ok {
		loaderBinary = "docker"
	}

	if _, err := exec.LookPath(loaderBinary); err != nil {
		return fmt.Errorf("%s not found in PATH: %w", loaderBinary, err)
	}

	fmt.Fprintf(os.Stderr, "Using %s as loader binary\n", loaderBinary)

	cmd := exec.Command(loaderBinary, "load")
	cmd.Stdin = tarReader
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s load failed: %w", loaderBinary, err)
	}

	return nil
}

// NormalizeTag normalizes a tag for Docker
func NormalizeTag(tag string) string {
	if tag == "" {
		return ""
	}

	// Docker load expects the full image reference
	// The normalization happens in the Load function in pkg/load
	return tag
}
