package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// Spec is the declarative description of the expected registry state after a
// `bazel run //:push`. It is checked into each e2e workspace root as
// registry_assertions.json and interpreted by this tool against the live,
// ephemeral registry the integration test runner started.
//
// Images are named `<repository>:<tag>` with NO registry component: the registry
// is injected at test time, so the asserter prepends the --registry host:port.
type Spec struct {
	// Images to assert about (a subset of everything pushed is fine).
	Images []ImageAssertion `json:"images"`
	// Signatures describes optional signature checks against the dedicated
	// cosign/notation registries (only exercised on signing-capable tests).
	Signatures *SignatureAssertions `json:"signatures,omitempty"`
}

// ImageAssertion describes the expected state of a single tagged image.
type ImageAssertion struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`

	// Structure of the manifest the tag resolves to.
	Structure *StructureAssertion `json:"structure,omitempty"`
	// Aliases are other tags that must resolve to the same digest as Tag.
	Aliases []string `json:"aliases,omitempty"`
	// ClosureIntact, when true, verifies that every referenced descriptor
	// (child manifests, config, layers, referrers and their closures) is
	// actually present in the registry.
	ClosureIntact bool `json:"closure_intact,omitempty"`
	// Referrers are the referring manifests expected to be attached to Tag.
	Referrers []ReferrerAssertion `json:"referrers,omitempty"`
}

// StructureAssertion describes the shape of a manifest. Empty/omitted fields
// are not checked.
type StructureAssertion struct {
	// Kind is "index" or "manifest".
	Kind string `json:"kind,omitempty"`
	// Platforms are the expected os/arch strings (e.g. "linux/amd64") of an
	// index's child manifests. Order-insensitive.
	Platforms []string `json:"platforms,omitempty"`
	// Layers is the expected number of layers of a single-arch manifest.
	Layers *int `json:"layers,omitempty"`
	// ConfigMediaType is the expected media type of the image config descriptor.
	ConfigMediaType string `json:"config_media_type,omitempty"`
	// ArtifactType is the expected top-level artifactType of the manifest.
	ArtifactType string `json:"artifact_type,omitempty"`
	// Annotations are manifest-level annotations that must be present (a subset).
	Annotations map[string]string `json:"annotations,omitempty"`
	// Labels are image config labels that must be present (a subset). Requires
	// fetching and parsing the config blob (single-arch manifests only).
	Labels map[string]string `json:"labels,omitempty"`
}

// ReferrerAssertion describes an expected referring manifest.
type ReferrerAssertion struct {
	// ArtifactType matches the referrer descriptor's artifactType.
	ArtifactType string `json:"artifact_type,omitempty"`
	// Count is the expected number of referrers with this ArtifactType.
	Count *int `json:"count,omitempty"`
	// Kind is "manifest" or "index" (the referrer's own media-type kind).
	Kind string `json:"kind,omitempty"`
	// Annotations that must be present on at least one matching referrer.
	Annotations map[string]string `json:"annotations,omitempty"`
}

// SignatureAssertions describes signature referrers on the per-signer registries.
type SignatureAssertions struct {
	Cosign   *SignatureAssertion `json:"cosign,omitempty"`
	Notation *SignatureAssertion `json:"notation,omitempty"`
}

// SignatureAssertion identifies the signed image (by repository/tag) and the
// expected signature artifact type attached to it as a referrer.
type SignatureAssertion struct {
	Repository   string `json:"repository"`
	Tag          string `json:"tag"`
	ArtifactType string `json:"artifact_type,omitempty"`
}

func loadSpec(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading spec: %w", err)
	}
	var spec Spec
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf("parsing spec %s: %w", path, err)
	}
	return &spec, nil
}
