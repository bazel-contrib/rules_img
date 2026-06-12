package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/google/go-containerregistry/pkg/name"
	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/registry"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/deployvfs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/load"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/persistentworker"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/push"
)

type deployWorkerHandler struct {
	pusher      *remote.Pusher
	jobs        int
	baseBuilder *deployvfs.Builder
}

func newDeployWorkerHandler(jobs int) *deployWorkerHandler {
	baseBuilder := deployvfs.NewBuilder(api.DeployManifest{}).
		WithContainerRegistryOption(registry.WithAuthFromMultiKeychain())
	baseBuilder, err := configureBuilderFromEnv(baseBuilder)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to configure VFS from environment: %v\n", err)
	}

	opts := []remote.Option{registry.WithAuthFromMultiKeychain(), remote.WithJobs(jobs)}
	p, err := remote.NewPusher(opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to create persistent pusher: %v\n", err)
	}
	return &deployWorkerHandler{pusher: p, jobs: jobs, baseBuilder: baseBuilder}
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

	vfsBuilder := h.baseBuilder.Clone().WithDeployManifest(dm)
	for _, layoutPath := range opts.ociLayouts {
		vfsBuilder = vfsBuilder.WithOCILayout(layoutPath)
	}
	for digest, filePath := range opts.explicitLayers {
		vfsBuilder = vfsBuilder.WithExplicitLayer(digest, filePath)
	}
	if opts.runfilesPrefix != "" {
		vfsBuilder = vfsBuilder.WithRunfilesRootSymlinksPrefix(opts.runfilesPrefix)
	}
	vfs, err := vfsBuilder.Build()
	if err != nil {
		return "", fmt.Errorf("building VFS: %w", err)
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
		if _, err := builder.Build().LoadAll(ctx, loadOps); err != nil {
			return "", fmt.Errorf("load: %w", err)
		}
	}

	return output.String(), nil
}

func (h *deployWorkerHandler) pushOps(ctx context.Context, vfs *deployvfs.VFS, ops []api.IndexedPushDeployOperation, strategy string, opts *workerOpts) ([]string, error) {
	uploadBuilder := push.NewBuilder(vfs).
		WithPusher(h.pusher).
		WithJobs(h.jobs).
		WithRemoteOptions(registry.WithAuthFromMultiKeychain())
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
	explicitLayers     map[string]string
	runfilesPrefix     string
	overrideRegistry   string
	overrideRepository string
	platforms          []string
}

func parseWorkerArgs(args []string) (*workerOpts, error) {
	opts := &workerOpts{
		explicitLayers: make(map[string]string),
	}

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
			digest, path, ok := strings.Cut(value, "=")
			if !ok {
				return nil, fmt.Errorf("--layer must be in format digest=path, got %q", value)
			}
			opts.explicitLayers[digest] = path
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
		}
	}

	if len(opts.requestFiles) == 0 {
		return nil, fmt.Errorf("at least one --request-file is required")
	}
	return opts, nil
}

func persistentWorker(jobs int) error {
	handler := newDeployWorkerHandler(jobs)
	worker := persistentworker.NewWorker(handler)
	return worker.Run()
}
