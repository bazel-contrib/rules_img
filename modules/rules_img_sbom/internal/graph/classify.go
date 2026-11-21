// Copied from https://github.com/bazel-contrib/supply-chain/blob/c18a7daa7a4ed5e6d523a19f070c0cba19118419/lib/supplychain-go/internal/sbom/classify.go
// (Apache-2.0), which is internal to its module and cannot be imported.

package graph

import (
	"fmt"
	"sort"
)

type GraphConfig struct {
	SchemaVersion string       `json:"schema_version"`
	RootTarget    string       `json:"root_target"`
	Nodes         []NodeConfig `json:"nodes"`
	Edges         []EdgeConfig `json:"edges"`
}

type Classifications struct {
	RootComponent *NodeConfig     `json:"root_component,omitempty"`
	Dependencies  DependencyNodes `json:"dependencies"`
}

type DependencyNodes struct {
	Direct     []NodeConfig `json:"direct"`
	Transitive []NodeConfig `json:"transitive"`
}

func ComputeClassifications(graph GraphConfig, allowMissingRootMetadata bool) (Classifications, error) {
	graph = NormalizeGraph(graph)
	scopes, err := calculateScopes(graph.RootTarget, graph.Nodes, graph.Edges, allowMissingRootMetadata)
	if err != nil {
		return Classifications{}, err
	}

	return Classifications{
		RootComponent: scopes.root,
		Dependencies: DependencyNodes{
			Direct:     scopes.direct,
			Transitive: scopes.transitive,
		},
	}, nil
}

// NormalizeGraph projects target paths onto package metadata, then merges
// aliases. Classification and document generation must use the same graph.
func NormalizeGraph(graph GraphConfig) GraphConfig {
	if len(graph.Nodes) == 0 {
		return graph
	}
	metadataLabels := make(map[string]bool)
	nodes := make([]NodeConfig, 0, len(graph.Nodes))
	for _, node := range graph.Nodes {
		if node.MetadataFile != "" {
			metadataLabels[node.Label] = true
			nodes = append(nodes, node)
		} else if node.Label == graph.RootTarget {
			nodes = append(nodes, node)
		}
	}
	adjacency := make(map[string][]EdgeConfig)
	for _, edge := range graph.Edges {
		adjacency[edge.From] = append(adjacency[edge.From], edge)
	}
	edges := make([]EdgeConfig, 0, len(graph.Edges))
	for _, node := range nodes {
		seen := map[string]bool{node.Label: true}
		pending := append([]EdgeConfig(nil), adjacency[node.Label]...)
		for len(pending) > 0 {
			edge := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			if seen[edge.To] {
				continue
			}
			seen[edge.To] = true
			if metadataLabels[edge.To] {
				edges = append(edges, EdgeConfig{From: node.Label, To: edge.To, Type: edge.Type})
			} else {
				pending = append(pending, adjacency[edge.To]...)
			}
		}
	}
	graph.Nodes = nodes
	graph.Edges = edges
	return deduplicateByMetadata(graph)
}

