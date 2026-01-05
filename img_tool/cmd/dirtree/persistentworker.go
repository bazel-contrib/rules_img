package dirtree

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	remoteexecution "github.com/bazel-contrib/rules_img/img_tool/pkg/proto/remote-apis/build/bazel/remote/execution/v2"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/persistentworker"
	"google.golang.org/protobuf/proto"
)

// cachedFileDigest stores cached file digest information
type cachedFileDigest struct {
	digest       *remoteexecution.Digest
	isExecutable bool
}

type persistentDirtreeBuilder struct {
	// Cache file digests by Bazel input digest
	fileDigestCache map[string]*cachedFileDigest
	cacheMutex      sync.RWMutex
}

func newPersistentDirtreeBuilder() *persistentDirtreeBuilder {
	return &persistentDirtreeBuilder{
		fileDigestCache: make(map[string]*cachedFileDigest),
	}
}

// HandleRequest processes a single work request and returns the response.
// This implements the persistentworker.Handler interface.
func (pb *persistentDirtreeBuilder) HandleRequest(ctx context.Context, req persistentworker.WorkRequest) persistentworker.WorkResponse {
	resp := persistentworker.WorkResponse{
		RequestId: req.RequestId,
		ExitCode:  0,
		Output:    "",
	}

	if req.Verbosity > 1 {
		reqJSON, _ := json.MarshalIndent(req, "", "  ")
		fmt.Fprintf(os.Stderr, "[request %d] Received work request:\n%s\n", req.RequestId, string(reqJSON))
		defer func() {
			respJSON, _ := json.MarshalIndent(resp, "", "  ")
			fmt.Fprintf(os.Stderr, "[request %d] Sending work response:\n%s\n", req.RequestId, string(respJSON))
		}()
	}

	// Parse dirtree request arguments (Bazel already expanded argfiles)
	dirtreeReq, err := parseDirtreeRequest(req.Arguments)
	if err != nil {
		resp.ExitCode = 1
		resp.Output = fmt.Sprintf("Failed to parse dirtree request: %v", err)
		return resp
	}

	// Build input digest map for caching
	inputDigests := make(map[string]string)
	for _, input := range req.Inputs {
		inputDigests[input.Path] = input.Digest
	}

	// Build the directory tree with cache
	stats, err := pb.buildDirtreeWithCache(dirtreeReq, req.SandboxDir, inputDigests)
	if err != nil {
		resp.ExitCode = 1
		resp.Output = fmt.Sprintf("Failed to build directory tree: %v", err)
		return resp
	}

	if req.Verbosity > 1 {
		fmt.Fprintf(os.Stderr, "[request %d] Cache stats: %d hits, %d misses, %d total\n",
			req.RequestId, stats.cacheHits, stats.cacheMisses, stats.totalFiles)
	}

	return resp
}

// buildStats tracks cache performance
type buildStats struct {
	cacheHits   int
	cacheMisses int
	totalFiles  int
}

// buildDirtreeWithCache builds the directory tree using the cache
func (pb *persistentDirtreeBuilder) buildDirtreeWithCache(req *dirtreeRequest, sandboxDir string, inputDigests map[string]string) (*buildStats, error) {
	stats := &buildStats{}

	// Read the inputs file
	entries, err := readInputsFile(req.inputsFile, sandboxDir)
	if err != nil {
		return stats, fmt.Errorf("reading inputs file: %w", err)
	}

	// Build the directory tree structure
	root := buildTree(entries)

	// Calculate digests bottom-up and collect all directory protos
	dirProtos := make(map[string]*remoteexecution.Directory)
	rootDigest, err := pb.calculateDigestsWithCache(root, dirProtos, sandboxDir, inputDigests, req.digestFunction, stats)
	if err != nil {
		return stats, fmt.Errorf("calculating digests: %w", err)
	}

	// Create output directory for proto messages
	protoOutputDir := req.protoOutputDir
	if sandboxDir != "" && !filepath.IsAbs(protoOutputDir) {
		protoOutputDir = filepath.Join(sandboxDir, protoOutputDir)
	}
	if err := os.MkdirAll(protoOutputDir, 0o755); err != nil {
		return stats, fmt.Errorf("creating proto output directory: %w", err)
	}

	// Write all directory proto messages
	for digestStr, dirProto := range dirProtos {
		if err := writeProtoMessage(protoOutputDir, digestStr, req.digestFunction, dirProto); err != nil {
			return stats, fmt.Errorf("writing proto message: %w", err)
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
		return stats, fmt.Errorf("marshaling digest proto: %w", err)
	}

	if err := os.WriteFile(digestOutputPath, digestData, 0o644); err != nil {
		return stats, fmt.Errorf("writing digest output: %w", err)
	}

	return stats, nil
}

// calculateDigestsWithCache calculates digests for all directories bottom-up using cache
func (pb *persistentDirtreeBuilder) calculateDigestsWithCache(node *dirNode, dirProtos map[string]*remoteexecution.Directory, sandboxDir string, inputDigests map[string]string, digestFunction string, stats *buildStats) (*remoteexecution.Digest, error) {
	// First, recursively calculate digests for all subdirectories
	subdirDigests := make(map[string]*remoteexecution.Digest)
	for name, child := range node.subdirs {
		childDigest, err := pb.calculateDigestsWithCache(child, dirProtos, sandboxDir, inputDigests, digestFunction, stats)
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
			stats.totalFiles++

			// Calculate file digest with cache
			fileDigest, isExecutable, err := pb.calculateFileDigestCached(entry.sourcePath, sandboxDir, inputDigests, digestFunction, stats)
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

// calculateFileDigestCached calculates file digest with caching
func (pb *persistentDirtreeBuilder) calculateFileDigestCached(filePath string, sandboxDir string, inputDigests map[string]string, digestFunction string, stats *buildStats) (*remoteexecution.Digest, bool, error) {
	// Get the Bazel input digest for this file
	var bazelDigest string
	if sandboxDir != "" && !filepath.IsAbs(filePath) {
		// Check both with and without sandbox dir
		bazelDigest = inputDigests[filepath.Join(sandboxDir, filePath)]
		if bazelDigest == "" {
			bazelDigest = inputDigests[filePath]
		}
	} else {
		bazelDigest = inputDigests[filePath]
	}

	// Create cache key that includes the digest function
	cacheKey := digestFunction + ":" + bazelDigest

	// Check cache if we have a digest
	if bazelDigest != "" {
		pb.cacheMutex.RLock()
		cached, found := pb.fileDigestCache[cacheKey]
		pb.cacheMutex.RUnlock()

		if found {
			stats.cacheHits++
			return cached.digest, cached.isExecutable, nil
		}
	}

	// Cache miss - compute the digest
	stats.cacheMisses++
	digest, isExecutable, err := calculateFileDigest(filePath, sandboxDir, digestFunction)
	if err != nil {
		return nil, false, err
	}

	// Store in cache if we have a Bazel digest
	if bazelDigest != "" {
		pb.cacheMutex.Lock()
		pb.fileDigestCache[cacheKey] = &cachedFileDigest{
			digest:       digest,
			isExecutable: isExecutable,
		}
		pb.cacheMutex.Unlock()
	}

	return digest, isExecutable, nil
}
