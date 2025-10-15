package deployvfs

import (
	"bytes"
	"fmt"
	"io"
	"os"

	registryv1 "github.com/malt3/go-containerregistry/pkg/v1"
	registrytypes "github.com/malt3/go-containerregistry/pkg/v1/types"
)

type index struct {
	root        blobEntry
	rawManifest []byte
	manifest    *registryv1.IndexManifest
	vfs         *VFS
}

func newIndex(vfs *VFS, hash registryv1.Hash) (*index, error) {
	root, found := vfs.manifests[hash.String()]
	if !found {
		return nil, fmt.Errorf("index manifest %s not found in VFS", hash.String())
	}
	rawManifestFile, err := root.Opener()
	if err != nil {
		return nil, fmt.Errorf("opening index manifest: %w", err)
	}
	defer func() {
		if closeErr := rawManifestFile.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "Error closing manifest file: %v\n", closeErr)
		}
	}()

	rawManifest, err := io.ReadAll(rawManifestFile)
	if err != nil {
		return nil, fmt.Errorf("reading index manifest: %w", err)
	}
	manifest, err := registryv1.ParseIndexManifest(bytes.NewReader(rawManifest))
	if err != nil {
		return nil, fmt.Errorf("parsing index manifest: %w", err)
	}

	return &index{
		root:        root,
		rawManifest: rawManifest,
		manifest:    manifest,
		vfs:         vfs,
	}, nil
}

func (img *index) MediaType() (registrytypes.MediaType, error) {
	return img.root.MediaType()
}

func (img *index) Digest() (registryv1.Hash, error) {
	return img.root.Digest()
}

func (img *index) Size() (int64, error) {
	return img.root.Size()
}

func (img *index) IndexManifest() (*registryv1.IndexManifest, error) {
	return img.manifest, nil
}

func (img *index) RawManifest() ([]byte, error) {
	return img.rawManifest, nil
}

func (img *index) Image(digest registryv1.Hash) (registryv1.Image, error) {
	return img.vfs.Image(digest)
}

func (img *index) ImageIndex(digest registryv1.Hash) (registryv1.ImageIndex, error) {
	panic("ImageIndex on vfs path is not implemented")
}
