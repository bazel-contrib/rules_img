package manifest

import (
	"fmt"
	"slices"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

// hasErofsLayer reports whether any layer's blob is an EROFS filesystem image.
func hasErofsLayer(layers []api.Descriptor) bool {
	for _, layer := range layers {
		if api.IsErofsLayer(layer.MediaType) {
			return true
		}
	}
	return false
}

// addOSFeature adds a feature to an os.features list, deduplicated and sorted so
// the config is byte-stable.
func addOSFeature(features []string, feature string) []string {
	if slices.Contains(features, feature) {
		return features
	}
	out := append(slices.Clone(features), feature)
	slices.Sort(out)
	return out
}

// checkSingleLayerFamily rejects a manifest that mixes tar and EROFS layers.
//
// The EROFS layer spec asks a manifest to stay within one layer media type
// family, and mixing is not just untidy: a runtime applies a tar layer by
// extracting it and an EROFS layer by mounting it, so a stack containing both
// cannot be composed. The common way to get here is pulling a tar base image and
// building EROFS layers on top, which is why the error says how to fix that.
func checkSingleLayerFamily(layers []api.Descriptor) error {
	var erofsLayers, tarLayers []string
	for _, layer := range layers {
		switch {
		case api.IsErofsLayer(layer.MediaType):
			erofsLayers = append(erofsLayers, layer.Digest)
		case isTarLayer(layer.MediaType):
			tarLayers = append(tarLayers, layer.Digest)
		}
	}
	if len(erofsLayers) == 0 || len(tarLayers) == 0 {
		return nil
	}
	return fmt.Errorf("manifest mixes tar and EROFS layers, which cannot be composed into one filesystem:\n"+
		"  tar layers:   %s\n"+
		"  EROFS layers: %s\n"+
		"Convert the tar layers with layer_from_tar(format = \"erofs\"), or build the whole image as tar layers",
		strings.Join(tarLayers, " "), strings.Join(erofsLayers, " "))
}

// isTarLayer reports whether a layer media type belongs to the tar family. Every
// standard OCI and Docker tar layer type carries a "tar" token; an empty layer
// or a non-tar artifact layer does not.
func isTarLayer(mediaType string) bool {
	return strings.Contains(mediaType, "tar")
}
