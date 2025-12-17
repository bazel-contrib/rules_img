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
	"strings"

	"golang.org/x/sync/errgroup"

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

	flagSet := flag.NewFlagSet("deploy", flag.ContinueOnError)
	flagSet.StringVar(&requestFile, "request-file", "", "Deploy manifest JSON request file")
	flagSet.StringVar(&runfilesRootSymlinksPrefix, "runfiles-root-symlinks-prefix", "", "Prefix for runfiles root symlinks")
	flagSet.Var(&additionalTags, "tag", "Additional tag to apply (can be used multiple times)")
	flagSet.Var(&additionalTags, "t", "Additional tag to apply (can be used multiple times)")
	flagSet.StringVar(&overrideRegistry, "registry", "", "Override registry to push to")
	flagSet.StringVar(&overrideRepository, "repository", "", "Override repository to push to")
	flagSet.StringVar(&platforms, "platform", "", "Comma-separated list of platforms to load (e.g., linux/amd64). If not set, loads the platform closest to the host (or the single available platform). Use 'all' to load the full multi-platform index. Doesn't affect push, only load.")

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

	if err := DeployWithExtras(ctx, rawRequest, []string(additionalTags), overrideRegistry, overrideRepository, platformList, runfilesRootSymlinksPrefix); err != nil {
		fmt.Fprintf(os.Stderr, "Error during deploy: %v\n", err)
		os.Exit(1)
	}
}

func DeployWithExtras(ctx context.Context, rawRequest []byte, additionalTags []string, overrideRegistry, overrideRepository string, platformList []string, runfilesRootSymlinksPrefix string) error {
	var req api.DeployManifest
	decoder := json.NewDecoder(bytes.NewReader(rawRequest))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return fmt.Errorf("unmarshalling deploy manifest file: %w", err)
	}

	reapiEndpoint := os.Getenv("IMG_REAPI_ENDPOINT")
	blobcacheEndpoint := os.Getenv("IMG_BLOB_CACHE_ENDPOINT")
	credentialHelperPath := credentialHelperPath()
	var credentialHelper credential.Helper
	if credentialHelperPath != "" {
		credentialHelper = credential.New(credentialHelperPath)
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
	if len(pushOperations) == 0 && len(loadOperations) == 0 {
		return fmt.Errorf("no push or load operations found in deploy manifest")
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
		casReader, err = cas.New(grpcClientConn)
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
	g, ctx := errgroup.WithContext(ctx)

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
		uploadBuilder.WithRemoteOptions(registry.WithAuthFromMultiKeychain())
		uploader := uploadBuilder.Build()

		g.Go(func() error {
			tags, err := uploader.PushAll(ctx, pushOperations, req.Settings.PushStrategy)
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
			_, err := builder.Build().LoadAll(ctx, loadOperations)
			return err
		})
	}

	if err := g.Wait(); err != nil {
		return fmt.Errorf("deploying images: %w", err)
	}

	// Print all pushed tags to stdout, one per line.
	for _, tag := range pushedTags {
		fmt.Println(tag)
	}
	// Note: loadedTags are already printed by the loader itself

	return nil
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
