package metadata

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"

	specv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/blake3sum"
)

// WriteLayerMetadata writes layer metadata in the format expected by SingleLayerInfo provider.
// The format includes: diff_id, mediaType, digest, size, annotations, and history.
//
// A nil history means "no history was supplied", so a synthetic single entry
// {created_by: "history missing"} is written to keep every layer's created_by
// marker non-empty. Callers that want a layer with no history entries at all
// (as opposed to a missing one) pass a non-nil empty slice, which is written
// through verbatim.
func WriteLayerMetadata(
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

	if history == nil {
		history = api.LayerHistory("")
	}

	metadata := api.Descriptor{
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

// ValidateCASDigest checks the value of a --cas-digest flag: the digest function
// whose digest of the layer blob is recorded in its metadata (see
// [api.Descriptor.CASDigests]), or "" for none.
func ValidateCASDigest(function string) error {
	switch function {
	case "", api.DigestBLAKE3:
		return nil
	}
	return fmt.Errorf("unsupported --cas-digest %q: only %q is supported", function, api.DigestBLAKE3)
}

// RecordCASDigest adds the digest of the layer blob at blobPath under the given
// digest function to the layer metadata file at metadataPath, next to the SHA256
// it already holds. A Bazel remote cache that does not use SHA256 addresses the
// blob by that digest, so a lazy push needs to know it without having the blob.
// An empty function does nothing.
//
// The metadata must describe the blob: a size that differs means the two files
// do not belong together, and recording a digest for the wrong bytes would only
// show up much later as a layer that cannot be found.
func RecordCASDigest(function, blobPath, metadataPath string) error {
	if function == "" {
		return nil
	}
	if err := ValidateCASDigest(function); err != nil {
		return err
	}
	digest, size, err := blake3sum.File(blobPath)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return fmt.Errorf("reading layer metadata: %w", err)
	}
	var descriptor api.Descriptor
	if err := json.Unmarshal(data, &descriptor); err != nil {
		return fmt.Errorf("decoding layer metadata %s: %w", metadataPath, err)
	}
	if descriptor.Size != size {
		return fmt.Errorf("layer metadata %s describes a blob of %d bytes, but %s has %d", metadataPath, descriptor.Size, blobPath, size)
	}
	if descriptor.CASDigests == nil {
		descriptor.CASDigests = map[string]string{}
	}
	descriptor.CASDigests[function] = fmt.Sprintf("%x", digest)
	return writeDescriptor(metadataPath, descriptor)
}

func writeDescriptor(path string, descriptor api.Descriptor) (retErr error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("rewriting layer metadata: %w", err)
	}
	defer func() {
		if err := f.Close(); retErr == nil && err != nil {
			retErr = fmt.Errorf("closing layer metadata: %w", err)
		}
	}()
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(descriptor); err != nil {
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
