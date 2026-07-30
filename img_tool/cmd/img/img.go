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
	ocilayoutcmd "github.com/bazel-contrib/rules_img/img_tool/cmd/ocilayoutcmd"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/ocilayoutmetadata"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/optimize"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/pull"
	pushcmd "github.com/bazel-contrib/rules_img/img_tool/cmd/push"
	socicmd "github.com/bazel-contrib/rules_img/img_tool/cmd/soci"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/sparseocilayout"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/syncocirefgraph"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/validate"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/registryopts"
)

const usage = `Usage: img [COMMAND] [ARGS...]

Global flags (accepted by any command):
  --verbose                enables debug logging
  --insecure               allows registries to be addressed over plain HTTP and
                           accepts untrusted TLS certificates (like crane's
                           --insecure). Also settable via IMG_INSECURE=1.

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
  soci-index               creates a SOCI Index Manifest v2 from per-layer ztoc blobs
  ztoc                     generates a ztoc (SOCI table of contents) for a gzip-compressed layer
  compact-stream           inspects or reconstructs a compact stream (subcommands: reconstruct, list)
  cas-dir                  builds a content-addressed directory (sha256/<hex>) from input files
  sync-oci-ref-graph       syncs OCI reference graph by downloading manifests in parallel
  validate                 validates layers and images
  image-structure-test     validates an image's structure (config + mtree) against container-structure-test configs
  deploy                   pushes an image to a registry or loads it into a local container runtime
  deploy-metadata          calculates metadata for deploying an image (push/load)
  deploy-merge             merges multiple deploy manifests into a single deployment
  push                     pushes image blobs/manifests at build time (subcommands: blob, manifest)`

func Run(ctx context.Context, args []string) {
	// Handle the global flags for all subcommands. We strip them from
	// the arguments before dispatching so each subcommand's own flag parser
	// doesn't have to know about them.
	args = handleGlobalFlags(args)

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
	case "soci-index":
		socicmd.SociIndexProcess(ctx, args[2:])
	case "ztoc":
		socicmd.ZtocProcess(ctx, args[2:])
	case "validate":
		validate.ValidationProcess(ctx, args[2:])
	case "deploy":
		deploy.DeployProcess(ctx, args[2:])
	case "push":
		pushcmd.PushProcess(ctx, args[2:])
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
		ocilayoutcmd.OCILayoutProcess(ctx, args[2:])
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

// handleGlobalFlags looks for the global flags (--verbose, --insecure) anywhere
// in args, applies them, and returns args with those flags removed so the
// individual subcommand flag parsers don't see them.
//
//   - --verbose enables debug logging to stderr.
//   - --insecure lets every registry operation talk plain HTTP and accept
//     untrusted TLS certificates, like crane's --insecure. It is only ever
//     enabling: without the flag, IMG_INSECURE still decides.
func handleGlobalFlags(args []string) []string {
	filtered := make([]string, 0, len(args))
	verbose := false
	insecure := false
	for _, arg := range args {
		switch arg {
		case "--verbose", "-verbose":
			verbose = true
			continue
		case "--insecure", "-insecure":
			insecure = true
			continue
		}
		filtered = append(filtered, arg)
	}
	if verbose {
		logs.Debug.SetOutput(os.Stderr)
	}
	if insecure {
		registryopts.SetInsecure(true)
	}
	return filtered
}
