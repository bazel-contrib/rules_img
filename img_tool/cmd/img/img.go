package main

import (
	"context"
	"fmt"
	"os"

	"github.com/bazelbuild/rules_go/go/runfiles"

	"github.com/bazel-contrib/rules_img/img_tool/cmd/compress"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/deploy"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/deploymetadata"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/dockersave"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/downloadblob"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/expandtemplate"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/index"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/indexfromocilayout"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/layer"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/layermeta"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/manifest"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/manifestfromocilayout"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/ocilayout"
	"github.com/bazel-contrib/rules_img/img_tool/cmd/validate"
)

const usage = `Usage: img [COMMAND] [ARGS...]

Commands:
  compress                 (re-)compresses a layer
  docker-save              assembles a Docker save compatible directory or tarball
  download-blob            downloads a single blob from a registry
  expand-template          expands Go templates in push request JSON
  index                    creates a multi-platform image index
  index-from-oci-layout    converts an OCI layout to an image index
  layer                    creates a layer from files
  layer-metadata           creates a layer metadata file from a layer
  manifest                 creates an image manifest and config from layers
  manifest-from-oci-layout converts an OCI layout to an image manifest
  oci-layout               assembles an OCI layout directory from manifest and layers
  validate                 validates layers and images
  push                     pushes an image to a registry
  deploy-metadata          calculates metadata for deploying an image (push/load)
  deploy-merge             merges multiple deploy manifests into a single deployment`

func Run(ctx context.Context, args []string) {
	if runfilesDispatch(ctx, args[1:]) {
		// Check if we got a special command
		// via runfiels root symlinks.
		// If so, we don't need to
		// invoke the normal command line interface.
		return
	}

	// Otherwise, we invoke the normal command line interface.
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}

	command := args[1]
	switch command {
	case "layer":
		layer.LayerProcess(ctx, args[2:])
	case "layer-metadata":
		layermeta.LayerMetadataProcess(ctx, args[2:])
	case "manifest":
		manifest.ManifestProcess(ctx, args[2:])
	case "manifest-from-oci-layout":
		manifestfromocilayout.ManifestFromOCILayoutProcess(ctx, args[2:])
	case "index":
		index.IndexProcess(ctx, args[2:])
	case "index-from-oci-layout":
		indexfromocilayout.IndexFromOCILayoutProcess(ctx, args[2:])
	case "validate":
		validate.ValidationProcess(ctx, args[2:])
	case "push":
		deploy.PushProcess(ctx, args[2:])
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
	case "oci-layout":
		ocilayout.OCILayoutProcess(ctx, args[2:])
	case "expand-template":
		expandtemplate.ExpandTemplateProcess(ctx, args[2:])
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}
}

func runfilesDispatch(ctx context.Context, args []string) bool {
	// Check if the command is run from a Bazel runfiles context
	// with a special root symlink indicating that this binary is used
	// to push/load an image.
	rf, err := runfiles.New()
	if err != nil {
		return false
	}
	requestPath, err := rf.Rlocation("dispatch.json")
	if err != nil {
		return false
	}
	if _, err := os.Stat(requestPath); err != nil {
		return false
	}

	rawRequest, err := os.ReadFile(requestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading request file: %v\n", err)
		os.Exit(1)
	}

	// If we got here, we are in a Bazel runfiles context
	// and we have a special root symlink indicating that this binary
	// is using a json command.

	deploy.DeployDispatch(ctx, rawRequest)

	return true
}

func main() {
	ctx := context.Background()
	Run(ctx, os.Args)
}
