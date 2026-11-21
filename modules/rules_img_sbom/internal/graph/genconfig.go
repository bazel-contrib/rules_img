// Copied from https://github.com/bazel-contrib/supply-chain/blob/c18a7daa7a4ed5e6d523a19f070c0cba19118419/lib/supplychain-go/internal/sbom/genconfig.go
// (Apache-2.0), which is internal to its module and cannot be imported.

package graph

type GenConfig struct {
	SchemaVersion string       `json:"schema_version"`
	RootTarget    string       `json:"root_target"`
	Nodes         []NodeConfig `json:"nodes"`
	Edges         []EdgeConfig `json:"edges"`
}

type NodeConfig struct {
	Label        string `json:"label"`
	MetadataFile string `json:"metadata_file"`
}

type EdgeConfig struct {
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type"`
}
