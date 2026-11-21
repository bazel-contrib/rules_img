// Package bom is a format-neutral model of an SBOM, with readers for Bazel
// package metadata and for existing CycloneDX and SPDX documents, and writers
// for both formats. Base image SBOMs can be inherited across formats because
// both sides go through this model.
package bom

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"time"

	supplychain "github.com/bazel-contrib/supply-chain/lib/supplychain-go"
)

// Component types, named after CycloneDX's.
const (
	TypeApplication     = "application"
	TypeContainer       = "container"
	TypeFile            = "file"
	TypeFramework       = "framework"
	TypeLibrary         = "library"
	TypeOperatingSystem = "operating-system"
)

// License of a component: exactly one of the fields is set.
type License struct {
	// Expression is an SPDX license expression (possibly a single identifier).
	Expression string
	// Name names a license that has no SPDX identifier.
	Name string
	// Attribute is the license of a Bazel package, rendered with full fidelity
	// (including the text of custom licenses).
	Attribute *supplychain.LicenseAttribute
}

// ExternalBOM refers to another SBOM that describes a component.
type ExternalBOM struct {
	// Format of the referenced document: "cyclonedx" or "spdx".
	Format string
	// URL identifies the document: a CycloneDX BOM-Link (urn:cdx:<serial>/<version>)
	// or an SPDX document namespace.
	URL string
	// SHA256 and SHA1 of the referenced document's bytes.
	SHA256, SHA1 string
	// DescribedID is the SPDX identifier of the element the referenced SPDX
	// document describes, or the bom-ref of a CycloneDX document's subject.
	DescribedID string
	// Artifact is the digest of the OCI artifact carrying the document, if known.
	Artifact string
	// Comment describes the reference. It is generated from Format and Artifact
	// if empty; references read from an imported SBOM keep their own.
	Comment string
	// OmitComment leaves the CycloneDX reference without a comment.
	OmitComment bool
}

// ExtractedLicense is an SPDX license that is not on the SPDX license list.
type ExtractedLicense struct {
	ID, Name, Text string
}

// Component is a package, an image, an operating system, ...
type Component struct {
	// Ref identifies the component within the document (a CycloneDX bom-ref).
	Ref  string
	Type string
	// Group is the CycloneDX group (for example a Maven group id), if any.
	Group       string
	Name        string
	Version     string
	PURL        string
	CPEs        []string
	Licenses    []License
	Supplier    string
	Description string
	DownloadURL string
	// Hashes maps CycloneDX hash algorithm names ("SHA-256", ...) to hex values.
	Hashes       map[string]string
	ExternalBOMs []ExternalBOM
}

// Document is an SBOM.
type Document struct {
	// Primary is what the document describes: the image.
	Primary    Component
	Components []Component
	// Dependencies maps a component's Ref to the Refs it depends on (or contains).
	Dependencies map[string][]string
	// ExtractedLicenses are referenced by LicenseRef-... identifiers in license
	// expressions inherited from SPDX documents.
	ExtractedLicenses []ExtractedLicense
	Created           time.Time
	// OmitCycloneDXDependencies leaves the dependency graph out of the
	// CycloneDX document (an index SBOM that only lists its manifests).
	OmitCycloneDXDependencies bool
}

// Builder assembles a Document, keeping component references unique.
type Builder struct {
	doc  Document
	refs map[string]bool
	deps map[string]map[string]bool
}

// NewBuilder starts a document describing primary.
func NewBuilder(primary Component, created time.Time) *Builder {
	return &Builder{
		doc:  Document{Primary: primary, Dependencies: map[string][]string{}, Created: created},
		refs: map[string]bool{primary.Ref: true},
		deps: map[string]map[string]bool{},
	}
}

// Add adds a component, unless one with the same Ref is already present. It
// reports whether the component was added.
func (b *Builder) Add(c Component) bool {
	if c.Ref == "" || b.refs[c.Ref] {
		return false
	}
	b.refs[c.Ref] = true
	b.doc.Components = append(b.doc.Components, c)
	return true
}

// Has reports whether a component with the given Ref is present.
func (b *Builder) Has(ref string) bool { return b.refs[ref] }

// Depend records that from depends on to.
func (b *Builder) Depend(from, to string) {
	if from == to || from == "" || to == "" {
		return
	}
	if b.deps[from] == nil {
		b.deps[from] = map[string]bool{}
	}
	b.deps[from][to] = true
}

// AddExtractedLicenses adds the extracted licenses that are not present yet.
func (b *Builder) AddExtractedLicenses(licenses []ExtractedLicense) {
	seen := map[string]bool{}
	for _, l := range b.doc.ExtractedLicenses {
		seen[l.ID] = true
	}
	for _, l := range licenses {
		if !seen[l.ID] {
			seen[l.ID] = true
			b.doc.ExtractedLicenses = append(b.doc.ExtractedLicenses, l)
		}
	}
}

// Primary returns the primary component, for modification.
func (b *Builder) Primary() *Component { return &b.doc.Primary }

// Component returns the component with the given Ref, for modification.
func (b *Builder) Component(ref string) *Component {
	if b.doc.Primary.Ref == ref {
		return &b.doc.Primary
	}
	for i := range b.doc.Components {
		if b.doc.Components[i].Ref == ref {
			return &b.doc.Components[i]
		}
	}
	return nil
}

// Document returns the assembled document, in a deterministic order.
func (b *Builder) Document() Document {
	doc := b.doc
	sort.SliceStable(doc.Components, func(i, j int) bool { return doc.Components[i].Ref < doc.Components[j].Ref })
	doc.Dependencies = map[string][]string{}
	for from, tos := range b.deps {
		if !b.refs[from] {
			continue
		}
		for to := range tos {
			if b.refs[to] {
				doc.Dependencies[from] = append(doc.Dependencies[from], to)
			}
		}
		sort.Strings(doc.Dependencies[from])
	}
	sort.Slice(doc.ExtractedLicenses, func(i, j int) bool { return doc.ExtractedLicenses[i].ID < doc.ExtractedLicenses[j].ID })
	return doc
}

// Checksums returns the hex SHA-256 and SHA-1 of data.
func Checksums(data []byte) (string, string) {
	s256 := sha256.Sum256(data)
	s1 := sha1.Sum(data)
	return hex.EncodeToString(s256[:]), hex.EncodeToString(s1[:])
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
