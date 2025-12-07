package downloadblob

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/malt3/go-containerregistry/pkg/name"
	"github.com/malt3/go-containerregistry/pkg/v1/remote"

	reg "github.com/bazel-contrib/rules_img/img_tool/pkg/auth/registry"
)

type Source struct {
	Repository string
	Registry   string
}

func DownloadBlobProcess(ctx context.Context, args []string) {
	var digest string
	var outputPath string
	var sources stringSliceFlag
	var executable bool

	flagSet := flag.NewFlagSet("download-blob", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Downloads a single blob from a container registry.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img download-blob [OPTIONS]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"img download-blob --digest sha256:abc123... --source myapp@index.docker.io --output blob.tar.gz",
			"img download-blob --digest sha256:abc123... --source myapp@index.docker.io --source myapp@mirror.io --output blob.tar.gz",
		}
		fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
	}

	flagSet.StringVar(&digest, "digest", "", "The digest of the blob to download (required)")
	flagSet.StringVar(&outputPath, "output", "", "Output file path (required)")
	flagSet.Var(&sources, "source", "Source in format repository@registry (can be specified multiple times, defaults to using index.docker.io)")
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

	// Parse sources
	var sourcesList []Source
	if len(sources) == 0 {
		fmt.Fprintf(os.Stderr, "Error: at least one --source is required\n")
		flagSet.Usage()
		os.Exit(1)
	}

	for _, src := range sources {
		parts := strings.SplitN(src, "@", 2)
		if len(parts) != 2 {
			fmt.Fprintf(os.Stderr, "Error: source must be in format repository@registry, got: %s\n", src)
			os.Exit(1)
		}
		sourcesList = append(sourcesList, Source{
			Repository: parts[0],
			Registry:   parts[1],
		})
	}

	// Randomize source order for load distribution
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	rnd.Shuffle(len(sourcesList), func(i, j int) {
		sourcesList[i], sourcesList[j] = sourcesList[j], sourcesList[i]
	})

	if !strings.HasPrefix(digest, "sha256:") {
		digest = "sha256:" + digest
	}

	// Try each source until success
	var lastErr error
	for _, source := range sourcesList {
		err := downloadFromRegistry(source.Registry, source.Repository, digest, outputPath)
		if err == nil {
			if executable {
				if err := os.Chmod(outputPath, 0o755); err != nil {
					fmt.Fprintf(os.Stderr, "Error: Failed to set executable permission on output file: %v\n", err)
					os.Exit(1)
				}
			} else {
				if err := os.Chmod(outputPath, 0o644); err != nil {
					fmt.Fprintf(os.Stderr, "Error: Failed to remove executable permission on output file: %v\n", err)
					os.Exit(1)
				}
			}
			return
		}
		lastErr = err
		fmt.Fprintf(os.Stderr, "Failed to download from %s/%s: %v\n", source.Registry, source.Repository, err)
	}

	fmt.Fprintf(os.Stderr, "Error: Failed to download blob from all sources: %v\n", lastErr)
	os.Exit(1)
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
