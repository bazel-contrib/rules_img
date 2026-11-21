package sbom_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cdx "github.com/CycloneDX/cyclonedx-go"
	spdxjson "github.com/spdx/tools-golang/json"
	spdx "github.com/spdx/tools-golang/spdx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	imageDigest  = flag.String("image_digest", "", "digest output of //tests:image")
	baseManifest = flag.String("base_manifest", "", "digest of the base manifest")
	baseSBOM     = flag.String("base_sbom", "", "digest of the base SBOM's referrer")
)

// runfile resolves a $(rootpath): tests run in their package's directory.
func runfile(path string) string {
	return filepath.Join(os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"), path)
}

// files maps the base names of the files in a $(rootpaths) environment variable to their paths.
func files(t *testing.T, variable string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, path := range strings.Fields(os.Getenv(variable)) {
		out[filepath.Base(path)] = runfile(path)
	}
	return out
}

func readCycloneDX(t *testing.T, path string) cdx.BOM {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var bom cdx.BOM
	require.NoError(t, cdx.NewBOMDecoder(bytes.NewReader(data), cdx.BOMFileFormatJSON).Decode(&bom))
	assert.Equal(t, cdx.SpecVersion1_6, bom.SpecVersion)
	return bom
}

func readSPDX(t *testing.T, path string) *spdx.Document {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	doc, err := spdxjson.Read(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, "SPDX-2.3", doc.SPDXVersion)
	return doc
}

func byRef(bom cdx.BOM) map[string]cdx.Component {
	out := map[string]cdx.Component{}
	for _, c := range *bom.Components {
		out[c.BOMRef] = c
	}
	return out
}

func purlsOf(doc *spdx.Document) map[string]*spdx.Package {
	out := map[string]*spdx.Package{}
	for _, pkg := range doc.Packages {
		for _, ref := range pkg.PackageExternalReferences {
			if ref.RefType == "purl" {
				out[ref.Locator] = pkg
			}
		}
	}
	return out
}

func digest(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(runfile(*imageDigest))
	require.NoError(t, err)
	return strings.TrimSpace(string(data))
}

func TestImageSBOMs(t *testing.T) {
	sboms := files(t, "IMAGE_SBOMS")
	require.Len(t, sboms, 2, "one SBOM per format: %v", sboms)
	// Named after the image's label, which isn't where the image is pushed.
	imageRef := "//tests:image@" + digest(t)
	imageHash := strings.TrimPrefix(digest(t), "sha256:")
	basePURL := "pkg:docker/example/base@" + *baseManifest + "?repository_url=registry.example.com"

	t.Run("cyclonedx", func(t *testing.T) {
		bom := readCycloneDX(t, sboms["image.cdx.json"])

		// The SBOM is about the manifest it is attached to.
		primary := bom.Metadata.Component
		assert.Equal(t, cdx.ComponentTypeContainer, primary.Type)
		assert.Empty(t, primary.Version, "the digest is its hash, not a version")
		assert.Equal(t, imageRef, primary.BOMRef)
		assert.Equal(t, "//tests:image (linux/amd64)", primary.Name)
		assert.Empty(t, primary.PackageURL, "where the image is pushed is unknown when its SBOM is built")
		assert.Equal(t, []cdx.Hash{{Algorithm: cdx.HashAlgoSHA256, Value: imageHash}}, *primary.Hashes)

		components := byRef(bom)
		require.Contains(t, components, basePURL, "the selected base manifest is a component")
		require.Contains(t, components, "os-alpine", "the base SBOM's operating system is inherited")
		require.Contains(t, components, "pkg:apk/alpine/musl@1.2.5-r9?arch=x86_64", "the base SBOM's packages are inherited")
		require.Contains(t, components, "pkg:generic/hello@1.0.0", "the layer's package metadata is a component")

		reference := (*components[basePURL].ExternalReferences)[0]
		assert.Equal(t, cdx.ERTypeBOM, reference.Type)
		assert.Equal(t, "urn:cdx:5d4ec0f9-79a5-4f39-a0d2-6b0b6a0c0b11/1", reference.URL)
		assert.Contains(t, reference.Comment, *baseSBOM)

		// Reproducible by default: no creation time.
		assert.Empty(t, bom.Metadata.Timestamp)
	})

	t.Run("spdx", func(t *testing.T) {
		doc := readSPDX(t, sboms["image.spdx.json"])
		assert.Equal(t, "1970-01-01T00:00:00Z", doc.CreationInfo.Created, "SPDX requires a creation time: the epoch by default")
		packages := purlsOf(doc)
		var image *spdx.Package
		for _, pkg := range doc.Packages {
			if pkg.PackageSPDXIdentifier == "Image" {
				image = pkg
			}
		}
		require.NotNil(t, image, "the image is the package SPDXRef-Image")
		assert.Equal(t, "CONTAINER", image.PrimaryPackagePurpose)
		assert.Empty(t, image.PackageVersion, "the digest is its checksum, not a version")
		assert.Empty(t, image.PackageExternalReferences, "where the image is pushed is unknown when its SBOM is built")
		require.Len(t, image.PackageChecksums, 1)
		assert.Equal(t, "SHA256", string(image.PackageChecksums[0].Algorithm))
		assert.Equal(t, imageHash, image.PackageChecksums[0].Value)
		require.Contains(t, packages, basePURL)
		require.Contains(t, packages, "pkg:generic/hello@1.0.0")

		// The base only has a CycloneDX SBOM: it is inherited across formats.
		require.Contains(t, packages, "pkg:apk/alpine/musl@1.2.5-r9?arch=x86_64")
		assert.Equal(t, "MIT", packages["pkg:apk/alpine/musl@1.2.5-r9?arch=x86_64"].PackageLicenseDeclared)
		var purposes []string
		for _, pkg := range doc.Packages {
			purposes = append(purposes, pkg.PrimaryPackagePurpose)
		}
		assert.Contains(t, purposes, "OPERATING-SYSTEM")
	})
}

func TestIndexSBOMs(t *testing.T) {
	sboms := files(t, "INDEX_SBOMS")
	require.Len(t, sboms, 6, "the index ships its own SBOMs and those of its manifests, in both formats: %v", sboms)

	// The CycloneDX SBOM of the index refers to the CycloneDX SBOMs of its manifests.
	// Each manifest's entry is the primary component of the manifest's own SBOM
	// (the same reference and name), and refers to that SBOM.
	type child struct {
		primary cdx.Component
		serial  string
	}
	children := map[string]child{}
	for _, name := range []string{"image.cdx.json", "image_arm64.cdx.json"} {
		bom := readCycloneDX(t, sboms[name])
		children[bom.Metadata.Component.BOMRef] = child{*bom.Metadata.Component, "urn:cdx:" + strings.TrimPrefix(bom.SerialNumber, "urn:uuid:") + "/1"}
	}
	index := readCycloneDX(t, sboms["index.cdx.json"])
	assert.True(t, strings.HasPrefix(index.Metadata.Component.BOMRef, "//tests:index@sha256:"))
	assert.Equal(t, "//tests:index (multi-arch)", index.Metadata.Component.Name)
	assert.Empty(t, index.Metadata.Component.PackageURL)
	var names []string
	for _, c := range *index.Components {
		child, ok := children[c.BOMRef]
		if !ok {
			continue
		}
		assert.Equal(t, child.primary.Name, c.Name, "the same name as in the child's SBOM")
		names = append(names, c.Name)
		assert.Empty(t, c.PackageURL)
		assert.Equal(t, cdx.HashAlgoSHA256, (*c.Hashes)[0].Algorithm)
		require.NotNil(t, c.ExternalReferences)
		assert.Equal(t, child.serial, (*c.ExternalReferences)[0].URL, "should refer to the child's CycloneDX SBOM")
	}
	assert.ElementsMatch(t, []string{"//tests:image (linux/amd64)", "//tests:image_arm64 (linux/arm64/v8)"}, names)

	// A scanner given the index reads only its SBOM: it has the packages of the manifests.
	components := byRef(index)
	require.Contains(t, components, "pkg:generic/hello@1.0.0")
	require.Contains(t, components, "pkg:apk/alpine/musl@1.2.5-r9?arch=x86_64", "the amd64 manifest's base packages")
	require.Contains(t, components, "os-alpine")

	// The SPDX SBOM of the index refers to the SPDX SBOMs of its manifests.
	namespaces := map[string]bool{}
	for _, name := range []string{"image.spdx.json", "image_arm64.spdx.json"} {
		namespaces[readSPDX(t, sboms[name]).DocumentNamespace] = true
	}
	spdxIndex := readSPDX(t, sboms["index.spdx.json"])
	require.Len(t, spdxIndex.ExternalDocumentReferences, 2)
	for _, ref := range spdxIndex.ExternalDocumentReferences {
		assert.True(t, namespaces[ref.URI], "%s should be a child's SPDX SBOM", ref.URI)
	}
	require.Contains(t, purlsOf(spdxIndex), "pkg:generic/hello@1.0.0")
}
