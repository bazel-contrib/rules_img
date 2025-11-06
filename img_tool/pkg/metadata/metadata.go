package metadata

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

// WriteLayerMetadata writes layer metadata in the format expected by LayerInfo provider.
// The format includes: name, diff_id, mediaType, digest, size, and annotations.
func WriteLayerMetadata(
	name string,
	diffID string,
	mediaType string,
	digest string,
	size int64,
	annotations map[string]string,
	outputFile io.Writer,
) error {
	// Merge and sort annotations for determinism
	mergedAnnotations := make(map[string]string)
	if annotations != nil {
		keys := make([]string, 0, len(annotations))
		for k := range annotations {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			mergedAnnotations[k] = annotations[k]
		}
	}

	metadata := api.Descriptor{
		Name:        name,
		DiffID:      diffID,
		MediaType:   mediaType,
		Digest:      digest,
		Size:        size,
		Annotations: mergedAnnotations,
	}

	encoder := json.NewEncoder(outputFile)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(metadata); err != nil {
		return fmt.Errorf("encoding metadata: %w", err)
	}
	return nil
}

// MergeAnnotations merges user annotations with layer annotations, with layer annotations taking precedence.
// Returns a new map with sorted keys for determinism.
func MergeAnnotations(userAnnotations map[string]string, layerAnnotations map[string]string) map[string]string {
	merged := make(map[string]string)

	// First add user annotations in sorted order to ensure determinism
	if userAnnotations != nil {
		keys := make([]string, 0, len(userAnnotations))
		for k := range userAnnotations {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			merged[k] = userAnnotations[k]
		}
	}

	// Then add layer annotations (these take precedence)
	for k, v := range layerAnnotations {
		merged[k] = v
	}

	return merged
}
