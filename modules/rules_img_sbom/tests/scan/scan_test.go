package scan_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cdx "github.com/CycloneDX/cyclonedx-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestImageSBOM checks the SBOM of //tests/scan:image, built with a toolchain that scans layers
// and also reads Bazel package metadata.
func TestImageSBOM(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"), os.Getenv("IMAGE_SBOM")))
	require.NoError(t, err)
	var bom cdx.BOM
	require.NoError(t, cdx.NewBOMDecoder(bytes.NewReader(data), cdx.BOMFileFormatJSON).Decode(&bom))
	require.NotNil(t, bom.Components)

	purls := map[string]int{}
	byPURL := map[string]cdx.Component{}
	var os []cdx.Component
	for _, c := range *bom.Components {
		if c.PackageURL != "" {
			// Without the qualifiers, which differ between generators.
			purl, _, _ := strings.Cut(c.PackageURL, "?")
			purls[purl]++
			byPURL[purl] = c
		}
		if c.Type == cdx.ComponentTypeOS {
			os = append(os, c)
		}
	}

	// Operating system packages from the package database, with the distribution of the base
	// image's layer, as the image's layer left them: openssl upgraded.
	assert.Contains(t, purls, "pkg:deb/debian/libc6@2.36-9%2Bdeb12u4")
	assert.Contains(t, purls, "pkg:deb/debian/openssl@3.0.2-1")
	assert.NotContains(t, purls, "pkg:deb/debian/openssl@3.0.1-1")
	assert.Equal(t, "pkg:deb/debian/openssl@3.0.2-1?arch=amd64&distro=debian-12", byPURL["pkg:deb/debian/openssl@3.0.2-1"].PackageURL)
	require.Len(t, os, 1)
	assert.Equal(t, "debian", os[0].Name)
	assert.Equal(t, "12", os[0].Version)

	// The Go module in the binary, which the Bazel package metadata also describes: one
	// component, with the package metadata's package URL.
	assert.Equal(t, 1, purls["pkg:golang/github.com/google/uuid@v1.6.0"], "one uuid component: %v", purls)
	assert.Contains(t, byPURL["pkg:golang/github.com/google/uuid@v1.6.0"].PackageURL, "checksum=")
	// The standard library, which only the scan finds.
	found := false
	for purl := range purls {
		if strings.HasPrefix(purl, "pkg:golang/stdlib@") {
			found = true
		}
	}
	assert.True(t, found, "the Go standard library: %v", purls)

	// A Python package, in the EROFS layer.
	assert.Contains(t, purls, "pkg:pypi/requests@2.31.0")
}
