package bom

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	packageurl "github.com/package-url/packageurl-go"

	"github.com/bazel-contrib/rules_img_sbom/internal/graph"
)

// Base image SBOM handling.
const (
	// BaseInherit copies the components of the base image's SBOM.
	BaseInherit = "inherit"
	// BaseReference only refers to the base image's SBOM.
	BaseReference = "reference"
	// BaseIgnore leaves the base image's SBOM out.
	BaseIgnore = "ignore"
)

// Descriptor is an OCI descriptor.
type Descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant,omitempty"`
	} `json:"platform,omitempty"`
}

// BaseImage identifies a pulled base image manifest.
type BaseImage struct {
	Repository string   `json:"repository"`
	Registries []string `json:"registries"`
	Digest     string   `json:"digest"`
}

// BaseSBOM is an SBOM of the base image (see Unwrap for the supported encodings).
type BaseSBOM struct {
	Path     string `json:"path"`
	Format   string `json:"format"`
	Encoding string `json:"encoding"`
	// Digest of the SBOM's referrer manifest, if it was pulled.
	Digest string `json:"digest"`
	// Scope is "manifest" for an SBOM of the base manifest, or "index" for one of
	// the index it was selected from.
	Scope string `json:"scope"`
}

// Manifest describes an image manifest to write an SBOM for.
type Manifest struct {
	Name       string
	Descriptor Descriptor
	Graph      graph.GraphConfig
	BaseImage  *BaseImage
	BaseSBOMs  []BaseSBOM
	BaseMode   string
	// Format of the SBOM being written: base SBOMs in this format are preferred.
	Format  string
	Created time.Time
	// Log receives warnings (an unreadable base SBOM, ...).
	Log io.Writer
}

// Child is a child manifest of an image index.
type Child struct {
	Descriptor Descriptor
	// SBOM is the path of the child's SBOM, or "".
	SBOM string
	// SBOMArtifact is the digest of the artifact carrying the child's SBOM, or "".
	SBOMArtifact string
}

// How an index's SBOM incorporates the SBOMs of its manifests.
const (
	// IndexAggregate copies the components of the manifests' SBOMs into the
	// index's, which scanners that read the SBOM of the reference they are
	// given (an index, for a multi-platform tag) need.
	IndexAggregate = "aggregate"
	// IndexReference only lists the manifests and refers to their SBOMs.
	IndexReference = "reference"
)

// Index describes an image index to write an SBOM for.
type Index struct {
	Name       string
	Descriptor Descriptor
	Children   []Child
	// Mode is IndexAggregate (the default) or IndexReference.
	Mode string
	// OmitChildReferences leaves out the references to the children's SBOMs,
	// for children whose SBOMs aren't published with the index.
	OmitChildReferences bool
	Created             time.Time
}

// ImagePURL returns the Docker package URL of an image named name (a
// repository path) at digest.
func ImagePURL(name, digest string, qualifiers map[string]string) string {
	namespace, base := "", name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		namespace, base = name[:i], name[i+1:]
	}
	return packageurl.NewPackageURL("docker", namespace, base, digest, packageurl.QualifiersFromMap(qualifiers), "").ToString()
}

// pulledImageComponent describes an image pulled from a registry, whose
// repository is known: it has a package URL. Like a built image, it is
// identified by its digest as its SHA-256 hash, not as a version.
func pulledImageComponent(repository, digest string, qualifiers map[string]string) Component {
	purl := ImagePURL(repository, digest, qualifiers)
	component := Component{Ref: purl, Type: TypeContainer, Name: repository, PURL: purl}
	component.Hashes = digestHashes(digest)
	return component
}

