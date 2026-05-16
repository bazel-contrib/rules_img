package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/malt3/go-containerregistry/pkg/name"
	registryv1 "github.com/malt3/go-containerregistry/pkg/v1"
	"github.com/malt3/go-containerregistry/pkg/v1/remote"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/credential"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/protohelper"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/registry"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/deployvfs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/load"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/proto/blobcache"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/push"
)

func DeployProcess(ctx context.Context, args []string) {
	var requestFile string
	var runfilesRootSymlinksPrefix string
	var additionalTags stringSliceFlag
	var overrideRegistry string
	var overrideRepository string
	var platforms string
	var pushJobs int

	flagSet := flag.NewFlagSet("deploy", flag.ContinueOnError)
	flagSet.StringVar(&requestFile, "request-file", "", "Deploy manifest JSON request file")
	flagSet.StringVar(&runfilesRootSymlinksPrefix, "runfiles-root-symlinks-prefix", "", "Prefix for runfiles root symlinks")
	flagSet.Var(&additionalTags, "tag", "Additional tag to apply (can be used multiple times)")
	flagSet.Var(&additionalTags, "t", "Additional tag to apply (can be used multiple times)")
	flagSet.StringVar(&overrideRegistry, "registry", "", "Override registry to push to")
	flagSet.StringVar(&overrideRepository, "repository", "", "Override repository to push to")
	flagSet.StringVar(&platforms, "platform", "", "Comma-separated list of platforms to load (e.g., linux/amd64). If not set, loads the platform closest to the host (or the single available platform). Use 'all' to load the full multi-platform index. Doesn't affect push, only load.")
	flagSet.IntVar(&pushJobs, "jobs", 0, "Number of parallel push threads (overrides push_jobs setting; 0 means use setting or default)")

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}

	if flagSet.NArg() != 0 {
		flagSet.Usage()
		os.Exit(1)
	}

	if requestFile == "" {
		fmt.Fprintln(os.Stderr, "Error: --request-file is required")
		flagSet.Usage()
		os.Exit(1)
	}

	rawRequest, err := os.ReadFile(requestFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading request file %s: %v\n", requestFile, err)
		os.Exit(1)
	}

	// Parse platforms
	var platformList []string
	if platforms != "" {
		platformList = strings.Split(platforms, ",")
		// Trim whitespace from each platform
		for i, p := range platformList {
			platformList[i] = strings.TrimSpace(p)
		}
	}

	if err := DeployWithExtras(ctx, rawRequest, []string(additionalTags), overrideRegistry, overrideRepository, platformList, runfilesRootSymlinksPrefix, pushJobs); err != nil {
		fmt.Fprintf(os.Stderr, "Error during deploy: %v\n", err)
		os.Exit(1)
	}
}

