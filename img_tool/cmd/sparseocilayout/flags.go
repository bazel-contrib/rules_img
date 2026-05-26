package sparseocilayout

import (
	"fmt"
	"strings"
)

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return fmt.Sprintf("%v", *s)
}

func (s *stringSliceFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

type layerMappingFlag []string

func (l *layerMappingFlag) String() string {
	return strings.Join(*l, ",")
}

func (l *layerMappingFlag) Set(value string) error {
	*l = append(*l, value)
	return nil
}
