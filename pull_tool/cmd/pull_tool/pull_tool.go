package main

import (
	"context"
	"fmt"
	"os"

	"github.com/bazel-contrib/rules_img/pull_tool/cmd/downloadblob"
	"github.com/bazel-contrib/rules_img/pull_tool/cmd/internal/pull"
)

const usage = `Usage: pull_tool [COMMAND] [ARGS...]

Commands:
  pull             pulls an image from a registry
  download-blob    downloads a single blob from a registry
  mkdir [DIR]      creates a directory structure (including parents) `

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
	case "mkdir":
		if len(args) != 3 {
			fmt.Fprintln(os.Stderr, "Usage: pull_tool mkdir [DIR]")
			os.Exit(1)
		}
		dir := args[2]
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to create directory %s: %v\n", dir, err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}
}

func main() {
	ctx := context.Background()
	Run(ctx, os.Args)
}
