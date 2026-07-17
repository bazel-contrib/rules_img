package ocilayout

import (
	"encoding/json"
	"maps"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// ManifestFilter selects which manifests of an index are included and which is
// the default (used for the Docker manifest.json). It receives the index's
// manifest descriptors and returns the indices to include plus the default.
type ManifestFilter func(manifests []ManifestDescriptor) (included []int, defaultIdx int)

// ManifestDescriptor is the subset of an index manifest descriptor a
// ManifestFilter needs.
type ManifestDescriptor struct {
	Platform *v1.Platform
	Digest   v1.Hash
	Size     int64
}

// dockerManifest is one entry of a Docker "save" manifest.json.
type dockerManifest struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

// SparseLayerDescriptor is the shape written to <hex>.descriptor.json for a
// sparse layer. Moved verbatim from cmd/sparseocilayout.layerDescriptor.
type SparseLayerDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// DescriptorsForTags generates descriptors for the given content and tags.
// Tooling that ingests OCI layouts (containerd load, docker image load,
// container load, ...) recovers image names from the root index descriptors
// via well-known annotations:
//   - "io.containerd.image.name" — the full image name (<registry>/<repository>:<tag>)
//   - "com.apple.containerization.image.name" — the full image name
//   - "org.opencontainers.image.ref.name" — per the OCI spec the tag only
//     ("latest"), but we default to the full reference because tools like
//     skopeo require a fully-qualified reference. Pass tagOnly=true for the
//     spec-compliant short form.
//
// The "org.opencontainers.image.ref.name" value need not be unique within the
// index (surprising, but allowed by the spec); consumers selecting by tag
// SHOULD pick the first match. See
// https://github.com/opencontainers/image-spec/issues/581
//
// Annotations from the referenced content are copied into the produced
// descriptors; tag annotations take precedence.
func DescriptorsForTags(ociTags []string, mediaType types.MediaType, data []byte, digest v1.Hash, artifactType string, tagOnly bool) []v1.Descriptor {
	size := int64(len(data))

	var parsed struct {
		Annotations map[string]string `json:"annotations,omitempty"`
	}
	json.Unmarshal(data, &parsed)

	if len(ociTags) == 0 {
		desc := v1.Descriptor{MediaType: mediaType, Digest: digest, Size: size, Annotations: parsed.Annotations}
		if artifactType != "" {
			desc.ArtifactType = artifactType
		}
		return []v1.Descriptor{desc}
	}

	descs := make([]v1.Descriptor, 0, len(ociTags))
	for _, repoTag := range ociTags {
		annotations := make(map[string]string)
		maps.Copy(annotations, parsed.Annotations)
		annotations[api.AnnotationContainerdImageName] = repoTag
		annotations[api.AnnotationAppleContainerizationImageName] = repoTag
		if tagOnly {
			if ref, err := name.NewTag(repoTag, name.WithDefaultTag("")); err == nil && ref.TagStr() != "" {
				annotations[api.AnnotationOCIImageRefName] = ref.TagStr()
			}
		} else {
			annotations[api.AnnotationOCIImageRefName] = repoTag
		}
		desc := v1.Descriptor{
			MediaType:   mediaType,
			Digest:      digest,
			Size:        size,
			Annotations: annotations,
		}
		if artifactType != "" {
			desc.ArtifactType = artifactType
		}
		descs = append(descs, desc)
	}
	return descs
}
