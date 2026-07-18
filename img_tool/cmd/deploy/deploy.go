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
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/google/go-containerregistry/pkg/name"
	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/credential"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/protohelper"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/registry"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/deployvfs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/gateway"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/load"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/persistentworker"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/proto/blobcache"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/push"
)

func DeployProcess(ctx context.Context, args []string) {
	// Check for persistent worker mode before parsing other flags
	processedArgs, isPersistentWorker, err := persistentworker.ParseArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing args: %v\n", err)
		os.Exit(1)
	}
	if isPersistentWorker {
		jobs := extractJobsFlag(processedArgs)
		if err := persistentWorker(jobs); err != nil {
			fmt.Fprintf(os.Stderr, "Error in persistent worker: %v\n", err)
			os.Exit(1)
		}
		return
	}
	args = processedArgs

	var requestFiles stringSliceFlag
	var runfilesRootSymlinksPrefix string
	var additionalTags stringSliceFlag
	var overrideRegistry string
	var overrideRepository string
	var platforms string
	var ociLayouts stringSliceFlag
	var explicitLayers stringSliceFlag
	var jobs int

	flagSet := flag.NewFlagSet("deploy", flag.ContinueOnError)
	flagSet.Var(&requestFiles, "request-file", "Deploy manifest JSON request file (can be used multiple times)")
	flagSet.StringVar(&runfilesRootSymlinksPrefix, "runfiles-root-symlinks-prefix", "", "Prefix for runfiles root symlinks")
	flagSet.Var(&additionalTags, "tag", "Additional tag to apply (can be used multiple times)")
	flagSet.Var(&additionalTags, "t", "Additional tag to apply (can be used multiple times)")
	flagSet.StringVar(&overrideRegistry, "registry", "", "Override registry for push and split-mode load operations (load ops with a registry/repository set; the rules_oci tag-only fallback is left unchanged)")
	flagSet.StringVar(&overrideRepository, "repository", "", "Override repository for push and split-mode load operations (load ops with a registry/repository set; the rules_oci tag-only fallback is left unchanged)")
	flagSet.StringVar(&platforms, "platform", "", "Comma-separated list of platforms to load (e.g., linux/amd64). If not set, loads the platform closest to the host (or the single available platform). Use 'all' to load the full multi-platform index. Doesn't affect push, only load.")
	flagSet.Var(&ociLayouts, "oci-layout", "Path to an OCI layout directory, sparse or standard (can be used multiple times)")
	flagSet.Var(&explicitLayers, "layer", "Explicit layer in digest=path format (can be used multiple times)")
	flagSet.IntVar(&jobs, "jobs", 16, "Maximum number of parallel push operations")

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}

	if flagSet.NArg() != 0 {
		flagSet.Usage()
		os.Exit(1)
	}

	if len(requestFiles) == 0 {
		fmt.Fprintln(os.Stderr, "Error: at least one --request-file is required")
		flagSet.Usage()
		os.Exit(1)
	}

	// Parse explicit layers into a map
	layerMap := make(map[string]string)
	for _, layerFlag := range explicitLayers {
		digest, filePath, ok := strings.Cut(layerFlag, "=")
		if !ok {
			fmt.Fprintf(os.Stderr, "Error: --layer must be in format digest=path, got %q\n", layerFlag)
			os.Exit(1)
		}
		layerMap[digest] = filePath
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

	// Read and merge all request files
	rawRequest, err := mergeRequestFiles(requestFiles)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	opts := DeployOptions{
		AdditionalTags:             []string(additionalTags),
		OverrideRegistry:           overrideRegistry,
		OverrideRepository:         overrideRepository,
		PlatformList:               platformList,
		RunfilesRootSymlinksPrefix: runfilesRootSymlinksPrefix,
		OCILayouts:                 []string(ociLayouts),
		ExplicitLayers:             layerMap,
		Jobs:                       jobs,
	}

	if err := DeployWithExtras(ctx, rawRequest, opts); err != nil {
		fmt.Fprintf(os.Stderr, "Error during deploy: %v\n", err)
		os.Exit(1)
	}
}