// builtImageComponent describes an image built by Bazel. Where it will be
// pushed is unknown when its SBOM is built (one SBOM serves every push of the
// image, to any registry), so it has no package URL: every package URL type
// for images needs the repository's name (pkg:docker even defaults to Docker
// Hub without a repository_url). It is identified by its digest, as its SHA-256
// hash (it has no version), and by name@digest as its reference. name, the image
// target's label, is the display name, followed by what the image is for: its
// platform ("linux/amd64"), or "multi-arch" for an index.
func builtImageComponent(name, digest, platform string) Component {
	display := name
	if platform != "" {
		display += " (" + platform + ")"
	}
	return Component{
		Ref:    name + "@" + digest,
		Type:   TypeContainer,
		Name:   display,
		Hashes: digestHashes(digest),
	}
}

// multiArch is what an index's display name says it is for.
const multiArch = "multi-arch"

func digestHashes(digest string) map[string]string {
	if hex, ok := strings.CutPrefix(digest, "sha256:"); ok {
		return map[string]string{"SHA-256": hex}
	}
	return nil
}

// platformOf returns the platform a manifest's descriptor names, as
// os/arch[/variant], or "" if it names none.
func platformOf(d Descriptor) string {
	if d.Platform == nil || d.Platform.OS == "" {
		return ""
	}
	platform := d.Platform.OS + "/" + d.Platform.Architecture
	if d.Platform.Variant != "" {
		platform += "/" + d.Platform.Variant
	}
	return platform
}

// ComposeManifest builds the SBOM of an image manifest: the image, the Bazel
// packages of its layers, and its base image (with the base image's SBOM
// inherited or referenced, as BaseMode says).
func ComposeManifest(m Manifest) (Document, error) {
	log := m.Log
	if log == nil {
		log = io.Discard
	}
	b := NewBuilder(builtImageComponent(m.Name, m.Descriptor.Digest, platformOf(m.Descriptor)), m.Created)
	primary := b.Primary().Ref
	if err := AddBazelPackages(b, m.Graph); err != nil {
		return Document{}, err
	}

	baseRef := ""
	if m.BaseImage != nil && m.BaseImage.Digest != "" {
		qualifiers := map[string]string{}
		if len(m.BaseImage.Registries) > 0 {
			qualifiers["repository_url"] = m.BaseImage.Registries[0]
		}
		base := pulledImageComponent(m.BaseImage.Repository, m.BaseImage.Digest, qualifiers)
		b.Add(base)
		b.Depend(primary, base.Ref)
		baseRef = base.Ref
	}

	if m.BaseMode == BaseIgnore {
		return b.Document(), nil
	}
	var imported *Imported
	var used BaseSBOM
	for _, candidate := range orderBaseSBOMs(m.BaseSBOMs, m.Format) {
		data, err := os.ReadFile(candidate.Path)
		if err != nil {
			return Document{}, fmt.Errorf("reading base SBOM: %w", err)
		}
		doc, err := Read(data)
		if err != nil {
			fmt.Fprintf(log, "warning: skipping base SBOM %s (%s): %v\n", candidate.Digest, candidate.Path, err)
			continue
		}
		imported, used = doc, candidate
		break
	}
	if imported == nil {
		return b.Document(), nil
	}

	reference := imported.Reference
	reference.Artifact = used.Digest
	if baseRef == "" && len(imported.Described) > 0 {
		// A base built in this build: its own SBOM describes it.
		described := imported.Described[0]
		if described.Type == "" || described.Type == TypeLibrary {
			described.Type = TypeContainer
		}
		b.Add(described)
		b.Depend(primary, described.Ref)
		baseRef = described.Ref
	}
	if baseRef == "" {
		fmt.Fprintf(log, "warning: the base SBOM %s describes nothing; it is not attributed to a base image\n", used.Path)
		return b.Document(), nil
	}
	base := b.Component(baseRef)
	base.ExternalBOMs = append(base.ExternalBOMs, reference)
	if len(imported.Described) > 0 && base.Ref != imported.Described[0].Ref {
		// The SBOMs the base SBOM itself points to for its subject (a full SBOM
		// in an SBOM database, say, when the attached one is only a stub).
		base.ExternalBOMs = append(base.ExternalBOMs, imported.Described[0].ExternalBOMs...)
	}

	if m.BaseMode == BaseInherit {
		inherit(b, imported, baseRef)
	}
	return b.Document(), nil
}

