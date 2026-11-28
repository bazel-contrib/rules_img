package bom

import (
	"fmt"
	"strings"

	supplychain "github.com/bazel-contrib/supply-chain/lib/supplychain-go"

	"github.com/bazel-contrib/rules_img_sbom/internal/graph"
)

// AddBazelPackages adds the packages of a gather_metadata graph, whose root
// target is the image (the Builder's primary component), with the
// dependencies between them.
func AddBazelPackages(b *Builder, g graph.GraphConfig) error {
	classifications, err := graph.ComputeClassifications(g, true)
	if err != nil {
		return fmt.Errorf("classifying the package metadata graph: %w", err)
	}
	g = graph.NormalizeGraph(g)

	refOf := map[string]string{g.RootTarget: b.Primary().Ref}
	nodes := append(append([]graph.NodeConfig{}, classifications.Dependencies.Direct...), classifications.Dependencies.Transitive...)
	for _, node := range nodes {
		if node.MetadataFile == "" {
			continue
		}
		component, err := bazelComponent(node)
		if err != nil {
			return err
		}
		refOf[node.Label] = b.Merge(component)
	}
	for _, edge := range g.Edges {
		from, fromOK := refOf[edge.From]
		to, toOK := refOf[edge.To]
		if fromOK && toOK {
			b.Depend(from, to)
		}
	}
	return nil
}

func bazelComponent(node graph.NodeConfig) (Component, error) {
	metadata, err := supplychain.ReadPackageMetadataFromFile(node.MetadataFile)
	if err != nil {
		return Component{}, fmt.Errorf("reading package metadata %s of %s: %w", node.MetadataFile, node.Label, err)
	}
	purl := metadata.GetPURL()
	name := purl.Name
	if purl.Namespace != "" {
		name = purl.Namespace + "/" + name
	}
	component := Component{
		Ref:     purl.String(),
		Type:    TypeLibrary,
		Name:    name,
		Version: purl.Version,
		PURL:    purl.String(),
	}
	if purl.Type == "docker" || purl.Type == "oci" {
		component.Type = TypeContainer
	}
	qualifiers := purl.Qualifiers.Map()
	for _, key := range []string{"download_url", "url_download"} {
		if value := strings.TrimSpace(qualifiers[key]); value != "" {
			component.DownloadURL = value
			break
		}
	}
	license, err := supplychain.GetLicenseAttribute(metadata)
	if err != nil {
		return Component{}, fmt.Errorf("reading the license of %s: %w", node.Label, err)
	}
	if license != nil {
		component.Licenses = []License{{Attribute: license}}
	}
	return component, nil
}
