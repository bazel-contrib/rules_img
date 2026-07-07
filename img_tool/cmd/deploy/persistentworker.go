package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/google/go-containerregistry/pkg/name"
	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/registry"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/deployvfs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/gateway"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/load"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/persistentworker"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/push"
)

type deployWorkerHandler struct {
	pusher        *remote.Pusher
	jobs          int
	baseBuilder   *deployvfs.Builder
	pushTransport http.RoundTripper

	// globalSink, when set, redirects every request's operations into a shared
	// local sink (distribution/oci) instead of a registry. It is not
	// concurrency-safe, so access is serialized by sinkMu.
	globalSink sink
	sinkMu     sync.Mutex
}

func newDeployWorkerHandler(jobs int, sinkSpec string) (*deployWorkerHandler, error) {
	// Configure optional registry gateways. When IMG_REGISTRY_*_GATEWAY is set,
	// push and base-image (pull) requests are routed through the gateway; when
	// unset, WrapTransport returns the base transport unchanged.
	pushTransport, err := gateway.WrapTransport(remote.DefaultTransport, gateway.ModePush)
	if err != nil {
		return nil, fmt.Errorf("configuring push gateway: %w", err)
	}
	pullTransport, err := gateway.WrapTransport(remote.DefaultTransport, gateway.ModePull)
	if err != nil {
		return nil, fmt.Errorf("configuring pull gateway: %w", err)
	}

	baseBuilder := deployvfs.NewBuilder(api.DeployManifest{}).
		WithContainerRegistryOption(registry.WithAuthFromMultiKeychain()).
		WithContainerRegistryOption(remote.WithTransport(pullTransport))
	// We set needsCAS to true unconditionally.
	// The reason is that we just cannot know in advance whether a future work request
	// wants to connect to the remote cache or not.
	baseBuilder, err = configureBuilderFromEnv(baseBuilder, true /* needsCAS */, jobs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to configure VFS from environment: %v\n", err)
	}

	h := &deployWorkerHandler{jobs: jobs, baseBuilder: baseBuilder, pushTransport: pushTransport}

	if sinkSpec != "" {
		// A global distribution/oci sink redirects every request; no pusher is
		// created because no registry I/O is performed.
		kind, path, err := parseSink(sinkSpec)
		if err != nil {
			return nil, err
		}
		s, err := newSink(kind, path)
		if err != nil {
			return nil, fmt.Errorf("creating global sink: %w", err)
		}
		h.globalSink = s
		return h, nil
	}

	opts := []remote.Option{registry.WithAuthFromMultiKeychain(), remote.WithTransport(pushTransport), remote.WithJobs(jobs)}
	p, err := remote.NewPusher(opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to create persistent pusher: %v\n", err)
	}
	h.pusher = p
	return h, nil
}

func (h *deployWorkerHandler) HandleRequest(ctx context.Context, req persistentworker.WorkRequest) persistentworker.WorkResponse {
	output, err := h.processRequest(ctx, req)
	if err != nil {
		return persistentworker.WorkResponse{
			ExitCode:  1,
			Output:    err.Error(),
			RequestId: req.RequestId,
		}
	}
	return persistentworker.WorkResponse{
		ExitCode:  0,
		Output:    output,
		RequestId: req.RequestId,
	}
}

func (h *deployWorkerHandler) processRequest(ctx context.Context, req persistentworker.WorkRequest) (string, error) {
	opts, err := parseWorkerArgs(req.Arguments)
	if err != nil {
		return "", fmt.Errorf("parsing arguments: %w", err)
	}

	rawRequest, err := mergeRequestFiles(opts.requestFiles)
	if err != nil {
		return "", err
	}

	var dm api.DeployManifest
	if err := json.Unmarshal(rawRequest, &dm); err != nil {
		return "", fmt.Errorf("unmarshalling deploy manifest: %w", err)
	}

	vfsBuilder := h.baseBuilder.Clone().WithDeployManifest(dm).WithContext(ctx)
	for _, layoutPath := range opts.ociLayouts {
		vfsBuilder = vfsBuilder.WithOCILayout(layoutPath)
	}
	for _, spec := range opts.layers {
		vfsBuilder = vfsBuilder.WithLayer(spec)
	}
	if opts.runfilesPrefix != "" {
		vfsBuilder = vfsBuilder.WithRunfilesRootSymlinksPrefix(opts.runfilesPrefix)
	}
	vfs, err := vfsBuilder.Build()
	if err != nil {
		return "", fmt.Errorf("building VFS: %w", err)
	}

	// Sink routing bypasses the pusher/loader/tagger entirely (no registry or
	// daemon network I/O for the destination).
	if h.globalSink != nil {
		if opts.sink != "" {
			return "", fmt.Errorf("--sink cannot be set in a work request when img deploy was started with a global --sink")
		}
		return h.routeGlobalSink(ctx, vfs, dm, opts)
	}
	if opts.sink != "" {
		return h.routePerRequestSink(ctx, vfs, dm, opts)
	}

	var output strings.Builder

	pushOps, err := dm.PushOperations()
	if err != nil {
		return "", err
	}
	if len(pushOps) > 0 {
		tags, err := h.pushOps(ctx, vfs, pushOps, dm.Settings.PushStrategy, opts)
		if err != nil {
			return "", fmt.Errorf("push: %w", err)
		}
		for _, tag := range tags {
			output.WriteString(tag)
			output.WriteByte('\n')
		}
	}

	registryTagOps, err := dm.RegistryTagOperations()
	if err != nil {
		return "", err
	}
	if len(registryTagOps) > 0 {
		tags, err := h.registryTagOps(ctx, vfs, registryTagOps, dm.Settings.PushStrategy, opts)
		if err != nil {
			return "", fmt.Errorf("registry_tag: %w", err)
		}
		for _, tag := range tags {
			output.WriteString(tag)
			output.WriteByte('\n')
		}
	}

	loadOps, err := dm.LoadOperations()
	if err != nil {
		return "", err
	}
	if len(loadOps) > 0 {
		builder := load.NewBuilder(vfs)
		if len(opts.platforms) > 0 {
			builder = builder.WithPlatforms(opts.platforms)
		}
		// Overrides apply only to split-mode load ops (non-empty
		// registry/repository); the loader leaves the rules_oci fallback alone.
		if opts.overrideRegistry != "" {
			builder = builder.WithOverrideRegistry(opts.overrideRegistry)
		}
		if opts.overrideRepository != "" {
			builder = builder.WithOverrideRepository(opts.overrideRepository)
		}
		if _, err := builder.Build().LoadAll(ctx, loadOps); err != nil {
			return "", fmt.Errorf("load: %w", err)
		}
	}

	return output.String(), nil
}

