package pushcmd

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/registry"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/deployvfs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/gateway"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/push"
)

func manifestProcess(ctx context.Context, args []string) {
	var (
		requestFile        string
		ociLayouts         stringSliceFlag
		layerResults       stringSliceFlag
		manifestRepository string
		mode               string
		markerPath         string
		jobs               int
	)

	flagSet := flag.NewFlagSet("push manifest", flag.ContinueOnError)
	flagSet.StringVar(&requestFile, "request-file", "", "Push-only deploy manifest JSON. Required.")
	flagSet.Var(&ociLayouts, "oci-layout", "Path to a (sparse) OCI layout with the manifest(s) + config (can be repeated). Required.")
	flagSet.Var(&layerResults, "layer-result", "Path to a JSON result from `push blob` recording a layer's location for cross-mounting (can be repeated).")
	flagSet.StringVar(&manifestRepository, "manifest-repository", "", "Repository to upload the manifest(s)/index and config to instead of the operation's own repository. Layer blobs are still cross-mounted from where `push blob` put them (this does not change blob mounting).")
	flagSet.StringVar(&mode, "mode", "enabled", "Failure mode: 'best_effort' (log, don't fail build) or 'enabled' (fail build).")
	flagSet.StringVar(&markerPath, "marker", "", "Path to the marker file to write on success. Required.")
	flagSet.IntVar(&jobs, "jobs", 16, "Maximum number of parallel push operations.")

	if err := flagSet.Parse(args); err != nil {
		os.Exit(1)
	}
	if requestFile == "" || markerPath == "" {
		fmt.Fprintln(os.Stderr, "Error: --request-file and --marker are required")
		os.Exit(1)
	}

	finish(mode, markerPath, nil, pushManifest(ctx, requestFile, []string(ociLayouts), []string(layerResults), manifestRepository, jobs))
}

func pushManifest(ctx context.Context, requestFile string, ociLayouts, layerResults []string, manifestRepository string, jobs int) error {
	raw, err := os.ReadFile(requestFile)
	if err != nil {
		return fmt.Errorf("reading request file %s: %w", requestFile, err)
	}
	var req api.DeployManifest
	if err := json.Unmarshal(raw, &req); err != nil {
		return fmt.Errorf("parsing request file %s: %w", requestFile, err)
	}

	// Route base-image (pull) reads and the manifest push through the configured
	// registry gateway when one is set; otherwise these are the base transport.
	pushTransport, err := gateway.WrapTransport(remote.DefaultTransport, gateway.ModePush)
	if err != nil {
		return fmt.Errorf("configuring push gateway: %w", err)
	}
	pullTransport, err := gateway.WrapTransport(remote.DefaultTransport, gateway.ModePull)
	if err != nil {
		return fmt.Errorf("configuring pull gateway: %w", err)
	}

	builder := deployvfs.NewBuilder(req).
		WithContainerRegistryOption(registry.WithAuthFromMultiKeychain()).
		WithContainerRegistryOption(remote.WithTransport(pullTransport)).
		WithContext(ctx)
	for _, layout := range ociLayouts {
		builder = builder.WithOCILayout(layout)
	}
	for _, resultPath := range layerResults {
		result, err := readBlobResult(resultPath)
		if err != nil {
			return err
		}
		builder = builder.WithCrossMountSource(result.Digest, api.CrossMountSource{
			Registry:   result.Registry,
			Repository: result.Repository,
		})
	}
	vfs, err := builder.Build()
	if err != nil {
		return fmt.Errorf("building VFS: %w", err)
	}

	pushOps, err := req.PushOperations()
	if err != nil {
		return err
	}
	if len(pushOps) == 0 {
		return fmt.Errorf("no push operations found in request file")
	}

	uploaderBuilder := push.NewBuilder(vfs).
		WithJobs(jobs).
		WithRemoteOptions(registry.WithAuthFromMultiKeychain(), remote.WithTransport(pushTransport))
	// Redirect the manifest/index (and the config uploaded alongside it) to the
	// manifest-staging repository when configured. Layer blobs are unaffected: they
	// are cross-mounted from wherever `push blob` put them (recorded via the
	// per-layer cross-mount sources above), not re-uploaded into this repository.
	if manifestRepository != "" {
		uploaderBuilder = uploaderBuilder.WithOverrideRepository(manifestRepository)
	}
	uploader := uploaderBuilder.Build()
	if _, err := uploader.PushAll(ctx, pushOps, req.Settings.PushStrategy); err != nil {
		return fmt.Errorf("pushing manifests: %w", err)
	}
	return nil
}

func readBlobResult(path string) (BlobResult, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return BlobResult{}, fmt.Errorf("reading layer result %s: %w", path, err)
	}
	var result BlobResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return BlobResult{}, fmt.Errorf("parsing layer result %s: %w", path, err)
	}
	if result.Digest == "" || result.Repository == "" {
		return BlobResult{}, fmt.Errorf("layer result %s missing digest/repository", path)
	}
	return result, nil
}
