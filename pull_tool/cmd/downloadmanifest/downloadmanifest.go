package downloadmanifest

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	reg "github.com/bazel-contrib/rules_img/pull_tool/pkg/auth/registry"
)

func DownloadManifestProcess(ctx context.Context, args []string) {
	var digest string
	var tag string
	var repository string
	var outputPath string
	var registries stringSliceFlag
	var printDigest bool

	flagSet := flag.NewFlagSet("download-manifest", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Downloads a manifest from a container registry by digest or tag.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: pull_tool download-manifest [OPTIONS]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"pull_tool download-manifest --digest sha256:abc123... --repository myapp --output manifest.json",
			"pull_tool download-manifest --tag latest --repository myapp --registry docker.io --output manifest.json",
		}
		fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
	}

	flagSet.StringVar(&digest, "digest", "", "The digest of the manifest to download")
	flagSet.StringVar(&tag, "tag", "", "The tag of the manifest to download")
	flagSet.StringVar(&repository, "repository", "", "Repository name of the image (required)")
	flagSet.StringVar(&outputPath, "output", "", "Output file path (required)")
	flagSet.Var(&registries, "registry", "Registry to use (can be specified multiple times, defaults to docker.io)")
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
	if repository == "" {
		fmt.Fprintf(os.Stderr, "Error: --repository is required\n")
		flagSet.Usage()
		os.Exit(1)
	}
	if outputPath == "" && !printDigest {
		fmt.Fprintf(os.Stderr, "Error: --output is required\n")
		flagSet.Usage()
		os.Exit(1)
	}

	// Default to docker.io if no registries specified
	if len(registries) == 0 {
		registries = []string{"docker.io"}
	}

	// Add sha256: prefix if not present for digest
	if digest != "" && !strings.HasPrefix(digest, "sha256:") {
		digest = "sha256:" + digest
	}

	// Try each registry until success
	var lastErr error
	var resolvedDigest string
	for _, registry := range registries {
		var err error
		if digest != "" {
			err = downloadManifestByDigest(registry, repository, digest, outputPath, printDigest, &resolvedDigest)
		} else {
			err = downloadManifestByTag(registry, repository, tag, outputPath, printDigest, &resolvedDigest)
		}
		if err == nil {
			break
		}
		lastErr = err
		fmt.Fprintf(os.Stderr, "Failed to download from %s: %v\n", registry, err)
	}

	if lastErr != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to download manifest from all registries: %v\n", lastErr)
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
