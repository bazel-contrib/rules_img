package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/imagebuildah"
	buildahcli "go.podman.io/buildah/pkg/cli"
	"go.podman.io/buildah/util"
	"go.podman.io/image/v5/pkg/compression"
	"go.podman.io/storage"
)

const pushCompressionLevel = 1

type buildRequest struct {
	dockerfile   string
	contextDir   string
	blobCacheDir string
	buildArgs    []string
	cacheFrom    string
	cacheTo      string
	blobSeeds    []blobSeed
}

func buildImage(ctx context.Context, store storage.Store, req buildRequest) (string, error) {
	options, containerfiles, removeAll, err := genBuildOptions(req)
	defer func() {
		for _, path := range removeAll {
			_ = os.RemoveAll(path)
		}
	}()
	if err != nil {
		return "", err
	}

	options.Compression = define.Zstd

	if req.cacheTo != "" {
		seedBlobLocations(options.SystemContext, req.blobSeeds)
	}

	pusher := newDeferredCachePusher(ctx, store, options)
	options.CachePushSourceLookupReferenceFunc = pusher.sourceLookup
	options.CachePushDestinationLookupReferenceFunc = discardDestinationLookup

	imageID, _, err := imagebuildah.BuildDockerfiles(ctx, store, options, containerfiles...)
	pushErr := pusher.wait()
	if err != nil {
		return "", fmt.Errorf("buildah build %s: %w", req.dockerfile, err)
	}
	if pushErr != nil {
		return "", pushErr
	}
	return imageID, nil
}

func genBuildOptions(req buildRequest) (define.BuildOptions, []string, []string, error) {
	layerResults := buildahcli.LayerResults{}
	budResults := buildahcli.BudResults{}
	fromAndBudResults := buildahcli.FromAndBudResults{}
	userNSResults := buildahcli.UserNSResults{}
	namespaceResults := buildahcli.NameSpaceResults{}

	budFlags := buildahcli.GetBudFlags(&budResults)
	budFlags.StringVar(&budResults.Runtime, "runtime", util.Runtime(), "path to an alternate runtime")
	layerFlags := buildahcli.GetLayerFlags(&layerResults)
	fromAndBudFlags, err := buildahcli.GetFromAndBudFlags(&fromAndBudResults, &userNSResults, &namespaceResults)
	if err != nil {
		return define.BuildOptions{}, nil, nil, fmt.Errorf("assembling buildah build flags: %w", err)
	}

	cmd := &cobra.Command{Use: "build"}
	flags := cmd.Flags()
	flags.AddFlagSet(&budFlags)
	flags.AddFlagSet(&layerFlags)
	flags.AddFlagSet(&fromAndBudFlags)
	flags.SetNormalizeFunc(buildahcli.AliasFlags)

	var setErr error
	set := func(name, value string) {
		if setErr != nil {
			return
		}
		if err := flags.Set(name, value); err != nil {
			setErr = fmt.Errorf("setting buildah flag --%s=%q: %w", name, value, err)
		}
	}
	set("blob-cache", req.blobCacheDir)
	set("tls-verify", "false")
	set("retry-delay", "2ms")
	set("force-compression", "false")
	set("compression-format", compression.Zstd.Name())
	set("compression-level", strconv.Itoa(pushCompressionLevel))
	set("network", "host")
	set("layers", "true")
	set("format", "oci")
	set("file", req.dockerfile)
	if req.cacheFrom != "" {
		set("cache-from", req.cacheFrom)
	}
	if req.cacheTo != "" {
		set("cache-to", req.cacheTo)
	}
	for _, arg := range req.buildArgs {
		set("build-arg", arg)
	}
	for _, host := range flagAddHosts {
		set("add-host", host)
	}
	if setErr != nil {
		return define.BuildOptions{}, nil, nil, setErr
	}

	iopts := buildahcli.BuildOptions{
		LayerResults:      &layerResults,
		BudResults:        &budResults,
		UserNSResults:     &userNSResults,
		FromAndBudResults: &fromAndBudResults,
		NameSpaceResults:  &namespaceResults,
		Logwriter:         logPipe,
	}
	options, containerfiles, removeAll, err := buildahcli.GenBuildOptions(cmd, []string{req.contextDir}, iopts)
	if err != nil {
		return options, containerfiles, removeAll, fmt.Errorf("generating buildah build options: %w", err)
	}
	return options, containerfiles, removeAll, nil
}

func checkStorageBackend(store storage.Store) {
	if driver := store.GraphDriverName(); driver != "overlay" {
		fmt.Fprintf(os.Stderr, "buildah_tool | WARNING | storage driver %q is suboptimal, expected native overlay\n", driver)
	}

	status, err := store.Status()
	if err != nil {
		return
	}
	for _, entry := range status {
		if entry[0] == "Native Overlay Diff" && strings.TrimSpace(entry[1]) == "false" {
			fmt.Fprintf(os.Stderr, "buildah_tool | WARNING | overlay is using fuse-overlayfs instead of native diff, this may degrade build performance; make sure /var/lib/containers/storage is stored on native filesystem (not overlay)\n")
		}
	}
}
