package merkle

import (
	"encoding/binary"
	"fmt"
	"hash"
	"io/fs"
	"time"
)

type FileNode struct {
	Name        metadataString
	Size        metadataSize
	ContentHash metadataBytes
	Mtime       metadataTime
	Mode        metadataMode
}

// DefaultFileNode creates a FileNode with normalized metadata.
// This is the default metadata used in tar files, unless otherwise specified.
func DefaultFileNode(contentHash []byte, info fs.FileInfo) FileNode {
	return FileNode{
		Name:        metadataString(info.Name()),
		Size:        metadataSize(info.Size()),
		ContentHash: contentHash,
		Mode:        0o755,
	}
}

// DetailedFileNode creates a FileNode with detailed metadata.
// The extra metadata can be used for special purposes,
// but doing so avoids deduplication of the file.
func DetailedFileNode(contentHash []byte, info fs.FileInfo) FileNode {
	return FileNode{
		Name:        metadataString(info.Name()),
		Size:        metadataSize(info.Size()),
		ContentHash: contentHash,
		Mtime:       metadataTime(info.ModTime().UTC().Truncate(time.Second)),
		Mode:        metadataMode(info.Mode() & fs.ModePerm),
	}
}

func (n FileNode) Fingerprint(h hash.Hash) {
	h.Write([]byte{FileNodeUUID})
	n.Name.Fingerprint(h)
	n.Size.Fingerprint(h)
	n.ContentHash.Fingerprint(h)
	n.Mtime.Fingerprint(h)
	n.Mode.Fingerprint(h)
}

type DirectoryNode struct {
	Name metadataString
	Hash metadataBytes
}

func (n DirectoryNode) Fingerprint(h hash.Hash) {
	h.Write([]byte{DirectoryNodeUUID})
	n.Name.Fingerprint(h)
	n.Hash.Fingerprint(h)
}

type Directory struct {
	Files       metadataSlice[FileNode]
	Directories metadataSlice[DirectoryNode]
}

func (n Directory) Hash(h hash.Hash) []byte {
	h.Write([]byte{DirectryUUID})
	n.Files.Fingerprint(h)
	n.Directories.Fingerprint(h)
	return h.Sum(nil)
}

type TreeHasher struct{}

type metadataString string

func (m metadataString) Fingerprint(h hash.Hash) {
	if err := binary.Write(h, binary.BigEndian, uint64(len(m))); err != nil {
		panic(fmt.Sprintf("binary.Write failed: %v", err))
	}
	h.Write([]byte(m))
}

type metadataBytes []byte

func (m metadataBytes) Fingerprint(h hash.Hash) {
	if err := binary.Write(h, binary.BigEndian, uint64(len(m))); err != nil {
		panic(fmt.Sprintf("binary.Write failed: %v", err))
	}
	h.Write(m)
}

type metadataSlice[T fingerprintable] []T

func (m metadataSlice[T]) Fingerprint(h hash.Hash) {
	if err := binary.Write(h, binary.BigEndian, uint64(len(m))); err != nil {
		panic(fmt.Sprintf("binary.Write failed: %v", err))
	}
	for _, item := range m {
		item.Fingerprint(h)
	}
}

type metadataSize int64

func (m metadataSize) Fingerprint(h hash.Hash) {
	_ = binary.Write(h, binary.BigEndian, uint64(m))
}

type metadataMode fs.FileMode

func (m metadataMode) Fingerprint(h hash.Hash) {
	_ = binary.Write(h, binary.BigEndian, uint32(m))
}

type metadataTime time.Time

func (m metadataTime) Fingerprint(h hash.Hash) {
	ts := time.Time(m).Unix()
	_ = binary.Write(h, binary.BigEndian, uint64(ts))
}

type fingerprintable interface {
	Fingerprint(h hash.Hash)
}

const (
	FileNodeUUID      = 0x01
	DirectoryNodeUUID = 0x02
	SymlinkNodeUUID   = 0x03 // unused, but reserved for future use

	// DirectoryUUID is used to identify an (unnamed) directory
	// together with the hashes of its children.
	// (like the top-level directory in a tree artifact).
	DirectryUUID = 0xff
)