// mergeRequestFiles reads multiple deploy manifest files and merges them into a single manifest.
// Operations are concatenated; settings from the last file win.
func mergeRequestFiles(paths []string) ([]byte, error) {
	if len(paths) == 1 {
		raw, err := os.ReadFile(paths[0])
		if err != nil {
			return nil, fmt.Errorf("reading request file %s: %w", paths[0], err)
		}
		return raw, nil
	}

	var merged api.DeployManifest
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("reading request file %s: %w", p, err)
		}
		var dm api.DeployManifest
		if err := json.Unmarshal(raw, &dm); err != nil {
			return nil, fmt.Errorf("parsing request file %s: %w", p, err)
		}
		merged.Operations = append(merged.Operations, dm.Operations...)
		merged.Settings = dm.Settings
	}

	out, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("marshalling merged manifest: %w", err)
	}
	return out, nil
}

// DeployOptions contains all configuration for a deploy operation.
type DeployOptions struct {
	AdditionalTags             []string
	OverrideRegistry           string
	OverrideRepository         string
	PlatformList               []string
	RunfilesRootSymlinksPrefix string
	OCILayouts                 []string
	ExplicitLayers             map[string]string
	Jobs                       int
}

func DeployWithExtras(ctx context.Context, rawRequest []byte, opts DeployOptions) error {
	var req api.DeployManifest
	decoder := json.NewDecoder(bytes.NewReader(rawRequest))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return fmt.Errorf("unmarshalling deploy manifest file: %w", err)
	}

	// Configure optional registry gateways. When IMG_REGISTRY_*_GATEWAY is set,
	// push requests and base-image (pull) reads are routed through the gateway.
	// When unset, WrapTransport returns the base transport unchanged.
	pushTransport, err := gateway.WrapTransport(remote.DefaultTransport, gateway.ModePush)
	if err != nil {
		return fmt.Errorf("configuring push gateway: %w", err)
	}
	pullTransport, err := gateway.WrapTransport(remote.DefaultTransport, gateway.ModePull)
	if err != nil {
		return fmt.Errorf("configuring pull gateway: %w", err)
	}

	vfsBuilder := deployvfs.NewBuilder(req).
		WithContainerRegistryOption(registry.WithAuthFromMultiKeychain()).
		WithContainerRegistryOption(remote.WithTransport(pullTransport)).
		WithContext(ctx)
	hasLazyStrategy := false
	baseOps, err := req.BaseOperations()
	if err != nil {
		return fmt.Errorf("checking operations for lazy strategy: %w", err)
	}
	for _, op := range baseOps {
		var strategy string
		switch op.Command {
		case "push":
			strategy = req.Settings.PushStrategy
		case "load":
			strategy = req.Settings.LoadStrategy
		}
		if strategy == "lazy" {
			hasLazyStrategy = true
			break
		}
	}
	vfsBuilder, err = configureBuilderFromEnv(vfsBuilder, hasLazyStrategy, opts.Jobs)
	if err != nil {
		return err
	}
	if opts.RunfilesRootSymlinksPrefix != "" {
		vfsBuilder = vfsBuilder.WithRunfilesRootSymlinksPrefix(opts.RunfilesRootSymlinksPrefix)
	}
	for _, layoutPath := range opts.OCILayouts {
		vfsBuilder = vfsBuilder.WithOCILayout(layoutPath)
	}
	for digest, filePath := range opts.ExplicitLayers {
		vfsBuilder = vfsBuilder.WithExplicitLayer(digest, filePath)
	}
	vfs, err := vfsBuilder.Build()
	if err != nil {
		return fmt.Errorf("building VFS: %w", err)
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

	// check if any operation requires a blob cache endpoint
	var blobcacheClient blobcache.BlobsClient
	haveBlobCacheCient := false
	if len(pushOperations) > 0 && req.Settings.PushStrategy == "cas_registry" {
		blobcacheEndpoint := os.Getenv("IMG_BLOB_CACHE_ENDPOINT")
		if blobcacheEndpoint == "" {
			return fmt.Errorf("IMG_BLOB_CACHE_ENDPOINT environment variable must be set for cas_registry push strategy")
		}
		credHelper := credentialHelperInstance()
		grpcClientConn, err := protohelper.Client(blobcacheEndpoint, credHelper)
		if err != nil {
			return fmt.Errorf("Failed to create gRPC client connection: %w", err)
		}
		blobcacheClient = blobcache.NewBlobsClient(grpcClientConn)
		haveBlobCacheCient = true
	}

	// Create a pusher for registry_tag operations only.
	// Push operations use an internally-created pusher in PushAll (with progress tracking).
	var pusher *remote.Pusher
	if len(registryTagOperations) > 0 && req.Settings.PushStrategy != "bes" {
		pusher, err = remote.NewPusher(registry.WithAuthFromMultiKeychain(), remote.WithTransport(pushTransport), remote.WithJobs(opts.Jobs))
		if err != nil {
			return fmt.Errorf("creating pusher: %w", err)
		}
	}

	var pushedTags []string
	// groupCtx is cancelled once g.Wait returns; keep the outer ctx for work after it (registry_tag ops).
	g, groupCtx := errgroup.WithContext(ctx)

	if len(pushOperations) > 0 {
		uploadBuilder := push.NewBuilder(vfs).
			WithJobs(opts.Jobs).
			WithRemoteOptions(registry.WithAuthFromMultiKeychain(), remote.WithTransport(pushTransport))
		if haveBlobCacheCient {
			uploadBuilder = uploadBuilder.WithBlobcacheClient(blobcacheClient)
		}
		if opts.OverrideRegistry != "" {
			uploadBuilder = uploadBuilder.WithOverrideRegistry(opts.OverrideRegistry)
		}
		if opts.OverrideRepository != "" {
			uploadBuilder = uploadBuilder.WithOverrideRepository(opts.OverrideRepository)
		}
		if len(opts.AdditionalTags) > 0 {
			uploadBuilder = uploadBuilder.WithExtraTags(opts.AdditionalTags)
		}
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
			if len(opts.PlatformList) > 0 {
				builder = builder.WithPlatforms(opts.PlatformList)
			}
			if len(opts.AdditionalTags) > 0 {
				builder = builder.WithExtraTags(opts.AdditionalTags)
			}
			// Overrides apply only to split-mode load ops (non-empty
			// registry/repository); the loader leaves the rules_oci fallback alone.
			if opts.OverrideRegistry != "" {
				builder = builder.WithOverrideRegistry(opts.OverrideRegistry)
			}
			if opts.OverrideRepository != "" {
				builder = builder.WithOverrideRepository(opts.OverrideRepository)
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
	fmt.Fprintf(os.Stderr, "    blob transfers: %d from disk, %d from disk cache, %d from container registry, %d from remote cache, %d from compact stream\n", stats.BlobsFromLocalDisk.Load(), stats.BlobsFromDiskCache.Load(), stats.BlobsFromRegistry.Load(), stats.BlobsFromRemoteCache.Load(), stats.BlobsFromCompactStream.Load())

	// Print all pushed tags to stdout, one per line.
	for _, tag := range pushedTags {
		fmt.Println(tag)
	}
	// Note: loadedTags are already printed by the loader itself

	if len(registryTagOperations) > 0 {
		extraTagNames, err := applyRegistryTagOperations(ctx, vfs, pusher, registryTagOperations, req.Settings.PushStrategy, opts.OverrideRegistry, opts.OverrideRepository, opts.Jobs)
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
func applyRegistryTagOperations(ctx context.Context, vfs *deployvfs.VFS, pusher *remote.Pusher, ops []api.IndexedRegistryTagDeployOperation, strategy, overrideRegistry, overrideRepository string, jobs int) ([]string, error) {
	if strategy == "bes" {
		return nil, nil
	}

	type pushItem struct {
		ref      name.Reference
		taggable remote.Taggable
	}
	var items []pushItem
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
			items = append(items, pushItem{ref: ref, taggable: taggable})
			tagNames = append(tagNames, ref.String())
		}
	}
	if len(items) == 0 {
		return nil, nil
	}

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(jobs)

	for _, item := range items {
		item := item
		g.Go(func() error {
			return pusher.Push(ctx, item.ref, item.taggable)
		})
	}

	if err := g.Wait(); err != nil {
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
	// Registry auth uses IMG_CREDENTIAL_HELPER_OCI_REGISTRY; this path is for
	// the remote cache / REAPI gRPC connection, so it honors the remote-cache
	// scoped helper before the generic one.
	if credentialHelper := registry.RemoteCacheCredentialHelper(); credentialHelper != "" {
		return credentialHelper
	}
	// If no credential helper is configured, look for one in the workspace.
	// This is useful for local development.
	workingDirectory := os.Getenv("BUILD_WORKSPACE_DIRECTORY")
	defaultPathHelper, err := exec.LookPath(filepath.FromSlash(path.Join(workingDirectory, "tools", "credential-helper")))
	if err == nil && defaultPathHelper != "" {
		return defaultPathHelper
	}
	return ""
}

func credentialHelperInstance() credential.Helper {
	credPath := credentialHelperPath()
	if credPath != "" {
		return credential.New(credPath, nil)
	}
	return credential.NopHelper()
}

func configureBuilderFromEnv(builder *deployvfs.Builder, needsCAS bool, jobs int) (*deployvfs.Builder, error) {
	diskCachePath := os.Getenv("IMG_DISK_CACHE")
	if diskCachePath != "" {
		builder = builder.WithDiskCache(diskCachePath)
	}

	if needsCAS {
		reapiEndpoint := os.Getenv("IMG_REAPI_ENDPOINT")
		if reapiEndpoint != "" {
			reapiInstanceName := os.Getenv("IMG_REAPI_INSTANCE_NAME")
			credHelper := credentialHelperInstance()
			// A single gRPC connection multiplexes all CAS reads onto one TCP
			// connection, which bottlenecks bulk downloads on high-latency
			// links. Optionally open a pool of connections and round-robin
			// reads across them (cf. Bazel's --remote_max_connections).
			numConns := reapiMaxConnections(jobs)
			members := make([]*cas.CAS, 0, numConns)
			for range numConns {
				grpcConn, err := protohelper.Client(reapiEndpoint, credHelper)
				if err != nil {
					return nil, fmt.Errorf("creating gRPC client for REAPI: %w", err)
				}
				member, err := cas.New(grpcConn, cas.WithInstanceName(reapiInstanceName))
				if err != nil {
					return nil, fmt.Errorf("creating CAS client: %w", err)
				}
				members = append(members, member)
			}
			builder = builder.WithCASReader(cas.NewPool(members))
		}
	}

	return builder, nil
}

// reapiMaxConnections returns the size of the gRPC connection pool used to read
// blobs from the remote CAS. It defaults to jobs (the number of parallel push
// operations, i.e. the maximum number of concurrent reads in flight), which can
// be overridden with IMG_REAPI_MAX_CONNECTIONS. Values below 1 or unparseable
// values fall back to the default with a warning.
func reapiMaxConnections(jobs int) int {
	if jobs < 1 {
		jobs = 1
	}
	raw := os.Getenv("IMG_REAPI_MAX_CONNECTIONS")
	if raw == "" {
		return jobs
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		fmt.Fprintf(os.Stderr, "WARNING: ignoring invalid IMG_REAPI_MAX_CONNECTIONS=%q, using %d\n", raw, jobs)
		return jobs
	}
	return n
}

func extractJobsFlag(args []string) int {
	for i := 0; i < len(args); i++ {
		key, value, hasValue := strings.Cut(args[i], "=")
		if key == "--jobs" {
			if !hasValue && i+1 < len(args) {
				value = args[i+1]
			}
			var j int
			if _, err := fmt.Sscanf(value, "%d", &j); err == nil && j > 0 {
				return j
			}
		}
	}
	return 16
}