// inherit copies the components of an imported SBOM, and the dependencies
// between them, into b. What the imported SBOM describes becomes subjectRef,
// a component of b.
func inherit(b *Builder, imported *Imported, subjectRef string) {
	refOf := map[string]string{}
	for _, described := range imported.Described {
		refOf[described.Ref] = subjectRef
	}
	mapRef := func(ref string) string {
		if mapped, ok := refOf[ref]; ok {
			return mapped
		}
		return ref
	}
	for _, c := range imported.Components {
		b.Add(c)
	}
	dependedOn := map[string]bool{}
	hasSubjectDeps := false
	for from, tos := range imported.Dependencies {
		for _, to := range tos {
			b.Depend(mapRef(from), mapRef(to))
			dependedOn[to] = true
			if mapRef(from) == subjectRef {
				hasSubjectDeps = true
			}
		}
	}
	if !hasSubjectDeps {
		// The SBOM doesn't say what its subject depends on: everything in it is
		// part of the subject.
		for _, c := range imported.Components {
			if !dependedOn[c.Ref] {
				b.Depend(subjectRef, c.Ref)
			}
		}
	}
	b.AddExtractedLicenses(imported.ExtractedLicenses)
}

// orderBaseSBOMs puts the SBOMs of the base manifest before those of the index
// it was selected from (which describe every platform), and within each, those
// in the given format first, so a base SBOM is only converted between formats
// when there is no better one. The order is kept otherwise.
func orderBaseSBOMs(sboms []BaseSBOM, format string) []BaseSBOM {
	rank := func(s BaseSBOM) int {
		r := 0
		if s.Scope == "index" {
			r += 2
		}
		if format != "" && s.Format != format {
			r++
		}
		return r
	}
	ordered := append([]BaseSBOM(nil), sboms...)
	sort.SliceStable(ordered, func(i, j int) bool { return rank(ordered[i]) < rank(ordered[j]) })
	return ordered
}

// ComposeIndex builds the SBOM of an image index: the index, and each child
// manifest with a reference to the child's own SBOM. A child is the primary
// component of its own SBOM (the same reference and name), so the two
// documents name it alike. Unless the mode is IndexReference, the components of
// the children's SBOMs are copied in too (once, if several children share
// them), each child depending on its own. With IndexReference, the index's SBOM
// is only a list of its manifests and their SBOMs: CycloneDX gets no dependency
// graph, and the references to the children's SBOMs no comments.
func ComposeIndex(i Index) (Document, error) {
	b := NewBuilder(builtImageComponent(i.Name, i.Descriptor.Digest, multiArch), i.Created)
	primary := b.Primary().Ref
	reference := i.Mode == IndexReference
	for _, child := range i.Children {
		// Named after the index if the child has no SBOM to take its name from.
		component := builtImageComponent(i.Name, child.Descriptor.Digest, platformOf(child.Descriptor))
		var imported *Imported
		if child.SBOM != "" {
			data, err := os.ReadFile(child.SBOM)
			if err != nil {
				return Document{}, fmt.Errorf("reading the SBOM of %s: %w", child.Descriptor.Digest, err)
			}
			if imported, err = Read(data); err != nil {
				return Document{}, fmt.Errorf("reading the SBOM of %s: %w", child.Descriptor.Digest, err)
			}
			if len(imported.Described) > 0 && imported.Described[0].Ref != "" {
				component.Ref = imported.Described[0].Ref
				component.Name = imported.Described[0].Name
			}
			if !i.OmitChildReferences {
				external := imported.Reference
				external.Artifact = child.SBOMArtifact
				external.OmitComment = reference
				component.ExternalBOMs = []ExternalBOM{external}
			}
		}
		b.Add(component)
		b.Depend(primary, component.Ref)
		if imported != nil && !reference {
			inherit(b, imported, component.Ref)
		}
	}
	doc := b.Document()
	// SPDX keeps the relationships: they are what connects its packages to the
	// index.
	doc.OmitCycloneDXDependencies = reference
	return doc, nil
}
