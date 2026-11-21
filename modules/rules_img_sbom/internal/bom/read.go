package bom

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	cdx "github.com/CycloneDX/cyclonedx-go"
	spdxjson "github.com/spdx/tools-golang/json"
	spdx "github.com/spdx/tools-golang/spdx"
	"github.com/spdx/tools-golang/spdx/v2/common"
)

// Imported is an existing SBOM, such as the SBOM of a base image.
type Imported struct {
	Format string
	// Described are the components the document describes (the base image).
	Described []Component
	// Components are all other components.
	Components   []Component
	Dependencies map[string][]string
	// ExtractedLicenses are the document's licenses that are not on the SPDX list.
	ExtractedLicenses []ExtractedLicense
	// Reference refers to the document.
	Reference ExternalBOM
}

// Unwrap returns the SBOM document inside data, which may be the document
// itself, an in-toto statement whose predicate is the document (or the
// document as a JSON string), a DSSE envelope of such a statement (as cosign's
// .att tags carry it), or a sigstore bundle with such an envelope.
// The wrapping is detected from the content, so a mislabeled attestation
// still unwraps. Signatures are not verified: that is up to the policy that
// consumes the resulting SBOM.
func Unwrap(data []byte) ([]byte, error) {
	for range 4 {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(data, &probe); err != nil {
			return nil, fmt.Errorf("not a JSON object: %w", err)
		}
		switch {
		case probe["dsseEnvelope"] != nil:
			// A sigstore bundle.
			data = probe["dsseEnvelope"]
		case probe["payload"] != nil && probe["payloadType"] != nil:
			// A DSSE envelope.
			var payload string
			if err := json.Unmarshal(probe["payload"], &payload); err != nil {
				return nil, fmt.Errorf("parsing the DSSE payload: %w", err)
			}
			decoded, err := base64.StdEncoding.DecodeString(payload)
			if err != nil {
				if decoded, err = base64.RawURLEncoding.DecodeString(payload); err != nil {
					return nil, fmt.Errorf("decoding the DSSE payload: %w", err)
				}
			}
			data = decoded
		case probe["predicate"] != nil && (probe["_type"] != nil || probe["predicateType"] != nil):
			// An in-toto statement. Some tools wrap the predicate as {"Data": "<document>"},
			// others (distroless, say) record the document as a JSON string.
			predicate := probe["predicate"]
			var wrapped struct {
				Data string `json:"Data"`
			}
			var document string
			if json.Unmarshal(predicate, &document) == nil {
				predicate = []byte(document)
			} else if json.Unmarshal(predicate, &wrapped) == nil && wrapped.Data != "" {
				predicate = []byte(wrapped.Data)
			}
			data = predicate
		default:
			return data, nil
		}
	}
	return nil, fmt.Errorf("too many levels of wrapping")
}

// DetectFormat returns "cyclonedx" or "spdx" for an SBOM document, or "".
func DetectFormat(doc []byte) string {
	var probe struct {
		BOMFormat   string `json:"bomFormat"`
		SPDXVersion string `json:"spdxVersion"`
	}
	if json.Unmarshal(doc, &probe) != nil {
		return ""
	}
	switch {
	case probe.BOMFormat == "CycloneDX":
		return "cyclonedx"
	case strings.HasPrefix(probe.SPDXVersion, "SPDX-2."):
		return "spdx"
	}
	return ""
}

// Read parses an SBOM, unwrapping it first (see Unwrap).
func Read(data []byte) (*Imported, error) {
	doc, err := Unwrap(data)
	if err != nil {
		return nil, err
	}
	var imported *Imported
	switch format := DetectFormat(doc); format {
	case "cyclonedx":
		imported, err = readCycloneDX(doc)
	case "spdx":
		imported, err = readSPDX(doc)
	default:
		return nil, fmt.Errorf("not a CycloneDX or SPDX 2.x JSON document")
	}
	if err != nil {
		return nil, err
	}
	imported.Reference.SHA256, imported.Reference.SHA1 = Checksums(doc)
	return imported, nil
}

