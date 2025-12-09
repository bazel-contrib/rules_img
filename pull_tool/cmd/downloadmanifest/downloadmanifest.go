package downloadmanifest

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	reg "github.com/bazel-contrib/rules_img/pull_tool/pkg/auth/registry"
)

func DownloadManifestProcess(ctx context.Context, args []string) {
	var digest string
	var tag string
	var outputPath string
	var sources stringSliceFlag
	var printDigest bool

	flagSet := flag.NewFlagSet("download-manifest", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Downloads a manifest from a container registry by digest or tag.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: pull_tool download-manifest [OPTIONS]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"pull_tool download-manifest --digest sha256:abc123... --source library/ubuntu@index.docker.io --output manifest.json",
			"pull_tool download-manifest --tag latest --source library/ubuntu@index.docker.io --source my-mirror/ubuntu@mirror.io --output manifest.json",
		}
		fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
	}

	flagSet.StringVar(&digest, "digest", "", "The digest of the manifest to download")
	flagSet.StringVar(&tag, "tag", "", "The tag of the manifest to download")
	flagSet.StringVar(&outputPath, "output", "", "Output file path (required unless --print-digest is used)")
	flagSet.Var(&sources, "source", "Source in format repository@registry (can be specified multiple times for mirrors)")
	flagSet.BoolVar(&printDigest, "print-digest", false, "Print only the digest to stdout and exit (useful for learning digests from tags)")

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}

	if digest == "" && tag == "" {
		fmt.Fprintf(os.Stderr, "Error: either --digest or --tag is required\n")
		flagSet.Usage()
		os.Exit(1)
	}
	if digest != "" && tag != "" {
		fmt.Fprintf(os.Stderr, "Error: cannot specify both --digest and --tag\n")
		flagSet.Usage()
		os.Exit(1)
	}
	if len(sources) == 0 {
		fmt.Fprintf(os.Stderr, "Error: at least one --source is required\n")
		flagSet.Usage()
		os.Exit(1)
	}
	if outputPath == "" && !printDigest {
		fmt.Fprintf(os.Stderr, "Error: --output is required\n")
		flagSet.Usage()
		os.Exit(1)
	}

	// Add sha256: prefix if not present for digest
	if digest != "" && !strings.HasPrefix(digest, "sha256:") {
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
	fmt.Printf("trying the following sources: %v\n", sourcesList)

	// Try each source until success
	var lastErr error
	var resolvedDigest string
	for _, source := range sourcesList {
		var err error
		if digest != "" {
			err = downloadManifestByDigest(source.Registry, source.Repository, digest, outputPath, printDigest, &resolvedDigest)
		} else {
			err = downloadManifestByTag(source.Registry, source.Repository, tag, outputPath, printDigest, &resolvedDigest)
		}
		lastErr = err
		if err == nil {
			break
		}
		fmt.Fprintf(os.Stderr, "Failed to download from %s/%s: %v\n", source.Registry, source.Repository, err)
	}

	if lastErr != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to download manifest from all sources: %v\n", lastErr)
		os.Exit(1)
	}

	// If print-digest mode, just print the digest and exit
	if printDigest {
		fmt.Println(resolvedDigest)
		return
	}

	// Set file permissions after successful download
	if err := os.Chmod(outputPath, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to set permission on output file: %v\n", err)
		os.Exit(1)
	}
}

type Source struct {
	Repository string
	Registry   string
}

func downloadManifestByDigest(registry, repository, digest, outputPath string, printDigest bool, resolvedDigest *string) error {
	ref, err := name.NewDigest(fmt.Sprintf("%s/%s@%s", registry, repository, digest))
	if err != nil {
		return fmt.Errorf("creating manifest reference: %w", err)
	}

	return downloadManifest(ref, outputPath, printDigest, resolvedDigest)
}

func downloadManifestByTag(registry, repository, tag, outputPath string, printDigest bool, resolvedDigest *string) error {
	ref, err := name.NewTag(fmt.Sprintf("%s/%s:%s", registry, repository, tag))
	if err != nil {
		return fmt.Errorf("creating tag reference: %w", err)
	}

	return downloadManifest(ref, outputPath, printDigest, resolvedDigest)
}

func downloadManifest(ref name.Reference, outputPath string, printDigest bool, resolvedDigest *string) error {
	descriptor, err := remote.Get(ref, reg.WithAuthFromMultiKeychain())
	if err != nil {
		return fmt.Errorf("getting manifest: %w", err)
	}

	// Store the resolved digest
	*resolvedDigest = descriptor.Descriptor.Digest.String()

	// If print-digest mode, we're done - no need to write the file
	if printDigest {
		return nil
	}

	// Check if the output path ends with well known pattern "blobs/sha256/<hash>".
	// If so, create the parent directories.
	parentDir := filepath.Dir(outputPath)
	if strings.HasSuffix(parentDir, filepath.Join("blobs", "sha256")) {
		if err := os.MkdirAll(parentDir, 0o755); err != nil {
			return fmt.Errorf("creating parent directories for blobs: %w", err)
		}
	}

	outputFile, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("opening output file: %w", err)
	}
	defer outputFile.Close()

	_, err = outputFile.Write(descriptor.Manifest)
	if err != nil {
		return fmt.Errorf("writing manifest data: %w", err)
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
