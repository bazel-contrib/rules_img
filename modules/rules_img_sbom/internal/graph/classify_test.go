// Copied from https://github.com/bazel-contrib/supply-chain/blob/c18a7daa7a4ed5e6d523a19f070c0cba19118419/lib/supplychain-go/internal/sbom/classify_test.go
// (Apache-2.0), which is internal to its module and cannot be imported.

package graph_test

import (
	"reflect"
	"testing"

	sbom "github.com/bazel-contrib/rules_img_sbom/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeGraph_PackagePathsAndAliases(t *testing.T) {
	graph := sbom.GraphConfig{
		RootTarget: "//app:root",
		Nodes: []sbom.NodeConfig{
			{Label: "//app:root", MetadataFile: "root.json"},
			{Label: "//app:wrapper"},
			{Label: "//app:internal"},
			{Label: "//module:a", MetadataFile: "module.json"},
			{Label: "//module:z", MetadataFile: "module.json"},
			{Label: "//module:wrapper"},
			{Label: "//transitive:dep", MetadataFile: "dep.json"},
		},
		Edges: []sbom.EdgeConfig{
			{From: "//app:root", To: "//app:wrapper", Type: "depends_on"},
			{From: "//app:wrapper", To: "//app:internal", Type: "depends_on"},
			{From: "//app:internal", To: "//app:wrapper", Type: "depends_on"},
			{From: "//app:internal", To: "//module:z", Type: "depends_on"},
			{From: "//module:z", To: "//module:a", Type: "depends_on"},
			{From: "//module:z", To: "//module:wrapper", Type: "depends_on"},
			{From: "//module:wrapper", To: "//transitive:dep", Type: "depends_on"},
			{From: "//module:a", To: "//transitive:dep", Type: "depends_on"},
			{From: "//transitive:dep", To: "//module:z", Type: "depends_on"},
		},
	}
	normalized := sbom.NormalizeGraph(graph)
	require.Len(t, normalized.Nodes, 3)
	assert.ElementsMatch(t, []sbom.EdgeConfig{
		{From: "//app:root", To: "//module:a", Type: "depends_on"},
		{From: "//module:a", To: "//transitive:dep", Type: "depends_on"},
		{From: "//transitive:dep", To: "//module:a", Type: "depends_on"},
	}, normalized.Edges)
	assert.True(t, reflect.DeepEqual(normalized, sbom.NormalizeGraph(normalized)), "normalization must be idempotent")
	classifications, err := sbom.ComputeClassifications(graph, false)
	require.NoError(t, err)
	require.Len(t, classifications.Dependencies.Direct, 1)
	assert.Equal(t, "//module:a", classifications.Dependencies.Direct[0].Label)
	require.Len(t, classifications.Dependencies.Transitive, 1)
	assert.Equal(t, "//transitive:dep", classifications.Dependencies.Transitive[0].Label)

	graph.Nodes[0].MetadataFile = ""
	classifications, err = sbom.ComputeClassifications(graph, true)
	require.NoError(t, err)
	assert.Nil(t, classifications.RootComponent)
	require.Len(t, classifications.Dependencies.Direct, 1)
	assert.Equal(t, "//module:a", classifications.Dependencies.Direct[0].Label)
	require.Len(t, classifications.Dependencies.Transitive, 1)
}

func TestComputeClassifications_WithRootMetadata(t *testing.T) {
	graph := sbom.GraphConfig{
		SchemaVersion: "1.0",
		RootTarget:    "//app:binary",
		Nodes: []sbom.NodeConfig{
			{Label: "//app:binary", MetadataFile: "app.json"},
			{Label: "//lib:lib", MetadataFile: "lib.json"},
			{Label: "@@uuid//:uuid", MetadataFile: "uuid.json"},
		},
		Edges: []sbom.EdgeConfig{
			{From: "//app:binary", To: "//lib:lib", Type: "depends_on"},
			{From: "//lib:lib", To: "@@uuid//:uuid", Type: "depends_on"},
		},
	}

	classifications, err := sbom.ComputeClassifications(graph, false)
	require.NoError(t, err)

	// Root has metadata, should be root component
	require.NotNil(t, classifications.RootComponent)
	assert.Equal(t, "//app:binary", classifications.RootComponent.Label)

	// lib is direct dependency of root
	require.Len(t, classifications.Dependencies.Direct, 1)
	assert.Equal(t, "//lib:lib", classifications.Dependencies.Direct[0].Label)

	// uuid is transitive
	require.Len(t, classifications.Dependencies.Transitive, 1)
	assert.Equal(t, "@@uuid//:uuid", classifications.Dependencies.Transitive[0].Label)
}

