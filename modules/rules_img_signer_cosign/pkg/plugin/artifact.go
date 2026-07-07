package plugin

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

const (
	ociLayoutVersion = "1.0.0"
	// EmptyConfigMediaType is the media type of the canonical empty config blob
	// ("{}") used by artifact manifests.
	EmptyConfigMediaType = "application/vnd.oci.empty.v1+json"
)

// ArtifactLayer is a single layer to embed in an artifact manifest.
type ArtifactLayer struct {
	MediaType string
	Data      []byte
}

// BuildArtifact constructs an OCI artifact image (an image manifest with the
// empty "{}" config, the given artifactType, layers, optional subject, and
// annotations) as a v1.Image.
func BuildArtifact(artifactType string, layers []ArtifactLayer, subject *v1.Descriptor, annotations map[string]string) (v1.Image, error) {
	emptyConfig := []byte("{}")
	blobs := map[string][]byte{}

	configDigest := sha256Hex(emptyConfig)
	blobs[configDigest] = emptyConfig
	manifest := v1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		ArtifactType:  artifactType,
		Config: v1.Descriptor{
			MediaType: EmptyConfigMediaType,
			Digest:    v1.Hash{Algorithm: "sha256", Hex: configDigest},
			Size:      int64(len(emptyConfig)),
			Data:      emptyConfig,
		},
		Subject:     subject,
		Annotations: annotations,
	}
	for _, layer := range layers {
		h := sha256Hex(layer.Data)
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
	return partial.CompressedToImage(&builtImage{
		rawManifest: rawManifest,
		manifest:    &manifest,
		blobs:       blobs,
	})
}

// WriteArtifact serializes artifact images as an OCI image layout tar stream,
// preserving each manifest's bytes verbatim (so digests and subject/artifactType
// survive intact).
func WriteArtifact(w io.Writer, imgs []v1.Image) error {
	if len(imgs) == 0 {
		return fmt.Errorf("WriteArtifact: no images to write")
	}
	tw := tar.NewWriter(w)
	defer tw.Close()

	blobsMem := map[string][]byte{}
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
				return fmt.Errorf("computing layer digest: %w", err)
			}
			rc, err := layer.Compressed()
			if err != nil {
				return fmt.Errorf("opening layer: %w", err)
			}
			data, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				return fmt.Errorf("reading layer: %w", err)
			}
			blobsMem[ld.Hex] = data
		}
	}

	ociIndex := v1.IndexManifest{
		SchemaVersion: 2,
		MediaType:     types.OCIImageIndex,
		Manifests:     indexDescs,
	}
	if err := writeJSON(tw, "oci-layout", map[string]string{"imageLayoutVersion": ociLayoutVersion}); err != nil {
		return err
	}
	if err := writeJSON(tw, "index.json", ociIndex); err != nil {
		return err
	}
	if err := writeDir(tw, "blobs/"); err != nil {
		return err
	}
	if err := writeDir(tw, "blobs/sha256/"); err != nil {
		return err
	}
	digests := make([]string, 0, len(blobsMem))
	for d := range blobsMem {
		digests = append(digests, d)
	}
	sort.Strings(digests)
	for _, d := range digests {
		if err := writeFile(tw, "blobs/sha256/"+d, blobsMem[d]); err != nil {
			return err
		}
	}
	return nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func writeJSON(tw *tar.Writer, name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(tw, name, data)
}

func writeFile(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func writeDir(tw *tar.Writer, name string) error {
	return tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Typeflag: tar.TypeDir})
}

// builtImage is an in-memory partial.CompressedImageCore for BuildArtifact.
type builtImage struct {
	rawManifest []byte
	manifest    *v1.Manifest
	blobs       map[string][]byte
}

func (b *builtImage) MediaType() (types.MediaType, error) { return b.manifest.MediaType, nil }
func (b *builtImage) RawManifest() ([]byte, error)        { return b.rawManifest, nil }

func (b *builtImage) RawConfigFile() ([]byte, error) {
	data, ok := b.blobs[b.manifest.Config.Digest.Hex]
	if !ok {
		return nil, fmt.Errorf("config blob missing")
	}
	return data, nil
}

func (b *builtImage) LayerByDigest(h v1.Hash) (partial.CompressedLayer, error) {
	data, ok := b.blobs[h.Hex]
	if !ok {
		return nil, fmt.Errorf("layer blob %s missing", h)
	}
	mt := types.MediaType("application/octet-stream")
	for _, l := range b.manifest.Layers {
		if l.Digest == h {
			mt = l.MediaType
			break
		}
	}
	return &builtLayer{h: h, data: data, mediaType: mt}, nil
}

type builtLayer struct {
	h         v1.Hash
	data      []byte
	mediaType types.MediaType
}

func (l *builtLayer) Digest() (v1.Hash, error)            { return l.h, nil }
func (l *builtLayer) Size() (int64, error)                { return int64(len(l.data)), nil }
func (l *builtLayer) MediaType() (types.MediaType, error) { return l.mediaType, nil }
func (l *builtLayer) Compressed() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(l.data)), nil
}