func DeployWithExtras(ctx context.Context, rawRequest []byte, additionalTags []string, overrideRegistry, overrideRepository string, platformList []string, runfilesRootSymlinksPrefix string, pushJobsOverride int) error {
	var req api.DeployManifest
	decoder := json.NewDecoder(bytes.NewReader(rawRequest))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return fmt.Errorf("unmarshalling deploy manifest file: %w", err)
	}

	reapiEndpoint := os.Getenv("IMG_REAPI_ENDPOINT")
	reapiInstanceName := os.Getenv("IMG_REAPI_INSTANCE_NAME")
	blobcacheEndpoint := os.Getenv("IMG_BLOB_CACHE_ENDPOINT")
	credentialHelperPath := credentialHelperPath()
	var credentialHelper credential.Helper
	if credentialHelperPath != "" {
		credentialHelper = credential.New(credentialHelperPath, nil)
	} else {
		credentialHelper = credential.NopHelper()
	}

	pushOperations, err := req.PushOperations()
	if err != nil {
		return err
	}
	loadOperations, err := req.LoadOperations()
	if err != nil {
		return err
	}
	registryTagOperations, err := req.RegistryTagOperations()
	if err != nil {
		return err
	}
	if len(pushOperations) == 0 && len(loadOperations) == 0 && len(registryTagOperations) == 0 {
		return fmt.Errorf("no push, load, or registry_tag operations found in deploy manifest")
	}

	// check if any operation requires a reapi endpoint
	var casReader *cas.CAS
	if (len(pushOperations) > 0 && req.Settings.PushStrategy == "lazy") || (len(loadOperations) > 0 && req.Settings.LoadStrategy == "lazy") {
		if reapiEndpoint == "" {
			return fmt.Errorf("IMG_REAPI_ENDPOINT environment variable must be set for lazy push/load strategy")
		}
		grpcClientConn, err := protohelper.Client(reapiEndpoint, credentialHelper)
		if err != nil {
			return fmt.Errorf("Failed to create gRPC client connection: %w", err)
		}
		casReader, err = cas.New(grpcClientConn, cas.WithInstanceName(reapiInstanceName))
		if err != nil {
			return fmt.Errorf("creating CAS client: %w", err)
		}
	}
	// check if any operation requires a blob cache endpoint
	var blobcacheClient blobcache.BlobsClient
	haveBlobCacheCient := false
	if len(pushOperations) > 0 && req.Settings.PushStrategy == "cas_registry" {
		if blobcacheEndpoint == "" {
			return fmt.Errorf("IMG_BLOB_CACHE_ENDPOINT environment variable must be set for cas_registry push strategy")
		}
		grpcClientConn, err := protohelper.Client(blobcacheEndpoint, credentialHelper)
		if err != nil {
			return fmt.Errorf("Failed to create gRPC client connection: %w", err)
		}
		blobcacheClient = blobcache.NewBlobsClient(grpcClientConn)
		haveBlobCacheCient = true
	}

	vfsBuilder := deployvfs.Builder(req).WithContainerRegistryOption(registry.WithAuthFromMultiKeychain())
	if runfilesRootSymlinksPrefix != "" {
		vfsBuilder = vfsBuilder.WithRunfilesRootSymlinksPrefix(runfilesRootSymlinksPrefix)
	}
	if casReader != nil {
		vfsBuilder = vfsBuilder.WithCASReader(casReader)
	}
	vfs, err := vfsBuilder.Build()
	if err != nil {
		return fmt.Errorf("building VFS: %w", err)
	}

	var pushedTags []string
	// groupCtx is cancelled once g.Wait returns; keep the outer ctx for work after it (registry_tag ops).
	g, groupCtx := errgroup.WithContext(ctx)

	if len(pushOperations) > 0 {
		uploadBuilder := push.NewBuilder(vfs)
		if haveBlobCacheCient {
			uploadBuilder = uploadBuilder.WithBlobcacheClient(blobcacheClient)
		}
		if overrideRegistry != "" {
			uploadBuilder = uploadBuilder.WithOverrideRegistry(overrideRegistry)
		}
		if overrideRepository != "" {
			uploadBuilder = uploadBuilder.WithOverrideRepository(overrideRepository)
		}
		if len(additionalTags) > 0 {
			uploadBuilder = uploadBuilder.WithExtraTags(additionalTags)
		}
		// CLI flag takes precedence over settings value; 0 means "use default"
		pushJobs := pushJobsOverride
		if pushJobs == 0 {
			pushJobs = req.Settings.PushJobs
		}
		if pushJobs > 0 {
			uploadBuilder = uploadBuilder.WithJobs(pushJobs)
		}
		uploadBuilder.WithRemoteOptions(registry.WithAuthFromMultiKeychain())
		uploader := uploadBuilder.Build()

		g.Go(func() error {
			tags, err := uploader.PushAll(groupCtx, pushOperations, req.Settings.PushStrategy)
			if err != nil {
				return err
			}
			pushedTags = tags
			return nil
		})
	}
	if len(loadOperations) > 0 {
		g.Go(func() error {
			builder := load.NewBuilder(vfs)
			if len(platformList) > 0 {
				builder = builder.WithPlatforms(platformList)
			}
			if len(additionalTags) > 0 {
				builder = builder.WithExtraTags(additionalTags)
			}
			// LoadAll prints the loaded tags itself, so we discard the return value
			_, err := builder.Build().LoadAll(groupCtx, loadOperations)
			return err
		})
	}

	if err := g.Wait(); err != nil {
		return fmt.Errorf("deploying images: %w", err)
	}

	// Print VFS statistics to stderr
	stats := vfs.Stats()
	fmt.Fprintf(os.Stderr, "    layer transfers: %d from disk, %d from container registry, %d from remote cache\n", stats.LayersFromLocalDisk.Load(), stats.LayersFromRegistry.Load(), stats.LayersFromRemoteCache.Load())

	// Print all pushed tags to stdout, one per line.
	for _, tag := range pushedTags {
		fmt.Println(tag)
	}
	// Note: loadedTags are already printed by the loader itself

	if len(registryTagOperations) > 0 {
		extraTagNames, err := applyRegistryTagOperations(ctx, vfs, registryTagOperations, req.Settings.PushStrategy, overrideRegistry, overrideRepository)
		if err != nil {
			return err
		}
		for _, t := range extraTagNames {
			fmt.Println(t)
		}
	}

	return nil
}

