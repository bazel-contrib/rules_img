package deploymetadata

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/argfile"
)

var (
	pushStrategy              string
	loadStrategy              string
	mergeLayerHintsInputPaths []string
	mergeLayerHintsOutputPath string
)

func DeployMergeProcess(ctx context.Context, args []string) {
	expandedArgs, err := argfile.Expand(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing argfile: %v\n", err)
		os.Exit(1)
	}
	args = expandedArgs

	flagSet := flag.NewFlagSet("deploy-merge", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Merges multiple deploy manifests into a single unified deployment.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img deploy-merge [flags] [input1.json] [input2.json] ... [output.json]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"img deploy-merge --push-strategy=lazy --load-strategy=eager push1.json push2.json load1.json merged.json",
		}
		fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
		os.Exit(1)
	}
	flagSet.StringVar(&pushStrategy, "push-strategy", "lazy", `Push strategy to use for all push operations. One of "eager", "lazy", "cas_registry", or "bes".`)
	flagSet.StringVar(&loadStrategy, "load-strategy", "lazy", `Load strategy to use for all load operations. One of "eager", "lazy".`)
	var operations []string
	flagSet.Func("operation", `(Optional, repeatable) restrict the merged operations to a kind: "push" or "load". If omitted, all operations are kept.`, func(value string) error {
		switch value {
		case "push", "load":
			operations = append(operations, value)
			return nil
		default:
			return fmt.Errorf("invalid operation %q (want %q or %q)", value, "push", "load")
		}
	})
	flagSet.Func("layer-hints-input", `(Optional) path to layer hints file to merge. Can be specified multiple times.`, func(value string) error {
		mergeLayerHintsInputPaths = append(mergeLayerHintsInputPaths, value)
		return nil
	})
	flagSet.StringVar(&mergeLayerHintsOutputPath, "layer-hints-output", "", `(Optional) path to write merged layer hints output.`)

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}

	if flagSet.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "Error: at least one input file and one output file are required")
		flagSet.Usage()
		os.Exit(1)
	}

	inputPaths := flagSet.Args()[:flagSet.NArg()-1]
	outputPath := flagSet.Args()[flagSet.NArg()-1]

	// Validate strategies
	switch pushStrategy {
	case "eager", "lazy", "cas_registry", "bes":
		// valid strategies
	default:
		fmt.Fprintf(os.Stderr, "Error: invalid push strategy %q\n", pushStrategy)
		flagSet.Usage()
		os.Exit(1)
	}

	switch loadStrategy {
	case "eager", "lazy":
		// valid strategies
	default:
		fmt.Fprintf(os.Stderr, "Error: invalid load strategy %q\n", loadStrategy)
		flagSet.Usage()
		os.Exit(1)
	}

	// Validate layer hints flags
	if (len(mergeLayerHintsInputPaths) == 0 && mergeLayerHintsOutputPath != "") || (len(mergeLayerHintsInputPaths) > 0 && mergeLayerHintsOutputPath == "") {
		fmt.Fprintln(os.Stderr, "Error: both --layer-hints-input and --layer-hints-output must be specified together")
		flagSet.Usage()
		os.Exit(1)
	}

	// Merge layer hints if provided
	if len(mergeLayerHintsInputPaths) > 0 {
		if err := mergeLayerHintsFiles(mergeLayerHintsInputPaths, mergeLayerHintsOutputPath); err != nil {
			fmt.Fprintf(os.Stderr, "Error merging layer hints: %v\n", err)
			os.Exit(1)
		}
	}

	if err := MergeDeployManifests(ctx, inputPaths, outputPath, operations); err != nil {
		fmt.Fprintf(os.Stderr, "Error merging deploy manifests: %v\n", err)
		os.Exit(1)
	}
}

// keepOperation reports whether an operation with the given command should be
// retained given the requested operation kinds. Push, registry_tag, and referrer
// (also command "push") operations count as the "push" kind; load operations
// count as "load". Unrecognized commands are always kept so unknown operation
// types are never silently dropped.
func keepOperation(command string, kinds map[string]bool) bool {
	switch command {
	case "load":
		return kinds["load"]
	case "push", "registry_tag":
		return kinds["push"]
	default:
		return true
	}
}

// MergeDeployManifests concatenates the operations of every input manifest into a
// single manifest written to outputPath. When operations is non-empty, only the
// requested operation kinds ("push" and/or "load") are retained; an empty
// operations slice keeps every operation.
func MergeDeployManifests(ctx context.Context, inputPaths []string, outputPath string, operations []string) error {
	var kinds map[string]bool
	if len(operations) > 0 {
		kinds = make(map[string]bool, len(operations))
		for _, kind := range operations {
			kinds[kind] = true
		}
	}

	var allOperations []json.RawMessage

	// Read and merge all input deploy manifests
	for _, inputPath := range inputPaths {
		data, err := os.ReadFile(inputPath)
		if err != nil {
			return fmt.Errorf("reading input file %s: %w", inputPath, err)
		}

		var deployManifest api.DeployManifest
		if err := json.Unmarshal(data, &deployManifest); err != nil {
			return fmt.Errorf("unmarshalling deploy manifest from %s: %w", inputPath, err)
		}

		// Append the operations from this manifest, keeping only the requested
		// kinds when a filter was provided.
		for _, rawOp := range deployManifest.Operations {
			if kinds != nil {
				var head struct {
					Command string `json:"command"`
				}
				if err := json.Unmarshal(rawOp, &head); err != nil {
					return fmt.Errorf("unmarshalling operation command from %s: %w", inputPath, err)
				}
				if !keepOperation(head.Command, kinds) {
					continue
				}
			}
			allOperations = append(allOperations, rawOp)
		}
	}

	// Create merged deploy manifest with unified settings
	mergedManifest := api.DeployManifest{
		Operations: allOperations,
		Settings: api.DeploySettings{
			PushStrategy: pushStrategy,
			LoadStrategy: loadStrategy,
		},
	}

	// Marshal and write output
	output, err := json.Marshal(mergedManifest)
	if err != nil {
		return fmt.Errorf("marshalling merged deploy manifest: %w", err)
	}

	if err := os.WriteFile(outputPath, output, 0o644); err != nil {
		return fmt.Errorf("writing output file: %w", err)
	}

	return nil
}
