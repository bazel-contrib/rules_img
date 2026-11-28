// Package scan finds the packages in one layer with syft, used as a library:
// the layer is read once, as an uncompressed tar, into a file tree that syft's
// catalogers search (see resolver), with nothing but the layer's own files, and
// without the network.
package scan

import (
	"context"
	"fmt"
	"io"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/anchore/syft/syft"
	"github.com/anchore/syft/syft/artifact"
	"github.com/anchore/syft/syft/cataloging/pkgcataloging"
	"github.com/anchore/syft/syft/file"
	"github.com/anchore/syft/syft/linux"
	"github.com/anchore/syft/syft/pkg"
	"github.com/anchore/syft/syft/source"
	packageurl "github.com/package-url/packageurl-go"

	// The driver syft reads RPM databases (sqlite since RHEL 9 / Fedora 33) with,
	// as the syft CLI registers it.
	_ "modernc.org/sqlite"

	"github.com/bazel-contrib/rules_img_sbom/internal/layerscan"
)

// Options configure a scan.
type Options struct {
	// Layer describes the layer, for the result and syft's description of it.
	Layer layerscan.Layer
	// TempDir holds the spooled file contents ("" is the system's).
	TempDir string
	// Parallelism is the number of catalogers syft runs at once (0 for one per
	// CPU).
	Parallelism int
}

// Scan scans the layer in r, an uncompressed tar.
func Scan(ctx context.Context, r io.Reader, opts Options) (*layerscan.Result, error) {
	index, err := indexTar(r, opts.TempDir)
	if err != nil {
		return nil, err
	}
	defer index.Close()

	src := &layerSource{
		resolver: index.resolver,
		description: source.Description{
			ID:   opts.Layer.Digest,
			Name: opts.Layer.Digest,
			// As a container image (of one layer), so syft picks the catalogers of
			// installed software (package databases, binaries, ...) rather than
			// those of a source tree (lock files).
			Metadata: source.ImageMetadata{
				UserInput: opts.Layer.Digest,
				MediaType: opts.Layer.MediaType,
				Layers:    []source.LayerMetadata{{MediaType: opts.Layer.MediaType, Digest: opts.Layer.Digest}},
			},
		},
	}
	s, err := syft.CreateSBOM(ctx, src, config(opts))
	if err != nil {
		return nil, fmt.Errorf("scanning the layer: %w", err)
	}

	result := &layerscan.Result{
		Version:    layerscan.Version,
		Scanner:    layerscan.Tool{Name: "syft", Version: syftVersion()},
		Layer:      opts.Layer,
		Distro:     distro(s.Artifacts.LinuxDistribution),
		Packages:   []layerscan.Package{},
		Paths:      index.paths,
		Whiteouts:  index.whiteouts,
		OpaqueDirs: index.opaqueDirs,
	}
	if result.Paths == nil {
		result.Paths = []string{}
	}
	for p := range s.Artifacts.Packages.Enumerate() {
		result.Packages = append(result.Packages, convert(p))
	}
	sort.Slice(result.Packages, func(i, j int) bool {
		a, b := result.Packages[i], result.Packages[j]
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		if a.PURL != b.PURL {
			return a.PURL < b.PURL
		}
		return strings.Join(a.Locations, "\n") < strings.Join(b.Locations, "\n")
	})
	return result, nil
}

// config is syft's default configuration, without anything that looks beyond
// the layer: no network (license lookups, Maven Central, ...), and no local
// module or Maven caches of the machine the scan runs on. Files are not
// cataloged: only packages are of interest.
func config(opts Options) *syft.CreateSBOMConfig {
	packages := pkgcataloging.DefaultConfig()
	packages.Golang.SearchRemoteLicenses = false
	packages.Golang.SearchLocalModCacheLicenses = false
	packages.Golang.LocalModCacheDir = ""
	packages.Golang.SearchLocalVendorLicenses = false
	packages.Golang.Proxies = nil
	packages.JavaArchive = packages.JavaArchive.WithUseNetwork(false).WithUseMavenLocalRepository(false)
	packages.JavaScript.SearchRemoteLicenses = false
	packages.Python.SearchRemoteLicenses = false
	return syft.DefaultCreateSBOMConfig().
		WithTool("rules_img_sbom", "").
		WithParallelism(opts.Parallelism).
		WithPackagesConfig(packages).
		WithoutFiles()
}

func syftVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, dep := range info.Deps {
		if dep.Path == "github.com/anchore/syft" {
			return dep.Version
		}
	}
	return ""
}

func distro(release *linux.Release) *layerscan.Distro {
	if release == nil || release.ID == "" {
		return nil
	}
	return &layerscan.Distro{
		ID:         release.ID,
		IDLike:     release.IDLike,
		VersionID:  release.VersionID,
		BuildID:    release.BuildID,
		Name:       release.Name,
		Version:    release.Version,
		PrettyName: release.PrettyName,
		CPEName:    release.CPEName,
	}
}

func convert(p pkg.Package) layerscan.Package {
	out := layerscan.Package{
		Name:    p.Name,
		Version: p.Version,
		Type:    string(p.Type),
		Kind:    "library",
		PURL:    p.PURL,
		FoundBy: p.FoundBy,
	}
	switch p.Type {
	case pkg.BinaryPkg, pkg.LinuxKernelPkg:
		out.Kind = "application"
	}
	if out.PURL == "" {
		out.PURLTemplate = purlTemplate(p)
	}
	for _, c := range p.CPEs {
		out.CPEs = append(out.CPEs, c.Attributes.String())
	}
	for _, l := range p.Licenses.ToSlice() {
		switch {
		case l.SPDXExpression != "":
			out.Licenses = append(out.Licenses, layerscan.License{Expression: l.SPDXExpression})
		case l.Value != "":
			out.Licenses = append(out.Licenses, layerscan.License{Name: l.Value})
		}
	}
	var primary, all []string
	for _, l := range p.Locations.ToSlice() {
		all = append(all, l.RealPath)
		if l.Annotations[pkg.EvidenceAnnotationKey] == pkg.PrimaryEvidenceAnnotation {
			primary = append(primary, l.RealPath)
		}
	}
	if len(primary) == 0 {
		primary = all
	}
	out.Locations = dedupSorted(primary)
	return out
}

// purlTemplate is the package URL of an operating system package without its
// distribution, which syft leaves the package URL empty without: the layer has
// no os-release, it is in a layer below. The qualifiers are those syft writes.
func purlTemplate(p pkg.Package) *layerscan.PURLTemplate {
	switch m := p.Metadata.(type) {
	case pkg.DpkgDBEntry:
		qualifiers := map[string]string{pkg.PURLQualifierArch: m.Architecture}
		if m.Source != "" {
			upstream := m.Source
			if m.SourceVersion != "" {
				upstream += "@" + m.SourceVersion
			}
			qualifiers[pkg.PURLQualifierUpstream] = upstream
		}
		return &layerscan.PURLTemplate{Type: packageurl.TypeDebian, Name: m.Package, Version: m.Version, Qualifiers: qualifiers}
	case pkg.ApkDBEntry:
		qualifiers := map[string]string{pkg.PURLQualifierArch: m.Architecture}
		if m.OriginPackage != m.Package {
			qualifiers[pkg.PURLQualifierUpstream] = m.OriginPackage
		}
		return &layerscan.PURLTemplate{Type: packageurl.TypeApk, Name: m.Package, Version: m.Version, Qualifiers: qualifiers}
	case pkg.RpmDBEntry:
		qualifiers := map[string]string{pkg.PURLQualifierArch: m.Arch, pkg.PURLQualifierUpstream: m.SourceRpm}
		if m.Epoch != nil {
			qualifiers[pkg.PURLQualifierEpoch] = fmt.Sprint(*m.Epoch)
		}
		return &layerscan.PURLTemplate{Type: packageurl.TypeRPM, Name: m.Name, Version: m.Version + "-" + m.Release, Qualifiers: qualifiers}
	}
	return nil
}

func dedupSorted(values []string) []string {
	sort.Strings(values)
	var out []string
	for i, v := range values {
		if i == 0 || v != values[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// layerSource is a syft source of one layer's files.
type layerSource struct {
	resolver    file.Resolver
	description source.Description
}

func (s *layerSource) ID() artifact.ID { return artifact.ID(s.description.ID) }

func (s *layerSource) FileResolver(source.Scope) (file.Resolver, error) { return s.resolver, nil }

func (s *layerSource) Describe() source.Description { return s.description }

func (s *layerSource) Close() error { return nil }
