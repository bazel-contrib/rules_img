package index

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	specsv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

type manifestDescriptors []specsv1.Descriptor

func (d *manifestDescriptors) String() string {
	if d == nil {
		return ""
	}
	var sb strings.Builder
	for i, d := range *d {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(string(d.Digest))
	}
	return sb.String()
}

func (d *manifestDescriptors) Set(value string) error {
	rawDescriptor, err := os.ReadFile(value)
	if err != nil {
		return err
	}
	var descriptor specsv1.Descriptor
	if err := json.Unmarshal(rawDescriptor, &descriptor); err != nil {
		return err
	}
	*d = append(*d, descriptor)
	return nil
}

type annotations map[string]string

func (a *annotations) String() string {
	if a == nil {
		return ""
	}
	var sb strings.Builder
	keys := slices.Collect(maps.Keys(*a))
	slices.Sort(keys)
	for i, key := range keys {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(key)
		sb.WriteString("=")
		sb.WriteString((*a)[key])
	}
	return sb.String()
}

func (a *annotations) Set(value string) error {
	if *a == nil {
		*a = make(annotations)
	}
	kv := strings.SplitN(value, "=", 2)
	if len(kv) != 2 {
		return fmt.Errorf("expected annoation as key-value pair separated by equals, but got %s", kv)
	}
	(*a)[kv[0]] = kv[1]
	return nil
}

// sociEntries collects repeated --soci-entry
// <soci-index-descriptor>=<image-manifest-descriptor> flags. Each becomes an
// extra OCI image index entry pointing at a SOCI index, annotated with the
// digest of the image manifest it belongs to (SOCI v2 discovery).
type sociEntries []specsv1.Descriptor

func (s *sociEntries) String() string {
	if s == nil {
		return ""
	}
	parts := make([]string, 0, len(*s))
	for _, d := range *s {
		parts = append(parts, string(d.Digest))
	}
	return strings.Join(parts, ", ")
}

func (s *sociEntries) Set(value string) error {
	sociPath, manifestPath, ok := strings.Cut(value, "=")
	if !ok || sociPath == "" || manifestPath == "" {
		return fmt.Errorf("expected --soci-entry as <soci-descriptor>=<image-manifest-descriptor>, but got %q", value)
	}

	sociDesc, err := readDescriptorFile(sociPath)
	if err != nil {
		return fmt.Errorf("reading SOCI index descriptor %s: %w", sociPath, err)
	}
	manifestDesc, err := readDescriptorFile(manifestPath)
	if err != nil {
		return fmt.Errorf("reading image manifest descriptor %s: %w", manifestPath, err)
	}
	if manifestDesc.Digest == "" {
		return fmt.Errorf("image manifest descriptor %s has no digest", manifestPath)
	}

	if sociDesc.Annotations == nil {
		sociDesc.Annotations = make(map[string]string)
	}
	sociDesc.Annotations[api.SociImageManifestDigestAnnotation] = manifestDesc.Digest.String()
	*s = append(*s, sociDesc)
	return nil
}

func readDescriptorFile(path string) (specsv1.Descriptor, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return specsv1.Descriptor{}, err
	}
	var desc specsv1.Descriptor
	if err := json.Unmarshal(data, &desc); err != nil {
		return specsv1.Descriptor{}, err
	}
	return desc, nil
}
