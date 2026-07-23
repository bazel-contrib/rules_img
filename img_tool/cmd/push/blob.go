package pushcmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/name"
	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/deployvfs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/gateway"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/registryopts"
)

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	if s == nil {
		return ""
	}
	return fmt.Sprintf("%v", []string(*s))
}

func (s *stringSliceFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

// configurationFile is the expanded push configuration written by the image
// rules ({registry, repository, tags}). Only registry/repository are used here.
type configurationFile struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
}

func blobProcess(ctx context.Context, args []string) {
	var (
		configurationPath string
		metadataPath      string
		blobPath          string
		compactStreamPath string
		casDir            string
		sources           stringSliceFlag
		blobRepository    string
		mediaType         string
		mode              string
		outputPath        string
	)

	flagSet := flag.NewFlagSet("push blob", flag.ContinueOnError)
	flagSet.StringVar(&configurationPath, "configuration-file", "", "Path to the configuration file ({registry, repository}). Required.")
	flagSet.StringVar(&metadataPath, "metadata", "", "Path to the blob metadata JSON (digest/mediaType/size). Optional: when omitted, the descriptor is derived by hashing --blob (used for the config blob, whose descriptor has no standalone file).")
	flagSet.StringVar(&blobPath, "blob", "", "Path to the materialized blob (layer or config) to push.")
	flagSet.StringVar(&compactStreamPath, "compact-stream", "", "Path to the layer's compact stream (.cstream) to reconstruct and push.")
	flagSet.StringVar(&casDir, "cas-dir", "", "Content-addressed input directory (.inputfilecas) for compact-stream reconstruction.")
	flagSet.Var(&sources, "source", "Upstream source as registry/repository to stream a shallow layer from (can be repeated).")
	flagSet.StringVar(&blobRepository, "blob-repository", "", "Repository to push the blob to instead of the configuration repository.")
	flagSet.StringVar(&mediaType, "media-type", "", "Media type recorded in the result when the descriptor is derived from --blob (no --metadata). Informational; unused for the content-addressed upload.")
	flagSet.StringVar(&mode, "mode", "enabled", "Failure mode: 'best_effort' (log, don't fail build) or 'enabled' (fail build).")
	flagSet.StringVar(&outputPath, "output", "", "Path to write the JSON result describing where the blob landed. Required.")

	if err := flagSet.Parse(args); err != nil {
		os.Exit(1)
	}
	if configurationPath == "" || outputPath == "" {
		fmt.Fprintln(os.Stderr, "Error: --configuration-file and --output are required")
		os.Exit(1)
	}

	desc, err := resolveDescriptor(metadataPath, blobPath, mediaType)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	cfg, err := readConfiguration(configurationPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	targetRepository := cfg.Repository
	if blobRepository != "" {
		targetRepository = blobRepository
	}

	result := BlobResult{
		Registry:   cfg.Registry,
		Repository: targetRepository,
		Digest:     desc.Digest,
		MediaType:  desc.MediaType,
		Size:       desc.Size,
	}
	resultBytes, err := json.Marshal(result)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshalling result: %v\n", err)
		os.Exit(1)
	}

	pushErr := pushBlob(ctx, cfg.Registry, targetRepository, desc, blobPath, compactStreamPath, casDir, []string(sources))
	finish(mode, outputPath, resultBytes, pushErr)
}

func pushBlob(ctx context.Context, registryStr, targetRepository string, desc api.Descriptor, blobPath, compactStreamPath, casDir string, sources []string) error {
	// Route the upload (and any shallow-source fetch) through the configured
	// registry gateway when one is set, with the enforced auth/retry defaults.
	pushOpts, err := registryopts.Push()
	if err != nil {
		return fmt.Errorf("configuring push options: %w", err)
	}
	pullTransport, err := registryopts.Transport(gateway.ModePull)
	if err != nil {
		return fmt.Errorf("configuring pull transport: %w", err)
	}

	repoRef, err := name.NewRepository(registryStr + "/" + targetRepository)
	if err != nil {
		return fmt.Errorf("parsing target repository %s/%s: %w", registryStr, targetRepository, err)
	}

	layer, err := layerForBlob(ctx, desc, blobPath, compactStreamPath, casDir, sources, pullTransport)
	if err != nil {
		return err
	}

	pusher, err := remote.NewPusher(pushOpts.Remote()...)
	if err != nil {
		return fmt.Errorf("creating pusher: %w", err)
	}
	if err := pusher.Upload(ctx, repoRef, layer); err != nil {
		return fmt.Errorf("uploading blob %s to %s: %w", desc.Digest, repoRef, err)
	}
	return nil
}

