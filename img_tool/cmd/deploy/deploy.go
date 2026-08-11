package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
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
	"github.com/bazel-contrib/rules_img/img_tool/pkg/progress"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/proto/blobcache"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/push"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/registryopts"
)

func DeployProcess(ctx context.Context, args []string) {
	// Check for persistent worker mode before parsing other flags
	processedArgs, isPersistentWorker, err := persistentworker.ParseArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing args: %v\n", err)
		os.Exit(1)
	}
	sinkSpec := extractSinkFlag(processedArgs)
	// A global oci-tar/docker-save sink cannot run under the persistent worker,
	// so specifying one on the command line forces one-shot mode. A global
	// distribution/oci sink is compatible with the worker and is applied to
	// every incoming request.
	if isPersistentWorker && sinkSpec != "" {
		kind, _, err := parseSink(sinkSpec)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		if !kind.globalOnly() {
			isPersistentWorker = false
		}
	}
	if isPersistentWorker {
		jobs := extractJobsFlag(processedArgs)
		if err := applyProgressMode(extractProgressFlag(processedArgs), true); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		if err := persistentWorker(jobs, sinkSpec); err != nil {
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
	var sink string
	var progressMode string
	var signSettingFiles stringSliceFlag
	var defaultSignSetting string
	var signForce bool
	var signTargetsFlag string

	flagSet := flag.NewFlagSet("deploy", flag.ContinueOnError)
	flagSet.Var(&requestFiles, "request-file", "Deploy manifest JSON request file (can be used multiple times)")
	flagSet.StringVar(&runfilesRootSymlinksPrefix, "runfiles-root-symlinks-prefix", "", "Prefix for runfiles root symlinks")
	flagSet.Var(&additionalTags, "tag", "Additional tag to apply (can be used multiple times)")
	flagSet.Var(&additionalTags, "t", "Additional tag to apply (can be used multiple times)")
	flagSet.StringVar(&overrideRegistry, "registry", "", "Override registry for push and split-mode load operations (load ops with a registry/repository set; the rules_oci tag-only fallback is left unchanged)")
	flagSet.StringVar(&overrideRepository, "repository", "", "Override repository for push and split-mode load operations (load ops with a registry/repository set; the rules_oci tag-only fallback is left unchanged)")
	flagSet.StringVar(&platforms, "platform", "", "Comma-separated list of platforms to load (e.g., linux/amd64). If not set, loads the platform closest to the host (or the single available platform). Use 'all' to load the full multi-platform index. Doesn't affect push, only load.")
	flagSet.Var(&ociLayouts, "oci-layout", "Path to an OCI layout directory, sparse or standard (can be used multiple times)")
	flagSet.Var(&explicitLayers, "layer", "Layer as digest=path or a bare path (can be used multiple times). The file may be a raw compressed layer blob or a compact stream (.cstream), auto-detected. For a bare path: a raw blob is hashed to derive its digest; a .cstream must embed its compressed digest.")
	flagSet.IntVar(&jobs, "jobs", defaultDeployJobs(), "Maximum number of concurrent requests to the destination registry, and of parallel push operations (defaults to GOMAXPROCS)")
	flagSet.StringVar(&sink, "sink", "", "Override the destination of all push/load/registry_tag operations for testing. Format: <type>:<path> where type is one of oci-tar, docker-save, oci, distribution, distribution-flat. No registry or daemon network I/O is performed.")
	flagSet.StringVar(&progressMode, "progress", "", "How to report progress on stderr: 'bar' (interactive progress bars), 'log' (one crane-style line per blob), 'none', or 'auto' (default: bars on a terminal, log lines otherwise). Overridable with $IMG_PROGRESS.")
	flagSet.Var(&signSettingFiles, "sign_setting_file", "Additional sign_setting config file to ingest for signing (can be used multiple times)")
	flagSet.StringVar(&defaultSignSetting, "default_sign_setting", "", "Default sign_setting for operations without one: a path to a config file, or sha256:<hex> referencing a discovered setting")
	flagSet.BoolVar(&signForce, "sign_force", false, "Sign every push operation using the default sign_setting, even operations not configured to sign at build time")
	flagSet.StringVar(&signTargetsFlag, "sign_targets", "", "Override which descriptors are signed: a comma-separated list of roots,child_manifests,referrers or 'all'")

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

	if err := applyProgressMode(progressMode, false); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		flagSet.Usage()
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
		Layers:                     []string(explicitLayers),
		Jobs:                       jobs,
		Sink:                       sink,
		SignSettingFiles:           []string(signSettingFiles),
		DefaultSignSetting:         defaultSignSetting,
		SignForce:                  signForce,
		SignTargets:                splitCommaList(signTargetsFlag),
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
	Layers                     []string // raw --layer specs: "digest=path" or bare "path" (raw blob or .cstream)
	Jobs                       int
	Sink                       string

	// Signing options.
	SignSettingFiles   []string // extra sign_setting config files to ingest
	DefaultSignSetting string   // path or "sha256:<hex>" default setting
	SignForce          bool     // sign all push ops using the default setting
	SignTargets        []string // override sign-target selection (roots/child_manifests/referrers/all)
}

func DeployWithExtras(ctx context.Context, rawRequest []byte, opts DeployOptions) error {
	// --jobs is the ceiling on requests in flight to the destination registry.
	registryopts.LimitConcurrencyToJobs(opts.Jobs)

	var req api.DeployManifest
	decoder := json.NewDecoder(bytes.NewReader(rawRequest))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return fmt.Errorf("unmarshalling deploy manifest file: %w", err)
	}

	// Configure optional registry gateways. When IMG_REGISTRY_*_GATEWAY is set,
	// push requests and base-image (pull) reads are routed through the gateway.
	// When unset, WrapTransport returns the base transport unchanged.
	pushTransport, err := registryopts.Transport(gateway.ModePush)
	if err != nil {
		return fmt.Errorf("configuring push transport: %w", err)
	}
	pullTransport, err := registryopts.Transport(gateway.ModePull)
	if err != nil {
		return fmt.Errorf("configuring pull transport: %w", err)
	}

	vfsBuilder := deployvfs.NewBuilder(req).
		WithContainerRegistryOptions(registryopts.Default().WithTransport(pullTransport).Remote()...).
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
	vfsBuilder, blobCache, err := configureBuilderFromEnv(vfsBuilder, hasLazyStrategy, opts.Jobs)
	if err != nil {
		return err
	}
	if blobCache != nil {
		defer blobCache.Close()
	}
	if opts.RunfilesRootSymlinksPrefix != "" {
		vfsBuilder = vfsBuilder.WithRunfilesRootSymlinksPrefix(opts.RunfilesRootSymlinksPrefix)
	}
	for _, layoutPath := range opts.OCILayouts {
		vfsBuilder = vfsBuilder.WithOCILayout(layoutPath)
	}
	for _, spec := range opts.Layers {
		vfsBuilder = vfsBuilder.WithLayer(spec)
	}
	// Blob-staging repository: layer blobs are pushed to req.Settings.BlobRepository
	// and cross-mounted from there when the manifests are pushed to their real
	// repositories. Register the cross-mount sources before building the VFS so
	// VFS.Layer wraps those blobs as remote.MountableLayer.
	if req.Settings.BlobRepository != "" {
		stagingOps, err := req.PushOperations()
		if err != nil {
			return err
		}
		for _, op := range stagingOps {
			src := api.CrossMountSource{Repository: req.Settings.BlobRepository}
			for _, manifest := range op.Manifests {
				for _, layer := range manifest.LayerBlobs {
					vfsBuilder = vfsBuilder.WithCrossMountSource(layer.Digest, src)
				}
			}
		}
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

	// When a --sink override is active, capture every operation into the local
	// sink instead of pushing to a registry or loading into a daemon. This
	// performs no registry/daemon network I/O for the destination (source blobs
	// are still resolved from the VFS as usual).
	if opts.Sink != "" {
		return deployToSink(ctx, opts.Sink, vfs, blobCache, pushOperations, loadOperations, registryTagOperations, req.Settings, opts)
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
		pusher, err = remote.NewPusher(registryopts.Default().WithTransport(pushTransport).WithJobs(opts.Jobs).Remote()...)
		if err != nil {
			return fmt.Errorf("creating pusher: %w", err)
		}
	}

	// Report how concurrent the registry traffic got (opt-in, see
	// registryopts.EnvLogConcurrency), including on the failure paths below.
	defer registryopts.LogConcurrencySummary(os.Stderr)

	var pushedTags []string
	// groupCtx is cancelled once g.Wait returns; keep the outer ctx for work after it (registry_tag ops).
	g, groupCtx := errgroup.WithContext(ctx)

	// When a blob-staging repository is configured, upload every layer blob to it
	// first (synchronously) so that the manifest push below can cross-mount them
	// from the staging repository instead of uploading their bytes to the real
	// repository. Skipped when layer pushes are forbidden: the blobs are then
	// expected to already be in the registry (e.g. pushed at build time).
	if req.Settings.BlobRepository != "" && !req.Settings.ForbidLayerPush && len(pushOperations) > 0 {
		if err := preUploadStagingBlobs(ctx, vfs, pushOperations, req.Settings.BlobRepository, opts.OverrideRegistry, opts.Jobs, pushTransport); err != nil {
			return fmt.Errorf("pre-uploading blobs to staging repository %q: %w", req.Settings.BlobRepository, err)
		}
	}

	if len(pushOperations) > 0 {
		uploadBuilder := push.NewBuilder(vfs).
			WithJobs(opts.Jobs).
			WithRemoteOptions(registryopts.Default().WithTransport(pushTransport).Remote()...)
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

	printBlobStats(vfs, blobCache)

	// Print all pushed tags to stdout, one per line.
	for _, tag := range pushedTags {
		fmt.Println(tag)
	}
	// Note: loadedTags are already printed by the loader itself

	// Sign pushed artifacts (referrers require the subjects to already exist in
	// the registry, so this runs after the push errgroup completes). Signing
	// creates its own pusher — the top-level pusher above is scoped to
	// registry_tag ops, and PushAll uses its own internal pusher.
	if len(pushOperations) > 0 {
		if err := applySignOperations(ctx, pushOperations, req.Settings, signOptions{
			settingFiles:       opts.SignSettingFiles,
			defaultSetting:     opts.DefaultSignSetting,
			force:              opts.SignForce,
			targetOverride:     opts.SignTargets,
			overrideRegistry:   opts.OverrideRegistry,
			overrideRepository: opts.OverrideRepository,
			pushTransport:      pushTransport,
			jobs:               opts.Jobs,
		}); err != nil {
			return err
		}
	}

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
			ref, err := name.NewTag(baseRef+":"+tag, registryopts.NameOptions()...)
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

// preUploadStagingBlobs uploads every layer blob of the given push operations to
// the staging repository (within each operation's registry, honoring an override).
// It is used when req.Settings.BlobRepository is set: after this returns, the
// manifest push cross-mounts the blobs from the staging repository. pushTransport
// routes the uploads through the configured push gateway (if any).
func preUploadStagingBlobs(ctx context.Context, vfs *deployvfs.VFS, ops []api.IndexedPushDeployOperation, blobRepository, overrideRegistry string, jobs int, pushTransport http.RoundTripper) error {
	if jobs < 1 {
		jobs = 1
	}
	pusher, err := remote.NewPusher(registryopts.Default().WithTransport(pushTransport).WithJobs(jobs).Remote()...)
	if err != nil {
		return fmt.Errorf("creating pusher: %w", err)
	}

	type uploadItem struct {
		repo   name.Repository
		digest registryv1.Hash
	}
	seen := make(map[string]bool)
	var items []uploadItem
	for _, op := range ops {
		reg := op.Registry
		if overrideRegistry != "" {
			reg = overrideRegistry
		}
		repo, err := name.NewRepository(reg+"/"+blobRepository, registryopts.NameOptions()...)
		if err != nil {
			return fmt.Errorf("parsing staging repository %s/%s: %w", reg, blobRepository, err)
		}
		for _, manifest := range op.Manifests {
			for _, layer := range manifest.LayerBlobs {
				key := reg + "/" + blobRepository + "@" + layer.Digest
				if seen[key] {
					continue
				}
				seen[key] = true
				h, err := registryv1.NewHash(layer.Digest)
				if err != nil {
					return fmt.Errorf("parsing layer digest %s: %w", layer.Digest, err)
				}
				items = append(items, uploadItem{repo: repo, digest: h})
			}
		}
	}

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(jobs)
	for _, it := range items {
		it := it
		g.Go(func() error {
			layer, err := vfs.RawLayer(it.digest)
			if err != nil {
				return fmt.Errorf("resolving blob %s: %w", it.digest, err)
			}
			if err := pusher.Upload(ctx, it.repo, layer); err != nil {
				return fmt.Errorf("uploading blob %s to %s: %w", it.digest, it.repo, err)
			}
			return nil
		})
	}
	return g.Wait()
}

// printBlobStats reports where the deployed blobs came from, and how the local
// CAS blob cache did, on stderr.
func printBlobStats(vfs *deployvfs.VFS, blobCache *cas.CachingReader) {
	stats := vfs.Stats()
	fmt.Fprintf(os.Stderr, "    blob transfers: %d from disk, %d from disk cache, %d from container registry, %d from remote cache, %d from compact stream\n", stats.BlobsFromLocalDisk.Load(), stats.BlobsFromDiskCache.Load(), stats.BlobsFromRegistry.Load(), stats.BlobsFromRemoteCache.Load(), stats.BlobsFromCompactStream.Load())
	if blobCache == nil {
		return
	}
	cacheStats := blobCache.Stats()
	if cacheStats.Hits == 0 && cacheStats.Fetches == 0 && cacheStats.Fallbacks == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "    remote cache blobs: %d from local cache (%s), %d fetched (%s), %d deduplicated, %d evicted\n",
		cacheStats.Hits, humanizeBytes(cacheStats.BytesFromCache),
		cacheStats.Fetches, humanizeBytes(cacheStats.BytesFetched),
		cacheStats.Deduped, cacheStats.Evicted)
	if cacheStats.DiskDisabled {
		fmt.Fprintf(os.Stderr, "    remote cache blobs: local caching was disabled (see the warning above)\n")
	}
}

// humanizeBytes renders a byte count with a binary unit.
func humanizeBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/unit)
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

// configureBuilderFromEnv points the VFS builder at the blob sources described
// by the environment: the Bazel disk cache and, when needed, the remote CAS.
//
// CAS reads go through a local blob cache (see casBlobCacheFromEnv), which is
// returned so the caller can report its statistics and close it. It is nil when
// no CAS is configured or the cache is disabled.
func configureBuilderFromEnv(builder *deployvfs.Builder, needsCAS bool, jobs int) (*deployvfs.Builder, *cas.CachingReader, error) {
	diskCachePath := os.Getenv("IMG_DISK_CACHE")
	if diskCachePath != "" {
		builder = builder.WithDiskCache(diskCachePath)
	}

	if !needsCAS {
		return builder, nil, nil
	}
	reapiEndpoint := os.Getenv("IMG_REAPI_ENDPOINT")
	if reapiEndpoint == "" {
		return builder, nil, nil
	}

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
			return nil, nil, fmt.Errorf("creating gRPC client for REAPI: %w", err)
		}
		member, err := cas.New(grpcConn, cas.WithInstanceName(reapiInstanceName))
		if err != nil {
			return nil, nil, fmt.Errorf("creating CAS client: %w", err)
		}
		members = append(members, member)
	}
	pool := cas.NewPool(members)

	blobCache, err := casBlobCacheFromEnv(pool)
	if err != nil {
		// Caching is an optimization: read straight from the remote cache instead.
		fmt.Fprintf(os.Stderr, "WARNING: not caching remote cache blobs locally: %v\n", err)
		return builder.WithCASReader(pool), nil, nil
	}
	if blobCache == nil {
		return builder.WithCASReader(pool), nil, nil
	}
	return builder.WithCASReader(blobCache), blobCache, nil
}

