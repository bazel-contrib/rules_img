package bom_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazel-contrib/rules_img_sbom/internal/bom"
	"github.com/bazel-contrib/rules_img_sbom/internal/layerscan"
)

func writeScan(t *testing.T, dir, name string, result layerscan.Result) string {
	t.Helper()
	result.Version = layerscan.Version
	data, err := json.Marshal(result)
	require.NoError(t, err)
	return writeFile(t, dir, name, string(data))
}

const status = "/var/lib/dpkg/status"

// The base image's layer has the distribution and its packages; the image's
// own layer upgrades one of them (rewriting the package database) and adds a
// Go binary, built from the module the Bazel package metadata describes too.
func scannedLayers(t *testing.T, dir string) []bom.Layer {
	base := writeScan(t, dir, "base.scan.json", layerscan.Result{
		Layer:  layerscan.Layer{Digest: "sha256:base"},
		Distro: &layerscan.Distro{ID: "debian", VersionID: "12", PrettyName: "Debian GNU/Linux 12 (bookworm)"},
		Packages: []layerscan.Package{
			{Name: "libc6", Version: "2.36-9", Type: "deb", Kind: "library", PURL: "pkg:deb/debian/libc6@2.36-9?arch=amd64&distro=debian-12", Locations: []string{status}},
			{Name: "openssl", Version: "3.0.1", Type: "deb", Kind: "library", PURL: "pkg:deb/debian/openssl@3.0.1?arch=amd64&distro=debian-12", Locations: []string{status}},
			{Name: "busybox", Version: "1.36", Type: "binary", Kind: "application", PURL: "pkg:generic/busybox@1.36", Locations: []string{"/bin/busybox"}},
		},
		Paths:     []string{"/bin/busybox", "/etc/os-release", status},
		Whiteouts: []string{},
	})
	app := writeScan(t, dir, "app.scan.json", layerscan.Result{
		Layer: layerscan.Layer{Digest: "sha256:app"},
		Packages: []layerscan.Package{
			{Name: "libc6", Version: "2.36-9", Type: "deb", Kind: "library", Locations: []string{status},
				PURLTemplate: &layerscan.PURLTemplate{Type: "deb", Name: "libc6", Version: "2.36-9", Qualifiers: map[string]string{"arch": "amd64"}}},
			{Name: "openssl", Version: "3.0.2", Type: "deb", Kind: "library", Locations: []string{status},
				PURLTemplate: &layerscan.PURLTemplate{Type: "deb", Name: "openssl", Version: "3.0.2", Qualifiers: map[string]string{"arch": "amd64"}}},
			{Name: "github.com/example/module", Version: "v1.2.3", Type: "go-module", Kind: "library", PURL: "pkg:golang/github.com/example/module@v1.2.3",
				CPEs: []string{"cpe:2.3:a:example:module:v1.2.3:*:*:*:*:*:*:*"}, Licenses: []layerscan.License{{Expression: "MIT"}}, Locations: []string{"/app/server"}},
			{Name: "stdlib", Version: "go1.25.1", Type: "go-module", Kind: "library", PURL: "pkg:golang/stdlib@go1.25.1", Locations: []string{"/app/server"}},
		},
		Paths:     []string{"/app/server", status},
		Whiteouts: []string{"/bin/busybox"},
	})
	return []bom.Layer{{Scan: base, Base: true}, {Scan: app}}
}

