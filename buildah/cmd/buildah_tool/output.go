package main

import (
	"encoding/json"
	"fmt"
	"os"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func writeOutputs(manifestBytes []byte, blobFiles, localBlobs map[string]string, baseLayerList []layerMetadata, baseLayers map[string]layerMetadata) error {
	var pushMf v1.Manifest
	if err := json.Unmarshal(manifestBytes, &pushMf); err != nil {
		return fmt.Errorf("parsing pushed manifest: %w", err)
	}

	baseLayerCount := len(baseLayerList)
	if len(pushMf.Layers) != baseLayerCount+len(flagOutputLayerBlobs) {
		return fmt.Errorf("expected %d + %d layers in manifest but got %d, set layer_count = %d",
			baseLayerCount, len(flagOutputLayerBlobs), len(pushMf.Layers), len(pushMf.Layers)-baseLayerCount)
	}
	if len(flagOutputLayerMetadata) != len(flagOutputLayerBlobs) {
		return fmt.Errorf("expected %d --output-layer-metadata paths but got %d", len(flagOutputLayerBlobs), len(flagOutputLayerMetadata))
	}

	for i, base := range baseLayerList {
		if base.Digest == "" {
			continue
		}
		if got := pushMf.Layers[i].Digest; got != base.Digest {
			return fmt.Errorf("manifest base layer digest mismatch | index: %d manifest: %s base: %s", i, got, base.Digest)
		}
	}

	configDgst := pushMf.Config.Digest.String()
	configFile, ok := blobFiles[configDgst]
	if !ok {
		return fmt.Errorf("config blob %s not found", pushMf.Config.Digest)
	}
	configData, err := os.ReadFile(configFile)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	var cfg v1.Image
	if err := json.Unmarshal(configData, &cfg); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}
	if err := os.Rename(configFile, *flagOutputConfig); err != nil {
		return fmt.Errorf("moving config: %w", err)
	}

	uploadCurrentPath := map[string]string{}
	perLayerHistory := splitHistoryPerLayer(cfg.History, len(cfg.RootFS.DiffIDs))

	newLayerDescs := pushMf.Layers[baseLayerCount:]
	for i, desc := range newLayerDescs {
		dgst := desc.Digest.String()
		diffIdx := baseLayerCount + i
		if diffIdx >= len(cfg.RootFS.DiffIDs) {
			return fmt.Errorf("diff idx out of range: %d", diffIdx)
		}
		diffID := cfg.RootFS.DiffIDs[diffIdx].String()
		dst := flagOutputLayerBlobs[i]

		if src, ok := blobFiles[dgst]; ok {
			if err := moveOrCopy(src, dst, uploadCurrentPath); err != nil {
				return fmt.Errorf("placing layer blob %d: %w", i, err)
			}
		} else if base, ok := baseLayers[dgst]; ok {
			baseBlob, ok := localBlobs[base.Digest.String()]
			if !ok {
				return fmt.Errorf("layer %d dedups to base layer %s but its blob is not available locally (pulled-base dedup is not supported)", i, base.Digest)
			}
			if err := copyFile(baseBlob, dst); err != nil {
				return fmt.Errorf("copying base-shared layer blob %d: %w", i, err)
			}
		} else {
			return fmt.Errorf("new layer blob %s (diff_id %s) not in uploads or known base layers", dgst, diffID)
		}

		if err := writeJSON(flagOutputLayerMetadata[i], layerMetadata{
			DiffID:      cfg.RootFS.DiffIDs[diffIdx],
			MediaType:   desc.MediaType,
			Digest:      desc.Digest,
			Size:        desc.Size,
			Annotations: desc.Annotations,
			History:     perLayerHistory[diffIdx],
		}); err != nil {
			return err
		}
	}

	if err := os.WriteFile(*flagOutputManifest, manifestBytes, 0o644); err != nil {
		return fmt.Errorf("writing manifest: %w", err)
	}

	manifestDgst := digest.FromBytes(manifestBytes)
	descriptor := v1.Descriptor{
		MediaType: v1.MediaTypeImageManifest,
		Digest:    manifestDgst,
		Size:      int64(len(manifestBytes)),
		Platform: &v1.Platform{
			Architecture: *flagArchitecture,
			OS:           *flagOperatingSystem,
		},
	}
	if err := writeJSON(*flagOutputDescriptor, descriptor); err != nil {
		return err
	}
	if err := os.WriteFile(*flagOutputDigest, []byte(manifestDgst.String()), 0o644); err != nil {
		return fmt.Errorf("writing digest: %w", err)
	}
	return nil
}

func splitHistoryPerLayer(history []v1.History, numLayers int) [][]v1.History {
	perLayer := make([][]v1.History, numLayers)
	if numLayers == 0 {
		return perLayer
	}
	var current []v1.History
	layerIndex := 0
	for _, entry := range history {
		current = append(current, entry)
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

func moveOrCopy(src, dst string, currentPath map[string]string) error {
	if cur, moved := currentPath[src]; moved {
		return copyFile(cur, dst)
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", src, dst, err)
	}
	currentPath[src] = dst
	return nil
}

func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshalling %s: %w", path, err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