// Environment variables configuring the local cache of blobs read from the
// remote CAS.
const (
	// envCASCache turns the local blob cache off when set to 0/false/off/no.
	envCASCache = "IMG_CAS_CACHE"
	// envCASCacheDir overrides the cache directory. Defaults to IMG_DISK_CACHE
	// (sharing Bazel's disk cache) and, failing that, to a directory under the
	// user's cache directory.
	envCASCacheDir = "IMG_CAS_CACHE_DIR"
	// envCASCacheMaxSize limits the cached bytes, evicting the least recently
	// used blobs beyond that. Accepts a plain byte count or a KiB/MiB/GiB suffix.
	envCASCacheMaxSize = "IMG_CAS_CACHE_MAX_SIZE"
	// envCASCacheBufferSize is how much of a blob is written to disk before
	// readers can consume it.
	envCASCacheBufferSize = "IMG_CAS_CACHE_BUFFER_SIZE"
)

// casCacheConfig is how the local cache of remote-CAS blobs is configured.
type casCacheConfig struct {
	enabled    bool
	dir        string // empty: let the cache pick its default directory
	maxSize    int64  // 0: unlimited
	bufferSize int
}

// casCacheConfigFromEnv reads the local blob cache configuration from the
// environment.
func casCacheConfigFromEnv() casCacheConfig {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envCASCache))) {
	case "0", "false", "off", "no":
		return casCacheConfig{}
	}
	config := casCacheConfig{enabled: true, bufferSize: cas.DefaultCacheBufferSize}

	// A directory the user names -- their own, or Bazel's disk cache -- is theirs
	// to manage, so it stays unlimited unless they ask for a limit. The default
	// directory has nobody else looking after it and gets one.
	config.dir = os.Getenv(envCASCacheDir)
	if config.dir == "" {
		config.dir = os.Getenv("IMG_DISK_CACHE")
	}
	if config.dir == "" {
		config.maxSize = cas.DefaultCacheMaxSize
	}
	if size, ok := byteSizeFromEnv(envCASCacheMaxSize, config.maxSize); ok {
		config.maxSize = size
	}
	if size, ok := byteSizeFromEnv(envCASCacheBufferSize, int64(config.bufferSize)); ok {
		config.bufferSize = int(size)
	}
	return config
}

