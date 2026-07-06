package main

import (
	"context"
	"fmt"
	"os"

	"github.com/google/go-containerregistry/pkg/logs"

	"github.com/bazel-contrib/rules_img/img_tool/cmd/casdir"
	compactstreamcmd "github.com/bazel-contrib/rules_img/img_tool/cmd/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/compress"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/cst"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/deploy"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/deploymetadata"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/dockersave"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/downloadblob"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/downloadmanifest"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/expandtemplate"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/hash"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/index"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/indexfromocilayout"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/layer"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/manifest"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/manifestfromocilayout"
	mtreecmd "github.com/bazel-contrib/rules_img/img_tool/cmd/mtree"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/ocilayout"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/ocilayoutmetadata"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/optimize"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/pull"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/sparseocilayout"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/syncocirefgraph"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/validate"
)

const usage = `Usage: img [COMMAND] [ARGS...]

Global flags (accepted by any command):
  --verbose                enables debug logging

Commands:
  compress                 (re-)compresses a layer
  docker-save              assembles a Docker save compatible directory or tarball
  download-blob            downloads a single blob from a registry
  download-manifest        downloads a manifest by digest or tag from a registry
  expand-template          expands Go templates in push request JSON
  hash                     computes file hashes and layer metadata (supports persistent worker mode)
  index                    creates a multi-platform image index
  index-from-oci-layout    converts an OCI layout to an image index
  layer                    creates a layer from files
  manifest                 creates an image manifest and config from layers
  manifest-from-oci-layout converts an OCI layout to an image manifest
  mtree                    writes an mtree spec of a layer's metadata and merges mtree files
  oci-layout               assembles an OCI layout directory from manifest and layers
  oci-layout-metadata      extracts per-platform config and mtree from an OCI image layout
  optimize                 rewrites image metadata after layer optimization
  pull                     pulls an image from a registry
  sparse-oci-layout        assembles a sparse OCI layout (without layer blobs) from manifest and layers
  compact-stream           inspects or reconstructs a compact stream (subcommands: reconstruct, list)
  cas-dir                  builds a content-addressed directory (sha256/<hex>) from input files
  sync-oci-ref-graph       syncs OCI reference graph by downloading manifests in parallel
  validate                 validates layers and images
  image-structure-test     validates an image's structure (config + mtree) against container-structure-test configs
  deploy                   pushes an image to a registry or loads it into a local container runtime
  deploy-metadata          calculates metadata for deploying an image (push/load)
  deploy-merge             merges multiple deploy manifests into a single deployment`

func Run(ctx context.Context, args []string) {
	// Handle the global --verbose flag for all subcommands. We strip it from
	// the arguments before dispatching so each subcommand's own flag parser
	// doesn't have to know about it.
	args = handleVerbose(args)

	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}

	command := args[1]
	switch command {
	case "layer":
		layer.LayerProcess(ctx, args[2:])
	case "manifest":
		manifest.ManifestProcess(ctx, args[2:])
	case "manifest-from-oci-layout":
		manifestfromocilayout.ManifestFromOCILayoutProcess(ctx, args[2:])
	case "mtree":
		mtreecmd.MtreeProcess(ctx, args[2:])
	case "oci-layout-metadata":
		ocilayoutmetadata.OCILayoutMetadataProcess(ctx, args[2:])
	case "image-structure-test":
		cst.Process(ctx, args[2:])
	case "index":
		index.IndexProcess(ctx, args[2:])
	case "index-from-oci-layout":
		indexfromocilayout.IndexFromOCILayoutProcess(ctx, args[2:])
	case "validate":
		validate.ValidationProcess(ctx, args[2:])
	case "deploy":
		deploy.DeployProcess(ctx, args[2:])
	case "deploy-metadata":
		deploymetadata.DeployMetadataProcess(ctx, args[2:])
	case "deploy-merge":
		deploymetadata.DeployMergeProcess(ctx, args[2:])
	case "compress":
		compress.CompressProcess(ctx, args[2:])
	case "docker-save":
		dockersave.DockerSaveProcess(ctx, args[2:])
	case "download-blob":
		downloadblob.DownloadBlobProcess(ctx, args[2:])
	case "download-manifest":
		downloadmanifest.DownloadManifestProcess(ctx, args[2:])
	case "pull":
		pull.PullProcess(ctx, args[2:])
	case "sync-oci-ref-graph":
		syncocirefgraph.SyncOCIRefGraphProcess(ctx, args[2:])
	case "hash":
		hash.HashProcess(ctx, args[2:])
	case "oci-layout":
		ocilayout.OCILayoutProcess(ctx, args[2:])
	case "optimize":
		optimize.OptimizeProcess(ctx, args[2:])
	case "sparse-oci-layout":
		sparseocilayout.SparseOCILayoutProcess(ctx, args[2:])
	case "compact-stream":
		compactstreamcmd.CompactStreamProcess(ctx, args[2:])
	case "cas-dir":
		casdir.CASDirProcess(ctx, args[2:])
	case "expand-template":
		expandtemplate.ExpandTemplateProcess(ctx, args[2:])
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}
}

func main() {
	ctx := context.Background()
	Run(ctx, os.Args)
}

// handleVerbose looks for a global --verbose (or -verbose) flag anywhere in
// args. If present, it enables debug logging to stderr and returns args with
// the flag removed so the individual subcommand flag parsers don't see it.
func handleVerbose(args []string) []string {
	filtered := make([]string, 0, len(args))
	verbose := false
	for _, arg := range args {
		switch arg {
		case "--verbose", "-verbose":
			verbose = true
			continue
		}
		filtered = append(filtered, arg)
	}
	if verbose {
		logs.Debug.SetOutput(os.Stderr)
	}
	return filtered
}
