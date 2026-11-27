package pull

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/gateway"
	imgreferrers "github.com/bazel-contrib/rules_img/img_tool/pkg/referrers"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/registryopts"
)

// CosignSubjectsInput is the --input of `img cosign-subjects`.
type CosignSubjectsInput struct {
	// Tree are the manifests of the pulled image: the root and, for an index,
	// its children.
	Tree []string `json:"tree"`
	// Artifacts are the referrers without a subject field to attribute.
	Artifacts []CosignSubjectsArtifact `json:"artifacts"`
	// Sources (repository@registry) to fetch the layers that Blobs doesn't
	// have from.
	Sources []string `json:"sources,omitempty"`
	// Index is the digest of the image index the tree is rooted at, if it is
	// one: an artifact it lists as a BuildKit attestation manifest is about the
	// manifest the index names. It is fetched from Sources.
	Index string `json:"index,omitempty"`
}

// CosignSubjectsArtifact is a referrer manifest to attribute.
type CosignSubjectsArtifact struct {
	Digest string `json:"digest"`
	// Manifest is the path of the manifest.
	Manifest string `json:"manifest"`
	// Blobs maps the digests of layers that were downloaded already to their
	// paths.
	Blobs map[string]string `json:"blobs,omitempty"`
}

// CosignSubjectsProcess implements `img cosign-subjects`, which attributes
// pinned referrers that have no subject field: BuildKit attestation manifests
// (oci-artifact=false), which the image index ties to the manifest they're
// about, and cosign's tag-scheme signatures and attestations, which their
// signed payloads tie to it. The repository rules use it, since Starlark can't
// decode a DSSE payload.
func CosignSubjectsProcess(ctx context.Context, args []string) {
	var inputPath, outputPath string
	flagSet := flag.NewFlagSet("cosign-subjects", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Attributes cosign's tag-scheme signatures and attestations to the manifest of a pulled image their signed payloads name.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img cosign-subjects --input=<json> --output=<json>\n")
		flagSet.PrintDefaults()
	}
	flagSet.StringVar(&inputPath, "input", "", `Path to a JSON object {"tree": [digests], "artifacts": [{"digest", "manifest": path, "blobs": {digest: path}}], "sources": ["repository@registry"], "index": digest} (required)`)
	flagSet.StringVar(&outputPath, "output", "", "Path to write the result to, as JSON mapping each artifact to its subject and cosign tag suffix (required)")
	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}
	if inputPath == "" || outputPath == "" {
		fmt.Fprintf(os.Stderr, "Error: --input and --output are required\n")
		flagSet.Usage()
		os.Exit(1)
	}
	if err := cosignSubjects(ctx, inputPath, outputPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// CosignSubject is what `img cosign-subjects` reports for an artifact.
type CosignSubject struct {
	Subject string `json:"subject"`
	Tag     string `json:"tag"`
}

func cosignSubjects(ctx context.Context, inputPath, outputPath string) error {
	data, err := os.ReadFile(inputPath)
	if err != nil {
		return err
	}
	var input CosignSubjectsInput
	if err := json.Unmarshal(data, &input); err != nil {
		return fmt.Errorf("parsing %s: %w", inputPath, err)
	}
	remote, err := remoteBlobReader(ctx, input.Sources)
	if err != nil {
		return err
	}
	var index []byte
	if input.Index != "" {
		if index, err = fetchIndexFromSources(ctx, input.Index, input.Sources); err != nil {
			return fmt.Errorf("fetching the image index %s: %w", input.Index, err)
		}
	}
	result := map[string]CosignSubject{}
	for _, artifact := range input.Artifacts {
		// A BuildKit attestation manifest of the index: the index names its subject.
		if subject, ok, err := imgreferrers.BuildKitAttestationSubject(index, artifact.Digest); err != nil {
			return err
		} else if ok {
			if !slices.Contains(input.Tree, subject) {
				return fmt.Errorf("BuildKit attestation manifest %s is about %s, which is not part of the image", artifact.Digest, subject)
			}
			result[artifact.Digest] = CosignSubject{Subject: subject}
			continue
		}
		raw, err := os.ReadFile(artifact.Manifest)
		if err != nil {
			return err
		}
		read := func(layer v1.Descriptor) ([]byte, error) {
			if path, ok := artifact.Blobs[layer.Digest.String()]; ok {
				return os.ReadFile(path)
			}
			if remote == nil {
				return nil, fmt.Errorf("not downloaded, and no source to fetch it from")
			}
			return remote(layer)
		}
		cosign, subject, err := imgreferrers.AttributeCosignArtifact(raw, read, input.Tree)
		if err != nil {
			return fmt.Errorf("referrer %s has no subject, and is not one of cosign's signatures or attestations of the image: %w", artifact.Digest, err)
		}
		result[artifact.Digest] = CosignSubject{Subject: subject, Tag: cosign.Suffix}
	}
	out, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return os.WriteFile(outputPath, out, 0o644)
}

// remoteBlobReader reads blobs from the first of sources that has them, or
// returns nil without sources.
func remoteBlobReader(ctx context.Context, sources []string) (func(v1.Descriptor) ([]byte, error), error) {
	if len(sources) == 0 {
		return nil, nil
	}
	transport, err := registryopts.Transport(gateway.ModePull)
	if err != nil {
		return nil, fmt.Errorf("configuring registry gateway: %w", err)
	}
	options := ReferrersOptions(transport)
	var readers []func(v1.Descriptor) ([]byte, error)
	for _, source := range sources {
		repo, err := sourceRepository(source)
		if err != nil {
			return nil, err
		}
		readers = append(readers, imgreferrers.RemoteLayers(ctx, repo, options))
	}
	return func(layer v1.Descriptor) ([]byte, error) {
		var lastErr error
		for _, read := range readers {
			data, err := read(layer)
			if err == nil {
				return data, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}, nil
}

// fetchIndexFromSources returns the image index with the given digest from the first of sources
// that has it.
func fetchIndexFromSources(ctx context.Context, digest string, sources []string) ([]byte, error) {
	transport, err := registryopts.Transport(gateway.ModePull)
	if err != nil {
		return nil, fmt.Errorf("configuring registry gateway: %w", err)
	}
	return fetchManifestFromSources(ctx, digest, sources, ReferrersOptions(transport))
}

