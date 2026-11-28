// Package layerscan is the result of scanning one layer for packages: what the
// scanner (img-sbom scan-layer) writes, and the SBOM writer reads to add the
// packages an image's layers contain to its SBOM. It has no dependency on the
// scanner, so the SBOM writer doesn't link it.
package layerscan

import (
	"encoding/json"
	"os"
	"path"
	"sort"
	"strings"

	packageurl "github.com/package-url/packageurl-go"
)

// Version is the version of the result format.
const Version = 1

// Result is the result of scanning a layer.
type Result struct {
	Version int `json:"version"`
	// Scanner names the scanner and its version.
	Scanner Tool `json:"scanner"`
	// Layer describes the scanned layer.
	Layer Layer `json:"layer"`
	// Distro is the Linux distribution the layer's own os-release (or similar)
	// names, or nil if the layer has none.
	Distro *Distro `json:"distro,omitempty"`
	// Packages found in the layer, in a deterministic order.
	Packages []Package `json:"packages"`
	// Paths are the layer's entries that are not directories, sorted.
	Paths []string `json:"paths"`
	// Whiteouts are the paths the layer deletes from the layers below it.
	Whiteouts []string `json:"whiteouts,omitempty"`
	// OpaqueDirs are the directories whose contents in the layers below the layer
	// hides.
	OpaqueDirs []string `json:"opaque_dirs,omitempty"`
}

// Tool names a tool and its version.
type Tool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Layer describes a scanned layer.
type Layer struct {
	MediaType string `json:"media_type,omitempty"`
	Digest    string `json:"digest,omitempty"`
	DiffID    string `json:"diff_id,omitempty"`
}

// Distro is a Linux distribution, as its os-release describes it.
type Distro struct {
	ID         string   `json:"id"`
	IDLike     []string `json:"id_like,omitempty"`
	VersionID  string   `json:"version_id,omitempty"`
	BuildID    string   `json:"build_id,omitempty"`
	Name       string   `json:"name,omitempty"`
	Version    string   `json:"version,omitempty"`
	PrettyName string   `json:"pretty_name,omitempty"`
	CPEName    string   `json:"cpe_name,omitempty"`
	// Path is the file the distribution was identified from.
	Path string `json:"path,omitempty"`
}

// License of a package: one of the fields is set.
type License struct {
	Expression string `json:"expression,omitempty"`
	Name       string `json:"name,omitempty"`
}

// Package is a package found in a layer.
type Package struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	// Type is the scanner's package type ("deb", "go-module", "python", ...).
	Type string `json:"type"`
	// Kind is the component type the package is: "library", "application",
	// "framework" or "operating-system".
	Kind string `json:"kind"`
	PURL string `json:"purl,omitempty"`
	// PURLTemplate is set for an operating system package whose package URL
	// names its Linux distribution, when the layer doesn't say which that is (it
	// is in a layer below): the package URL is completed with the image's
	// distribution (see FillPURL).
	PURLTemplate *PURLTemplate `json:"purl_template,omitempty"`
	CPEs         []string      `json:"cpes,omitempty"`
	Licenses     []License     `json:"licenses,omitempty"`
	Supplier     string        `json:"supplier,omitempty"`
	// Locations are the files the package was found by (its primary evidence:
	// a package database, a binary, ...).
	Locations []string `json:"locations,omitempty"`
	// FoundBy names the cataloger that found the package.
	FoundBy string `json:"found_by,omitempty"`
}

// PURLTemplate is a package URL without its distribution: the namespace and
// the "distro" qualifier.
type PURLTemplate struct {
	Type       string            `json:"type"`
	Name       string            `json:"name"`
	Version    string            `json:"version,omitempty"`
	Qualifiers map[string]string `json:"qualifiers,omitempty"`
}

// Read reads a result.
func Read(file string) (*Result, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var result Result
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// FillPURL completes a package URL template with a distribution the way syft
// writes the package URLs of operating system packages, or returns "" if the
// package type doesn't fit the distribution (a deb outside Debian and its
// derivatives).
func FillPURL(t PURLTemplate, distro *Distro) string {
	if distro == nil || distro.ID == "" {
		return ""
	}
	namespace := distro.ID
	switch t.Type {
	case packageurl.TypeDebian:
		if distro.ID != "debian" && !contains(distro.IDLike, "debian") {
			return ""
		}
	case packageurl.TypeApk:
		namespace = strings.ToLower(distro.ID)
	case packageurl.TypeRPM:
		if namespace == "rhel" || namespace == "hummingbird" {
			namespace = "redhat"
		}
		if strings.HasPrefix(namespace, "opensuse") {
			namespace = "opensuse"
		}
	}
	release := []string{distro.ID}
	if distro.VersionID != "" {
		release = append(release, distro.VersionID)
	} else if distro.BuildID != "" {
		release = append(release, distro.BuildID)
	}
	values := map[string]string{"distro": strings.Join(release, "-")}
	for key, value := range t.Qualifiers {
		if value != "" {
			values[key] = value
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var qualifiers packageurl.Qualifiers
	for _, key := range keys {
		qualifiers = append(qualifiers, packageurl.Qualifier{Key: key, Value: values[key]})
	}
	return packageurl.NewPackageURL(t.Type, namespace, t.Name, t.Version, qualifiers, "").ToString()
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Shadow tells which files of a layer the layers above it replace or delete.
// Add the layers from the top down, asking about each before adding it.
type Shadow struct {
	paths   map[string]bool
	deleted map[string]bool
	opaque  map[string]bool
}

// NewShadow returns a Shadow of no layers.
func NewShadow() *Shadow {
	return &Shadow{paths: map[string]bool{}, deleted: map[string]bool{}, opaque: map[string]bool{}}
}

// Add adds a layer, the one below those added before.
func (s *Shadow) Add(r *Result) {
	for _, p := range r.Paths {
		s.paths[clean(p)] = true
	}
	for _, p := range r.Whiteouts {
		s.deleted[clean(p)] = true
	}
	for _, p := range r.OpaqueDirs {
		s.opaque[clean(p)] = true
	}
}

// Hides reports whether a file of a layer below those added is replaced or
// deleted by one of them: they have a file (or a whiteout) at its path, or a
// file at, a whiteout of, or an opaque directory in place of one of its parent
// directories.
func (s *Shadow) Hides(file string) bool {
	p := clean(file)
	if s.paths[p] || s.deleted[p] {
		return true
	}
	for dir := path.Dir(p); dir != "/"; dir = path.Dir(dir) {
		if s.paths[dir] || s.deleted[dir] || s.opaque[dir] {
			return true
		}
	}
	return s.opaque["/"]
}

func clean(p string) string {
	return path.Clean("/" + strings.TrimPrefix(p, "./"))
}
