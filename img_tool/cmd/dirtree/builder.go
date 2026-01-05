package dirtree

import (
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	remoteexecution "github.com/bazel-contrib/rules_img/img_tool/pkg/proto/remote-apis/build/bazel/remote/execution/v2"
	"github.com/zeebo/blake3"
	"google.golang.org/protobuf/proto"
)

// fileEntry represents a file entry from the inputs file
type fileEntry struct {
	pathInImage string
	fileType    api.FileType
	sourcePath  string
}

// dirNode represents a directory node in the tree
type dirNode struct {
	path     string
	files    map[string]*fileEntry // basename -> entry
	subdirs  map[string]*dirNode   // basename -> child directory
	symlinks map[string]string     // basename -> target
}

// newHasher creates a new hasher for the specified digest function
func newHasher(digestFunction string) (hash.Hash, error) {
	switch digestFunction {
	case "sha1":
		return sha1.New(), nil
	case "sha256":
		return sha256.New(), nil
	case "sha384":
		return sha512.New384(), nil
	case "sha512":
		return sha512.New(), nil
	case "blake3":
		return blake3.New(), nil
	default:
		return nil, fmt.Errorf("unsupported digest function: %s", digestFunction)
	}
}

// hashData computes the hash of data using the specified digest function
func hashData(data []byte, digestFunction string) (string, error) {
	h, err := newHasher(digestFunction)
	if err != nil {
		return "", err
	}
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashFile computes the hash of a file using the specified digest function
func hashFile(path string, digestFunction string) (string, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, fmt.Errorf("reading file: %w", err)
	}

	hashStr, err := hashData(data, digestFunction)
	if err != nil {
		return "", 0, err
	}

	return hashStr, int64(len(data)), nil
}

// buildDirtree is the main function that builds the directory tree and writes outputs
func buildDirtree(req *dirtreeRequest, sandboxDir string) error {
	// Read the inputs file
	entries, err := readInputsFile(req.inputsFile, sandboxDir)
	if err != nil {
		return fmt.Errorf("reading inputs file: %w", err)
	}

	// Build the directory tree structure
	root := buildTree(entries)

	// Calculate digests bottom-up and collect all directory protos
	dirProtos := make(map[string]*remoteexecution.Directory)
	rootDigest, err := calculateDigests(root, dirProtos, sandboxDir, req.digestFunction)
	if err != nil {
		return fmt.Errorf("calculating digests: %w", err)
	}

	// Create output directory for proto messages
	protoOutputDir := req.protoOutputDir
	if sandboxDir != "" && !filepath.IsAbs(protoOutputDir) {
		protoOutputDir = filepath.Join(sandboxDir, protoOutputDir)
	}
	if err := os.MkdirAll(protoOutputDir, 0o755); err != nil {
		return fmt.Errorf("creating proto output directory: %w", err)
	}

	// Write all directory proto messages
	for digestStr, dirProto := range dirProtos {
		if err := writeProtoMessage(protoOutputDir, digestStr, req.digestFunction, dirProto); err != nil {
			return fmt.Errorf("writing proto message: %w", err)
		}
	}

	// Write the root digest to output file as proto message (binary format)
	digestOutputPath := req.digestOutput
	if sandboxDir != "" && !filepath.IsAbs(digestOutputPath) {
		digestOutputPath = filepath.Join(sandboxDir, digestOutputPath)
	}

	// Marshal the digest proto to binary format
	digestData, err := proto.Marshal(rootDigest)
	if err != nil {
		return fmt.Errorf("marshaling digest proto: %w", err)
	}

	if err := os.WriteFile(digestOutputPath, digestData, 0o644); err != nil {
		return fmt.Errorf("writing digest output: %w", err)
	}

	return nil
}

// readInputsFile reads the inputs file in the same format as layer paramfile
func readInputsFile(inputsFile string, sandboxDir string) ([]*fileEntry, error) {
	inputsPath := inputsFile
	if sandboxDir != "" && !filepath.IsAbs(inputsPath) {
		inputsPath = filepath.Join(sandboxDir, inputsPath)
	}

	data, err := os.ReadFile(inputsPath)
	if err != nil {
		return nil, fmt.Errorf("reading file: %w", err)
	}

	var entries []*fileEntry
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, "\x00", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid format at line %d: %s", i+1, line)
		}

		pathInImage := parts[0]
		if pathInImage == "" {
			return nil, fmt.Errorf("empty path at line %d", i+1)
		}
		if strings.HasPrefix(pathInImage, "/") {
			return nil, fmt.Errorf("path cannot start with '/' at line %d: %s", i+1, pathInImage)
		}

		rest := parts[1]
		if len(rest) < 2 {
			return nil, fmt.Errorf("invalid format at line %d: missing type", i+1)
		}

		typeChar := rest[0]
		sourcePath := rest[1:]

		var fileType api.FileType
		switch typeChar {
		case 'f':
			fileType = api.RegularFile
		case 'd':
			fileType = api.Directory
		case 'l':
			fileType = api.Symlink
		default:
			return nil, fmt.Errorf("invalid type '%c' at line %d", typeChar, i+1)
		}

		entries = append(entries, &fileEntry{
			pathInImage: pathInImage,
			fileType:    fileType,
			sourcePath:  sourcePath,
		})
	}

	return entries, nil
}

// buildTree builds the directory tree structure from file entries
func buildTree(entries []*fileEntry) *dirNode {
	root := &dirNode{
		path:     "",
		files:    make(map[string]*fileEntry),
		subdirs:  make(map[string]*dirNode),
		symlinks: make(map[string]string),
	}

	for _, entry := range entries {
		insertIntoTree(root, entry)
	}

	return root
}

