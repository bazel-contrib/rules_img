package contentmanifest

import (
	"bufio"
	"fmt"
	"iter"
	"os"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

type MultiImporter struct {
	manifestPaths []string
	algorithm     api.HashAlgorithm
	fs            vfs
}

func NewMultiImporter(manifestPaths []string, algorithm api.HashAlgorithm) *MultiImporter {
	return &MultiImporter{
		manifestPaths: manifestPaths,
		algorithm:     algorithm,
		fs:            osFS{},
	}
}

func (i *MultiImporter) AddOne(manifestPath string) {
	i.manifestPaths = append(i.manifestPaths, manifestPath)
}

func (i *MultiImporter) AddCollection(collectionPath string) error {
	collection, err := i.fs.Open(collectionPath)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := collection.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "Error closing collection file: %v\n", closeErr)
		}
	}()

	scanner := bufio.NewScanner(collection)
	for scanner.Scan() {
		i.manifestPaths = append(i.manifestPaths, scanner.Text())
	}
	return scanner.Err()
}

func (i *MultiImporter) BlobHashes() iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		for _, manifestPath := range i.manifestPaths {
			manifest := fileManifest{
				manifestPath: manifestPath,
				algorithm:    i.algorithm,
				fs:           i.fs,
			}
			for hash, err := range manifest.BlobHashes() {
				if !yield(hash, err) {
					return
				}
			}
		}
	}
}

func (i *MultiImporter) NodeHashes() iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		for _, manifestPath := range i.manifestPaths {
			manifest := fileManifest{
				manifestPath: manifestPath,
				algorithm:    i.algorithm,
				fs:           i.fs,
			}
			for hash, err := range manifest.NodeHashes() {
				if !yield(hash, err) {
					return
				}
			}
		}
	}
}

func (i *MultiImporter) TreeHashes() iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		for _, manifestPath := range i.manifestPaths {
			manifest := fileManifest{
				manifestPath: manifestPath,
				algorithm:    i.algorithm,
				fs:           i.fs,
			}
			for hash, err := range manifest.TreeHashes() {
				if !yield(hash, err) {
					return
				}
			}
		}
	}
}
