package downloadblob

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	reg "github.com/bazel-contrib/rules_img/pull_tool/pkg/auth/registry"
)

func DownloadBlobProcess(ctx context.Context, args []string) {
	var digest string
	var repository string
	var outputPath string
	var registries stringSliceFlag
	var executable bool

	flagSet := flag.NewFlagSet("download-blob", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Downloads a single blob from a container registry.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: pull_tool download-blob [OPTIONS]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"pull_tool download-blob --digest sha256:abc123... --repository myapp --output blob.tar.gz",
			"pull_tool download-blob --digest sha256:abc123... --repository myapp --registry docker.io --output blob.tar.gz",
		}
		fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
	}

	flagSet.StringVar(&digest, "digest", "", "The digest of the blob to download (required)")
	flagSet.StringVar(&repository, "repository", "", "Repository name of the image (required)")
	flagSet.StringVar(&outputPath, "output", "", "Output file path (required)")
	flagSet.Var(&registries, "registry", "Registry to use (can be specified multiple times, defaults to docker.io)")
	flagSet.BoolVar(&executable, "executable", false, "Mark the output file executable")

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}

	if digest == "" {
		fmt.Fprintf(os.Stderr, "Error: --digest is required\n")
		flagSet.Usage()
		os.Exit(1)
	}
	if repository == "" {
		fmt.Fprintf(os.Stderr, "Error: --repository is required\n")
		flagSet.Usage()
		os.Exit(1)
	}
	if outputPath == "" {
		fmt.Fprintf(os.Stderr, "Error: --output is required\n")
		flagSet.Usage()
		os.Exit(1)
	}

	// Default to docker.io if no registries specified
	if len(registries) == 0 {
		registries = []string{"docker.io"}
	}

	if !strings.HasPrefix(digest, "sha256:") {
		digest = "sha256:" + digest
	}

	// Try each registry until success
	var lastErr error
	for _, registry := range registries {
		err := downloadFromRegistry(registry, repository, digest, outputPath)
		if err == nil {
			break
		}
		lastErr = err
		fmt.Fprintf(os.Stderr, "Failed to download from %s: %v\n", registry, err)
	}

	if lastErr != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to download blob from all registries: %v\n", lastErr)
		os.Exit(1)
	}

	// Set file permissions after successful download
	if executable {
		if err := os.Chmod(outputPath, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "Error: Failed to set executable permission on output file: %v\n", err)
			os.Exit(1)
		}
	} else {
		if err := os.Chmod(outputPath, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "Error: Failed to set permission on output file: %v\n", err)
			os.Exit(1)
		}
	}
}

func downloadFromRegistry(registry, repository, digest, outputPath string) error {
	ref, err := name.NewDigest(fmt.Sprintf("%s/%s@%s", registry, repository, digest))
	if err != nil {
		return fmt.Errorf("creating blob reference: %w", err)
	}

	layer, err := remote.Layer(ref, reg.WithAuthFromMultiKeychain())
	if err != nil {
		return fmt.Errorf("getting layer: %w", err)
	}

	outputFile, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("opening output file: %w", err)
	}
	defer outputFile.Close()

	rc, err := layer.Compressed()
	if err != nil {
		return fmt.Errorf("getting compressed layer: %w", err)
	}
	defer rc.Close()

	_, err = io.Copy(outputFile, rc)
	if err != nil {
		return fmt.Errorf("writing layer data: %w", err)
	}

	return nil
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSliceFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}