// sinkOperations extracts the push, load and registry_tag operations from a
// deploy manifest.
func sinkOperations(dm api.DeployManifest) ([]api.IndexedPushDeployOperation, []api.IndexedLoadDeployOperation, []api.IndexedRegistryTagDeployOperation, error) {
	pushOps, err := dm.PushOperations()
	if err != nil {
		return nil, nil, nil, err
	}
	loadOps, err := dm.LoadOperations()
	if err != nil {
		return nil, nil, nil, err
	}
	tagOps, err := dm.RegistryTagOperations()
	if err != nil {
		return nil, nil, nil, err
	}
	return pushOps, loadOps, tagOps, nil
}

// routeGlobalSink captures a request's operations into the shared global sink.
// Access is serialized because the incremental dir sinks are not concurrency-
// safe, and the sink is flushed after each request so its on-disk state stays
// valid between requests.
func (h *deployWorkerHandler) routeGlobalSink(ctx context.Context, vfs *deployvfs.VFS, dm api.DeployManifest, opts *workerOpts) (string, error) {
	pushOps, loadOps, tagOps, err := sinkOperations(dm)
	if err != nil {
		return "", err
	}
	h.sinkMu.Lock()
	defer h.sinkMu.Unlock()
	refs, err := routeToSink(ctx, h.globalSink, vfs, pushOps, loadOps, tagOps, sinkRouteOptions{
		overrideRegistry:   opts.overrideRegistry,
		overrideRepository: opts.overrideRepository,
	})
	if err != nil {
		return "", err
	}
	// Capture signatures into the shared sink before flushing, so they are
	// included in the sink and (for distribution) in the referrers listings.
	// Runtime sign overrides are command-line only; the worker signs per the
	// manifest's per-operation Sign config discovered from runfiles.
	if err := signIntoSink(ctx, h.globalSink, pushOps, dm.Settings, signOptions{
		overrideRegistry:   opts.overrideRegistry,
		overrideRepository: opts.overrideRepository,
	}); err != nil {
		return "", err
	}
	if err := h.globalSink.Close(); err != nil {
		return "", fmt.Errorf("flushing sink: %w", err)
	}
	return refsOutput(refs), nil
}

// routePerRequestSink captures a request's operations into an isolated,
// request-scoped oci-tar/docker-save sink.
func (h *deployWorkerHandler) routePerRequestSink(ctx context.Context, vfs *deployvfs.VFS, dm api.DeployManifest, opts *workerOpts) (string, error) {
	kind, path, err := parseSink(opts.sink)
	if err != nil {
		return "", err
	}
	s, err := newSink(kind, path)
	if err != nil {
		return "", fmt.Errorf("creating sink: %w", err)
	}
	pushOps, loadOps, tagOps, err := sinkOperations(dm)
	if err != nil {
		return "", err
	}
	refs, err := routeToSink(ctx, s, vfs, pushOps, loadOps, tagOps, sinkRouteOptions{
		overrideRegistry:   opts.overrideRegistry,
		overrideRepository: opts.overrideRepository,
	})
	if err != nil {
		s.Close()
		return "", err
	}
	// Capture signatures into the sink before finalizing (see routeGlobalSink).
	if err := signIntoSink(ctx, s, pushOps, dm.Settings, signOptions{
		overrideRegistry:   opts.overrideRegistry,
		overrideRepository: opts.overrideRepository,
	}); err != nil {
		s.Close()
		return "", err
	}
	if err := s.Close(); err != nil {
		return "", fmt.Errorf("finalizing sink: %w", err)
	}
	return refsOutput(refs), nil
}