// casBlobCacheFromEnv wraps upstream in a local blob cache, which deduplicates
// concurrent reads of one blob and keeps what it fetched on disk for later runs.
// It returns nil if the cache is disabled.
func casBlobCacheFromEnv(upstream cas.BlobSource) (*cas.CachingReader, error) {
	config := casCacheConfigFromEnv()
	if !config.enabled {
		return nil, nil
	}
	return cas.NewCachingReader(upstream,
		cas.WithCacheDir(config.dir),
		cas.WithCacheMaxSize(config.maxSize),
		cas.WithCacheBufferSize(config.bufferSize),
	)
}

// byteSizeFromEnv reads a byte count from an environment variable, warning and
// falling back to fallback if it cannot be parsed.
func byteSizeFromEnv(name string, fallback int64) (int64, bool) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false
	}
	size, err := parseByteSize(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: ignoring invalid %s=%q: %v, using %d\n", name, raw, err, fallback)
		return 0, false
	}
	return size, true
}

// parseByteSize parses a byte count, optionally with a binary unit suffix
// (K/KB/KiB, M/MB/MiB, G/GB/GiB, T/TB/TiB, case-insensitive). Suffixes are
// powers of 1024 either way, as everywhere else in Bazel's ecosystem.
func parseByteSize(raw string) (int64, error) {
	digits := strings.TrimRight(raw, "kKmMgGtTiIbB \t")
	unit := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(raw, digits)))
	value, err := strconv.ParseFloat(strings.TrimSpace(digits), 64)
	if err != nil {
		return 0, fmt.Errorf("not a byte count")
	}
	multiplier := int64(1)
	switch unit {
	case "", "b":
	case "k", "kb", "kib":
		multiplier = 1 << 10
	case "m", "mb", "mib":
		multiplier = 1 << 20
	case "g", "gb", "gib":
		multiplier = 1 << 30
	case "t", "tb", "tib":
		multiplier = 1 << 40
	default:
		return 0, fmt.Errorf("unknown unit %q", unit)
	}
	if value < 0 {
		return 0, fmt.Errorf("negative size")
	}
	return int64(value * float64(multiplier)), nil
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