func readCycloneDX(doc []byte) (*Imported, error) {
	var bom cdx.BOM
	if err := cdx.NewBOMDecoder(bytes.NewReader(doc), cdx.BOMFileFormatJSON).Decode(&bom); err != nil {
		return nil, fmt.Errorf("parsing CycloneDX: %w", err)
	}
	imported := &Imported{Format: "cyclonedx", Dependencies: map[string][]string{}}
	if bom.SerialNumber != "" {
		imported.Reference.URL = fmt.Sprintf("urn:cdx:%s/%d", strings.TrimPrefix(bom.SerialNumber, "urn:uuid:"), bom.Version)
	}
	imported.Reference.Format = "cyclonedx"
	if bom.Metadata != nil && bom.Metadata.Component != nil {
		described := fromCycloneDX(*bom.Metadata.Component)
		imported.Described = append(imported.Described, described)
		imported.Reference.DescribedID = described.Ref
		if bom.Metadata.Component.Components != nil {
			collectCycloneDX(*bom.Metadata.Component.Components, &imported.Components)
		}
	}
	if bom.Components != nil {
		collectCycloneDX(*bom.Components, &imported.Components)
	}
	if bom.Dependencies != nil {
		for _, dependency := range *bom.Dependencies {
			if dependency.Dependencies != nil {
				imported.Dependencies[dependency.Ref] = append(imported.Dependencies[dependency.Ref], *dependency.Dependencies...)
			}
		}
	}
	return imported, nil
}

func collectCycloneDX(components []cdx.Component, into *[]Component) {
	for _, c := range components {
		*into = append(*into, fromCycloneDX(c))
		if c.Components != nil {
			collectCycloneDX(*c.Components, into)
		}
	}
}

func fromCycloneDX(c cdx.Component) Component {
	component := Component{
		Ref:         c.BOMRef,
		Type:        string(c.Type),
		Group:       c.Group,
		Name:        c.Name,
		Version:     c.Version,
		PURL:        c.PackageURL,
		Description: c.Description,
	}
	if component.Ref == "" {
		component.Ref = c.PackageURL
	}
	if component.Ref == "" {
		component.Ref = "component:" + c.Group + "/" + c.Name + "@" + c.Version
	}
	if c.CPE != "" {
		component.CPEs = []string{c.CPE}
	}
	if c.Supplier != nil {
		component.Supplier = c.Supplier.Name
	}
	if c.Licenses != nil {
		for _, choice := range *c.Licenses {
			switch {
			case choice.Expression != "":
				component.Licenses = append(component.Licenses, License{Expression: choice.Expression})
			case choice.License != nil && choice.License.ID != "":
				component.Licenses = append(component.Licenses, License{Expression: choice.License.ID})
			case choice.License != nil && choice.License.Name != "":
				component.Licenses = append(component.Licenses, License{Name: choice.License.Name})
			}
		}
	}
	if c.Hashes != nil {
		component.Hashes = map[string]string{}
		for _, hash := range *c.Hashes {
			component.Hashes[string(hash.Algorithm)] = hash.Value
		}
	}
	if c.ExternalReferences != nil {
		for _, ref := range *c.ExternalReferences {
			switch {
			case ref.Type == cdx.ERTypeDistribution && component.DownloadURL == "":
				component.DownloadURL = ref.URL
			case ref.Type == cdx.ERTypeBOM && ref.URL != "":
				// An SBOM of the component that lives elsewhere (another
				// document, an SBOM database, ...): keep pointing at it.
				external := ExternalBOM{URL: ref.URL, Comment: ref.Comment}
				if strings.HasPrefix(ref.URL, "urn:cdx:") {
					external.Format = "cyclonedx"
				}
				if ref.Hashes != nil {
					for _, hash := range *ref.Hashes {
						switch hash.Algorithm {
						case cdx.HashAlgoSHA256:
							external.SHA256 = hash.Value
						case cdx.HashAlgoSHA1:
							external.SHA1 = hash.Value
						}
					}
				}
				component.ExternalBOMs = append(component.ExternalBOMs, external)
			}
		}
	}
	return component
}

var spdxPurposeTypes = map[string]string{
	"APPLICATION":      TypeApplication,
	"CONTAINER":        TypeContainer,
	"FILE":             TypeFile,
	"FRAMEWORK":        TypeFramework,
	"LIBRARY":          TypeLibrary,
	"OPERATING-SYSTEM": TypeOperatingSystem,
	"OPERATING_SYSTEM": TypeOperatingSystem,
}

