package docker

import (
	"fmt"
	"io"
	"os"
	"os/exec"
)

// LoadWithDaemon pipes the tar stream to the specified daemon's "image load" command.
func LoadWithDaemon(tarReader io.Reader, daemon string) error {
	loaderBinary, ok := os.LookupEnv("LOADER_BINARY")
	if !ok {
		switch daemon {
		case "generic":
			return fmt.Errorf("generic daemon requires LOADER_BINARY environment variable to be set")
		case "containerization":
			loaderBinary = "container"
		default:
			loaderBinary = daemon
		}
	}
	return loadWithBinaryArgs(tarReader, loaderBinary, []string{"image", "load"})
}

func loadWithBinaryArgs(tarReader io.Reader, loaderBinary string, args []string) error {
	if _, err := exec.LookPath(loaderBinary); err != nil {
		return fmt.Errorf("%s not found in PATH: %w", loaderBinary, err)
	}

	fmt.Fprintf(os.Stderr, "Using %s as loader binary\n", loaderBinary)

	cmd := exec.Command(loaderBinary, args...)
	cmd.Stdin = tarReader
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %v failed: %w", loaderBinary, args, err)
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
