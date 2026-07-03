package api

import (
	"bytes"
	"encoding/json"
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

type LoadDeployOperation struct {
	BaseCommandOperation
	Tags   []string `json:"tags,omitempty"`
	Daemon string   `json:"daemon,omitempty"`
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
