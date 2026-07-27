package soci

import (
	"fmt"
	"strings"
)

// layerZtocPair associates an image layer's metadata file with the ztoc blob
// generated for that layer.
type layerZtocPair struct {
	metadataPath string
	ztocPath     string
}

// layerZtocPairs collects repeated --layer <layer-metadata.json>=<ztoc-path>
// flags, preserving order (the SOCI index lists ztocs in image-layer order).
type layerZtocPairs []layerZtocPair

func (p *layerZtocPairs) String() string {
	if p == nil {
		return ""
	}
	parts := make([]string, 0, len(*p))
	for _, pair := range *p {
		parts = append(parts, fmt.Sprintf("%s=%s", pair.metadataPath, pair.ztocPath))
	}
	return strings.Join(parts, ", ")
}

func (p *layerZtocPairs) Set(value string) error {
	metadata, ztoc, ok := strings.Cut(value, "=")
	if !ok {
		return fmt.Errorf("expected --layer as <layer-metadata>=<ztoc-path>, but got %q", value)
	}
	if metadata == "" || ztoc == "" {
		return fmt.Errorf("expected --layer as <layer-metadata>=<ztoc-path> with both parts set, but got %q", value)
	}
	*p = append(*p, layerZtocPair{metadataPath: metadata, ztocPath: ztoc})
	return nil
}
