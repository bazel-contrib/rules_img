package signer

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// ReadArtifactLayout parses an OCI image layout tar stream (as produced by a
// signer plugin) and returns every image manifest it contains as a v1.Image.
// The manifest bytes are preserved verbatim, so digests and the `subject`
// field are byte-identical to the plugin's output. Nested indexes are walked
// recursively; index manifests themselves are not returned (only the images
// they reference).
//
// Blobs are held in memory (signature artifacts are tiny), so the returned
// images are self-contained and safe to use without any backing files.
func ReadArtifactLayout(data []byte) ([]v1.Image, error) {
	blobs := map[string][]byte{} // sha256 hex -> bytes
	var indexJSON []byte

	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading OCI layout tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		switch {
		case name == "index.json":
			b, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("reading index.json: %w", err)
			}
			indexJSON = b
		case strings.HasPrefix(name, "blobs/sha256/"):
			hexDigest := strings.TrimPrefix(name, "blobs/sha256/")
			b, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("reading blob %s: %w", hexDigest, err)
			}
			// Integrity check: the blob must hash to its filename.
			sum := sha256.Sum256(b)
			if got := hex.EncodeToString(sum[:]); got != hexDigest {
				return nil, fmt.Errorf("blob %s has digest %s", hexDigest, got)
			}
			blobs[hexDigest] = b
		}
	}

	if indexJSON == nil {
		return nil, fmt.Errorf("OCI layout tar has no index.json")
	}
	var index v1.IndexManifest
	if err := json.Unmarshal(indexJSON, &index); err != nil {
		return nil, fmt.Errorf("parsing index.json: %w", err)
	}

	var imgs []v1.Image
	if err := collectImages(index.Manifests, blobs, &imgs); err != nil {
		return nil, err
	}
	return imgs, nil
}

func collectImages(descs []v1.Descriptor, blobs map[string][]byte, out *[]v1.Image) error {
	for _, desc := range descs {
		raw, ok := blobs[desc.Digest.Hex]
		if !ok {
			return fmt.Errorf("manifest blob %s referenced by index is missing from layout", desc.Digest)
		}
		switch {
		case desc.MediaType.IsIndex():
			var sub v1.IndexManifest
			if err := json.Unmarshal(raw, &sub); err != nil {
				return fmt.Errorf("parsing nested index %s: %w", desc.Digest, err)
			}
			if err := collectImages(sub.Manifests, blobs, out); err != nil {
				return err
			}
		default:
			// Everything that is not an index is treated as an image manifest
			// (signature artifacts use application/vnd.oci.image.manifest.v1+json).
			img, err := imageFromBlobs(desc.MediaType, raw, blobs)
			if err != nil {
				return fmt.Errorf("building artifact image %s: %w", desc.Digest, err)
			}
			*out = append(*out, img)
		}
	}
	return nil
}

func imageFromBlobs(mediaType types.MediaType, rawManifest []byte, blobs map[string][]byte) (v1.Image, error) {
	var manifest v1.Manifest
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	if mediaType == "" {
		mediaType = manifest.MediaType
	}
	if mediaType == "" {
		mediaType = types.OCIManifestSchema1
	}
	return partial.CompressedToImage(&memImage{
		mediaType:   mediaType,
		rawManifest: rawManifest,
		manifest:    &manifest,
		blobs:       blobs,
	})
}

// memImage is an in-memory partial.CompressedImageCore backed by the blobs of a
// parsed OCI layout.
type memImage struct {
	mediaType   types.MediaType
	rawManifest []byte
	manifest    *v1.Manifest
	blobs       map[string][]byte
}

func (m *memImage) MediaType() (types.MediaType, error) { return m.mediaType, nil }
func (m *memImage) RawManifest() ([]byte, error)        { return m.rawManifest, nil }

func (m *memImage) RawConfigFile() ([]byte, error) {
	b, ok := m.blobs[m.manifest.Config.Digest.Hex]
	if !ok {
		return nil, fmt.Errorf("config blob %s missing from layout", m.manifest.Config.Digest)
	}
	return b, nil
}

func (m *memImage) LayerByDigest(h v1.Hash) (partial.CompressedLayer, error) {
	b, ok := m.blobs[h.Hex]
	if !ok {
		return nil, fmt.Errorf("layer blob %s missing from layout", h)
	}
	mt := types.MediaType("application/octet-stream")
	for _, l := range m.manifest.Layers {
		if l.Digest == h {
			mt = l.MediaType
			break
		}
	}
	return &memLayer{h: h, data: b, mediaType: mt}, nil
}

// memLayer is an in-memory partial.CompressedLayer.
type memLayer struct {
	h         v1.Hash
	data      []byte
	mediaType types.MediaType
}

func (l *memLayer) Digest() (v1.Hash, error)            { return l.h, nil }
func (l *memLayer) Size() (int64, error)                { return int64(len(l.data)), nil }
func (l *memLayer) MediaType() (types.MediaType, error) { return l.mediaType, nil }
func (l *memLayer) Compressed() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(l.data)), nil
}
