// Package blake3sum computes BLAKE3 digests of layer blobs.
//
// OCI registries only ever see SHA256, but a Bazel remote cache started with
// --digest_function=blake3 stores every blob -- layers included -- under its
// BLAKE3 hash. A lazy push has to find the layer in that cache without
// downloading it first, so the BLAKE3 digest has to be known up front: the
// layer's build action records it in the layer metadata, next to the SHA256
// that everything the registry sees is based on.
package blake3sum

import (
	"fmt"
	"hash"
	"io"
	"os"

	"github.com/zeebo/blake3"
)

// Name is how the BLAKE3 digest function is spelled everywhere in rules_img:
// in layer metadata, in deploy manifests, in settings, and in REAPI resource
// names (blobs/blake3/<hash>/<size>).
const Name = "blake3"

// New returns a hasher computing 256-bit BLAKE3 digests, the size REAPI uses.
func New() hash.Hash {
	return blake3.New()
}

// Sum computes the BLAKE3 digest of everything r yields and reports how many
// bytes that was.
func Sum(r io.Reader) (digest []byte, size int64, err error) {
	h := New()
	size, err = io.Copy(h, r)
	if err != nil {
		return nil, size, err
	}
	return h.Sum(nil), size, nil
}

// File computes the BLAKE3 digest of the file at path.
func File(path string) (digest []byte, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()
	digest, size, err = Sum(f)
	if err != nil {
		return nil, size, fmt.Errorf("hashing %s: %w", path, err)
	}
	return digest, size, nil
}