// defaultDeployJobs is the default push parallelism for `img deploy`. Like
// `crane copy`, it defaults to GOMAXPROCS (the host CPU count) to maximize
// throughput. The shared registry defaults still use registryopts.DefaultJobs
// (4) when driven without an explicit --jobs; `img deploy` overrides that here.
func defaultDeployJobs() int {
	return runtime.GOMAXPROCS(0)
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
	return defaultDeployJobs()
}

// extractSinkFlag pre-scans args for --sink so DeployProcess can decide whether
// a global oci-tar/docker-save sink must force one-shot mode before the normal
// flag set is parsed.
func extractSinkFlag(args []string) string {
	return extractFlag(args, "--sink")
}

// extractProgressFlag pre-scans args for --progress, which the persistent
// worker needs before (and instead of) the one-shot flag set is parsed.
func extractProgressFlag(args []string) string {
	return extractFlag(args, "--progress")
}

// extractFlag returns the value of the given `--flag=value` / `--flag value`
// argument, or the empty string if it is not present.
func extractFlag(args []string, flag string) string {
	for i := 0; i < len(args); i++ {
		key, value, hasValue := strings.Cut(args[i], "=")
		if key == flag {
			if !hasValue && i+1 < len(args) {
				value = args[i+1]
			}
			return value
		}
	}
	return ""
}

