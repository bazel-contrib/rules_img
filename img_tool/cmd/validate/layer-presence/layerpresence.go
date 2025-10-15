package layerpresence

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

var (
	layerMetadataArgs layerMetadata
	outputs           validationOutputs
)

func LayerPresenceProcess(_ context.Context, args []string) {
	flagSet := flag.NewFlagSet("layer-presence", flag.ExitOnError)
	flagSet.Usage = func() {
		_, _ = fmt.Fprintf(flagSet.Output(), "Checks the presence of required layers in an image.\nLayers are required if they have been used to deduplicate content in present layers.\n\n")
		_, _ = fmt.Fprintf(flagSet.Output(), "Usage: img validate layer-presence [--layer-metadata=layer_index=file] @[required_layers_params_file]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"img layer-presence --layer-metadata=0=base_layer_metadata.json --layer-metadata=1=top_layer_metadata.json @required_layers.params",
		}
		_, _ = fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			_, _ = fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
		os.Exit(1)
	}
	flagSet.Var(&layerMetadataArgs, "layer-metadata", `Key-value pairs of layer index number and associated layer metadata file (as produced by "img layer --metadata").`)
	flagSet.Var(&outputs, "file", `Write validation result to a file. "-" writes the output to stdout.`)

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}
	defer func() {
		for _, closer := range outputs {
			_ = closer.Close()
		}
	}()
	var writers []io.Writer
	for _, w := range outputs {
		writers = append(writers, w)
	}
	output := io.MultiWriter(writers...)
	if flagSet.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Missing positional argument for required layers parameter file")
		flagSet.Usage()
		os.Exit(1)
	}

	requiredLayers, err := parseParamFile(strings.TrimPrefix(flagSet.Arg(0), "@"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	presence := findMissingLayers(requiredLayers)
	var failed bool
	for _, layerPresence := range presence {
		if len(layerPresence.missingLayers) > 0 || len(layerPresence.misorderedLayers) > 0 {
			failed = true
			break
		}
	}
	if !failed {
		_, _ = fmt.Fprintln(output, "ok")
		os.Exit(0)
	}
	output = io.MultiWriter(output, os.Stderr)
	for _, layerPresence := range presence {
		ok := len(layerPresence.missingLayers) == 0 && len(layerPresence.misorderedLayers) == 0
		if ok {
			continue
		}
		_, _ = fmt.Fprintf(output, "layer[%d] %s:\n", layerPresence.index, layerPresence.metadata.Name)
		for _, missing := range layerPresence.missingLayers {
			_, _ = fmt.Fprintf(output, "  depends on layer %s, which is missing from the image\n", missing.Name)
		}
		for _, misordered := range layerPresence.misorderedLayers {
			_, _ = fmt.Fprintf(output, "  depends on layer %s, which is present but needs to be placed earlier in the list of layers\n", misordered.Name)
		}
	}
	os.Exit(1)
}

func findMissingLayers(requiredLayersForLayer map[int][]api.Descriptor) []layerPresenceInfo {
	indices := make([]int, 0, len(layerMetadataArgs))
	for index := range layerMetadataArgs {
		indices = append(indices, index)
	}
	slices.Sort(indices)
	providedLayers := make(map[string]api.Descriptor, len(indices))
	for _, index := range indices {
		metadata := layerMetadataArgs[index]
		providedLayers[metadata.Digest] = metadata
	}
	baseLayers := make(map[string]api.Descriptor)
	var presence []layerPresenceInfo
	for _, index := range indices {
		layerMetadata := layerMetadataArgs[index]
		baseLayers[layerMetadata.Digest] = layerMetadata
		requiredLayers, ok := requiredLayersForLayer[index]
		if !ok {
			requiredLayers = nil
		}
		var correctLayers, missingLayers, misorderedLayers []api.Descriptor
		for _, requiredLayer := range requiredLayers {
			if _, ok := baseLayers[requiredLayer.Digest]; ok {
				// layer is present and comes before current layer in order.
				// all good.
				correctLayers = append(correctLayers, requiredLayer)
				continue
			}
			if _, ok := providedLayers[requiredLayer.Digest]; ok {
				// layer is present but after the current layer.
				misorderedLayers = append(misorderedLayers, requiredLayer)
				continue
			}
			// layer doesn't exist in the image at all
			missingLayers = append(missingLayers, requiredLayer)
		}
		presence = append(presence, layerPresenceInfo{
			index:            index,
			metadata:         layerMetadata,
			correctLayers:    correctLayers,
			missingLayers:    missingLayers,
			misorderedLayers: misorderedLayers,
		})
	}
	return presence
}

func parseParamFile(requiredLayersParamPath string) (map[int][]api.Descriptor, error) {
	file, err := os.Open(requiredLayersParamPath)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "Error closing param file: %v\n", closeErr)
		}
	}()

	requiredLayersForLayer := make(map[int][]api.Descriptor)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		kv := strings.SplitN(line, "\x00", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("parsing param file %s: invalid line", requiredLayersParamPath)
		}
		index, err := strconv.Atoi(kv[0])
		if err != nil {
			return nil, fmt.Errorf("parsing param file %s: invalid index %s in line: %w", requiredLayersParamPath, kv[0], err)
		}
		rawMetadata, err := os.ReadFile(kv[1])
		if err != nil {
			return nil, fmt.Errorf("parsing param file %s: reading metadata file %s: %w", requiredLayersParamPath, kv[1], err)
		}
		var layerMeta api.Descriptor
		if err := json.Unmarshal(rawMetadata, &layerMeta); err != nil {
			return nil, fmt.Errorf("parsing param file %s: invalid layer metadata: %w", requiredLayersParamPath, err)
		}
		if _, ok := requiredLayersForLayer[index]; !ok {
			requiredLayersForLayer[index] = []api.Descriptor{}
		}
		layers := requiredLayersForLayer[index]
		layers = append(layers, layerMeta)
		requiredLayersForLayer[index] = layers
	}
	return requiredLayersForLayer, nil
}

type layerPresenceInfo struct {
	index            int
	metadata         api.Descriptor
	correctLayers    []api.Descriptor
	missingLayers    []api.Descriptor
	misorderedLayers []api.Descriptor
}