func TestComputeClassifications_BinaryEmbeddingLibrary_RequireMetadata(t *testing.T) {
	// Binary has no metadata - should error when metadata is required
	graph := sbom.GraphConfig{
		SchemaVersion: "1.0",
		RootTarget:    "//app:binary",
		Nodes: []sbom.NodeConfig{
			{Label: "//app:lib", MetadataFile: "lib.json"},
			{Label: "@@uuid//:uuid", MetadataFile: "uuid.json"},
		},
		Edges: []sbom.EdgeConfig{
			{From: "//app:lib", To: "@@uuid//:uuid", Type: "depends_on"},
		},
	}

	_, err := sbom.ComputeClassifications(graph, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "//app:binary")
	assert.Contains(t, err.Error(), "has no package metadata")
}

func TestComputeClassifications_BinaryEmbeddingLibrary_AllowMissing(t *testing.T) {
	// Binary has no metadata - allowed when flag is set
	graph := sbom.GraphConfig{
		SchemaVersion: "1.0",
		RootTarget:    "//app:binary",
		Nodes: []sbom.NodeConfig{
			{Label: "//app:lib", MetadataFile: "lib.json"},
			{Label: "@@uuid//:uuid", MetadataFile: "uuid.json"},
		},
		Edges: []sbom.EdgeConfig{
			{From: "//app:lib", To: "@@uuid//:uuid", Type: "depends_on"},
		},
	}

	classifications, err := sbom.ComputeClassifications(graph, true)
	require.NoError(t, err)

	// No root component when root has no metadata
	assert.Nil(t, classifications.RootComponent)

	// lib is direct (no incoming edges)
	require.Len(t, classifications.Dependencies.Direct, 1)
	assert.Equal(t, "//app:lib", classifications.Dependencies.Direct[0].Label)

	// uuid is transitive
	require.Len(t, classifications.Dependencies.Transitive, 1)
	assert.Equal(t, "@@uuid//:uuid", classifications.Dependencies.Transitive[0].Label)
}

func TestComputeClassifications_NoRootMetadata_MultipleNodesWithoutIncoming(t *testing.T) {
	// Multiple nodes without incoming edges, root has no metadata
	graph := sbom.GraphConfig{
		SchemaVersion: "1.0",
		RootTarget:    "//app:binary",
		Nodes: []sbom.NodeConfig{
			{Label: "//lib1:lib1", MetadataFile: "lib1.json"},
			{Label: "//lib2:lib2", MetadataFile: "lib2.json"},
			{Label: "@@uuid//:uuid", MetadataFile: "uuid.json"},
		},
		Edges: []sbom.EdgeConfig{
			{From: "//lib1:lib1", To: "@@uuid//:uuid", Type: "depends_on"},
		},
	}

	classifications, err := sbom.ComputeClassifications(graph, true)
	require.NoError(t, err)

	// No root component when root has no metadata
	assert.Nil(t, classifications.RootComponent)

	// Both lib1 and lib2 have no incoming edges, so both are direct
	require.Len(t, classifications.Dependencies.Direct, 2)
	directLabels := []string{classifications.Dependencies.Direct[0].Label, classifications.Dependencies.Direct[1].Label}
	assert.Contains(t, directLabels, "//lib1:lib1")
	assert.Contains(t, directLabels, "//lib2:lib2")

	// uuid is transitive
	require.Len(t, classifications.Dependencies.Transitive, 1)
	assert.Equal(t, "@@uuid//:uuid", classifications.Dependencies.Transitive[0].Label)
}

func TestComputeClassifications_EmptyGraph(t *testing.T) {
	graph := sbom.GraphConfig{
		SchemaVersion: "1.0",
		RootTarget:    "",
		Nodes:         []sbom.NodeConfig{},
		Edges:         []sbom.EdgeConfig{},
	}

	classifications, err := sbom.ComputeClassifications(graph, false)
	require.NoError(t, err)

	assert.Nil(t, classifications.RootComponent)
	assert.Empty(t, classifications.Dependencies.Direct)
	assert.Empty(t, classifications.Dependencies.Transitive)
}

