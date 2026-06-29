package metadata

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"

	specv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

// WriteLayerMetadata writes layer metadata in the format expected by SingleLayerInfo provider.
// The format includes: name, diff_id, mediaType, digest, size, annotations, and history.
//
// When history is empty, a synthetic single entry {created_by: name} is written so every
// layer carries at least a created_by marker.
func WriteLayerMetadata(
	name string,
	diffID string,
	mediaType string,
	digest string,
	size int64,
	annotations map[string]string,
	history []api.History,
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

	if len(history) == 0 {
		history = []api.History{{CreatedBy: name}}
	}

	metadata := api.Descriptor{
		Name:        name,
		DiffID:      diffID,
		MediaType:   mediaType,
		Digest:      digest,
		Size:        size,
		Annotations: mergedAnnotations,
		History:     history,
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

// SplitHistoryPerLayer distributes an OCI image config's history entries across
// the image's layers, mirroring the layout used by the image_import rule.
//
// Empty-layer history entries (metadata-only operations such as ENV/CMD) are
// bundled with the next non-empty layer; any trailing empty-layer entries are
// attached to the last non-empty layer so no history is lost. If the config
// history contains no non-empty entries at all, they are attached to the first
// layer (when one exists). The returned slice has one element per layer (index i
// corresponds to layer i); layers that receive no history get a nil slice, which
// callers may replace with a fallback (see WriteLayerMetadata).
func SplitHistoryPerLayer(history []specv1.History, numLayers int) [][]api.History {
	perLayer := make([][]api.History, numLayers)
	if numLayers == 0 {
		return perLayer
	}

	var current []api.History
	layerIndex := 0
	for _, entry := range history {
		current = append(current, historyFromOCI(entry))
		if !entry.EmptyLayer {
			if layerIndex < numLayers {
				perLayer[layerIndex] = current
				layerIndex++
			}
			current = nil
		}
	}
	if len(current) > 0 {
		if layerIndex > 0 {
			perLayer[layerIndex-1] = append(perLayer[layerIndex-1], current...)
		} else {
			perLayer[0] = current
		}
	}
	return perLayer
}

func historyFromOCI(entry specv1.History) api.History {
	return api.History{
		Created:    entry.Created,
		CreatedBy:  entry.CreatedBy,
		Author:     entry.Author,
		Comment:    entry.Comment,
		EmptyLayer: entry.EmptyLayer,
	}
}
