package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// The Bazel target info of a deploy: what `img deploy` hands a protocol 2 plugin
// as deploy.build_info_file when the signing_config sets build_info. It is
// written by the img_build_info_aspect and `img build-info`; see the "bazel-v1"
// definitions for how it is recorded.

// --bazel-info values: which of the targets below the deploy rule are recorded
// as resolved dependencies.
const (
	bazelInfoAll   = "all"   // pulled base images, images and layers
	bazelInfoBases = "bases" // pulled base images only
	bazelInfoNone  = "none"  // only the label of the deploy rule, as a target
)

type bazelTarget struct {
	Label         string `json:"label"`
	Configuration string `json:"configuration,omitempty"`
}

type bazelInfoImage struct {
	bazelTarget
	Kind     string `json:"kind"`
	Digest   string `json:"digest"`
	Platform *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	} `json:"platform,omitempty"`
	Base      string   `json:"base,omitempty"`
	Layers    []string `json:"layers,omitempty"`
	Manifests []string `json:"manifests,omitempty"`
}

type bazelInfoLayer struct {
	bazelTarget
	Digest string `json:"digest"`
}

type bazelInfoBase struct {
	Label      string   `json:"label"`
	Registries []string `json:"registries,omitempty"`
	Repository string   `json:"repository,omitempty"`
	Tag        string   `json:"tag,omitempty"`
	Digest     string   `json:"digest,omitempty"`
}

type bazelInfoFile struct {
	SchemaVersion int              `json:"schema_version"`
	Deploy        bazelTarget      `json:"deploy"`
	Images        []bazelInfoImage `json:"images"`
	Layers        []bazelInfoLayer `json:"layers"`
	BaseImages    []bazelInfoBase  `json:"base_images"`
}

func readBazelInfo(path string) (*bazelInfoFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var info bazelInfoFile
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if info.SchemaVersion != 1 {
		return nil, fmt.Errorf("%s: unsupported schema_version %d", path, info.SchemaVersion)
	}
	return &info, nil
}

// dependenciesOf lists the targets the image with the given digest is made of: the
// image (or the images of an index) and their layers, and the pulled images they
// are based on, following base images down. detail is a --bazel-info value.
func (i *bazelInfoFile) dependenciesOf(digest, detail string) []bazelResource {
	if detail == bazelInfoNone {
		return nil
	}
	type key struct{ label, configuration, digest string }
	seen := map[key]bool{}
	var images []bazelInfoImage
	// layerOrder lists the layer digests in the order the image stacks them.
	var layerOrder []string
	layerSeen := map[string]bool{}
	var baseLabels = map[string]bool{}

	var visit func(img bazelInfoImage)
	visitDigest := func(d string) {
		for _, img := range i.Images {
			if img.Digest == d {
				visit(img)
			}
		}
	}
	visit = func(img bazelInfoImage) {
		k := key{img.Label, img.Configuration, img.Digest}
		if seen[k] {
			return
		}
		seen[k] = true
		images = append(images, img)
		for _, l := range img.Layers {
			if !layerSeen[l] {
				layerSeen[l] = true
				layerOrder = append(layerOrder, l)
			}
		}
		for _, m := range img.Manifests {
			visitDigest(m)
		}
		if img.Base != "" {
			baseLabels[img.Base] = true
			for _, other := range i.Images {
				if other.Label == img.Base {
					visit(other)
				}
			}
		}
	}
	visitDigest(digest)

	var out []bazelResource
	for _, b := range i.BaseImages {
		if !baseLabels[b.Label] {
			continue
		}
		r, ok := b.resource()
		if ok {
			out = append(out, r)
		}
	}
	if detail != bazelInfoAll {
		return out
	}
	for _, img := range images {
		annotations := map[string]any{"kind": img.Kind, "configuration": img.Configuration}
		if p := img.Platform; p != nil && p.OS != "" && p.Architecture != "" {
			annotations["platform"] = formatPlatform(normalizePlatform(v1.Platform{OS: p.OS, Architecture: p.Architecture, Variant: p.Variant}))
		}
		out = append(out, bazelResource{
			Name:        img.Label,
			Digest:      digestMap(img.Digest),
			Annotations: annotations,
		})
	}
	// Layers in the order the image uses them, bottom first. A digest without a
	// layer target (a pulled layer) is not listed.
	for _, d := range layerOrder {
		for _, l := range i.Layers {
			if l.Digest != d {
				continue
			}
			out = append(out, bazelResource{
				Name:        l.Label,
				Digest:      digestMap(l.Digest),
				Annotations: map[string]any{"kind": "layer", "configuration": l.Configuration},
			})
		}
	}
	return out
}

// resource renders a pulled image like a --material: a package URL with the
// digest as a separate field.
func (b bazelInfoBase) resource() (bazelResource, bool) {
	if b.Digest == "" || len(b.Registries) == 0 || b.Repository == "" {
		return bazelResource{}, false
	}
	ref := b.Registries[0] + "/" + b.Repository
	if b.Tag != "" {
		ref += ":" + b.Tag
	} else {
		ref += "@" + b.Digest
	}
	purl, err := refToPURL(ref, nil)
	if err != nil {
		return bazelResource{}, false
	}
	annotations := map[string]any{"kind": "base-image", "registry": b.Registries[0], "repository": b.Repository}
	if b.Tag != "" {
		annotations["tag"] = b.Tag
	}
	return bazelResource{Name: b.Label, URI: purl, Digest: digestMap(b.Digest), Annotations: annotations}, true
}

// digestMap turns "sha256:<hex>" into {"sha256": "<hex>"}.
func digestMap(d string) map[string]string {
	alg, hex, ok := strings.Cut(d, ":")
	if !ok {
		return nil
	}
	return map[string]string{alg: hex}
}