func refsOutput(refs []string) string {
	var output strings.Builder
	for _, ref := range refs {
		output.WriteString(ref)
		output.WriteByte('\n')
	}
	return output.String()
}

func (h *deployWorkerHandler) pushOps(ctx context.Context, vfs *deployvfs.VFS, ops []api.IndexedPushDeployOperation, strategy string, opts *workerOpts) ([]string, error) {
	uploadBuilder := push.NewBuilder(vfs).
		WithPusher(h.pusher).
		WithJobs(h.jobs).
		WithRemoteOptions(registry.WithAuthFromMultiKeychain(), remote.WithTransport(h.pushTransport))
	if opts.overrideRegistry != "" {
		uploadBuilder = uploadBuilder.WithOverrideRegistry(opts.overrideRegistry)
	}
	if opts.overrideRepository != "" {
		uploadBuilder = uploadBuilder.WithOverrideRepository(opts.overrideRepository)
	}
	return uploadBuilder.Build().PushAll(ctx, ops, strategy)
}

func (h *deployWorkerHandler) registryTagOps(ctx context.Context, vfs *deployvfs.VFS, ops []api.IndexedRegistryTagDeployOperation, strategy string, opts *workerOpts) ([]string, error) {
	if strategy == "bes" {
		return nil, nil
	}
	if h.pusher == nil {
		return nil, fmt.Errorf("no pusher available for registry_tag operations")
	}

	type pushItem struct {
		ref      name.Reference
		taggable remote.Taggable
	}
	var items []pushItem
	var tagNames []string

	for _, op := range ops {
		opRegistry := op.Registry
		if opts.overrideRegistry != "" {
			opRegistry = opts.overrideRegistry
		}
		opRepository := op.Repository
		if opts.overrideRepository != "" {
			opRepository = opts.overrideRepository
		}
		baseRef := opRegistry + "/" + opRepository

		rootHash, err := registryv1.NewHash(op.Root.Digest)
		if err != nil {
			return nil, fmt.Errorf("parsing root digest for registry_tag: %w", err)
		}
		taggable, err := vfs.Taggable(rootHash)
		if err != nil {
			return nil, fmt.Errorf("locating manifest %s for registry_tag: %w", op.Root.Digest, err)
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
	g.SetLimit(h.jobs)

	for _, item := range items {
		item := item
		g.Go(func() error {
			return h.pusher.Push(ctx, item.ref, item.taggable)
		})
	}

	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("applying registry_tag operations: %w", err)
	}
	return tagNames, nil
}

type workerOpts struct {
	requestFiles       []string
	ociLayouts         []string
	layers             []string // raw --layer specs: "digest=path" or bare "path" (raw blob or .cstream)
	runfilesPrefix     string
	overrideRegistry   string
	overrideRepository string
	platforms          []string
	sink               string
}

func parseWorkerArgs(args []string) (*workerOpts, error) {
	opts := &workerOpts{}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		key, value, hasValue := strings.Cut(arg, "=")

		switch {
		case key == "--request-file":
			if !hasValue {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("--request-file requires a value")
				}
				i++
				value = args[i]
			}
			opts.requestFiles = append(opts.requestFiles, value)
		case key == "--oci-layout":
			if !hasValue {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("--oci-layout requires a value")
				}
				i++
				value = args[i]
			}
			opts.ociLayouts = append(opts.ociLayouts, value)
		case key == "--layer":
			if !hasValue {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("--layer requires a value")
				}
				i++
				value = args[i]
			}
			opts.layers = append(opts.layers, value)
		case key == "--runfiles-root-symlinks-prefix":
			if !hasValue {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("--runfiles-root-symlinks-prefix requires a value")
				}
				i++
				value = args[i]
			}
			opts.runfilesPrefix = value
		case key == "--registry":
			if !hasValue {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("--registry requires a value")
				}
				i++
				value = args[i]
			}
			opts.overrideRegistry = value
		case key == "--repository":
			if !hasValue {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("--repository requires a value")
				}
				i++
				value = args[i]
			}
			opts.overrideRepository = value
		case key == "--platform":
			if !hasValue {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("--platform requires a value")
				}
				i++
				value = args[i]
			}
			opts.platforms = strings.Split(value, ",")
			for j, p := range opts.platforms {
				opts.platforms[j] = strings.TrimSpace(p)
			}
		case key == "--sink":
			if !hasValue {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("--sink requires a value")
				}
				i++
				value = args[i]
			}
			kind, _, err := parseSink(value)
			if err != nil {
				return nil, err
			}
			if kind.globalOnly() {
				return nil, fmt.Errorf("--sink %q may only be set on the img deploy command line, not in a work request", value)
			}
			opts.sink = value
		}
	}

	if len(opts.requestFiles) == 0 {
		return nil, fmt.Errorf("at least one --request-file is required")
	}
	return opts, nil
}

func persistentWorker(jobs int, sinkSpec string) error {
	handler, err := newDeployWorkerHandler(jobs, sinkSpec)
	if err != nil {
		return err
	}
	worker := persistentworker.NewWorker(handler)
	return worker.Run()
}
