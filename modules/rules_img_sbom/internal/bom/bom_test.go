package bom_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cdx "github.com/CycloneDX/cyclonedx-go"
	spdxjson "github.com/spdx/tools-golang/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazel-contrib/rules_img_sbom/internal/bom"
	"github.com/bazel-contrib/rules_img_sbom/internal/graph"
)

const (
	imageDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	baseDigest  = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	sbomDigest  = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
)

// A base image SBOM as syft writes it: the image as metadata.component, an
// operating-system component (which grype needs to match distro packages) and
// the distro's packages.
const baseCycloneDX = `{
  "bomFormat": "CycloneDX",
  "specVersion": "1.6",
  "serialNumber": "urn:uuid:7a1e1a50-0d35-4b16-9c1e-4a8a4d1b0a01",
  "version": 1,
  "metadata": {"component": {"bom-ref": "base-image", "type": "container", "name": "debian", "version": "sha256:2222"}},
  "components": [
    {"bom-ref": "os-debian", "type": "operating-system", "name": "debian", "version": "12"},
    {"bom-ref": "pkg:deb/debian/libc6@2.36-9?arch=amd64", "type": "library", "name": "libc6", "version": "2.36-9",
     "purl": "pkg:deb/debian/libc6@2.36-9?arch=amd64", "licenses": [{"license": {"id": "LGPL-2.1-or-later"}}],
     "cpe": "cpe:2.3:a:libc6:libc6:2.36-9:*:*:*:*:*:*:*"}
  ]
}`

const baseSPDX = `{
  "spdxVersion": "SPDX-2.3",
  "dataLicense": "CC0-1.0",
  "SPDXID": "SPDXRef-DOCUMENT",
  "name": "debian",
  "documentNamespace": "https://example.com/spdxdocs/debian",
  "creationInfo": {"created": "2026-01-01T00:00:00Z", "creators": ["Tool: buildkit"]},
  "packages": [
    {"SPDXID": "SPDXRef-Image", "name": "debian", "versionInfo": "12", "downloadLocation": "NOASSERTION", "primaryPackagePurpose": "CONTAINER"},
    {"SPDXID": "SPDXRef-OS", "name": "debian", "versionInfo": "12", "downloadLocation": "NOASSERTION", "primaryPackagePurpose": "OPERATING-SYSTEM"},
    {"SPDXID": "SPDXRef-libc6", "name": "libc6", "versionInfo": "2.36-9", "downloadLocation": "NOASSERTION",
     "licenseDeclared": "LicenseRef-libc-extra",
     "externalRefs": [{"referenceCategory": "PACKAGE-MANAGER", "referenceType": "purl", "referenceLocator": "pkg:deb/debian/libc6@2.36-9?arch=amd64"}]}
  ],
  "hasExtractedLicensingInfos": [{"licenseId": "LicenseRef-libc-extra", "name": "libc extra", "extractedText": "some text"}],
  "relationships": [
    {"spdxElementId": "SPDXRef-DOCUMENT", "relationshipType": "DESCRIBES", "relatedSpdxElement": "SPDXRef-Image"},
    {"spdxElementId": "SPDXRef-Image", "relationshipType": "CONTAINS", "relatedSpdxElement": "SPDXRef-OS"},
    {"spdxElementId": "SPDXRef-Image", "relationshipType": "CONTAINS", "relatedSpdxElement": "SPDXRef-libc6"}
  ]
}`

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

// bazelGraph is an image whose layer depends on a Go module with an Apache-2.0 license.
func bazelGraph(t *testing.T, dir string) graph.GraphConfig {
	license := writeFile(t, dir, "license.json", `{"kind": {"identifier": "Apache-2.0", "name": "Apache License 2.0"}}`)
	metadata := writeFile(t, dir, "module.package-metadata.json", `{
		"purl": "pkg:golang/github.com/example/module@v1.2.3",
		"label": "@module//:package_metadata",
		"attributes": {"build.bazel.attribute.license": "`+license+`"}
	}`)
	return graph.GraphConfig{
		SchemaVersion: "1.0",
		RootTarget:    "//app:image",
		Nodes: []graph.NodeConfig{
			{Label: "//app:image"},
			{Label: "//app:layer"},
			{Label: "@module//:lib", MetadataFile: metadata},
		},
		Edges: []graph.EdgeConfig{
			{From: "//app:image", To: "//app:layer", Type: "depends_on"},
			{From: "//app:layer", To: "@module//:lib", Type: "depends_on"},
		},
	}
}