func TestComputeClassifications_SingleNodeNoRoot(t *testing.T) {
	// Root target has no metadata, single library node exists
	graph := sbom.GraphConfig{
		SchemaVersion: "1.0",
		RootTarget:    "//app:binary",
		Nodes: []sbom.NodeConfig{
			{Label: "//lib:lib", MetadataFile: "lib.json"},
		},
		Edges: []sbom.EdgeConfig{},
	}

	classifications, err := sbom.ComputeClassifications(graph, true)
	require.NoError(t, err)

	// No root component when root has no metadata
	assert.Nil(t, classifications.RootComponent)

	// Single library node is direct
	require.Len(t, classifications.Dependencies.Direct, 1)
	assert.Equal(t, "//lib:lib", classifications.Dependencies.Direct[0].Label)

	assert.Empty(t, classifications.Dependencies.Transitive)
}

func TestComputeClassifications_RootTargetNotInNodes(t *testing.T) {
	// Root target exists but has no metadata (not in nodes)
	graph := sbom.GraphConfig{
		SchemaVersion: "1.0",
		RootTarget:    "//app:binary",
		Nodes: []sbom.NodeConfig{
			{Label: "//lib:lib", MetadataFile: "lib.json"},
			{Label: "//dep:dep", MetadataFile: "dep.json"},
		},
		Edges: []sbom.EdgeConfig{
			{From: "//lib:lib", To: "//dep:dep", Type: "depends_on"},
		},
	}

	classifications, err := sbom.ComputeClassifications(graph, true)
	require.NoError(t, err)

	// No root component when root has no metadata
	assert.Nil(t, classifications.RootComponent)

	// lib has no incoming edges, so it's direct
	require.Len(t, classifications.Dependencies.Direct, 1)
	assert.Equal(t, "//lib:lib", classifications.Dependencies.Direct[0].Label)

	// dep is transitive
	require.Len(t, classifications.Dependencies.Transitive, 1)
	assert.Equal(t, "//dep:dep", classifications.Dependencies.Transitive[0].Label)
}

func TestComputeClassifications_DuplicateMetadataFile_CollapsesToSingleComponent(t *testing.T) {
	// Two distinct Bazel targets (e.g. two files in the same third-party
	// package) point at the same package_metadata file. They must collapse
	// into a single SBOM component, and the dependency edge between them
	// must not survive as a self-reference once collapsed.
	graph := sbom.GraphConfig{
		SchemaVersion: "1.0",
		RootTarget:    "//app:binary",
		Nodes: []sbom.NodeConfig{
			{Label: "//app:binary", MetadataFile: "app.json"},
			{Label: "//third_party/pkg:a.go", MetadataFile: "pkg.json"},
			{Label: "//third_party/pkg:b.go", MetadataFile: "pkg.json"},
		},
		Edges: []sbom.EdgeConfig{
			{From: "//app:binary", To: "//third_party/pkg:a.go", Type: "depends_on"},
			{From: "//app:binary", To: "//third_party/pkg:b.go", Type: "depends_on"},
			{From: "//third_party/pkg:a.go", To: "//third_party/pkg:b.go", Type: "depends_on"},
		},
	}

	classifications, err := sbom.ComputeClassifications(graph, false)
	require.NoError(t, err)

	require.NotNil(t, classifications.RootComponent)
	assert.Equal(t, "//app:binary", classifications.RootComponent.Label)

	// Only one component should remain for the shared metadata file.
	require.Len(t, classifications.Dependencies.Direct, 1)
	assert.Equal(t, "//third_party/pkg:a.go", classifications.Dependencies.Direct[0].Label)
	assert.Empty(t, classifications.Dependencies.Transitive)
}

func TestComputeClassifications_MalformedGraph_RootWithMetadataNoEdgesButOtherNodes(t *testing.T) {
	// Root has metadata but no edges, yet other nodes exist - malformed graph
	graph := sbom.GraphConfig{
		SchemaVersion: "1.0",
		RootTarget:    "//app:binary",
		Nodes: []sbom.NodeConfig{
			{Label: "//app:binary", MetadataFile: "app.json"}, // Root has metadata
			{Label: "//lib:lib", MetadataFile: "lib.json"},
			{Label: "@@uuid//:uuid", MetadataFile: "uuid.json"},
		},
		Edges: []sbom.EdgeConfig{
			// Root has no edges, but lib->uuid edge exists
			{From: "//lib:lib", To: "@@uuid//:uuid", Type: "depends_on"},
		},
	}

	_, err := sbom.ComputeClassifications(graph, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed graph")
	assert.Contains(t, err.Error(), "//app:binary")
	assert.Contains(t, err.Error(), "no dependency edges")
}