func readSPDX(doc []byte) (*Imported, error) {
	document, err := spdxjson.Read(bytes.NewReader(doc))
	if err != nil {
		return nil, fmt.Errorf("parsing SPDX: %w", err)
	}
	imported := &Imported{
		Format:       "spdx",
		Dependencies: map[string][]string{},
		Reference:    ExternalBOM{Format: "spdx", URL: document.DocumentNamespace},
	}
	// Packages without a purl are scoped to the document, so two documents'
	// SPDXRef-Package-1 don't collide once merged.
	scope, _ := Checksums([]byte(document.DocumentNamespace))
	refOf := map[common.ElementID]string{}
	components := map[common.ElementID]Component{}
	var order []common.ElementID
	for _, pkg := range document.Packages {
		component := fromSPDX(pkg, scope[:12])
		refOf[pkg.PackageSPDXIdentifier] = component.Ref
		components[pkg.PackageSPDXIdentifier] = component
		order = append(order, pkg.PackageSPDXIdentifier)
	}
	described := map[common.ElementID]bool{}
	for _, relationship := range document.Relationships {
		a, b := relationship.RefA, relationship.RefB
		if a.DocumentRefID != "" || b.DocumentRefID != "" {
			continue
		}
		switch strings.ToUpper(relationship.Relationship) {
		case "DESCRIBES":
			if a.ElementRefID == "DOCUMENT" {
				described[b.ElementRefID] = true
			}
		case "DESCRIBED_BY":
			if b.ElementRefID == "DOCUMENT" {
				described[a.ElementRefID] = true
			}
		case "DEPENDS_ON", "CONTAINS", "DYNAMIC_LINK", "STATIC_LINK", "RUNTIME_DEPENDENCY_OF":
			if refOf[a.ElementRefID] != "" && refOf[b.ElementRefID] != "" {
				from, to := refOf[a.ElementRefID], refOf[b.ElementRefID]
				if strings.ToUpper(relationship.Relationship) == "RUNTIME_DEPENDENCY_OF" {
					from, to = to, from
				}
				imported.Dependencies[from] = append(imported.Dependencies[from], to)
			}
		case "DEPENDENCY_OF", "CONTAINED_BY":
			if refOf[a.ElementRefID] != "" && refOf[b.ElementRefID] != "" {
				imported.Dependencies[refOf[b.ElementRefID]] = append(imported.Dependencies[refOf[b.ElementRefID]], refOf[a.ElementRefID])
			}
		}
	}
	for _, id := range order {
		if described[id] {
			imported.Described = append(imported.Described, components[id])
			if imported.Reference.DescribedID == "" {
				imported.Reference.DescribedID = string(id)
			}
		} else {
			imported.Components = append(imported.Components, components[id])
		}
	}
	for _, other := range document.OtherLicenses {
		imported.ExtractedLicenses = append(imported.ExtractedLicenses, ExtractedLicense{
			ID:   other.LicenseIdentifier,
			Name: other.LicenseName,
			Text: other.ExtractedText,
		})
	}
	return imported, nil
}

func fromSPDX(pkg *spdx.Package, scope string) Component {
	component := Component{
		Type:        TypeLibrary,
		Name:        pkg.PackageName,
		Version:     pkg.PackageVersion,
		Description: pkg.PackageDescription,
	}
	if t, ok := spdxPurposeTypes[strings.ToUpper(pkg.PrimaryPackagePurpose)]; ok {
		component.Type = t
	}
	for _, ref := range pkg.PackageExternalReferences {
		category := strings.ReplaceAll(strings.ToUpper(ref.Category), "_", "-")
		switch {
		case category == "PACKAGE-MANAGER" && ref.RefType == "purl" && component.PURL == "":
			component.PURL = ref.Locator
		case category == "SECURITY" && (ref.RefType == "cpe23Type" || ref.RefType == "cpe22Type"):
			component.CPEs = append(component.CPEs, ref.Locator)
		}
	}
	component.Ref = component.PURL
	if component.Ref == "" {
		component.Ref = "spdx:" + scope + ":" + string(pkg.PackageSPDXIdentifier)
	}
	if pkg.PackageSupplier != nil && pkg.PackageSupplier.Supplier != "" && pkg.PackageSupplier.Supplier != "NOASSERTION" {
		component.Supplier = pkg.PackageSupplier.Supplier
	}
	for _, expression := range []string{pkg.PackageLicenseConcluded, pkg.PackageLicenseDeclared} {
		if expression != "" && expression != "NOASSERTION" && expression != "NONE" {
			component.Licenses = []License{{Expression: expression}}
			break
		}
	}
	if location := pkg.PackageDownloadLocation; location != "" && location != "NOASSERTION" && location != "NONE" {
		component.DownloadURL = location
	}
	for _, checksum := range pkg.PackageChecksums {
		if component.Hashes == nil {
			component.Hashes = map[string]string{}
		}
		component.Hashes[cycloneDXHashAlgorithm(string(checksum.Algorithm))] = checksum.Value
	}
	return component
}

// cycloneDXHashAlgorithm maps an SPDX checksum algorithm (SHA256) to CycloneDX's name (SHA-256).
func cycloneDXHashAlgorithm(algorithm string) string {
	switch algorithm {
	case "SHA1":
		return "SHA-1"
	case "SHA256":
		return "SHA-256"
	case "SHA384":
		return "SHA-384"
	case "SHA512":
		return "SHA-512"
	case "SHA3-256", "SHA3-384", "SHA3-512", "MD5":
		return algorithm
	}
	return algorithm
}