func manifest(t *testing.T, mode, baseSBOM string) bom.Manifest {
	dir := t.TempDir()
	m := bom.Manifest{
		Name:       "app/image",
		Descriptor: platformDescriptor(imageDigest, "linux", "arm64", "v8"),
		Graph:      bazelGraph(t, dir),
		BaseImage:  &bom.BaseImage{Repository: "library/debian", Registries: []string{"index.docker.io"}, Digest: baseDigest},
		BaseMode:   mode,
	}
	if baseSBOM != "" {
		m.BaseSBOMs = []bom.BaseSBOM{{Path: writeFile(t, dir, "base.sbom", baseSBOM), Digest: sbomDigest}}
	}
	return m
}

func platformDescriptor(digest, os, arch, variant string) bom.Descriptor {
	d := bom.Descriptor{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: digest, Size: 100}
	d.Platform = &struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant,omitempty"`
	}{OS: os, Architecture: arch, Variant: variant}
	return d
}

func decodeCycloneDX(t *testing.T, data []byte) cdx.BOM {
	t.Helper()
	var decoded cdx.BOM
	require.NoError(t, cdx.NewBOMDecoder(bytes.NewReader(data), cdx.BOMFileFormatJSON).Decode(&decoded))
	return decoded
}

func components(b cdx.BOM) map[string]cdx.Component {
	out := map[string]cdx.Component{}
	if b.Components != nil {
		for _, c := range *b.Components {
			out[c.BOMRef] = c
		}
	}
	return out
}

func dependencies(b cdx.BOM) map[string][]string {
	out := map[string][]string{}
	if b.Dependencies != nil {
		for _, d := range *b.Dependencies {
			if d.Dependencies != nil {
				out[d.Ref] = *d.Dependencies
			}
		}
	}
	return out
}

// A built image has no package URL (where it is pushed is unknown when its
// SBOM is built): it is identified by its name and digest.
const imageRef = "app/image@sha256:1111111111111111111111111111111111111111111111111111111111111111"
const basePURL = "pkg:docker/library/debian@sha256:2222222222222222222222222222222222222222222222222222222222222222?repository_url=index.docker.io"

func TestManifestInheritsCycloneDX(t *testing.T) {
	doc, err := bom.ComposeManifest(manifest(t, bom.BaseInherit, baseCycloneDX))
	require.NoError(t, err)
	data, err := bom.WriteCycloneDX(doc, "urn:uuid:00000000-0000-5000-8000-000000000000")
	require.NoError(t, err)
	out := decodeCycloneDX(t, data)

	// The primary component is the image, by digest, named after its target and
	// platform.
	primary := out.Metadata.Component
	assert.Equal(t, cdx.ComponentTypeContainer, primary.Type)
	assert.Equal(t, "app/image (linux/arm64/v8)", primary.Name)
	assert.Equal(t, imageRef, primary.BOMRef)
	assert.Empty(t, primary.PackageURL, "a built image's repository is unknown")
	assert.Empty(t, primary.Version, "the digest is its hash, not a version")
	assert.Equal(t, "1111111111111111111111111111111111111111111111111111111111111111", (*primary.Hashes)[0].Value)

	all := components(out)
	require.Contains(t, all, basePURL)
	require.Contains(t, all, "os-debian", "the operating system must be inherited, scanners need it")
	assert.Equal(t, cdx.ComponentTypeOS, all["os-debian"].Type)
	require.Contains(t, all, "pkg:deb/debian/libc6@2.36-9?arch=amd64")
	require.Contains(t, all, "pkg:golang/github.com/example/module@v1.2.3")
	assert.Equal(t, "Apache-2.0", (*all["pkg:golang/github.com/example/module@v1.2.3"].Licenses)[0].License.ID)
	assert.NotContains(t, all, "base-image", "the base SBOM's subject is the base image component")

	// The base image refers to the SBOM it inherited from.
	base := all[basePURL]
	require.NotNil(t, base.ExternalReferences)
	reference := (*base.ExternalReferences)[0]
	assert.Equal(t, cdx.ERTypeBOM, reference.Type)
	assert.Equal(t, "urn:cdx:7a1e1a50-0d35-4b16-9c1e-4a8a4d1b0a01/1", reference.URL)
	assert.Contains(t, reference.Comment, sbomDigest)

	deps := dependencies(out)
	assert.ElementsMatch(t, []string{basePURL, "pkg:golang/github.com/example/module@v1.2.3"}, deps[imageRef])
	assert.ElementsMatch(t, []string{"os-debian", "pkg:deb/debian/libc6@2.36-9?arch=amd64"}, deps[basePURL])
}

