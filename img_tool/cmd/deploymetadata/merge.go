package deploymetadata

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

var (
	pushStrategy string
	loadStrategy string
)

func DeployMergeProcess(ctx context.Context, args []string) {
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

	if err := MergeDeployManifests(ctx, inputPaths, outputPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error merging deploy manifests: %v\n", err)
		os.Exit(1)
	}
}

func MergeDeployManifests(ctx context.Context, inputPaths []string, outputPath string) error {
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

		// Append all operations from this manifest
		allOperations = append(allOperations, deployManifest.Operations...)
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
