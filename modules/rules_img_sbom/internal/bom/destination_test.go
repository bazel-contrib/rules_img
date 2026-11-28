package bom_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazel-contrib/rules_img_sbom/internal/bom"
)

var ghcr = bom.Destination{Registry: "ghcr.io", Repository: "buildbarn/bb-storage"}

const (
	pushedImagePURL = "pkg:docker/buildbarn/bb-storage@sha256:1111111111111111111111111111111111111111111111111111111111111111?repository_url=ghcr.io"
	pushedIndexPURL = "pkg:docker/buildbarn/bb-storage@sha256:4444444444444444444444444444444444444444444444444444444444444444?repository_url=ghcr.io"
	indexDigest     = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
)

func TestDestinationPURL(t *testing.T) {
	// The package URL a pull of the image records (see image_purl in rules_img).
	assert.Equal(t, pushedImagePURL, ghcr.PURL(imageDigest))
}

func TestReadDestination(t *testing.T) {
	dir := t.TempDir()
	configuration := writeFile(t, dir, "configuration.json", `{"registry": "ghcr.io", "repository": "buildbarn/bb-storage", "tags": ["latest"]}`)
	d, err := bom.ReadDestination(configuration, "")
	require.NoError(t, err)
	assert.Equal(t, ghcr, d)

	// A destination file wins, as it does for the push.
	d, err = bom.ReadDestination(configuration, writeFile(t, dir, "destination", "registry.example.com/team/app\n"))
	require.NoError(t, err)
	assert.Equal(t, bom.Destination{Registry: "registry.example.com", Repository: "team/app"}, d)

	_, err = bom.ReadDestination(configuration, writeFile(t, dir, "bad", "no-slash\n"))
	assert.Error(t, err)
	_, err = bom.ReadDestination(writeFile(t, dir, "empty.json", `{"tags": ["latest"]}`), "")
	assert.Error(t, err)
}

func TestSetDestinationCycloneDX(t *testing.T) {
	doc, err := bom.ComposeManifest(manifest(t, bom.BaseInherit, baseCycloneDX))
	require.NoError(t, err)
	original, err := bom.WriteCycloneDX(doc, bom.CycloneDXSerial(doc.Primary.Ref))
	require.NoError(t, err)

	patched, err := bom.SetDestination(original, "cyclonedx", ghcr, imageDigest, "app/image")
	require.NoError(t, err)
	before, after := decodeCycloneDX(t, original), decodeCycloneDX(t, patched)

	primary := after.Metadata.Component
	assert.Equal(t, pushedImagePURL, primary.BOMRef, "referenced by its package URL, as a pulled image is")
	assert.Equal(t, pushedImagePURL, primary.PackageURL)
	assert.Equal(t, "buildbarn/bb-storage (linux/arm64/v8)", primary.Name, "the repository replaces the label")
	assert.Equal(t, bom.CycloneDXSerial(pushedImagePURL), after.SerialNumber, "a document of its own")
	assert.NotEqual(t, before.SerialNumber, after.SerialNumber)
	assert.Equal(t, *before.Metadata.Component.Hashes, *primary.Hashes)

	// Everything else is unchanged, and the dependency graph follows the new reference.
	assert.Equal(t, components(before), components(after))
	beforeDeps, afterDeps := dependencies(before), dependencies(after)
	require.Contains(t, beforeDeps, imageRef)
	assert.Equal(t, beforeDeps[imageRef], afterDeps[pushedImagePURL])
	delete(beforeDeps, imageRef)
	delete(afterDeps, pushedImagePURL)
	assert.Equal(t, beforeDeps, afterDeps)
	assert.NotContains(t, string(patched), imageRef, "no reference to the old bom-ref is left")

	// Deterministic.
	again, err := bom.SetDestination(original, "cyclonedx", ghcr, imageDigest, "app/image")
	require.NoError(t, err)
	assert.Equal(t, string(patched), string(again))
}

func TestSetDestinationSPDX(t *testing.T) {
	doc, err := bom.ComposeManifest(manifest(t, bom.BaseInherit, baseCycloneDX))
	require.NoError(t, err)
	original, err := bom.WriteSPDX(doc, bom.SPDXNamespace(doc.Primary.Ref))
	require.NoError(t, err)

	patched, err := bom.SetDestination(original, "spdx", ghcr, imageDigest, "app/image")
	require.NoError(t, err)
	assert.Contains(t, string(patched), bom.SPDXNamespace(pushedImagePURL))
	assert.NotContains(t, string(patched), bom.SPDXNamespace(imageRef))

	// What an index (or a downstream image) reads of it.
	imported, err := bom.Read(patched)
	require.NoError(t, err)
	require.Len(t, imported.Described, 1)
	assert.Equal(t, pushedImagePURL, imported.Described[0].PURL)
	assert.Equal(t, pushedImagePURL, imported.Described[0].Ref)
	assert.Equal(t, "buildbarn/bb-storage (linux/arm64/v8)", imported.Described[0].Name)

	before, err := bom.Read(original)
	require.NoError(t, err)
	assert.Equal(t, len(before.Components), len(imported.Components), "the packages are unchanged")
	assert.Equal(t, 1, strings.Count(string(patched), `"referenceType": "purl",`+"\n"+`          "referenceLocator": "`+pushedImagePURL+`"`), "exactly one purl")
}