func TestManifestReferencesBase(t *testing.T) {
	doc, err := bom.ComposeManifest(manifest(t, bom.BaseReference, baseCycloneDX))
	require.NoError(t, err)
	data, err := bom.WriteCycloneDX(doc, "urn:uuid:00000000-0000-5000-8000-000000000000")
	require.NoError(t, err)
	all := components(decodeCycloneDX(t, data))
	assert.NotContains(t, all, "os-debian", "a reference does not copy the base's components")
	require.Contains(t, all, basePURL)
	assert.Equal(t, cdx.ERTypeBOM, (*all[basePURL].ExternalReferences)[0].Type)
}

func TestManifestInheritsSPDXAcrossFormats(t *testing.T) {
	doc, err := bom.ComposeManifest(manifest(t, bom.BaseInherit, baseSPDX))
	require.NoError(t, err)

	// Into CycloneDX: the purpose becomes the component type.
	data, err := bom.WriteCycloneDX(doc, "urn:uuid:00000000-0000-5000-8000-000000000000")
	require.NoError(t, err)
	all := components(decodeCycloneDX(t, data))
	var types []cdx.ComponentType
	for _, c := range all {
		types = append(types, c.Type)
	}
	assert.Contains(t, types, cdx.ComponentTypeOS)
	require.Contains(t, all, "pkg:deb/debian/libc6@2.36-9?arch=amd64")
	assert.Equal(t, "https://example.com/spdxdocs/debian", (*all[basePURL].ExternalReferences)[0].URL)

	// Into SPDX: the inherited LicenseRef keeps its extracted text.
	data, err = bom.WriteSPDX(doc, bom.SPDXNamespace(imageRef))
	require.NoError(t, err)
	out, err := spdxjson.Read(bytes.NewReader(data))
	require.NoError(t, err)
	var ids []string
	for _, l := range out.OtherLicenses {
		ids = append(ids, l.LicenseIdentifier)
	}
	assert.Contains(t, ids, "LicenseRef-libc-extra")
	var purposes []string
	for _, p := range out.Packages {
		purposes = append(purposes, p.PrimaryPackagePurpose)
	}
	assert.Contains(t, purposes, "OPERATING-SYSTEM")
	assert.Equal(t, "Image", string(out.Packages[0].PackageSPDXIdentifier))
	assert.Equal(t, "CONTAINER", out.Packages[0].PrimaryPackagePurpose)

	// The base SPDX document is an external document reference.
	require.Len(t, out.ExternalDocumentReferences, 1)
	assert.Equal(t, "https://example.com/spdxdocs/debian", out.ExternalDocumentReferences[0].URI)
	assert.NotEmpty(t, out.ExternalDocumentReferences[0].Checksum.Value)
	found := false
	for _, r := range out.Relationships {
		if r.Relationship == "DESCRIBED_BY" && r.RefB.DocumentRefID != "" && r.RefB.ElementRefID == "Image" {
			found = true
		}
	}
	assert.True(t, found, "the base package must be DESCRIBED_BY the base document's subject")
}

func TestManifestUnwrapsAttestations(t *testing.T) {
	statement, err := json.Marshal(map[string]any{
		"_type":         "https://in-toto.io/Statement/v1",
		"predicateType": "https://cyclonedx.org/bom",
		"subject":       []any{map[string]any{"digest": map[string]string{"sha256": "2222"}}},
		"predicate":     json.RawMessage(baseCycloneDX),
	})
	require.NoError(t, err)
	bundle, err := json.Marshal(map[string]any{
		"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json",
		"dsseEnvelope": map[string]any{
			"payloadType": "application/vnd.in-toto+json",
			"payload":     base64.StdEncoding.EncodeToString(statement),
			"signatures":  []any{map[string]string{"sig": "c2ln"}},
		},
	})
	require.NoError(t, err)

	doc, err := bom.ComposeManifest(manifest(t, bom.BaseInherit, string(bundle)))
	require.NoError(t, err)
	refs := map[string]bool{}
	for _, c := range doc.Components {
		refs[c.Ref] = true
	}
	assert.True(t, refs["os-debian"], "the SBOM inside the sigstore bundle should be inherited")
}

