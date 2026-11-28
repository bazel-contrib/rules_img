package bom

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	cdx "github.com/CycloneDX/cyclonedx-go"
	"github.com/google/uuid"
	spdxjson "github.com/spdx/tools-golang/json"
	spdx "github.com/spdx/tools-golang/spdx"
	"github.com/spdx/tools-golang/spdx/v2/common"
)

// Destination is where an image is pushed: a registry and a repository on it.
// An SBOM of an image that is pushed somewhere known names the image there, by
// the package URL a pull of it records (see ImagePURL), so the image has the
// same identity in its own SBOM as in the SBOMs of the images built on it.
type Destination struct {
	Registry   string
	Repository string
}

// ReadDestination reads the destination of a push: the registry and repository
// of the push's configuration file (the expanded templates of a push spec), or
// of its destination file ("registry/repository", which wins, as it does for the
// push) if there is one.
func ReadDestination(configuration, destinationFile string) (Destination, error) {
	if destinationFile != "" {
		data, err := os.ReadFile(destinationFile)
		if err != nil {
			return Destination{}, fmt.Errorf("reading the destination file: %w", err)
		}
		line := strings.TrimSpace(string(data))
		registry, repository, ok := strings.Cut(line, "/")
		if strings.ContainsAny(line, "\r\n") || !ok || registry == "" || repository == "" {
			return Destination{}, fmt.Errorf("destination file %s must contain one line, registry/repository, got %q", destinationFile, line)
		}
		return Destination{Registry: registry, Repository: repository}, nil
	}
	data, err := os.ReadFile(configuration)
	if err != nil {
		return Destination{}, fmt.Errorf("reading the push configuration: %w", err)
	}
	var config struct {
		Registry   string `json:"registry"`
		Repository string `json:"repository"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return Destination{}, fmt.Errorf("parsing the push configuration %s: %w", configuration, err)
	}
	if config.Registry == "" || config.Repository == "" {
		return Destination{}, fmt.Errorf("the push configuration %s names no registry and repository", configuration)
	}
	return Destination{Registry: config.Registry, Repository: config.Repository}, nil
}

// PURL returns the package URL of the image with the given digest at d.
func (d Destination) PURL(digest string) string {
	return ImagePURL(d.Repository, digest, map[string]string{"repository_url": d.Registry})
}

// component describes the image with the given digest, pushed to d: what it is
// for (its platform, or multi-arch) follows its repository in its name, as it
// follows the label of an image that isn't pushed (see builtImageComponent).
func (d Destination) component(digest, platform string) Component {
	purl := d.PURL(digest)
	name := d.Repository
	if platform != "" {
		name += " (" + platform + ")"
	}
	return Component{Ref: purl, Type: TypeContainer, Name: name, PURL: purl, Hashes: digestHashes(digest)}
}

// CycloneDXSerial returns the serial number of the CycloneDX document whose
// primary component has the given Ref. It is derived from what the document
// describes, so rebuilding an unchanged image produces the same SBOM, and a
// document that names the image where it is pushed (its Ref is then the
// image's package URL) differs from one that doesn't.
func CycloneDXSerial(primaryRef string) string {
	return "urn:uuid:" + uuid.NewSHA1(uuid.NameSpaceURL, []byte(primaryRef)).String()
}

// SetDestination rewrites an image's SBOM (one of this tool's, in the given
// format) to name the image where it is pushed: its primary component, the
// image with the given digest, gets the image's package URL at d (also as its
// reference, as a pulled image has), and d's repository in place of the
// image's label (label, the image's display name) in its name. The document
// gets a serial number (CycloneDX) or namespace (SPDX) of its own, derived from
// the package URL.
func SetDestination(data []byte, format string, d Destination, digest, label string) ([]byte, error) {
	purl := d.PURL(digest)
	rename := func(name string) string {
		if rest, ok := strings.CutPrefix(name, label); ok && label != "" {
			return d.Repository + rest
		}
		return name
	}
	switch format {
	case "cyclonedx":
		return setCycloneDXDestination(data, purl, rename)
	case "spdx":
		return setSPDXDestination(data, purl, rename)
	default:
		return nil, fmt.Errorf("unknown format %q", format)
	}
}

func setCycloneDXDestination(data []byte, purl string, rename func(string) string) ([]byte, error) {
	var bom cdx.BOM
	if err := cdx.NewBOMDecoder(bytes.NewReader(data), cdx.BOMFileFormatJSON).Decode(&bom); err != nil {
		return nil, fmt.Errorf("parsing CycloneDX: %w", err)
	}
	if bom.Metadata == nil || bom.Metadata.Component == nil {
		return nil, fmt.Errorf("the CycloneDX document describes no component")
	}
	primary := bom.Metadata.Component
	old := primary.BOMRef
	primary.BOMRef = purl
	primary.PackageURL = purl
	primary.Name = rename(primary.Name)
	if bom.Dependencies != nil && old != "" {
		for i := range *bom.Dependencies {
			dependency := &(*bom.Dependencies)[i]
			if dependency.Ref == old {
				dependency.Ref = purl
			}
			if dependency.Dependencies != nil {
				for j, ref := range *dependency.Dependencies {
					if ref == old {
						(*dependency.Dependencies)[j] = purl
					}
				}
			}
		}
	}
	bom.SerialNumber = CycloneDXSerial(purl)

	var buf bytes.Buffer
	encoder := cdx.NewBOMEncoder(&buf, cdx.BOMFileFormatJSON)
	encoder.SetPretty(true)
	if err := encoder.EncodeVersion(&bom, cdx.SpecVersion1_6); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func setSPDXDestination(data []byte, purl string, rename func(string) string) ([]byte, error) {
	document, err := spdxjson.Read(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("parsing SPDX: %w", err)
	}
	described := common.ElementID("")
	for _, relationship := range document.Relationships {
		if strings.EqualFold(relationship.Relationship, "DESCRIBES") && relationship.RefA.ElementRefID == "DOCUMENT" && relationship.RefA.DocumentRefID == "" {
			described = relationship.RefB.ElementRefID
			break
		}
	}
	var primary *spdx.Package
	for _, pkg := range document.Packages {
		if pkg.PackageSPDXIdentifier == described {
			primary = pkg
			break
		}
	}
	if primary == nil {
		return nil, fmt.Errorf("the SPDX document describes no package")
	}
	references := []*spdx.PackageExternalReference{{Category: "PACKAGE-MANAGER", RefType: "purl", Locator: purl}}
	for _, ref := range primary.PackageExternalReferences {
		if !(ref.RefType == "purl" && strings.EqualFold(strings.ReplaceAll(ref.Category, "_", "-"), "PACKAGE-MANAGER")) {
			references = append(references, ref)
		}
	}
	primary.PackageExternalReferences = references
	primary.PackageName = rename(primary.PackageName)
	if document.DocumentName != "" {
		document.DocumentName = rename(document.DocumentName)
	}
	document.DocumentNamespace = SPDXNamespace(purl)

	var buf bytes.Buffer
	if err := spdxjson.Write(document, &buf, spdxjson.Indent("  ")); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
