package main

import (
	"context"
	"fmt"
	"os"

	"github.com/bazel-contrib/rules_img/pull_tool/cmd/downloadblob"
	"github.com/bazel-contrib/rules_img/pull_tool/cmd/downloadmanifest"
	"github.com/bazel-contrib/rules_img/pull_tool/cmd/internal/pull"
)

const usage = `Usage: pull_tool [COMMAND] [ARGS...]

Commands:
  pull                pulls an image from a registry
  download-blob       downloads a single blob from a registry
  download-manifest   downloads a manifest by digest or tag from a registry`

func Run(ctx context.Context, args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}

	command := args[1]
	switch command {
	case "pull":
		pull.PullProcess(ctx, args[2:])
	case "download-blob":
		downloadblob.DownloadBlobProcess(ctx, args[2:])
	case "download-manifest":
		downloadmanifest.DownloadManifestProcess(ctx, args[2:])
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}
}

func main() {
	ctx := context.Background()
	Run(ctx, os.Args)
}
