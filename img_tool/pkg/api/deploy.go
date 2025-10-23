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

type ManifestDeployInfo struct {
	// Descriptor of the manifest to push
	Descriptor Descriptor `json:"descriptor"`
	// Descriptor of the config to push
	Config Descriptor `json:"config"`
	// Descriptor of the layers to push
	LayerBlobs   []Descriptor `json:"layer_blobs"`
	MissingBlobs []string     `json:"missing_blobs,omitempty"`
}