// layerForBlob builds a registryv1.Layer for the blob from whichever source is
// available: a materialized blob file, a compact stream to reconstruct, or an
// upstream registry source to stream from (shallow base layer). pullTransport
// routes any upstream fetch through the configured pull gateway (if any).
func layerForBlob(ctx context.Context, desc api.Descriptor, blobPath, compactStreamPath, casDir string, sources []string, pullTransport http.RoundTripper) (registryv1.Layer, error) {
	switch {
	case blobPath != "":
		return deployvfs.NewLayer(desc, func() (io.ReadCloser, error) {
			return os.Open(blobPath)
		}), nil
	case compactStreamPath != "":
		if casDir == "" {
			return nil, fmt.Errorf("--cas-dir is required with --compact-stream")
		}
		return deployvfs.NewLayer(desc, func() (io.ReadCloser, error) {
			csFile, err := os.Open(compactStreamPath)
			if err != nil {
				return nil, fmt.Errorf("opening compact stream %s: %w", compactStreamPath, err)
			}
			store := &dirStore{shaDir: filepath.Join(casDir, "sha256")}
			pr, pw := io.Pipe()
			go func() {
				err := compactstream.Reconstruct(ctx, csFile, store, pw)
				csFile.Close()
				pw.CloseWithError(err)
			}()
			return pr, nil
		}), nil
	case len(sources) > 0:
		// Shallow base layer: stream from its upstream source. Wrap as a
		// MountableLayer so that, if the target is in the same registry, the
		// upload becomes a cross-repo mount instead of a byte transfer.
		src := sources[0]
		ref, err := name.NewDigest(src + "@" + desc.Digest)
		if err != nil {
			return nil, fmt.Errorf("parsing source reference %s@%s: %w", src, desc.Digest, err)
		}
		l, err := remote.Layer(ref, registryopts.Default().WithTransport(pullTransport).Remote()...)
		if err != nil {
			return nil, fmt.Errorf("resolving source layer %s: %w", ref, err)
		}
		return &remote.MountableLayer{Layer: l, Reference: ref}, nil
	default:
		return nil, fmt.Errorf("no blob source provided for %s (need --blob, --compact-stream, or --source)", desc.Digest)
	}
}

// resolveDescriptor returns the blob's descriptor. When metadataPath is set it is
// read from that file (the layer path, where a standalone descriptor exists).
// Otherwise the descriptor is derived by hashing blobPath: this is used for the
// config blob, which has no standalone metadata file. The blob is content-addressed,
// so sha256(blobPath) is exactly the digest the manifest references; mediaType is
// recorded verbatim (informational — it is not used for the upload itself).
func resolveDescriptor(metadataPath, blobPath, mediaType string) (api.Descriptor, error) {
	if metadataPath != "" {
		return readDescriptor(metadataPath)
	}
	if blobPath == "" {
		return api.Descriptor{}, fmt.Errorf("either --metadata or --blob is required to determine the blob descriptor")
	}
	f, err := os.Open(blobPath)
	if err != nil {
		return api.Descriptor{}, fmt.Errorf("opening blob %s: %w", blobPath, err)
	}
	defer f.Close()
	hasher := sha256.New()
	size, err := io.Copy(hasher, f)
	if err != nil {
		return api.Descriptor{}, fmt.Errorf("hashing blob %s: %w", blobPath, err)
	}
	return api.Descriptor{
		MediaType: mediaType,
		Digest:    fmt.Sprintf("sha256:%x", hasher.Sum(nil)),
		Size:      size,
	}, nil
}

func readDescriptor(path string) (api.Descriptor, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return api.Descriptor{}, fmt.Errorf("reading metadata %s: %w", path, err)
	}
	var desc api.Descriptor
	if err := json.Unmarshal(raw, &desc); err != nil {
		return api.Descriptor{}, fmt.Errorf("parsing metadata %s: %w", path, err)
	}
	if desc.Digest == "" {
		return api.Descriptor{}, fmt.Errorf("metadata %s has empty digest", path)
	}
	return desc, nil
}

func readConfiguration(path string) (configurationFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return configurationFile{}, fmt.Errorf("reading configuration %s: %w", path, err)
	}
	var cfg configurationFile
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return configurationFile{}, fmt.Errorf("parsing configuration %s: %w", path, err)
	}
	if cfg.Registry == "" || cfg.Repository == "" {
		return configurationFile{}, fmt.Errorf("configuration %s must contain non-empty 'registry' and 'repository'", path)
	}
	return cfg, nil
}

// dirStore is a compactstream.BlobStore backed by a content-addressed directory,
// where each blob is stored at sha256/<hex of content>.
type dirStore struct {
	shaDir string
}

func (s *dirStore) ReaderForBlob(_ context.Context, digest []byte, size int64) (io.ReadCloser, error) {
	path := filepath.Join(s.shaDir, hex.EncodeToString(digest))
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("blob sha256:%s (size %d) not found in content-addressed directory: %w", hex.EncodeToString(digest), size, err)
	}
	return f, nil
}
