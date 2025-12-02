package deployvfs

import (
	"bytes"
	"fmt"
	"io"

	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	registrytypes "github.com/google/go-containerregistry/pkg/v1/types"
)

type image struct {
	root        blobEntry
	rawManifest []byte
	rawConfig   []byte
	manifest    *registryv1.Manifest
	configFile  *registryv1.ConfigFile
	vfs         *VFS
}

func newImage(vfs *VFS, hash registryv1.Hash) (*image, error) {
	root, found := vfs.manifests[hash.String()]
	if !found {
		return nil, fmt.Errorf("image manifest %s not found in VFS", hash.String())
	}
	rawManifestFile, err := root.Opener()
	if err != nil {
		return nil, fmt.Errorf("opening image manifest: %w", err)
	}
	defer rawManifestFile.Close()

	rawManifest, err := io.ReadAll(rawManifestFile)
	if err != nil {
		return nil, fmt.Errorf("reading image manifest: %w", err)
	}
	manifest, err := registryv1.ParseManifest(bytes.NewReader(rawManifest))
	if err != nil {
		return nil, fmt.Errorf("parsing image manifest: %w", err)
	}
	configBlob, found := vfs.blobs[manifest.Config.Digest.String()]
	if !found {
		return nil, fmt.Errorf("config blob %s not found in VFS", manifest.Config.Digest.String())
	}
	rawConfigFile, err := configBlob.Opener()
	if err != nil {
		return nil, fmt.Errorf("opening image config: %w", err)
	}
	defer rawConfigFile.Close()
	rawConfig, err := io.ReadAll(rawConfigFile)
	if err != nil {
		return nil, fmt.Errorf("reading image config: %w", err)
	}
	configFile, err := registryv1.ParseConfigFile(bytes.NewReader(rawConfig))
	if err != nil {
		return nil, fmt.Errorf("parsing image config: %w", err)
	}

	return &image{
		root:        root,
		rawManifest: rawManifest,
		rawConfig:   rawConfig,
		manifest:    manifest,
		configFile:  configFile,
		vfs:         vfs,
	}, nil
}

func (img *image) Layers() ([]registryv1.Layer, error) {
	layers := make([]registryv1.Layer, len(img.manifest.Layers))
	for i, layerDesc := range img.manifest.Layers {
		layer, err := img.vfs.Layer(layerDesc.Digest)
		if err != nil {
			return nil, fmt.Errorf("getting layer %s: %w", layerDesc.Digest.String(), err)
		}
		layers[i] = layer
	}
	return layers, nil
}

func (img *image) MediaType() (registrytypes.MediaType, error) {
	return img.root.MediaType()
}

func (img *image) Size() (int64, error) {
	return img.root.Size()
}

func (img *image) ConfigName() (registryv1.Hash, error) {
	h, _, err := registryv1.SHA256(bytes.NewReader(img.rawConfig))
	return h, err
}

func (img *image) ConfigFile() (*registryv1.ConfigFile, error) {
	return img.configFile, nil
}

func (img *image) RawConfigFile() ([]byte, error) {
	return img.rawConfig, nil
}

func (img *image) Digest() (registryv1.Hash, error) {
	return img.root.Digest()
}

func (img *image) Manifest() (*registryv1.Manifest, error) {
	return img.manifest, nil
}

func (img *image) RawManifest() ([]byte, error) {
	return img.rawManifest, nil
}

func (img *image) LayerByDigest(digest registryv1.Hash) (registryv1.Layer, error) {
	return img.vfs.Layer(digest)
}

func (img *image) LayerByDiffID(diffID registryv1.Hash) (registryv1.Layer, error) {
	for i, diffID := range img.configFile.RootFS.DiffIDs {
		if diffID == diffID {
			return img.vfs.Layer(img.manifest.Layers[i].Digest)
		}
	}
	return nil, fmt.Errorf("layer with diffID %s not found", diffID.String())
}
