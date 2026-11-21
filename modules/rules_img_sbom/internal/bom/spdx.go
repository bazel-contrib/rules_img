package bom

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"

	supplychain "github.com/bazel-contrib/supply-chain/lib/supplychain-go"
	spdxjson "github.com/spdx/tools-golang/json"
	spdx "github.com/spdx/tools-golang/spdx"
	"github.com/spdx/tools-golang/spdx/v2/common"
)

// PrimarySPDXID is the SPDX identifier of the package describing the image.
const PrimarySPDXID = "SPDXRef-Image"

var spdxPurposes = map[string]string{
	TypeApplication:     "APPLICATION",
	TypeContainer:       "CONTAINER",
	TypeFile:            "FILE",
	TypeFramework:       "FRAMEWORK",
	TypeLibrary:         "LIBRARY",
	TypeOperatingSystem: "OPERATING-SYSTEM",
}

var spdxChecksumAlgorithms = map[string]common.ChecksumAlgorithm{
	"SHA-1":   common.SHA1,
	"SHA-256": common.SHA256,
	"SHA-384": common.SHA384,
	"SHA-512": common.SHA512,
	"MD5":     common.MD5,
}

// WriteSPDX encodes doc as SPDX 2.3 JSON. namespace is the document's
// (unique) namespace.
func WriteSPDX(doc Document, namespace string) ([]byte, error) {
	idOf := map[string]string{doc.Primary.Ref: PrimarySPDXID}
	for _, c := range doc.Components {
		sum := sha256.Sum256([]byte(c.Ref))
		idOf[c.Ref] = "SPDXRef-Package-" + hex.EncodeToString(sum[:8])
	}

	var externalDocuments []spdx.ExternalDocumentRef
	externalIDs := map[string]string{}
	var relationships []*spdx.Relationship
	relate := func(a, b common.DocElementID, kind string) {
		relationships = append(relationships, &spdx.Relationship{RefA: a, RefB: b, Relationship: kind})
	}
	relate(common.MakeDocElementID("", "DOCUMENT"), common.MakeDocElementID("", PrimarySPDXID), "DESCRIBES")

	licenseOf := map[*spdx.Package]*supplychain.LicenseAttribute{}
	toPackage := func(c Component, id string) *spdx.Package {
		name := c.Name
		if c.Group != "" {
			name = c.Group + "/" + name
		}
		pkg := &spdx.Package{
			PackageSPDXIdentifier:   common.ElementID(strings.TrimPrefix(id, "SPDXRef-")),
			PackageName:             name,
			PackageVersion:          c.Version,
			PackageDownloadLocation: "NOASSERTION",
			FilesAnalyzed:           false,
			PackageLicenseConcluded: "NOASSERTION",
			PackageLicenseDeclared:  "NOASSERTION",
			PackageCopyrightText:    "NOASSERTION",
			PackageDescription:      c.Description,
			PrimaryPackagePurpose:   spdxPurposes[c.Type],
		}
		if c.DownloadURL != "" {
			pkg.PackageDownloadLocation = c.DownloadURL
		}
		if c.Supplier != "" {
			supplier := c.Supplier
			supplierType := "Organization"
			for _, prefix := range []string{"Organization: ", "Person: "} {
				if strings.HasPrefix(supplier, prefix) {
					supplierType = strings.TrimSuffix(prefix, ": ")
					supplier = strings.TrimPrefix(supplier, prefix)
				}
			}
			pkg.PackageSupplier = &common.Supplier{Supplier: supplier, SupplierType: supplierType}
		}
		if c.PURL != "" {
			pkg.PackageExternalReferences = append(pkg.PackageExternalReferences, &spdx.PackageExternalReference{
				Category: "PACKAGE-MANAGER", RefType: "purl", Locator: c.PURL,
			})
		}
		for _, cpe := range c.CPEs {
			refType := "cpe23Type"
			if strings.HasPrefix(cpe, "cpe:/") {
				refType = "cpe22Type"
			}
			pkg.PackageExternalReferences = append(pkg.PackageExternalReferences, &spdx.PackageExternalReference{
				Category: "SECURITY", RefType: refType, Locator: cpe,
			})
		}
		for _, algorithm := range sortedKeys(c.Hashes) {
			if spdxAlgorithm, ok := spdxChecksumAlgorithms[algorithm]; ok {
				pkg.PackageChecksums = append(pkg.PackageChecksums, common.Checksum{Algorithm: spdxAlgorithm, Value: c.Hashes[algorithm]})
			}
		}
		for _, license := range c.Licenses {
			switch {
			case license.Attribute != nil:
				licenseOf[pkg] = license.Attribute
			case license.Expression != "":
				pkg.PackageLicenseDeclared = license.Expression
			}
		}
		for _, external := range c.ExternalBOMs {
			if external.Format == "spdx" && external.URL != "" && external.SHA1 != "" {
				refID, ok := externalIDs[external.URL]
				if !ok {
					refID = fmt.Sprintf("DocumentRef-%d", len(externalDocuments))
					externalIDs[external.URL] = refID
					externalDocuments = append(externalDocuments, spdx.ExternalDocumentRef{
						DocumentRefID: common.DocumentID(strings.TrimPrefix(refID, "DocumentRef-")),
						URI:           external.URL,
						Checksum:      common.Checksum{Algorithm: common.SHA1, Value: external.SHA1},
					})
				}
				described := external.DescribedID
				if described == "" {
					described = "SPDXRef-DOCUMENT"
				}
				relate(
					common.MakeDocElementID("", id),
					common.MakeDocElementID(strings.TrimPrefix(refID, "DocumentRef-"), strings.TrimPrefix(described, "SPDXRef-")),
					"DESCRIBED_BY",
				)
			} else if external.URL != "" {
				// A CycloneDX SBOM can't be an external SPDX document.
				pkg.PackageExternalReferences = append(pkg.PackageExternalReferences, &spdx.PackageExternalReference{
					Category: "OTHER", RefType: "cyclonedx-bom", Locator: external.URL,
					ExternalRefComment: "CycloneDX SBOM describing this package (sha256:" + external.SHA256 + ")",
				})
			}
		}
		return pkg
	}

	packages := []*spdx.Package{toPackage(doc.Primary, PrimarySPDXID)}
	for _, c := range doc.Components {
		packages = append(packages, toPackage(c, idOf[c.Ref]))
	}
	for _, from := range sortedKeys(doc.Dependencies) {
		for _, to := range doc.Dependencies[from] {
			if idOf[from] != "" && idOf[to] != "" {
				relate(
					common.MakeDocElementID("", strings.TrimPrefix(idOf[from], "SPDXRef-")),
					common.MakeDocElementID("", strings.TrimPrefix(idOf[to], "SPDXRef-")),
					"DEPENDS_ON",
				)
			}
		}
	}

	otherLicenses, err := declaredLicenses23(packages, licenseOf)
	if err != nil {
		return nil, err
	}
	for _, extracted := range doc.ExtractedLicenses {
		otherLicenses = append(otherLicenses, &spdx.OtherLicense{
			LicenseIdentifier: extracted.ID,
			LicenseName:       extracted.Name,
			ExtractedText:     extracted.Text,
		})
	}

	created := doc.Created
	if created.IsZero() {
		created = time.Unix(0, 0)
	}
	document := &spdx.Document{
		SPDXVersion:                "SPDX-2.3",
		DataLicense:                "CC0-1.0",
		SPDXIdentifier:             "DOCUMENT",
		DocumentName:               doc.Primary.Name,
		DocumentNamespace:          namespace,
		ExternalDocumentReferences: externalDocuments,
		CreationInfo:               &spdx.CreationInfo{Creators: []common.Creator{{Creator: ToolName, CreatorType: "Tool"}}, Created: created.UTC().Format(time.RFC3339)},
		Packages:                   packages,
		OtherLicenses:              dedupeOtherLicenses(otherLicenses),
		Relationships:              relationships,
	}
	var buf bytes.Buffer
	if err := spdxjson.Write(document, &buf, spdxjson.Indent("  ")); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func dedupeOtherLicenses(licenses []*spdx.OtherLicense) []*spdx.OtherLicense {
	seen := map[string]bool{}
	var out []*spdx.OtherLicense
	for _, l := range licenses {
		if !seen[l.LicenseIdentifier] {
			seen[l.LicenseIdentifier] = true
			out = append(out, l)
		}
	}
	return out
}

// SPDXNamespace returns a namespace for a document, derived from what it
// describes so a rebuild produces the same document.
func SPDXNamespace(primaryRef string) string {
	return "https://spdx.org/spdxdocs/" + url.PathEscape(primaryRef)
}
