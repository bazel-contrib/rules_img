package downloadblob

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	reg "github.com/bazel-contrib/rules_img/pull_tool/pkg/auth/registry"
)

func DownloadBlobProcess(ctx context.Context, args []string) {
	var digest string
	var outputPath string
	var sources stringSliceFlag
	var executable bool

	flagSet := flag.NewFlagSet("download-blob", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Downloads a single blob from a container registry.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: pull_tool download-blob [OPTIONS]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"pull_tool download-blob --digest sha256:abc123... --source library/ubuntu@index.docker.io --output blob.tar.gz",
			"pull_tool download-blob --digest sha256:abc123... --source library/ubuntu@index.docker.io --source my-mirror/ubuntu@mirror.io --output blob.tar.gz",
		}
		fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
	}

	flagSet.StringVar(&digest, "digest", "", "The digest of the blob to download (required)")
	flagSet.StringVar(&outputPath, "output", "", "Output file path (required)")
	flagSet.Var(&sources, "source", "Source in format repository@registry (can be specified multiple times for mirrors)")
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
	if outputPath == "" {
		fmt.Fprintf(os.Stderr, "Error: --output is required\n")
		flagSet.Usage()
		os.Exit(1)
	}
	if len(sources) == 0 {
		fmt.Fprintf(os.Stderr, "Error: at least one --source is required\n")
		flagSet.Usage()
		os.Exit(1)
	}

	if !strings.HasPrefix(digest, "sha256:") {
		digest = "sha256:" + digest
	}

	// Parse sources into repository@registry pairs
	var sourcesList []Source
	for _, src := range sources {
		parts := strings.SplitN(src, "@", 2)
		if len(parts) != 2 {
			fmt.Fprintf(os.Stderr, "Error: invalid source format '%s', expected repository@registry\n", src)
			os.Exit(1)
		}
		sourcesList = append(sourcesList, Source{
			Repository: parts[0],
			Registry:   parts[1],
		})
	}

	// Randomize sources for load distribution
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	rnd.Shuffle(len(sourcesList), func(i, j int) {
		sourcesList[i], sourcesList[j] = sourcesList[j], sourcesList[i]
	})

	// Try each source until success
	var lastErr error
	for _, source := range sourcesList {
		err := downloadFromRegistry(source.Registry, source.Repository, digest, outputPath)
		if err == nil {
			break
		}
		lastErr = err
		fmt.Fprintf(os.Stderr, "Failed to download from %s/%s: %v\n", source.Registry, source.Repository, err)
	}

	if lastErr != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to download blob from all sources: %v\n", lastErr)
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

type Source struct {
	Repository string
	Registry   string
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

	// Check if the output path ends with well known pattern "blobs/sha256/<hash>".
	// If so, create the parent directories.
	parentDir := filepath.Dir(outputPath)
	if strings.HasSuffix(outputPath, filepath.Join("blobs", "sha256", digest[len("sha256:"):])) {
		if err := os.MkdirAll(parentDir, 0o755); err != nil {
			return fmt.Errorf("creating parent directories for blobs: %w", err)
		}
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