// applyRegistryTagOperations writes the pre-expanded tags from registry_tag
// ops onto manifests already pushed by a preceding push op. Under the `bes`
// strategy the BES syncer is responsible for this, so we no-op.
func applyRegistryTagOperations(ctx context.Context, vfs *deployvfs.VFS, ops []api.IndexedRegistryTagDeployOperation, strategy, overrideRegistry, overrideRepository string) ([]string, error) {
	if strategy == "bes" {
		return nil, nil
	}

	todo := make(map[name.Reference]remote.Taggable)
	var tagNames []string
	for _, op := range ops {
		opRegistry := op.Registry
		if overrideRegistry != "" {
			opRegistry = overrideRegistry
		}
		opRepository := op.Repository
		if overrideRepository != "" {
			opRepository = overrideRepository
		}
		baseRef := opRegistry + "/" + opRepository

		rootHash, err := registryv1.NewHash(op.Root.Digest)
		if err != nil {
			return nil, fmt.Errorf("parsing root digest for registry_tag on %s: %w", baseRef, err)
		}
		taggable, err := vfs.Taggable(rootHash)
		if err != nil {
			return nil, fmt.Errorf("locating manifest %s for registry_tag on %s: %w", op.Root.Digest, baseRef, err)
		}
		for _, tag := range op.Tags {
			ref, err := name.NewTag(baseRef + ":" + tag)
			if err != nil {
				return nil, fmt.Errorf("creating registry_tag ref %q: %w", tag, err)
			}
			todo[ref] = taggable
			tagNames = append(tagNames, ref.String())
		}
	}
	if len(todo) == 0 {
		return nil, nil
	}

	opts := []remote.Option{remote.WithContext(ctx), registry.WithAuthFromMultiKeychain()}
	if err := remote.MultiWrite(todo, opts...); err != nil {
		return nil, fmt.Errorf("applying registry_tag operations: %w", err)
	}
	sort.Strings(tagNames)
	return tagNames, nil
}

// stringSliceFlag implements flag.Value for collecting multiple string values
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

func credentialHelperPath() string {
	credentialHelper := os.Getenv("IMG_CREDENTIAL_HELPER")
	if credentialHelper != "" {
		return credentialHelper
	}
	workingDirectory := os.Getenv("BUILD_WORKSPACE_DIRECTORY")
	defaultPathHelper, defaultPathHelperErr := exec.LookPath(filepath.FromSlash(path.Join(workingDirectory, "tools", "credential-helper")))
	tweagCredentialHelper, tweagErr := exec.LookPath("tweag-credential-helper")

	if defaultPathHelper != "" && defaultPathHelperErr == nil {
		// If IMG_CREDENTIAL_HELPER is not set, we look for a credential helper in the workspace.
		// This is useful for local development.
		return defaultPathHelper
	} else if tweagCredentialHelper != "" && tweagErr == nil {
		// If there is no credential helper in %workspace%/tools/credential_helper,
		// we look for the tweag-credential-helper in the PATH.
		return tweagCredentialHelper
	}
	return ""
}