// deduplicateByMetadata collapses graph nodes that share the same non-empty
// metadata file into a single canonical node.
//
// The graph has one node per Bazel target reached by the gather_metadata
// aspect, but many distinct targets commonly carry the same package_metadata
// (e.g. every file in a single third-party package, or multiple targets
// annotated with the same license declaration). Without collapsing those
// onto one identity, the SBOM generators downstream emit multiple components
// with the same purl/name (duplicate refs), and any dependency edge between
// two such targets turns into a self-referencing edge once both ends are
// mapped to the same component.
func deduplicateByMetadata(graph GraphConfig) GraphConfig {
	sortedNodes := make([]NodeConfig, len(graph.Nodes))
	copy(sortedNodes, graph.Nodes)
	sort.Slice(sortedNodes, func(i, j int) bool { return sortedNodes[i].Label < sortedNodes[j].Label })

	// Map every node's label to a canonical label. Nodes without metadata
	// keep their own identity since there is nothing to merge them on.
	canonicalByMetadata := make(map[string]string)
	remap := make(map[string]string)
	for _, node := range sortedNodes {
		if node.MetadataFile == "" {
			remap[node.Label] = node.Label
			continue
		}
		canon, ok := canonicalByMetadata[node.MetadataFile]
		if !ok {
			canonicalByMetadata[node.MetadataFile] = node.Label
			canon = node.Label
		}
		remap[node.Label] = canon
	}

	seenLabels := make(map[string]bool)
	nodes := make([]NodeConfig, 0, len(sortedNodes))
	for _, node := range sortedNodes {
		// A label can legitimately appear only once per the aspect's own
		// bookkeeping, but guard against literal duplicate entries (e.g. the
		// same target reached and recorded twice) in addition to collapsing
		// distinct labels that share metadata.
		if remap[node.Label] != node.Label || seenLabels[node.Label] {
			continue
		}
		seenLabels[node.Label] = true
		nodes = append(nodes, node)
	}

	seenEdges := make(map[EdgeConfig]bool)
	edges := make([]EdgeConfig, 0, len(graph.Edges))
	for _, edge := range graph.Edges {
		from, to := remapLabel(remap, edge.From), remapLabel(remap, edge.To)
		if from == to {
			// Collapsing duplicate nodes turned this into a self-reference.
			continue
		}
		e := EdgeConfig{From: from, To: to, Type: edge.Type}
		if seenEdges[e] {
			continue
		}
		seenEdges[e] = true
		edges = append(edges, e)
	}

	graph.Nodes = nodes
	graph.Edges = edges
	graph.RootTarget = remapLabel(remap, graph.RootTarget)
	sort.Slice(graph.Edges, func(i, j int) bool {
		a, b := graph.Edges[i], graph.Edges[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		return a.Type < b.Type
	})
	return graph
}

func remapLabel(remap map[string]string, label string) string {
	if canon, ok := remap[label]; ok {
		return canon
	}
	return label
}

type scopeResult struct {
	root       *NodeConfig
	direct     []NodeConfig
	transitive []NodeConfig
}

// calculateScopes determines root/direct/transitive classification
func calculateScopes(rootTarget string, nodes []NodeConfig, edges []EdgeConfig, allowMissingRootMetadata bool) (scopeResult, error) {
	result := scopeResult{
		root:       nil,
		direct:     []NodeConfig{},
		transitive: []NodeConfig{},
	}

	if rootTarget == "" {
		// No root target, treat all as direct
		for _, node := range nodes {
			result.direct = append(result.direct, node)
		}
		return result, nil
	}

	// Build adjacency list from edges
	adjacency := make(map[string][]string)
	for _, edge := range edges {
		adjacency[edge.From] = append(adjacency[edge.From], edge.To)
	}

	// Check if root_target has metadata
	rootHasMetadata := false
	for _, node := range nodes {
		if node.Label == rootTarget && node.MetadataFile != "" {
			nodeCopy := node
			result.root = &nodeCopy
			rootHasMetadata = true
			break
		}
	}

	// If root has no metadata and we require it, error
	if !rootHasMetadata && !allowMissingRootMetadata {
		return result, fmt.Errorf(
			"target %s has no package metadata. "+
				"Add package_metadata to the target's BUILD file or set require_root_metadata = False on the sbom() rule",
			rootTarget,
		)
	}

	// Find direct dependencies
	directDeps := make(map[string]bool)
	if rootHasMetadata {
		// Root has metadata - normal classification
		if len(adjacency[rootTarget]) > 0 {
			// Root has edges - those are direct deps
			for _, child := range adjacency[rootTarget] {
				directDeps[child] = true
			}
		} else {
			// Root has no edges
			if len(nodes) > 1 {
				// Other nodes exist but root has no edges to them - malformed graph
				return result, fmt.Errorf(
					"malformed graph: target %s has metadata but no dependency edges, yet graph contains %d nodes. "+
						"This suggests the graph was incorrectly constructed",
					rootTarget, len(nodes),
				)
			}
			// else: only root node exists, no dependencies - valid
		}
	} else {
		// A retained unannotated root still identifies its direct packages.
		for _, child := range adjacency[rootTarget] {
			directDeps[child] = true
		}
		// Older graphs omitted unannotated targets. Preserve their fallback.
		hasIncoming := make(map[string]bool)
		for _, edge := range edges {
			hasIncoming[edge.To] = true
		}

		for _, node := range nodes {
			if len(adjacency[rootTarget]) == 0 && node.Label != rootTarget && !hasIncoming[node.Label] {
				directDeps[node.Label] = true
			}
		}
	}

	// Classify all nodes
	for _, node := range nodes {
		if node.MetadataFile == "" {
			continue
		}
		label := node.Label
		if result.root != nil && label == result.root.Label {
			// Skip the root component itself
			continue
		}
		if label == rootTarget {
			// Skip root target if it has no metadata
			continue
		}

		if directDeps[label] {
			result.direct = append(result.direct, node)
		} else {
			result.transitive = append(result.transitive, node)
		}
	}

	return result, nil
}