func TestIndexWithDestination(t *testing.T) {
	dir := t.TempDir()
	child, err := bom.ComposeManifest(manifest(t, bom.BaseInherit, baseCycloneDX))
	require.NoError(t, err)
	childSBOM, err := bom.WriteCycloneDX(child, bom.CycloneDXSerial(child.Primary.Ref))
	require.NoError(t, err)
	pushedChildSBOM, err := bom.SetDestination(childSBOM, "cyclonedx", ghcr, imageDigest, "app/image")
	require.NoError(t, err)

	const otherDigest = "sha256:5555555555555555555555555555555555555555555555555555555555555555"
	doc, err := bom.ComposeIndex(bom.Index{
		Name:       "app/index",
		Descriptor: bom.Descriptor{MediaType: "application/vnd.oci.image.index.v1+json", Digest: indexDigest, Size: 50},
		Children: []bom.Child{
			{Descriptor: platformDescriptor(imageDigest, "linux", "arm64", "v8"), SBOM: writeFile(t, dir, "child.json", string(pushedChildSBOM)), SBOMArtifact: sbomDigest},
			{Descriptor: platformDescriptor(otherDigest, "linux", "amd64", "")},
		},
		Mode:        bom.IndexAggregate,
		Destination: &ghcr,
	})
	require.NoError(t, err)
	data, err := bom.WriteCycloneDX(doc, bom.CycloneDXSerial(doc.Primary.Ref))
	require.NoError(t, err)
	out := decodeCycloneDX(t, data)

	assert.Equal(t, pushedIndexPURL, out.Metadata.Component.BOMRef)
	assert.Equal(t, pushedIndexPURL, out.Metadata.Component.PackageURL)
	assert.Equal(t, "buildbarn/bb-storage (multi-arch)", out.Metadata.Component.Name)
	assert.Equal(t, bom.CycloneDXSerial(pushedIndexPURL), out.SerialNumber)

	all := components(out)
	// The child with an SBOM is its SBOM's primary component, which names it where it is pushed.
	require.Contains(t, all, pushedImagePURL)
	assert.Equal(t, pushedImagePURL, all[pushedImagePURL].PackageURL)
	assert.Equal(t, "buildbarn/bb-storage (linux/arm64/v8)", all[pushedImagePURL].Name)
	reference := (*all[pushedImagePURL].ExternalReferences)[0]
	assert.Equal(t, "urn:cdx:"+strings.TrimPrefix(bom.CycloneDXSerial(pushedImagePURL), "urn:uuid:")+"/1", reference.URL, "refers to the child's pushed SBOM")
	assert.Equal(t, sha256Hex(pushedChildSBOM), (*reference.Hashes)[0].Value)
	// One without is named after the destination too.
	otherPURL := ghcr.PURL(otherDigest)
	require.Contains(t, all, otherPURL)
	assert.Equal(t, "buildbarn/bb-storage (linux/amd64)", all[otherPURL].Name)
	assert.ElementsMatch(t, []string{pushedImagePURL, otherPURL}, dependencies(out)[pushedIndexPURL])
	require.Contains(t, all, "pkg:golang/github.com/example/module@v1.2.3", "the packages are still aggregated")

	// A child whose SBOM doesn't name it where it is pushed still gets the package URL.
	doc, err = bom.ComposeIndex(bom.Index{
		Name:        "app/index",
		Descriptor:  bom.Descriptor{MediaType: "application/vnd.oci.image.index.v1+json", Digest: indexDigest, Size: 50},
		Children:    []bom.Child{{Descriptor: platformDescriptor(imageDigest, "linux", "arm64", "v8"), SBOM: writeFile(t, dir, "unpushed.json", string(childSBOM))}},
		Mode:        bom.IndexReference,
		Destination: &ghcr,
	})
	require.NoError(t, err)
	require.Len(t, doc.Components, 1)
	assert.Equal(t, imageRef, doc.Components[0].Ref)
	assert.Equal(t, pushedImagePURL, doc.Components[0].PURL)
}