// Cosign's tag-scheme attestations (sha256-<hex>.att) are DSSE envelopes of
// in-toto v0.1 statements, and distroless records the SPDX document in them as
// a JSON string.
func TestManifestUnwrapsCosignTagAttestations(t *testing.T) {
	statement, err := json.Marshal(map[string]any{
		"_type":         "https://in-toto.io/Statement/v0.1",
		"predicateType": "https://spdx.dev/Document",
		"subject":       []any{map[string]any{"name": "gcr.io/distroless/static", "digest": map[string]string{"sha256": "2222"}}},
		"predicate":     baseSPDX,
	})
	require.NoError(t, err)
	envelope, err := json.Marshal(map[string]any{
		"payloadType": "application/vnd.in-toto+json",
		"payload":     base64.StdEncoding.EncodeToString(statement),
		"signatures":  []any{map[string]string{"keyid": "", "sig": "c2ln"}},
	})
	require.NoError(t, err)

	doc, err := bom.ComposeManifest(manifest(t, bom.BaseInherit, string(envelope)))
	require.NoError(t, err)
	purls := map[string]bool{}
	for _, c := range doc.Components {
		purls[c.PURL] = true
	}
	assert.True(t, purls["pkg:deb/debian/libc6@2.36-9?arch=amd64"], "the SPDX document inside the DSSE envelope should be inherited")
}

func TestManifestSkipsUnreadableBaseSBOM(t *testing.T) {
	m := manifest(t, bom.BaseInherit, `{"not": "an sbom"}`)
	var log strings.Builder
	m.Log = &log
	doc, err := bom.ComposeManifest(m)
	require.NoError(t, err)
	assert.Contains(t, log.String(), "skipping base SBOM")
	assert.Len(t, doc.Components, 2, "the base image and the Bazel package")
}

