package signer

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// This file holds test-only helpers that construct signer-plugin outputs (OCI
// artifact images and their OCI-layout tar serialization). They previously lived
// in pkg/ocitar, which was removed when image-layout writing was consolidated
// into pkg/ocilayout; the signer's runtime does not need them, so they are kept
// here, scoped to tests.

// testArtifactLayer is a single layer to embed in a test artifact manifest.
type testArtifactLayer struct {
	MediaType string
	Data      []byte
}

// buildTestArtifact constructs a signature-like OCI artifact image: an image
// manifest with the empty "{}" config, the given artifactType, layers, optional
// subject and annotations. It reuses the package's imageFromBlobs so the result
// is a self-contained in-memory v1.Image.
func buildTestArtifact(artifactType string, layers []testArtifactLayer, subject *v1.Descriptor, annotations map[string]string) (v1.Image, error) {
	emptyConfig := []byte("{}")
	blobs := map[string][]byte{}

	configHex := sha256HexTest(emptyConfig)
	blobs[configHex] = emptyConfig
	manifest := v1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		ArtifactType:  artifactType,
		Config: v1.Descriptor{
			MediaType: "application/vnd.oci.empty.v1+json",
			Digest:    v1.Hash{Algorithm: "sha256", Hex: configHex},
			Size:      int64(len(emptyConfig)),
			Data:      emptyConfig,
		},
		Subject:     subject,
		Annotations: annotations,
	}
	for _, layer := range layers {
		h := sha256HexTest(layer.Data)
		blobs[h] = layer.Data
		manifest.Layers = append(manifest.Layers, v1.Descriptor{
			MediaType: types.MediaType(layer.MediaType),
			Digest:    v1.Hash{Algorithm: "sha256", Hex: h},
			Size:      int64(len(layer.Data)),
		})
	}

	rawManifest, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("marshalling artifact manifest: %w", err)
	}
	return imageFromBlobs(types.OCIManifestSchema1, rawManifest, blobs)
}

func sha256HexTest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// writeArtifactTar serializes one or more OCI artifact images as an OCI image
// layout tar stream (the shape a signer plugin emits on stdout). Each manifest's
// raw bytes are written verbatim, so digests and `subject`/`artifactType` fields
// are preserved byte-for-byte, along with its config and layer blobs. index.json
// lists every artifact. It mirrors the removed ocitar.WriteArtifact.
func writeArtifactTar(w io.Writer, imgs []v1.Image) error {
	if len(imgs) == 0 {
		return fmt.Errorf("writeArtifactTar: no images to write")
	}

	tw := tar.NewWriter(w)
	defer tw.Close()

	blobsMem := map[string][]byte{}
	layerBlobs := map[string]v1.Layer{}
	indexDescs := make([]v1.Descriptor, 0, len(imgs))

	for _, img := range imgs {
		rawManifest, err := img.RawManifest()
		if err != nil {
			return fmt.Errorf("reading artifact manifest: %w", err)
		}
		mediaType, err := img.MediaType()
		if err != nil {
			return fmt.Errorf("reading artifact media type: %w", err)
		}
		digest, err := img.Digest()
		if err != nil {
			return fmt.Errorf("computing artifact digest: %w", err)
		}
		manifest, err := img.Manifest()
		if err != nil {
			return fmt.Errorf("parsing artifact manifest: %w", err)
		}

		indexDescs = append(indexDescs, v1.Descriptor{
			MediaType:    mediaType,
			Size:         int64(len(rawManifest)),
			Digest:       digest,
			ArtifactType: manifest.ArtifactType,
			Annotations:  manifest.Annotations,
		})
		blobsMem[digest.Hex] = rawManifest

		rawConfig, err := img.RawConfigFile()
		if err != nil {
			return fmt.Errorf("reading artifact config: %w", err)
		}
		blobsMem[manifest.Config.Digest.Hex] = rawConfig

		layers, err := img.Layers()
		if err != nil {
			return fmt.Errorf("reading artifact layers: %w", err)
		}
		for _, layer := range layers {
			ld, err := layer.Digest()
			if err != nil {
				return fmt.Errorf("computing artifact layer digest: %w", err)
			}
			layerBlobs[ld.Hex] = layer
		}
	}

	ociIndex := v1.IndexManifest{
		SchemaVersion: 2,
		MediaType:     types.OCIImageIndex,
		Manifests:     indexDescs,
	}

	if err := writeTarJSON(tw, "oci-layout", map[string]string{"imageLayoutVersion": "1.0.0"}); err != nil {
		return fmt.Errorf("writing oci-layout: %w", err)
	}
	if err := writeTarJSON(tw, "index.json", ociIndex); err != nil {
		return fmt.Errorf("writing index.json: %w", err)
	}
	if err := writeTarDir(tw, "blobs/"); err != nil {
		return err
	}
	if err := writeTarDir(tw, "blobs/sha256/"); err != nil {
		return err
	}

	memDigests := make([]string, 0, len(blobsMem))
	for d := range blobsMem {
		memDigests = append(memDigests, d)
	}
	sort.Strings(memDigests)
	for _, d := range memDigests {
		if err := writeTarFile(tw, "blobs/sha256/"+d, blobsMem[d]); err != nil {
			return fmt.Errorf("writing blob %s: %w", d, err)
		}
	}

	layerDigests := make([]string, 0, len(layerBlobs))
	for d := range layerBlobs {
		if _, ok := blobsMem[d]; ok {
			continue
		}
		layerDigests = append(layerDigests, d)
	}
	sort.Strings(layerDigests)
	for _, d := range layerDigests {
		if err := writeTarLayerBlob(tw, d, layerBlobs[d]); err != nil {
			return fmt.Errorf("writing layer blob %s: %w", d, err)
		}
	}
	return nil
}

func writeTarJSON(tw *tar.Writer, name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", name, err)
	}
	return writeTarFile(tw, name, data)
}

func writeTarDir(tw *tar.Writer, name string) error {
	return tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Typeflag: tar.TypeDir})
}

func writeTarFile(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(name), Mode: 0o644, Size: int64(len(data))}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func writeTarLayerBlob(tw *tar.Writer, hexDigest string, layer v1.Layer) error {
	size, err := layer.Size()
	if err != nil {
		return fmt.Errorf("layer size: %w", err)
	}
	rc, err := layer.Compressed()
	if err != nil {
		return fmt.Errorf("opening layer: %w", err)
	}
	defer rc.Close()
	if err := tw.WriteHeader(&tar.Header{Name: "blobs/sha256/" + hexDigest, Mode: 0o644, Size: size}); err != nil {
		return fmt.Errorf("writing tar header: %w", err)
	}
	if _, err := io.Copy(tw, rc); err != nil {
		return fmt.Errorf("copying layer data: %w", err)
	}
	return nil
}
