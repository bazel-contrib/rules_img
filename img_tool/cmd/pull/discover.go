package pull

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/gateway"
	imgreferrers "github.com/bazel-contrib/rules_img/img_tool/pkg/referrers"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/registryopts"
)

// DiscoverReferrersProcess implements `img discover-referrers`, which lists the
// referrers of manifests by artifact type. The repository rules use it when
// Bazel's downloader fetches the blobs: Bazel's downloader exposes neither
// status codes nor headers, which listing referrers per the distribution spec
// needs (the 404 that triggers the tag schema fallback, Link pagination).
func DiscoverReferrersProcess(ctx context.Context, args []string) {
	var subjects stringSliceFlag
	var artifactTypes stringSliceFlag
	var sources stringSliceFlag
	var outputPath string
	var index string

	flagSet := flag.NewFlagSet("discover-referrers", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Lists the referrers of manifests by artifact type, using the referrers API or the referrers tag schema.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img discover-referrers [OPTIONS]\n")
		flagSet.PrintDefaults()
		fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		fmt.Fprintf(flagSet.Output(), "  $ img discover-referrers --subject sha256:... --artifact-type application/vnd.cncf.notary.signature --source library/ubuntu@index.docker.io --output referrers.json\n")
	}
	flagSet.Var(&subjects, "subject", "Digest of a manifest whose referrers to list (can be specified multiple times)")
	flagSet.Var(&artifactTypes, "artifact-type", "Artifact type of referrers to keep, * for referrers of any type, cosign:sig / cosign:att for cosign's tag-scheme signatures / attestations, or buildkit:attestation for the BuildKit attestation manifests --index lists (can be specified multiple times)")
	flagSet.StringVar(&index, "index", "", "Digest of the image index the subjects belong to, whose entries name their BuildKit attestation manifests (for --artifact-type buildkit:attestation)")
	flagSet.Var(&sources, "source", "Source in the format repository@registry, tried in order until one answers (can be specified multiple times)")
	flagSet.StringVar(&outputPath, "output", "", "Path to write the result to, as JSON mapping each subject to an object of artifact type to referrer digests (required)")
	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}
	if outputPath == "" || len(sources) == 0 {
		fmt.Fprintf(os.Stderr, "Error: --output and at least one --source are required\n")
		flagSet.Usage()
		os.Exit(1)
	}

	transport, err := registryopts.Transport(gateway.ModePull)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error configuring registry gateway: %v\n", err)
		os.Exit(1)
	}

	options := ReferrersOptions(transport)
	if index != "" && slices.Contains(artifactTypes, imgreferrers.BuildKitAttestationType) {
		raw, err := fetchManifestFromSources(ctx, index, sources, options)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: fetching the image index %s: %v\n", index, err)
			os.Exit(1)
		}
		options.Index = raw
	}
	result := map[string]map[string][]string{}
	for _, subject := range subjects {
		found, err := discoverFromSources(ctx, subject, artifactTypes, sources, options)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		result[subject] = found
	}
	data, err := json.Marshal(result)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(outputPath, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "Error: writing %s: %v\n", outputPath, err)
		os.Exit(1)
	}
}

func discoverFromSources(ctx context.Context, subject string, artifactTypes, sources []string, options imgreferrers.Options) (map[string][]string, error) {
	var lastErr error
	for _, source := range sources {
		repo, err := sourceRepository(source)
		if err != nil {
			return nil, err
		}
		found, err := imgreferrers.Discover(ctx, repo, subject, artifactTypes, options)
		if err == nil {
			return found, nil
		}
		lastErr = err
		fmt.Fprintf(os.Stderr, "Warning: failed to discover referrers from %s: %v\n", source, err)
	}
	return nil, fmt.Errorf("discovering referrers of %s from all sources: %w", subject, lastErr)
}

// sourceRepository returns the repository of a source in the format
// repository@registry.
func sourceRepository(source string) (name.Repository, error) {
	i := strings.LastIndex(source, "@")
	if i < 0 {
		return name.Repository{}, fmt.Errorf("invalid source %q, want repository@registry", source)
	}
	repository, registry := source[:i], source[i+1:]
	repo, err := name.NewRepository(fmt.Sprintf("%s/%s", registry, repository), registryopts.NameOptions()...)
	if err != nil {
		return name.Repository{}, fmt.Errorf("invalid source %q: %w", source, err)
	}
	return repo, nil
}

// fetchManifestFromSources returns the manifest (or index) with the given digest from the first
// source that has it. go-containerregistry checks its content against the digest.
func fetchManifestFromSources(ctx context.Context, digest string, sources []string, options imgreferrers.Options) ([]byte, error) {
	var lastErr error
	for _, source := range sources {
		repo, err := sourceRepository(source)
		if err != nil {
			return nil, err
		}
		descriptor, err := remote.Get(repo.Digest(digest), append([]remote.Option{remote.WithContext(ctx)}, options.Remote...)...)
		if err == nil {
			return descriptor.Manifest, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no sources")
	}
	return nil, lastErr
}