// applyProgressMode selects how the deploy reports progress on stderr. An empty
// value auto-detects: interactive progress bars when stderr is a terminal,
// crane-style log lines otherwise. Progress bars can't work in the persistent
// worker - requests are multiplexed onto one stderr (the Bazel worker log) - so
// they degrade to log lines there.
func applyProgressMode(value string, isPersistentWorker bool) error {
	mode, err := progress.ParseMode(value)
	if err != nil {
		return err
	}
	if mode == progress.ModeAuto {
		mode = progress.AutoMode(progress.ModeLog)
	}
	if isPersistentWorker && mode == progress.ModeBar {
		mode = progress.ModeLog
	}
	progress.SetMode(mode)
	return nil
}

// deployToSink builds the requested sink, routes every operation into it, and
// prints the resulting image references. It performs no registry/daemon network
// I/O for the destination.
func deployToSink(ctx context.Context, spec string, vfs *deployvfs.VFS, blobCache *cas.CachingReader, pushOps []api.IndexedPushDeployOperation, loadOps []api.IndexedLoadDeployOperation, tagOps []api.IndexedRegistryTagDeployOperation, settings api.DeploySettings, opts DeployOptions) error {
	kind, path, err := parseSink(spec)
	if err != nil {
		return err
	}
	s, err := newSink(kind, path)
	if err != nil {
		return fmt.Errorf("creating sink: %w", err)
	}
	refs, err := routeToSink(ctx, s, vfs, pushOps, loadOps, tagOps, sinkRouteOptions{
		overrideRegistry:   opts.OverrideRegistry,
		overrideRepository: opts.OverrideRepository,
		additionalTags:     opts.AdditionalTags,
	})
	if err != nil {
		s.Close()
		return err
	}
	// Sign into the sink before Close: the signature artifacts are captured as
	// referrer manifests of their subjects, and the distribution sinks only
	// generate their referrers/ listings from the on-disk manifests at Close.
	if err := signIntoSink(ctx, s, pushOps, settings, signOptions{
		settingFiles:       opts.SignSettingFiles,
		defaultSetting:     opts.DefaultSignSetting,
		force:              opts.SignForce,
		targetOverride:     opts.SignTargets,
		overrideRegistry:   opts.OverrideRegistry,
		overrideRepository: opts.OverrideRepository,
	}); err != nil {
		s.Close()
		return err
	}
	if err := s.Close(); err != nil {
		return fmt.Errorf("finalizing sink: %w", err)
	}
	printBlobStats(vfs, blobCache)
	for _, ref := range refs {
		fmt.Println(ref)
	}
	return nil
}
