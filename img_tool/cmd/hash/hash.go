package hash

import (
	"context"
	"flag"
	"fmt"
	"log"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/persistentworker"
)

// HashProcess is the entry point for the hash subcommand.
func HashProcess(ctx context.Context, args []string) {
	if err := hashCmd(args); err != nil {
		log.Fatal(err)
	}
}

func hashCmd(args []string) error {
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
	cheatMode := flags.Bool("cheat-mode", false, "Enable cheat mode to extract hash from Bazel's input digest")

	if err := flags.Parse(args); err != nil {
		return err
	}

	hasher := newPersistentHasher(*cheatMode)
	worker := persistentworker.NewWorker(hasher)
	return worker.Run()
}

func oneShot(args []string) error {
	// Parse hash request arguments
	req, err := parseHashRequest(args)
	if err != nil {
		return err
	}

	// Compute hash and optional layer metadata (no sandboxDir in one-shot mode)
	var hashBytes []byte
	var layerMeta *layerMetadata

	if req.layerMeta {
		// Compute both hashes in a single pass for layer metadata
		hashBytes, layerMeta, err = computeLayerHashes(req.input, req.digest, "")
		if err != nil {
			return err
		}
	} else {
		// Just compute the hash
		hashBytes, err = computeHash(req.input, req.digest, "")
		if err != nil {
			return err
		}
	}

	// Write output (no sandboxDir in one-shot mode)
	return writeHashOutput(hashBytes, req, "", layerMeta)
}

// hashRequest holds parsed hash request parameters.
type hashRequest struct {
	digest      string
	encoding    string
	input       string
	output      string
	layerMeta   bool
	name        string
	annotations map[string]string
}

// annotationsFlag implements flag.Value for key-value pairs
type annotationsFlag map[string]string

func (a annotationsFlag) String() string {
	return ""
}

func (a annotationsFlag) Set(value string) error {
	parts := flag.CommandLine.Args()
	_ = parts // Suppress unused warning

	// Parse key=value format
	idx := 0
	for i, c := range value {
		if c == '=' {
			idx = i
			break
		}
	}
	if idx == 0 {
		return fmt.Errorf("annotation must be in format key=value, got: %s", value)
	}
	key := value[:idx]
	val := value[idx+1:]
	if key == "" {
		return fmt.Errorf("annotation key cannot be empty")
	}
	a[key] = val
	return nil
}

// parseHashRequest parses hash request arguments using a flag set.
func parseHashRequest(args []string) (*hashRequest, error) {
	flags := flag.NewFlagSet("hash", flag.ContinueOnError)
	digest := flags.String("digest", "sha256", "Hash algorithm (sha256 or sha512)")
	encoding := flags.String("encoding", "raw", "Output encoding (raw, hex, sri, oci-digest, layer-metadata)")
	name := flags.String("name", "", "Layer name (only used with layer-metadata encoding)")
	annotations := make(annotationsFlag)
	flags.Var(&annotations, "annotation", "Add an annotation as key=value (only used with layer-metadata encoding)")

	if err := flags.Parse(args); err != nil {
		return nil, err
	}

	// Validate digest algorithm
	if *digest != "sha256" && *digest != "sha512" {
		return nil, fmt.Errorf("invalid digest algorithm: %s (must be sha256 or sha512)", *digest)
	}

	// Check if layer-metadata encoding is requested
	layerMeta := *encoding == "layer-metadata"

	// Validate encoding
	if !layerMeta && *encoding != "raw" && *encoding != "hex" && *encoding != "sri" && *encoding != "oci-digest" {
		return nil, fmt.Errorf("invalid encoding: %s (must be raw, hex, sri, oci-digest, or layer-metadata)", *encoding)
	}

	// Get positional arguments
	positionalArgs := flags.Args()
	if len(positionalArgs) < 2 {
		return nil, fmt.Errorf("expected 2 positional arguments (input, output), got %d", len(positionalArgs))
	}

	return &hashRequest{
		digest:      *digest,
		encoding:    *encoding,
		input:       positionalArgs[0],
		output:      positionalArgs[1],
		layerMeta:   layerMeta,
		name:        *name,
		annotations: annotations,
	}, nil
}