func TestManifestAddsLayerScans(t *testing.T) {
	m := manifest(t, bom.BaseIgnore, "")
	m.Layers = scannedLayers(t, t.TempDir())
	doc, err := bom.ComposeManifest(m)
	require.NoError(t, err)
	data, err := bom.WriteCycloneDX(doc, "urn:uuid:00000000-0000-5000-8000-000000000000")
	require.NoError(t, err)
	decoded := decodeCycloneDX(t, data)
	got := components(decoded)

	// The Bazel package and the Go module found in the binary are one component:
	// the Bazel package's, completed with what the scan found.
	module := got["pkg:golang/github.com/example/module@v1.2.3"]
	assert.Equal(t, "github.com/example/module", module.Name)
	require.NotNil(t, module.Licenses)
	assert.Equal(t, "Apache-2.0", (*module.Licenses)[0].License.ID, "the Bazel package's license wins")
	assert.Equal(t, "cpe:2.3:a:example:module:v1.2.3:*:*:*:*:*:*:*", module.CPE)
	count := 0
	for _, c := range *decoded.Components {
		if c.PackageURL == "pkg:golang/github.com/example/module@v1.2.3" {
			count++
		}
	}
	assert.Equal(t, 1, count)

	// The standard library, which no Bazel package describes.
	assert.Contains(t, got, "pkg:golang/stdlib@go1.25.1")

	// The image's layer rewrote the package database: what it lists replaces what the base
	// image's did. Its packages get the distribution of the base image's layer.
	assert.Contains(t, got, "pkg:deb/debian/libc6@2.36-9?arch=amd64&distro=debian-12")
	assert.Contains(t, got, "pkg:deb/debian/openssl@3.0.2?arch=amd64&distro=debian-12")
	assert.NotContains(t, got, "pkg:deb/debian/openssl@3.0.1?arch=amd64&distro=debian-12", "upgraded")
	assert.NotContains(t, got, "pkg:generic/busybox@1.36", "deleted by the image's layer")

	// The operating system, which grype needs to match the distribution's packages.
	os := got["os:debian@12"]
	assert.Equal(t, cdxOperatingSystem, string(os.Type))
	assert.Equal(t, "debian", os.Name)
	assert.Equal(t, "12", os.Version)

	// What the base image's layers contain is part of the base image.
	deps := dependencies(decoded)
	assert.Contains(t, deps[basePURL], "os:debian@12")
	assert.Contains(t, deps[basePURL], "pkg:deb/debian/libc6@2.36-9?arch=amd64&distro=debian-12")
	assert.Contains(t, deps[imageRef], "pkg:deb/debian/openssl@3.0.2?arch=amd64&distro=debian-12")
	assert.Contains(t, deps[imageRef], "pkg:golang/stdlib@go1.25.1")
}

const cdxOperatingSystem = "operating-system"

// A scanned package that the base image's SBOM lists is merged into the base SBOM's component,
// and so is the operating system.
func TestManifestMergesLayerScansWithBaseSBOM(t *testing.T) {
	m := manifest(t, bom.BaseInherit, baseCycloneDX)
	dir := t.TempDir()
	m.Layers = []bom.Layer{{Base: true, Scan: writeScan(t, dir, "base.scan.json", layerscan.Result{
		Distro: &layerscan.Distro{ID: "debian", VersionID: "12"},
		Packages: []layerscan.Package{
			{Name: "libc6", Version: "2.36-9", Type: "deb", Kind: "library", PURL: "pkg:deb/debian/libc6@2.36-9?arch=amd64&distro=debian-12&upstream=glibc", Locations: []string{status}},
		},
		Paths: []string{status},
	})}}
	doc, err := bom.ComposeManifest(m)
	require.NoError(t, err)
	data, err := bom.WriteCycloneDX(doc, "urn:uuid:00000000-0000-5000-8000-000000000000")
	require.NoError(t, err)
	decoded := decodeCycloneDX(t, data)

	var libc, os []string
	for _, c := range *decoded.Components {
		switch {
		case c.Name == "libc6":
			libc = append(libc, c.BOMRef)
		case string(c.Type) == cdxOperatingSystem:
			os = append(os, c.BOMRef)
		}
	}
	assert.Equal(t, []string{"pkg:deb/debian/libc6@2.36-9?arch=amd64"}, libc, "one libc6: the base SBOM's")
	assert.Equal(t, []string{"os-debian"}, os, "one operating system: the base SBOM's")
}

// Without a layer that names the distribution, the base SBOM's operating system completes the
// package URLs.
func TestManifestCompletesScannedPURLsWithBaseSBOMDistro(t *testing.T) {
	m := manifest(t, bom.BaseInherit, baseCycloneDX)
	m.Layers = []bom.Layer{{Scan: writeScan(t, t.TempDir(), "app.scan.json", layerscan.Result{
		Packages: []layerscan.Package{{Name: "curl", Version: "7.88", Type: "deb", Kind: "library", Locations: []string{status},
			PURLTemplate: &layerscan.PURLTemplate{Type: "deb", Name: "curl", Version: "7.88", Qualifiers: map[string]string{"arch": "amd64"}}}},
		Paths: []string{status},
	})}}
	doc, err := bom.ComposeManifest(m)
	require.NoError(t, err)
	data, err := bom.WriteCycloneDX(doc, "urn:uuid:00000000-0000-5000-8000-000000000000")
	require.NoError(t, err)
	assert.Contains(t, components(decodeCycloneDX(t, data)), "pkg:deb/debian/curl@7.88?arch=amd64&distro=debian-12")
}

