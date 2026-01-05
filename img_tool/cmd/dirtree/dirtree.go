package dirtree

import (
	"context"
	"flag"
	"fmt"
	"log"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/persistentworker"
)

// DirtreeProcess is the entry point for the dirtree subcommand.
func DirtreeProcess(ctx context.Context, args []string) {
	if err := dirtreeCmd(args); err != nil {
		log.Fatal(err)
	}
}

func dirtreeCmd(args []string) error {
	processedArgs, isPersistentWorker, err := persistentworker.ParseArgs(args)
	if err != nil {
		return err
	}

	if isPersistentWorker {
		return persistentWorker(processedArgs)
	}
	return oneShot(processedArgs)
}

func persistentWorker(args []string) error {
	// Parse persistent worker flags
	flags := flag.NewFlagSet("persistent", flag.ContinueOnError)

	if err := flags.Parse(args); err != nil {
		return err
	}

	builder := newPersistentDirtreeBuilder()
	worker := persistentworker.NewWorker(builder)
	return worker.Run()
}

func oneShot(args []string) error {
	// Parse dirtree request arguments
	req, err := parseDirtreeRequest(args)
	if err != nil {
		return err
	}

	// Build the directory tree (no sandboxDir in one-shot mode)
	if err := buildDirtree(req, ""); err != nil {
		return err
	}

	return nil
}

// dirtreeRequest holds parsed dirtree request parameters.
type dirtreeRequest struct {
	inputsFile     string
	digestOutput   string
	protoOutputDir string
	digestFunction string
}

// parseDirtreeRequest parses dirtree request arguments using a flag set.
func parseDirtreeRequest(args []string) (*dirtreeRequest, error) {
	flags := flag.NewFlagSet("dirtree", flag.ContinueOnError)
	inputsFile := flags.String("inputs", "", "File containing input paths (path\\0type+file format)")
	digestOutput := flags.String("digest-output", "", "File to write the root directory digest")
	protoOutputDir := flags.String("proto-output-dir", "", "Directory to write proto messages (content-addressed)")
	digestFunction := flags.String("digest-function", "sha256", "Hash function to use (sha1, sha256, sha384, sha512, blake3)")

	if err := flags.Parse(args); err != nil {
		return nil, err
	}

	if *inputsFile == "" {
		return nil, fmt.Errorf("--inputs is required")
	}
	if *digestOutput == "" {
		return nil, fmt.Errorf("--digest-output is required")
	}
	if *protoOutputDir == "" {
		return nil, fmt.Errorf("--proto-output-dir is required")
	}

	// Normalize digest function name
	normalized, err := normalizeDigestFunction(*digestFunction)
	if err != nil {
		return nil, err
	}

	return &dirtreeRequest{
		inputsFile:     *inputsFile,
		digestOutput:   *digestOutput,
		protoOutputDir: *protoOutputDir,
		digestFunction: normalized,
	}, nil
}

// normalizeDigestFunction normalizes the digest function name to lowercase canonical form
func normalizeDigestFunction(name string) (string, error) {
	switch name {
	case "SHA-1", "SHA1", "sha1":
		return "sha1", nil
	case "SHA-256", "SHA256", "sha256":
		return "sha256", nil
	case "SHA-384", "SHA384", "sha384":
		return "sha384", nil
	case "SHA-512", "SHA512", "sha512":
		return "sha512", nil
	case "BLAKE3", "blake3":
		return "blake3", nil
	default:
		return "", fmt.Errorf("unsupported digest function: %s (supported: sha1, sha256, sha384, sha512, blake3)", name)
	}
}
