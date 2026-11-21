package bom

import (
	"bytes"
	"strings"

	cdx "github.com/CycloneDX/cyclonedx-go"
	supplychain "github.com/bazel-contrib/supply-chain/lib/supplychain-go"
)

// ToolName identifies the generator in the documents it writes.
const ToolName = "rules_img_sbom"

// WriteCycloneDX encodes doc as CycloneDX 1.6 JSON. serial is the document's
// serial number (urn:uuid:...).
func WriteCycloneDX(doc Document, serial string) ([]byte, error) {
	bom := cdx.NewBOM()
	bom.SerialNumber = serial
	bom.Version = 1
	primary := toCycloneDX(doc.Primary)
	bom.Metadata = &cdx.Metadata{
		Component: &primary,
		Tools: &cdx.ToolsChoice{Components: &[]cdx.Component{{
			Type: cdx.ComponentTypeApplication,
			Name: ToolName,
		}}},
	}
	if !doc.Created.IsZero() && doc.Created.Unix() != 0 {
		bom.Metadata.Timestamp = doc.Created.UTC().Format("2006-01-02T15:04:05Z")
	}
	if len(doc.Components) > 0 {
		components := make([]cdx.Component, 0, len(doc.Components))
		for _, c := range doc.Components {
			components = append(components, toCycloneDX(c))
		}
		bom.Components = &components
	}
	if len(doc.Dependencies) > 0 && !doc.OmitCycloneDXDependencies {
		refs := sortedKeys(doc.Dependencies)
		dependencies := make([]cdx.Dependency, 0, len(refs))
		for _, ref := range refs {
			on := append([]string(nil), doc.Dependencies[ref]...)
			dependencies = append(dependencies, cdx.Dependency{Ref: ref, Dependencies: &on})
		}
		bom.Dependencies = &dependencies
	}

	var buf bytes.Buffer
	encoder := cdx.NewBOMEncoder(&buf, cdx.BOMFileFormatJSON)
	encoder.SetPretty(true)
	if err := encoder.EncodeVersion(bom, cdx.SpecVersion1_6); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func toCycloneDX(c Component) cdx.Component {
	component := cdx.Component{
		BOMRef:      c.Ref,
		Type:        cdx.ComponentType(c.Type),
		Group:       c.Group,
		Name:        c.Name,
		Version:     c.Version,
		PackageURL:  c.PURL,
		Description: c.Description,
	}
	if component.Type == "" {
		component.Type = cdx.ComponentTypeLibrary
	}
	if len(c.CPEs) > 0 {
		component.CPE = c.CPEs[0]
	}
	if c.Supplier != "" {
		component.Supplier = &cdx.OrganizationalEntity{Name: strings.TrimPrefix(strings.TrimPrefix(c.Supplier, "Organization: "), "Person: ")}
	}
	var licenses cdx.Licenses
	for _, license := range c.Licenses {
		switch {
		case license.Attribute != nil:
			if rendered := licensesOf(license.Attribute); rendered != nil {
				licenses = append(licenses, *rendered...)
			}
		case license.Expression != "" && isSingleLicenseID(license.Expression):
			licenses = append(licenses, cdx.LicenseChoice{License: &cdx.License{ID: license.Expression}})
		case license.Expression != "":
			licenses = append(licenses, cdx.LicenseChoice{Expression: license.Expression})
		case license.Name != "":
			licenses = append(licenses, cdx.LicenseChoice{License: &cdx.License{Name: license.Name}})
		}
	}
	// CycloneDX allows several licenses, or a single expression.
	if len(licenses) > 1 {
		for _, choice := range licenses {
			if choice.Expression != "" {
				licenses = cdx.Licenses{choice}
				break
			}
		}
	}
	if len(licenses) > 0 {
		component.Licenses = &licenses
	}
	if len(c.Hashes) > 0 {
		hashes := make([]cdx.Hash, 0, len(c.Hashes))
		for _, algorithm := range sortedKeys(c.Hashes) {
			hashes = append(hashes, cdx.Hash{Algorithm: cdx.HashAlgorithm(algorithm), Value: c.Hashes[algorithm]})
		}
		component.Hashes = &hashes
	}
	var references []cdx.ExternalReference
	if c.DownloadURL != "" {
		references = append(references, cdx.ExternalReference{Type: cdx.ERTypeDistribution, URL: c.DownloadURL})
	}
	for _, external := range c.ExternalBOMs {
		reference := cdx.ExternalReference{Type: cdx.ERTypeBOM, URL: external.URL}
		var hashes []cdx.Hash
		if external.SHA256 != "" {
			hashes = append(hashes, cdx.Hash{Algorithm: cdx.HashAlgoSHA256, Value: external.SHA256})
		}
		if len(hashes) > 0 {
			reference.Hashes = &hashes
		}
		reference.Comment = external.Comment
		if reference.Comment == "" && !external.OmitComment {
			var comment []string
			if external.Format != "" {
				comment = append(comment, external.Format+" SBOM")
			} else {
				comment = append(comment, "SBOM")
			}
			if external.Artifact != "" {
				comment = append(comment, "OCI artifact "+external.Artifact)
			}
			reference.Comment = strings.Join(comment, ", ")
		}
		if reference.URL != "" {
			references = append(references, reference)
		}
	}
	if len(references) > 0 {
		component.ExternalReferences = &references
	}
	return component
}

// isSingleLicenseID reports whether an expression is a single license identifier.
func isSingleLicenseID(expression string) bool {
	return expression != "" && !strings.ContainsAny(expression, " ()") && !strings.HasSuffix(expression, "+")
}

// licensesOf converts the license attribute of a package to CycloneDX: a
// license identified by its SPDX identifier, or by its name if it is custom.
// Anything else is an SPDX license expression.
//
// Copied from https://github.com/bazel-contrib/supply-chain/blob/c18a7daa7a4ed5e6d523a19f070c0cba19118419/lib/supplychain-go/cmd/cyclonedx/cyclonedx.go (Apache-2.0).
func licensesOf(attribute *supplychain.LicenseAttribute) *cdx.Licenses {
	if attribute == nil {
		return nil
	}

	root := attribute.Root()
	if !root.IsSingle() || root.OrLater {
		return &cdx.Licenses{{Expression: root.String()}}
	}
	kind := root.Kind
	license := cdx.License{ID: kind.Identifier}
	if kind.IsCustom() {
		license = cdx.License{Name: kind.Name}
		if license.Name == "" {
			license.Name = kind.Identifier
		}
	}
	return &cdx.Licenses{{License: &license}}
}
