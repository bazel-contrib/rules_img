package api

import (
	"bytes"
	"encoding/json"
	"fmt"
)

type DeployManifest struct {
	Operations []json.RawMessage `json:"operations"`
	Settings   DeploySettings    `json:"settings"`
}

func (dm *DeployManifest) BaseOperations() ([]BaseCommandOperation, error) {
	var ops []BaseCommandOperation
	// for each raw operation, unmarshal into BaseCommandOperation to get the command type
	for _, rawOp := range dm.Operations {
		var baseOp BaseCommandOperation
		if err := json.Unmarshal(rawOp, &baseOp); err != nil {
			return nil, err
		}
		ops = append(ops, baseOp)
	}
	return ops, nil
}

func (dm *DeployManifest) PushOperations() ([]IndexedPushDeployOperation, error) {
	var ops []IndexedPushDeployOperation
	// for each raw operation, check if the command is "push" and unmarshal accordingly
	for i, rawOp := range dm.Operations {
		var baseOp BaseCommandOperation
		if err := json.Unmarshal(rawOp, &baseOp); err != nil {
			return nil, err
		}
		if baseOp.Command != "push" {
			continue
		}
		var pushOp PushDeployOperation
		decoder := json.NewDecoder(bytes.NewReader(rawOp))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&pushOp); err != nil {
			return nil, err
		}
		ops = append(ops, IndexedPushDeployOperation{
			I:                   i,
			Strategy:            dm.Settings.PushStrategy,
			PushDeployOperation: pushOp,
		})
	}
	return ops, nil
}

func (dm *DeployManifest) RegistryTagOperations() ([]IndexedRegistryTagDeployOperation, error) {
	var ops []IndexedRegistryTagDeployOperation
	for i, rawOp := range dm.Operations {
		var baseOp BaseCommandOperation
		if err := json.Unmarshal(rawOp, &baseOp); err != nil {
			return nil, err
		}
		if baseOp.Command != "registry_tag" {
			continue
		}
		var tagOp RegistryTagDeployOperation
		decoder := json.NewDecoder(bytes.NewReader(rawOp))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&tagOp); err != nil {
			return nil, err
		}
		ops = append(ops, IndexedRegistryTagDeployOperation{
			I:                          i,
			Strategy:                   dm.Settings.PushStrategy,
			RegistryTagDeployOperation: tagOp,
		})
	}
	return ops, nil
}

func (dm *DeployManifest) LoadOperations() ([]IndexedLoadDeployOperation, error) {
	var ops []IndexedLoadDeployOperation
	// for each raw operation, check if the command is "load" and unmarshal accordingly
	for i, rawOp := range dm.Operations {
		var baseOp BaseCommandOperation
		if err := json.Unmarshal(rawOp, &baseOp); err != nil {
			return nil, err
		}
		if baseOp.Command != "load" {
			continue
		}
		var loadOp LoadDeployOperation
		decoder := json.NewDecoder(bytes.NewReader(rawOp))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&loadOp); err != nil {
			return nil, err
		}
		ops = append(ops, IndexedLoadDeployOperation{
			I:                   i,
			Strategy:            dm.Settings.LoadStrategy,
			LoadDeployOperation: loadOp,
		})
	}
	return ops, nil
}

type DeploySettings struct {
	PushStrategy string `json:"push_strategy,omitempty"`
	LoadStrategy string `json:"load_strategy,omitempty"`
}

type BaseCommandOperation struct {
	Command   string               `json:"command"`   // "push" or "load"
	RootKind  string               `json:"root_kind"` // "manifest" or "index"
	Root      Descriptor           `json:"root"`      // the descriptor of the index / single manifest to push
	Manifests []ManifestDeployInfo `json:"manifests"` // for index push, the list of manifests to push. For single manifest push, this contains just one element.

	CrossMountHint *CrossMountSource `json:"cross_mount_hint,omitempty"` // repository from which layers can be cross-mounted

	PullInfo
}

type PushDeployOperation struct {
	BaseCommandOperation
	PushTarget
}

type IndexedPushDeployOperation struct {
	I        int
	Strategy string
	PushDeployOperation
}

// RegistryTagDeployOperation attaches extra tags to a manifest already pushed
// by a preceding push operation. Tags are pre-expanded at build time.
type RegistryTagDeployOperation struct {
	BaseCommandOperation
	PushTarget
}

type IndexedRegistryTagDeployOperation struct {
	I        int
	Strategy string
	RegistryTagDeployOperation
}

