// Package bom is a format-neutral model of an SBOM, with readers for Bazel
// package metadata and for existing CycloneDX and SPDX documents, and writers
// for both formats. Base image SBOMs can be inherited across formats because
// both sides go through this model.
package bom

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sort"
	"strings"
	"time"

	supplychain "github.com/bazel-contrib/supply-chain/lib/supplychain-go"
	packageurl "github.com/package-url/packageurl-go"
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
	// identities maps the identity of each component (see identity) to its Ref,
	// so Merge finds a component that another source describes too.
	identities map[string]string
}

// NewBuilder starts a document describing primary.
func NewBuilder(primary Component, created time.Time) *Builder {
	return &Builder{
		doc:        Document{Primary: primary, Dependencies: map[string][]string{}, Created: created},
		refs:       map[string]bool{primary.Ref: true},
		deps:       map[string]map[string]bool{},
		identities: map[string]string{},
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
	if id := identity(c); id != "" {
		if _, seen := b.identities[id]; !seen {
			b.identities[id] = c.Ref
		}
	}
	return true
}

// Merge adds a component, or, if the document already has one that is the same
// package (by package URL, see identity) or has the same Ref, completes that one
// with what c knows and it doesn't. It returns the Ref of the component in the
// document.
//
// The component added first wins where both say something different: its Ref,
// name, type and licenses are kept. Package URLs, CPEs, hashes and references to
// other SBOMs are combined.
func (b *Builder) Merge(c Component) string {
	if c.Ref == "" {
		return ""
	}
	existing := b.Find(c)
	if existing == "" {
		b.Add(c)
		return c.Ref
	}
	into := b.Component(existing)
	if into == nil {
		return existing
	}
	mergeInto(into, c)
	return existing
}

// Remove removes a component, and the dependencies from and to it. The
// primary component can't be removed.
func (b *Builder) Remove(ref string) {
	if !b.refs[ref] || ref == b.doc.Primary.Ref {
		return
	}
	delete(b.refs, ref)
	b.doc.Components = slices.DeleteFunc(b.doc.Components, func(c Component) bool { return c.Ref == ref })
	for id, r := range b.identities {
		if r == ref {
			delete(b.identities, id)
		}
	}
	delete(b.deps, ref)
	for _, tos := range b.deps {
		delete(tos, ref)
	}
}

// Find returns the Ref of the component in the document that Merge would merge
// c into, or "" if there is none.
func (b *Builder) Find(c Component) string {
	if b.refs[c.Ref] {
		return c.Ref
	}
	if id := identity(c); id != "" {
		return b.identities[id]
	}
	return ""
}

func mergeInto(into *Component, c Component) {
	fill := func(field *string, value string) {
		if *field == "" {
			*field = value
		}
	}
	fill(&into.Type, c.Type)
	fill(&into.Group, c.Group)
	fill(&into.Name, c.Name)
	fill(&into.Version, c.Version)
	fill(&into.PURL, c.PURL)
	fill(&into.Supplier, c.Supplier)
	fill(&into.Description, c.Description)
	fill(&into.DownloadURL, c.DownloadURL)
	if len(into.Licenses) == 0 {
		into.Licenses = c.Licenses
	}
	for _, cpe := range c.CPEs {
		if !slices.Contains(into.CPEs, cpe) {
			into.CPEs = append(into.CPEs, cpe)
		}
	}
	for algorithm, value := range c.Hashes {
		if into.Hashes == nil {
			into.Hashes = map[string]string{}
		}
		if _, ok := into.Hashes[algorithm]; !ok {
			into.Hashes[algorithm] = value
		}
	}
	for _, external := range c.ExternalBOMs {
		if !slices.ContainsFunc(into.ExternalBOMs, func(e ExternalBOM) bool { return e.URL == external.URL }) {
			into.ExternalBOMs = append(into.ExternalBOMs, external)
		}
	}
}

// identity is what makes two components the same package, whichever source
// describes it: its package URL, without the qualifiers and subpath that only
// say where it came from or how it was found (keeping those that tell builds
// apart: the architecture and epoch of an operating system package), and with
// the names of the package types that are case-insensitive normalized. An
// operating system without a package URL is identified by its name and version.
// Other components have no identity, and are only merged by Ref.
func identity(c Component) string {
	if c.PURL == "" {
		if c.Type == TypeOperatingSystem && c.Name != "" {
			return "os:" + strings.ToLower(c.Name) + "@" + c.Version
		}
		return ""
	}
	purl, err := packageurl.FromString(c.PURL)
	if err != nil {
		return c.PURL
	}
	typ := strings.ToLower(purl.Type)
	namespace, name := purl.Namespace, purl.Name
	switch typ {
	case "pypi":
		name = pypiName(name)
		namespace = strings.ToLower(namespace)
	case "golang", "github", "npm", "deb", "apk", "alpm", "bitbucket", "composer", "hex", "huggingface":
		namespace, name = strings.ToLower(namespace), strings.ToLower(name)
	}
	if typ == "golang" {
		// The standard library has no namespace in some generators' URLs.
		namespace = strings.TrimSuffix(namespace, "/")
	}
	qualifiers := purl.Qualifiers.Map()
	id := typ + "/" + namespace + "/" + name + "@" + purl.Version
	for _, key := range []string{"arch", "epoch"} {
		if value := qualifiers[key]; value != "" {
			id += "&" + key + "=" + value
		}
	}
	return id
}

// pypiName normalizes a Python package name as PEP 503 does.
func pypiName(name string) string {
	name = strings.ToLower(name)
	var out strings.Builder
	dash := false
	for _, r := range name {
		if r == '-' || r == '_' || r == '.' {
			if !dash {
				out.WriteRune('-')
			}
			dash = true
			continue
		}
		dash = false
		out.WriteRune(r)
	}
	return out.String()
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
