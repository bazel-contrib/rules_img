package sbom_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// pushedSBOM is an SBOM pushed to a destination, as read from the file a push ships.
type pushedSBOM struct {
	format  string
	primary string // the package URL of what it describes
	name    string
	serial  string // CycloneDX: the urn:cdx reference to it
	refs    []string
}

func readPushedSBOM(t *testing.T, path string) pushedSBOM {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	if bytes.Contains(data, []byte(`"bomFormat"`)) {
		bom := readCycloneDX(t, path)
		primary := bom.Metadata.Component
		assert.Equal(t, primary.PackageURL, primary.BOMRef, "referenced by its package URL, as a pulled image is")
		sbom := pushedSBOM{format: "cyclonedx", primary: primary.PackageURL, name: primary.Name, serial: "urn:cdx:" + strings.TrimPrefix(bom.SerialNumber, "urn:uuid:") + "/1"}
		// The references of the images in the same repository: an index's manifests.
		repository, _, _ := strings.Cut(primary.PackageURL, "@")
		if bom.Components != nil {
			for _, c := range *bom.Components {
				if c.ExternalReferences != nil && strings.HasPrefix(c.PackageURL, repository+"@") {
					for _, ref := range *c.ExternalReferences {
						sbom.refs = append(sbom.refs, ref.URL)
					}
				}
			}
		}
		return sbom
	}
	doc := readSPDX(t, path)
	for _, pkg := range doc.Packages {
		if pkg.PackageSPDXIdentifier != "Image" {
			continue
		}
		purls := 0
		sbom := pushedSBOM{format: "spdx", name: pkg.PackageName}
		for _, ref := range pkg.PackageExternalReferences {
			if ref.RefType == "purl" {
				sbom.primary = ref.Locator
				purls++
			}
		}
		assert.Equal(t, 1, purls, "one package URL: %s", path)
		assert.True(t, strings.HasPrefix(doc.DocumentNamespace, "https://spdx.org/spdxdocs/pkg:docker"), "a namespace of its own: %s", doc.DocumentNamespace)
		return sbom
	}
	t.Fatalf("%s describes no image", path)
	return pushedSBOM{}
}

func imagePURL(registry, repository, digest string) string {
	return "pkg:docker/" + repository + "@" + digest + "?repository_url=" + registry
}