// Without Bazel package metadata, only the scan describes the Go module.
func TestManifestWithoutPackageMetadata(t *testing.T) {
	m := manifest(t, bom.BaseIgnore, "")
	m.OmitPackageMetadata = true
	m.Layers = scannedLayers(t, t.TempDir())
	doc, err := bom.ComposeManifest(m)
	require.NoError(t, err)
	data, err := bom.WriteCycloneDX(doc, "urn:uuid:00000000-0000-5000-8000-000000000000")
	require.NoError(t, err)
	module := components(decodeCycloneDX(t, data))["pkg:golang/github.com/example/module@v1.2.3"]
	require.NotNil(t, module.Licenses)
	assert.Equal(t, "MIT", (*module.Licenses)[0].License.ID, "the scan's license")
}

func TestShadow(t *testing.T) {
	shadow := layerscan.NewShadow()
	shadow.Add(&layerscan.Result{Paths: []string{"/usr/bin/tool", "/lib"}, Whiteouts: []string{"/opt/old"}, OpaqueDirs: []string{"/srv"}})
	for file, hidden := range map[string]bool{
		"/usr/bin/tool":       true,
		"usr/bin/tool":        true,
		"/usr/bin/other":      false,
		"/opt/old/bin/x":      true,
		"/opt/other":          false,
		"/srv/data/file":      true,
		"/lib/libc.so":        true, // /lib is a file (a symlink, say) above
		"/usr/lib/libc.so.6":  false,
		"/var/lib/dpkg/files": false,
	} {
		assert.Equal(t, hidden, shadow.Hides(file), file)
	}
}

// A package of the base image's SBOM that a layer above the base image upgraded is replaced by
// what the upgrade installed, when the base image's layers were scanned too.
func TestManifestDropsBaseSBOMPackagesUpgradedAbove(t *testing.T) {
	m := manifest(t, bom.BaseInherit, baseCycloneDX)
	dir := t.TempDir()
	m.Layers = []bom.Layer{
		{Base: true, Scan: writeScan(t, dir, "base.scan.json", layerscan.Result{
			Distro: &layerscan.Distro{ID: "debian", VersionID: "12"},
			Packages: []layerscan.Package{{Name: "libc6", Version: "2.36-9", Type: "deb", Kind: "library",
				PURL: "pkg:deb/debian/libc6@2.36-9?arch=amd64&distro=debian-12", Locations: []string{status}}},
			Paths: []string{status},
		})},
		{Scan: writeScan(t, dir, "app.scan.json", layerscan.Result{
			Packages: []layerscan.Package{{Name: "libc6", Version: "2.36-10", Type: "deb", Kind: "library", Locations: []string{status},
				PURLTemplate: &layerscan.PURLTemplate{Type: "deb", Name: "libc6", Version: "2.36-10", Qualifiers: map[string]string{"arch": "amd64"}}}},
			Paths: []string{status},
		})},
	}
	doc, err := bom.ComposeManifest(m)
	require.NoError(t, err)
	data, err := bom.WriteCycloneDX(doc, "urn:uuid:00000000-0000-5000-8000-000000000000")
	require.NoError(t, err)
	decoded := decodeCycloneDX(t, data)
	var libc []string
	for _, c := range *decoded.Components {
		if c.Name == "libc6" {
			libc = append(libc, c.Version)
		}
	}
	assert.Equal(t, []string{"2.36-10"}, libc)
	for from, tos := range dependencies(decoded) {
		assert.NotContains(t, tos, "pkg:deb/debian/libc6@2.36-9?arch=amd64", from)
	}
	// The base SBOM's other components stay.
	assert.Contains(t, components(decoded), "os-debian")
}