// LoadDeployOperation describes loading an image into a local daemon. It mirrors
// PushTarget's Registry/Repository/Tags shape, but keeps every destination field
// optional: when only Tags are set (the rules_oci-compatible mode) the tags are
// already full image references and Registry/Repository are omitted entirely.
// When Registry and Repository are both set, Tags are bare tags and the full
// image names are reconstructed as "<registry>/<repository>:<tag>" (see
// ImageNames).
type LoadDeployOperation struct {
	BaseCommandOperation
	Registry   string   `json:"registry,omitempty"`
	Repository string   `json:"repository,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Daemon     string   `json:"daemon,omitempty"`
}

// ImageNames returns the fully-qualified image reference(s) this load operation
// applies. See QualifyLoadTags for the reconstruction rules.
func (o LoadDeployOperation) ImageNames() []string {
	return QualifyLoadTags(o.Registry, o.Repository, o.Tags)
}

// ValidateLoadDestination reports an error when exactly one of registry and
// repository is set. Both-set (split mode) and both-empty (verbatim,
// rules_oci-compatible mode) are valid; a lone registry or repository is a
// misconfiguration (for example a Go template that expanded to the empty string)
// that would otherwise silently fall back to verbatim mode. This mirrors the
// hard error the push path raises for a missing registry/repository, and must be
// checked against the post-template-expansion values (the Starlark-level guard
// only sees the raw, unexpanded strings).
func ValidateLoadDestination(registry, repository string) error {
	if (registry == "") != (repository == "") {
		return fmt.Errorf("load configuration: 'registry' and 'repository' must be set together or both empty; got registry=%q repository=%q", registry, repository)
	}
	return nil
}

// QualifyLoadTags reconstructs full image names for a load operation. When both
// registry and repository are set, each tag is expanded to
// "<registry>/<repository>:<tag>". When either is empty (backwards-compatible
// mode) the tags are returned verbatim, because they are already full image
// references. The returned slice is always a fresh copy so callers may mutate
// it freely.
func QualifyLoadTags(registry, repository string, tags []string) []string {
	if registry == "" || repository == "" {
		return append([]string(nil), tags...)
	}
	base := registry + "/" + repository
	names := make([]string, len(tags))
	for i, tag := range tags {
		names[i] = base + ":" + tag
	}
	return names
}

type IndexedLoadDeployOperation struct {
	I        int
	Strategy string
	LoadDeployOperation
}

type PushTarget struct {
	Registry   string   `json:"registry"`
	Repository string   `json:"repository"`
	Tags       []string `json:"tags,omitempty"`
}

type PullInfo struct {
	OriginalBaseImageRegistries []string `json:"original_registries,omitempty"`
	OriginalBaseImageRepository string   `json:"original_repository,omitempty"`
	OriginalBaseImageTag        string   `json:"original_tag,omitempty"`
	OriginalBaseImageDigest     string   `json:"original_digest,omitempty"`
}

type CrossMountSource struct {
	Registry   string `json:"registry,omitempty"`
	Repository string `json:"repository"`
}

type ManifestDeployInfo struct {
	// Descriptor of the manifest to push
	Descriptor Descriptor `json:"descriptor"`
	// Descriptor of the config to push
	Config Descriptor `json:"config"`
	// Descriptor of the layers to push, each carrying its own upstream sources.
	LayerBlobs []LayerBlob `json:"layer_blobs"`
}

// LayerBlob is the descriptor of a single layer together with the upstream
// sources it can be fetched from. It embeds Descriptor so the layer's
// mediaType/digest/size fields marshal inline; the extra "sources" field lists
// the registry/repository combinations the blob is available from (e.g. the
// shallow base image it was pulled from). Sources is empty for layers built
// locally that have no upstream origin.
type LayerBlob struct {
	Descriptor
	Sources []LayerSource `json:"sources,omitempty"`
	// CompactStream, when set, marks this layer as a compact-stream layer whose
	// compressed blob was never materialized. It holds the CAS digest and size of
	// the .cstream index, from which the layer is reconstructed (the .cstream
	// header carries the compression parameters and the expected output digest).
	// The input blobs referenced by the .cstream are resolved from the CAS by the
	// digests recorded in its reference table.
	CompactStream *Descriptor `json:"compact_stream,omitempty"`
}

// LayerSource identifies one place a layer blob can be fetched from. The blob is
// content-addressed, so only the registry and repository are needed; the digest
// is the layer's own descriptor digest. A layer may list multiple sources (for
// example the same repository mirrored across several registries, or the same
// blob shared by base images from different repositories).
type LayerSource struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
}