// The multi_deploy of images with push_specs pushes, to every destination, SBOMs that name the
// image there: what is checked is what the deploy ships, referrer by referrer.
func TestMultiDeployPushesSBOMsNamingTheirDestination(t *testing.T) {
	data, err := os.ReadFile(runfile(os.Getenv("DEPLOY_MANIFEST")))
	require.NoError(t, err)
	var deploy struct {
		Operations []struct {
			RootKind string `json:"root_kind"`
			Root     struct {
				Digest string `json:"digest"`
			} `json:"root"`
			RunfilesSlot string   `json:"runfiles_slot"`
			Registry     string   `json:"registry"`
			Repository   string   `json:"repository"`
			Tags         []string `json:"tags"`
		} `json:"operations"`
	}
	require.NoError(t, json.Unmarshal(data, &deploy))

	type destination struct{ registry, repository string }
	pushed := map[destination][]pushedSBOM{}
	shipped := map[string]bool{}
	for _, op := range deploy.Operations {
		if op.RootKind != "manifest" {
			continue
		}
		slots, err := filepath.Glob(filepath.Join(os.Getenv("TEST_SRCDIR"), "*", "*", op.RunfilesSlot))
		require.NoError(t, err)
		// Slots are content-addressed: any copy will do.
		require.NotEmpty(t, slots, "the runfiles slot of %s", op.Root.Digest)
		manifestData, err := os.ReadFile(filepath.Join(slots[0], "sparse_oci_layout", "blobs", "sha256", strings.TrimPrefix(op.Root.Digest, "sha256:")))
		require.NoError(t, err)
		var manifest struct {
			ArtifactType string `json:"artifactType"`
			Subject      *struct {
				Digest string `json:"digest"`
			} `json:"subject"`
			Layers []struct {
				Digest string `json:"digest"`
			} `json:"layers"`
		}
		require.NoError(t, json.Unmarshal(manifestData, &manifest))
		if manifest.Subject == nil {
			continue // an image, not a referrer
		}
		assert.Empty(t, op.Tags, "a referrer is pushed by digest")
		layer := filepath.Join(slots[0], "manifests", "0", "layer", "0")
		layerData, err := os.ReadFile(layer)
		require.NoError(t, err)
		assert.Equal(t, manifest.Layers[0].Digest, "sha256:"+sha256Hex(layerData), "the shipped layer is the referrer's")
		shipped[sha256Hex(layerData)] = true

		sbom := readPushedSBOM(t, layer)
		// It describes the image it is a referrer of, where it is pushed to.
		assert.Equal(t, imagePURL(op.Registry, op.Repository, manifest.Subject.Digest), sbom.primary, "the SBOM pushed to %s/%s", op.Registry, op.Repository)
		assert.True(t, strings.HasPrefix(sbom.name, op.Repository+" ("), "named after its repository: %q", sbom.name)
		d := destination{op.Registry, op.Repository}
		pushed[d] = append(pushed[d], sbom)
	}

	// The index's SBOMs and those of its two manifests, in both formats, to each of its
	// destinations (the templated repository expanded); the manifest's, to its own.
	ghcr := destination{"ghcr.io", "buildbarn/pushed"}
	mirror := destination{"registry.example.com", "mirror/app"}
	single := destination{"ghcr.io", "buildbarn/pushed_manifest"}
	require.Len(t, pushed, 3, "%v", pushed)
	assert.Len(t, pushed[ghcr], 6)
	assert.Len(t, pushed[mirror], 6)
	assert.Len(t, pushed[single], 2)

	// Each destination's index SBOM refers to the SBOMs of the manifests pushed with it.
	for _, d := range []destination{ghcr, mirror} {
		serials := map[string]bool{}
		var index *pushedSBOM
		for i, sbom := range pushed[d] {
			if sbom.format != "cyclonedx" {
				continue
			}
			if strings.HasSuffix(sbom.name, "(multi-arch)") {
				index = &pushed[d][i]
			} else {
				serials[sbom.serial] = true
			}
		}
		require.NotNil(t, index, "the index SBOM pushed to %v", d)
		require.Len(t, serials, 2)
		require.Len(t, index.refs, 2, "references to the manifests' SBOMs")
		for _, ref := range index.refs {
			assert.True(t, serials[ref], "%s refers to %s, which isn't pushed to %v", index.primary, ref, d)
		}
	}

	// Their sbom output group has exactly what the deploy ships.
	paths := strings.Fields(os.Getenv("DEPLOY_SBOMS"))
	assert.Len(t, paths, len(shipped))
	for _, path := range paths {
		data, err := os.ReadFile(runfile(path))
		require.NoError(t, err)
		assert.True(t, shipped[sha256Hex(data)], "%s is shipped by the deploy", path)
	}
}

func TestImagePushPushesSBOMsNamingTheDestination(t *testing.T) {
	paths := strings.Fields(os.Getenv("PUSH_SBOMS"))
	require.Len(t, paths, 6, "the index's SBOMs and its manifests', in both formats")
	for _, path := range paths {
		sbom := readPushedSBOM(t, runfile(path))
		assert.True(t, strings.HasPrefix(sbom.primary, "pkg:docker/buildbarn/index@sha256:"), sbom.primary)
		assert.True(t, strings.HasSuffix(sbom.primary, "?repository_url=ghcr.io"), sbom.primary)
	}
}

func TestImagePushWithoutDestinationSBOMsHasTheImages(t *testing.T) {
	assert.ElementsMatch(t, strings.Fields(os.Getenv("INDEX_SBOMS")), strings.Fields(os.Getenv("UNNAMED_PUSH_SBOMS")))
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
