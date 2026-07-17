package ocilayoutcmd

import (
	"fmt"
	"strings"
)

// stringSliceFlag is a custom flag type for string slices
type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return fmt.Sprintf("%v", *s)
}

func (s *stringSliceFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

// layerMapping represents a metadata file to blob file mapping
type layerMapping struct {
	metadata string
	blob     string
}

// layerMappingFlag is a custom flag type for layer mappings
type layerMappingFlag []layerMapping

func (l *layerMappingFlag) String() string {
	var parts []string
	for _, m := range *l {
		parts = append(parts, fmt.Sprintf("%s=%s", m.metadata, m.blob))
	}
	return strings.Join(parts, ",")
}

func (l *layerMappingFlag) Set(value string) error {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid layer format, expected metadata=blob, got %s", value)
	}
	*l = append(*l, layerMapping{metadata: parts[0], blob: parts[1]})
	return nil
}
