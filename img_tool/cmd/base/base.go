// Package base implements the `img base` command family, which describes the
// contents of a container base image without building one.
//
// Each verb writes a single base metadata stream (see pkg/basemeta): a
// compressed, length-delimited sequence of tar entry descriptions. Bazel rules
// pass those streams up the dependency graph via BaseImageContentInfo, and the
// base_image_layer rule finally merges them into one flat layer with
// `img layer --base-metadata`.
//
// Subcommands:
//
//	base etc environment    describes /etc/environment
//	base etc hosts          describes /etc/hosts
//	base etc release        describes /etc/os-release and /etc/lsb-release
//	base etc passwd         describes /etc/passwd, /etc/group, /etc/shadow and home directories
//	base trust-store        describes a CA certificate trust store
//	base system-libraries   describes shared libraries and the dynamic loader configuration
//	base skeleton           describes an empty Linux directory skeleton
package base

import (
	"context"
	"fmt"
	"os"
)

const usage = `Usage: img base <subcommand> [args...]

Subcommands:
  etc               describes files under /etc (subcommands: environment, hosts, release, passwd)
  trust-store       describes a CA certificate trust store
  system-libraries  describes shared libraries and the dynamic loader configuration
  skeleton          describes an empty Linux directory skeleton`

// BaseProcess dispatches to a base subcommand.
func BaseProcess(ctx context.Context, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}

	subcommand, rest := args[0], args[1:]
	switch subcommand {
	case "etc":
		etcProcess(ctx, rest)
	case "trust-store":
		trustStoreProcess(ctx, rest)
	case "system-libraries":
		systemLibrariesProcess(ctx, rest)
	case "skeleton":
		skeletonProcess(ctx, rest)
	default:
		fmt.Fprintf(os.Stderr, "Unknown base subcommand %q\n\n", subcommand)
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}
}

const etcUsage = `Usage: img base etc <subcommand> [args...]

Subcommands:
  environment  describes /etc/environment
  hosts        describes /etc/hosts
  release      describes /etc/os-release and /etc/lsb-release
  passwd       describes /etc/passwd, /etc/group, /etc/shadow and home directories`

func etcProcess(ctx context.Context, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, etcUsage)
		os.Exit(1)
	}

	subcommand, rest := args[0], args[1:]
	switch subcommand {
	case "environment":
		environmentProcess(ctx, rest)
	case "hosts":
		hostsProcess(ctx, rest)
	case "release":
		releaseProcess(ctx, rest)
	case "passwd":
		passwdProcess(ctx, rest)
	default:
		fmt.Fprintf(os.Stderr, "Unknown base etc subcommand %q\n\n", subcommand)
		fmt.Fprintln(os.Stderr, etcUsage)
		os.Exit(1)
	}
}