// insertIntoTree inserts a file entry into the tree
func insertIntoTree(root *dirNode, entry *fileEntry) {
	parts := strings.Split(entry.pathInImage, "/")

	// Navigate/create directory structure
	current := root
	for i := range len(parts) - 1 {
		dirName := parts[i]
		if _, exists := current.subdirs[dirName]; !exists {
			current.subdirs[dirName] = &dirNode{
				path:     path.Join(current.path, dirName),
				files:    make(map[string]*fileEntry),
				subdirs:  make(map[string]*dirNode),
				symlinks: make(map[string]string),
			}
		}
		current = current.subdirs[dirName]
	}

	// Add the file/symlink to the current directory
	basename := parts[len(parts)-1]
	switch entry.fileType {
	case api.RegularFile, api.Directory:
		current.files[basename] = entry
	case api.Symlink:
		current.symlinks[basename] = entry.sourcePath
	}
}

// calculateDigests calculates digests for all directories bottom-up
func calculateDigests(node *dirNode, dirProtos map[string]*remoteexecution.Directory, sandboxDir string, digestFunction string) (*remoteexecution.Digest, error) {
	// First, recursively calculate digests for all subdirectories
	subdirDigests := make(map[string]*remoteexecution.Digest)
	for name, child := range node.subdirs {
		childDigest, err := calculateDigests(child, dirProtos, sandboxDir, digestFunction)
		if err != nil {
			return nil, err
		}
		subdirDigests[name] = childDigest
	}

	// Build the Directory proto for this node
	dir := &remoteexecution.Directory{
		Files:       make([]*remoteexecution.FileNode, 0),
		Directories: make([]*remoteexecution.DirectoryNode, 0),
		Symlinks:    make([]*remoteexecution.SymlinkNode, 0),
	}

	// Add files
	for name, entry := range node.files {
		if entry.fileType == api.RegularFile {
			// Calculate file digest
			fileDigest, isExecutable, err := calculateFileDigest(entry.sourcePath, sandboxDir, digestFunction)
			if err != nil {
				return nil, fmt.Errorf("calculating digest for %s: %w", entry.sourcePath, err)
			}

			dir.Files = append(dir.Files, &remoteexecution.FileNode{
				Name:         name,
				Digest:       fileDigest,
				IsExecutable: isExecutable,
			})
		}
		// Skip directories here - they're handled via subdirs
	}

	// Add subdirectories
	for name, childDigest := range subdirDigests {
		dir.Directories = append(dir.Directories, &remoteexecution.DirectoryNode{
			Name:   name,
			Digest: childDigest,
		})
	}

	// Add symlinks
	for name, target := range node.symlinks {
		dir.Symlinks = append(dir.Symlinks, &remoteexecution.SymlinkNode{
			Name:   name,
			Target: target,
		})
	}

	// Sort for canonical ordering (required by Remote Execution API)
	sortDirectory(dir)

	// Calculate digest for this directory
	digest, err := calculateDirectoryDigest(dir, digestFunction)
	if err != nil {
		return nil, fmt.Errorf("calculating directory digest: %w", err)
	}

	// Store the proto
	dirProtos[digest.Hash] = dir

	return digest, nil
}

// sortDirectory sorts the contents of a directory for canonical ordering
func sortDirectory(dir *remoteexecution.Directory) {
	sort.Slice(dir.Files, func(i, j int) bool {
		return dir.Files[i].Name < dir.Files[j].Name
	})
	sort.Slice(dir.Directories, func(i, j int) bool {
		return dir.Directories[i].Name < dir.Directories[j].Name
	})
	sort.Slice(dir.Symlinks, func(i, j int) bool {
		return dir.Symlinks[i].Name < dir.Symlinks[j].Name
	})
}

// calculateFileDigest calculates the digest of a file using the specified hash function
func calculateFileDigest(filePath string, sandboxDir string, digestFunction string) (*remoteexecution.Digest, bool, error) {
	fullPath := filePath
	if sandboxDir != "" && !filepath.IsAbs(filePath) {
		fullPath = filepath.Join(sandboxDir, filePath)
	}

	hashStr, size, err := hashFile(fullPath, digestFunction)
	if err != nil {
		return nil, false, err
	}

	// Check if file is executable
	info, err := os.Stat(fullPath)
	if err != nil {
		return nil, false, fmt.Errorf("stat file: %w", err)
	}
	isExecutable := (info.Mode() & 0o111) != 0

	return &remoteexecution.Digest{
		Hash:      hashStr,
		SizeBytes: size,
	}, isExecutable, nil
}

// calculateDirectoryDigest calculates the digest of a Directory proto using the specified hash function
func calculateDirectoryDigest(dir *remoteexecution.Directory, digestFunction string) (*remoteexecution.Digest, error) {
	// Serialize the proto to binary format
	data, err := proto.Marshal(dir)
	if err != nil {
		return nil, fmt.Errorf("marshaling proto: %w", err)
	}

	hashStr, err := hashData(data, digestFunction)
	if err != nil {
		return nil, err
	}

	return &remoteexecution.Digest{
		Hash:      hashStr,
		SizeBytes: int64(len(data)),
	}, nil
}

// writeProtoMessage writes a proto message to a content-addressed file
func writeProtoMessage(outputDir string, digestHash string, digestFunction string, dir *remoteexecution.Directory) error {
	// Serialize the proto
	data, err := proto.Marshal(dir)
	if err != nil {
		return fmt.Errorf("marshaling proto: %w", err)
	}

	// Write to file named by digest
	filename := fmt.Sprintf("%s-%s", digestFunction, digestHash)
	filePath := filepath.Join(outputDir, filename)

	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		return fmt.Errorf("writing file: %w", err)
	}

	return nil
}
