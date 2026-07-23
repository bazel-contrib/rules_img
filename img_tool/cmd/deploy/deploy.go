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
	flagSet.IntVar(&jobs, "jobs", defaultDeployJobs(), "Maximum number of parallel push operations (defaults to GOMAXPROCS)")
	flagSet.StringVar(&sink, "sink", "", "Override the destination of all push/load/registry_tag operations for testing. Format: <type>:<path> where type is one of oci-tar, docker-save, oci, distribution, distribution-flat. No registry or daemon network I/O is performed.")
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
		return deployToSink(ctx, opts.Sink, vfs, pushOperations, loadOperations, registryTagOperations, req.Settings, opts)
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

	// Print VFS statistics to stderr
	stats := vfs.Stats()
	fmt.Fprintf(os.Stderr, "    blob transfers: %d from disk, %d from disk cache, %d from container registry, %d from remote cache, %d from compact stream\n", stats.BlobsFromLocalDisk.Load(), stats.BlobsFromDiskCache.Load(), stats.BlobsFromRegistry.Load(), stats.BlobsFromRemoteCache.Load(), stats.BlobsFromCompactStream.Load())

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
		repo, err := name.NewRepository(reg + "/" + blobRepository)
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
	for i := 0; i < len(args); i++ {
		key, value, hasValue := strings.Cut(args[i], "=")
		if key == "--sink" {
			if !hasValue && i+1 < len(args) {
				value = args[i+1]
			}
			return value
		}
	}
	return ""
}

// deployToSink builds the requested sink, routes every operation into it, and
// prints the resulting image references. It performs no registry/daemon network
// I/O for the destination.
func deployToSink(ctx context.Context, spec string, vfs *deployvfs.VFS, pushOps []api.IndexedPushDeployOperation, loadOps []api.IndexedLoadDeployOperation, tagOps []api.IndexedRegistryTagDeployOperation, settings api.DeploySettings, opts DeployOptions) error {
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
	stats := vfs.Stats()
	fmt.Fprintf(os.Stderr, "    blob transfers: %d from disk, %d from disk cache, %d from container registry, %d from remote cache, %d from compact stream\n", stats.BlobsFromLocalDisk.Load(), stats.BlobsFromDiskCache.Load(), stats.BlobsFromRegistry.Load(), stats.BlobsFromRemoteCache.Load(), stats.BlobsFromCompactStream.Load())
	for _, ref := range refs {
		fmt.Println(ref)
	}
	return nil
}
