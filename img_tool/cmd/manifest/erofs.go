package manifest

import (
	"slices"

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