func TestIndex(t *testing.T) {
	dir := t.TempDir()
	child, err := bom.ComposeManifest(manifest(t, bom.BaseInherit, baseCycloneDX))
	require.NoError(t, err)
	childSBOM, err := bom.WriteCycloneDX(child, "urn:uuid:11111111-1111-5111-8111-111111111111")
	require.NoError(t, err)

	descriptor := platformDescriptor(imageDigest, "linux", "arm64", "v8")
	// The index is a target of its own, named differently from its child.
	composeIndex := func(mode string) bom.Document {
		doc, err := bom.ComposeIndex(bom.Index{
			Name:       "app/index",
			Descriptor: bom.Descriptor{MediaType: "application/vnd.oci.image.index.v1+json", Digest: baseDigest, Size: 50},
			Children:   []bom.Child{{Descriptor: descriptor, SBOM: writeFile(t, dir, "child.json", string(childSBOM)), SBOMArtifact: sbomDigest}},
			Mode:       mode,
		})
		require.NoError(t, err)
		return doc
	}
	index := func(mode string) cdx.BOM {
		data, err := bom.WriteCycloneDX(composeIndex(mode), "urn:uuid:00000000-0000-5000-8000-000000000000")
		require.NoError(t, err)
		return decodeCycloneDX(t, data)
	}
	// The child's entry is the primary component of its own SBOM: the same
	// reference and name.
	childPrimary := decodeCycloneDX(t, childSBOM).Metadata.Component
	const childRef = "app/image@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	require.Equal(t, childRef, childPrimary.BOMRef)
	checkChild := func(t *testing.T, out cdx.BOM) {
		assert.Equal(t, "app/index@sha256:2222222222222222222222222222222222222222222222222222222222222222", out.Metadata.Component.BOMRef)
		assert.Equal(t, "app/index (multi-arch)", out.Metadata.Component.Name)
		assert.Empty(t, out.Metadata.Component.PackageURL)
		all := components(out)
		require.Contains(t, all, childRef)
		assert.Equal(t, childPrimary.Name, all[childRef].Name)
		assert.Equal(t, "app/image (linux/arm64/v8)", all[childRef].Name)
		assert.Empty(t, all[childRef].PackageURL)
		assert.Empty(t, all[childRef].Description)
		assert.Equal(t, []cdx.Hash{{Algorithm: cdx.HashAlgoSHA256, Value: "1111111111111111111111111111111111111111111111111111111111111111"}}, *all[childRef].Hashes)
		reference := (*all[childRef].ExternalReferences)[0]
		assert.Equal(t, cdx.ERTypeBOM, reference.Type)
		assert.Equal(t, "urn:cdx:11111111-1111-5111-8111-111111111111/1", reference.URL)
		assert.Equal(t, []cdx.Hash{{Algorithm: cdx.HashAlgoSHA256, Value: sha256Hex(childSBOM)}}, *reference.Hashes)
	}

	t.Run("reference", func(t *testing.T) {
		// A list of the manifests and their SBOMs, nothing else.
		out := index(bom.IndexReference)
		checkChild(t, out)
		assert.Len(t, *out.Components, 1, "only the manifests")
		assert.Nil(t, out.Dependencies, "no dependency graph")
		assert.Empty(t, (*components(out)[childRef].ExternalReferences)[0].Comment, "no comment on the reference")

		// SPDX keeps the relationship that connects the manifest to the index.
		spdx, err := bom.WriteSPDX(composeIndex(bom.IndexReference), bom.SPDXNamespace("app/index"))
		require.NoError(t, err)
		assert.Contains(t, string(spdx), `"relationshipType": "DEPENDS_ON"`)
	})

	t.Run("child without an SBOM", func(t *testing.T) {
		doc, err := bom.ComposeIndex(bom.Index{
			Name:       "app/index",
			Descriptor: bom.Descriptor{MediaType: "application/vnd.oci.image.index.v1+json", Digest: baseDigest, Size: 50},
			Children:   []bom.Child{{Descriptor: descriptor}},
			Mode:       bom.IndexReference,
		})
		require.NoError(t, err)
		require.Len(t, doc.Components, 1)
		assert.Equal(t, "app/index (linux/arm64/v8)", doc.Components[0].Name, "named after the index and its platform")
	})

	t.Run("aggregate without publishing the children's SBOMs", func(t *testing.T) {
		doc, err := bom.ComposeIndex(bom.Index{
			Name:                "app/image",
			Descriptor:          bom.Descriptor{MediaType: "application/vnd.oci.image.index.v1+json", Digest: baseDigest, Size: 50},
			Children:            []bom.Child{{Descriptor: descriptor, SBOM: writeFile(t, dir, "child.json", string(childSBOM)), SBOMArtifact: sbomDigest}},
			Mode:                bom.IndexAggregate,
			OmitChildReferences: true,
		})
		require.NoError(t, err)
		data, err := bom.WriteCycloneDX(doc, "urn:uuid:00000000-0000-5000-8000-000000000000")
		require.NoError(t, err)
		all := components(decodeCycloneDX(t, data))
		require.Contains(t, all, childRef)
		assert.Nil(t, all[childRef].ExternalReferences, "no reference to an SBOM that isn't published")
		require.Contains(t, all, "pkg:golang/github.com/example/module@v1.2.3", "the packages are still aggregated")
	})

	t.Run("aggregate", func(t *testing.T) {
		// What a scanner given the index needs: the packages of its manifests.
		out := index(bom.IndexAggregate)
		checkChild(t, out)
		all := components(out)
		require.Contains(t, all, "pkg:golang/github.com/example/module@v1.2.3")
		require.Contains(t, all, "os-debian")
		require.Contains(t, all, basePURL)
		// The child SBOM's subject is the child component, not another one.
		described := 0
		for _, c := range *out.Components {
			if c.Hashes != nil && (*c.Hashes)[0].Value == strings.TrimPrefix(imageDigest, "sha256:") {
				described++
			}
		}
		assert.Equal(t, 1, described, "the child SBOM's subject is the child component")
		deps := dependencies(out)
		assert.ElementsMatch(t, []string{basePURL, "pkg:golang/github.com/example/module@v1.2.3"}, deps[childRef])
		assert.ElementsMatch(t, []string{"os-debian", "pkg:deb/debian/libc6@2.36-9?arch=amd64"}, deps[basePURL])
		assert.Contains(t, (*components(out)[childRef].ExternalReferences)[0].Comment, sbomDigest, "the reference says which OCI artifact carries the SBOM")
	})
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestDeterministic(t *testing.T) {
	m := manifest(t, bom.BaseInherit, baseSPDX)
	var outputs [][]byte
	for range 2 {
		doc, err := bom.ComposeManifest(m)
		require.NoError(t, err)
		cyclonedx, err := bom.WriteCycloneDX(doc, "urn:uuid:00000000-0000-5000-8000-000000000000")
		require.NoError(t, err)
		spdx, err := bom.WriteSPDX(doc, bom.SPDXNamespace(imageRef))
		require.NoError(t, err)
		outputs = append(outputs, append(cyclonedx, spdx...))
	}
	assert.Equal(t, string(outputs[0]), string(outputs[1]))
}

func TestManifestPrefersBaseSBOMInItsFormat(t *testing.T) {
	dir := t.TempDir()
	cyclonedx := bom.BaseSBOM{Path: writeFile(t, dir, "base.cdx.json", baseCycloneDX), Format: "cyclonedx", Scope: "manifest"}
	spdx := bom.BaseSBOM{Path: writeFile(t, dir, "base.spdx.json", baseSPDX), Format: "spdx", Scope: "manifest"}
	reference := func(format string, sboms ...bom.BaseSBOM) string {
		m := manifest(t, bom.BaseReference, "")
		m.Format = format
		m.BaseSBOMs = sboms
		doc, err := bom.ComposeManifest(m)
		require.NoError(t, err)
		for _, c := range doc.Components {
			if c.Ref == basePURL {
				return c.ExternalBOMs[0].URL
			}
		}
		t.Fatal("no base image component")
		return ""
	}
	const cyclonedxURL = "urn:cdx:7a1e1a50-0d35-4b16-9c1e-4a8a4d1b0a01/1"
	const spdxURL = "https://example.com/spdxdocs/debian"

	// Each format builds on the base SBOM in its own format, whatever the order.
	assert.Equal(t, cyclonedxURL, reference("cyclonedx", spdx, cyclonedx))
	assert.Equal(t, spdxURL, reference("spdx", cyclonedx, spdx))

	// But an SBOM of the base manifest beats one of its index, whatever its format.
	indexSPDX := spdx
	indexSPDX.Scope = "index"
	assert.Equal(t, cyclonedxURL, reference("spdx", indexSPDX, cyclonedx))
}

// A stub base SBOM, as attached to images whose full SBOM lives in an SBOM
// database: nothing but the image, with a reference to the full SBOM.
const baseStubCycloneDX = `{
  "bomFormat": "CycloneDX",
  "specVersion": "1.6",
  "serialNumber": "urn:uuid:7a1e1a50-0d35-4b16-9c1e-4a8a4d1b0a02",
  "version": 1,
  "metadata": {"component": {"bom-ref": "base-image", "type": "container", "name": "debian", "version": "sha256:2222",
    "externalReferences": [{"type": "bom", "url": "https://sboms.example.com/fetch/42", "comment": "SBOM in the SBOM database",
      "hashes": [{"alg": "SHA-256", "content": "4444444444444444444444444444444444444444444444444444444444444444"}]}]}},
  "components": []
}`

func TestManifestKeepsReferencesOfStubBaseSBOM(t *testing.T) {
	for _, mode := range []string{bom.BaseInherit, bom.BaseReference} {
		t.Run(mode, func(t *testing.T) {
			doc, err := bom.ComposeManifest(manifest(t, mode, baseStubCycloneDX))
			require.NoError(t, err)
			data, err := bom.WriteCycloneDX(doc, "urn:uuid:00000000-0000-5000-8000-000000000000")
			require.NoError(t, err)
			all := components(decodeCycloneDX(t, data))
			require.Contains(t, all, basePURL)
			require.NotNil(t, all[basePURL].ExternalReferences)
			references := *all[basePURL].ExternalReferences
			require.Len(t, references, 2, "the stub, and the SBOM it refers to")
			assert.Equal(t, "urn:cdx:7a1e1a50-0d35-4b16-9c1e-4a8a4d1b0a02/1", references[0].URL)
			assert.Equal(t, cdx.ERTypeBOM, references[1].Type)
			assert.Equal(t, "https://sboms.example.com/fetch/42", references[1].URL)
			assert.Equal(t, "SBOM in the SBOM database", references[1].Comment)
			require.NotNil(t, references[1].Hashes)
			assert.Equal(t, "4444444444444444444444444444444444444444444444444444444444444444", (*references[1].Hashes)[0].Value)
		})
	}
}
