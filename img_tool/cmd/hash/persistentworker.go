package hash

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/persistentworker"
)

type persistentHasher struct {
	sha256Cache         map[string][]byte
	sha256Mutex         sync.RWMutex
	sha512Cache         map[string][]byte
	sha512Mutex         sync.RWMutex
	diffIDCache         map[string][]byte // Maps bazel digest -> uncompressed SHA256
	diffIDMutex         sync.RWMutex
	layerFormatCache    map[string]string // Maps bazel digest -> layer format
	layerFormatMutex    sync.RWMutex
	compressedSizeCache map[string]int64 // Maps bazel digest -> compressed size
	compressedSizeMutex sync.RWMutex
	cheatMode           bool
}

func newPersistentHasher(cheatMode bool) *persistentHasher {
	return &persistentHasher{
		sha256Cache:         make(map[string][]byte),
		sha512Cache:         make(map[string][]byte),
		diffIDCache:         make(map[string][]byte),
		layerFormatCache:    make(map[string]string),
		compressedSizeCache: make(map[string]int64),
		cheatMode:           cheatMode,
	}
}

// tryExtractHashFromDigest attempts to extract a SHA256 hash from Bazel's digest.
// It tries to base64 decode the digest, then hex decode that result.
// Returns the hash bytes if successful (must be 32 bytes), nil otherwise.
//
// Sources;
//   - Protobuf JSON format for fields of type bytes encodes the bytes as base64.
//     https://github.com/protocolbuffers/protobuf/blob/a04d9e4bfdc374b0b75bdded628151d99f12a2ce/java/util/src/main/java/com/google/protobuf/util/JsonFormat.java#L1246-L1250
//   - Bazel encodes digests as hex strings. Here is where the digest string is set:
//     https://cs.opensource.google/bazel/bazel/+/master:src/main/java/com/google/devtools/build/lib/worker/JsonWorkerMessageProcessor.java;l=91;drc=c46b9ceee1d90a04fed3b217be53fa87397fcff4
func tryExtractHashFromDigest(digest string) []byte {
	// Try base64 decode
	b64Decoded, err := base64.StdEncoding.DecodeString(digest)
	if err != nil {
		return nil
	}

	// Try hex decode the base64-decoded data
	hexDecoded, err := hex.DecodeString(string(b64Decoded))
	if err != nil {
		return nil
	}

	// Check if it's the right length for SHA256 (32 bytes)
	if len(hexDecoded) != 32 {
		return nil
	}

	return hexDecoded
}

// HandleRequest processes a single work request and returns the response.
// This implements the persistentworker.Handler interface.
func (ph *persistentHasher) HandleRequest(ctx context.Context, req persistentworker.WorkRequest) persistentworker.WorkResponse {
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

	// Parse hash request arguments (Bazel already expanded argfiles)
	hashReq, err := parseHashRequest(req.Arguments)
	if err != nil {
		resp.ExitCode = 1
		resp.Output = fmt.Sprintf("Failed to parse hash request: %v", err)
		return resp
	}

	// Validate digest algorithm
	if hashReq.digest != "sha256" && hashReq.digest != "sha512" {
		resp.ExitCode = 1
		resp.Output = fmt.Sprintf("Unsupported digest for persistent worker: %s (must be sha256 or sha512)", hashReq.digest)
		return resp
	}

	// Find the input in the inputs list to get its digest
	var inputDigest string
	for _, input := range req.Inputs {
		if input.Path == hashReq.input {
			inputDigest = input.Digest
			break
		}
	}

	// Try cheat mode: extract hash from Bazel's digest
	if ph.cheatMode && inputDigest != "" && hashReq.digest == "sha256" {
		if hashBytes := tryExtractHashFromDigest(inputDigest); hashBytes != nil {
			if req.Verbosity > 1 {
				fmt.Fprintf(os.Stderr, "[request %d] Cheat mode: extracted hash from digest %s\n", req.RequestId, inputDigest)
			}
			// Use extracted hash directly (don't cache cheated requests)
			// Note: cheat mode doesn't support layer metadata
			if err := writeHashOutput(hashBytes, hashReq, req.SandboxDir, nil); err != nil {
				resp.ExitCode = 1
				resp.Output = fmt.Sprintf("Failed to write output: %v", err)
			}
			return resp
		}
		if req.Verbosity > 1 {
			fmt.Fprintf(os.Stderr, "[request %d] Cheat mode: failed to extract hash from digest, falling back to normal hashing\n", req.RequestId)
		}
	}

	// Check cache if we have a digest
	if inputDigest != "" {
		// Use the appropriate cache based on digest algorithm
		var cachedHash []byte
		var cachedDiffID []byte
		var cacheHit bool
		var diffIDCacheHit bool

		switch hashReq.digest {
		case "sha256":
			ph.sha256Mutex.RLock()
			cachedHash, cacheHit = ph.sha256Cache[inputDigest]
			ph.sha256Mutex.RUnlock()
		case "sha512":
			ph.sha512Mutex.RLock()
			cachedHash, cacheHit = ph.sha512Cache[inputDigest]
			ph.sha512Mutex.RUnlock()
		}

		// Check diffID, format, and size caches if layer metadata is needed
		var cachedFormat string
		var cachedSize int64
		var formatCacheHit, sizeCacheHit bool

		if hashReq.layerMeta && hashReq.digest == "sha256" {
			ph.diffIDMutex.RLock()
			cachedDiffID, diffIDCacheHit = ph.diffIDCache[inputDigest]
			ph.diffIDMutex.RUnlock()

			ph.layerFormatMutex.RLock()
			cachedFormat, formatCacheHit = ph.layerFormatCache[inputDigest]
			ph.layerFormatMutex.RUnlock()

			ph.compressedSizeMutex.RLock()
			cachedSize, sizeCacheHit = ph.compressedSizeCache[inputDigest]
			ph.compressedSizeMutex.RUnlock()
		}

		// Only use cache if we have all required data
		canUseCache := cacheHit && (!hashReq.layerMeta || (diffIDCacheHit && formatCacheHit && sizeCacheHit))

		if canUseCache {
			if req.Verbosity > 1 {
				fmt.Fprintf(os.Stderr, "[request %d] Full cache hit for input %s (digest: %s)\n", req.RequestId, hashReq.input, inputDigest)
			}

			// Reconstruct layer metadata from cached values (no file I/O!)
			var layerMeta *layerMetadata
			if hashReq.layerMeta {
				layerMeta = &layerMetadata{
					diffID:         cachedDiffID,
					compressedSize: cachedSize,
					layerFormat:    api.LayerFormat(cachedFormat),
				}
			}

			// Use cached hash
			if err := writeHashOutput(cachedHash, hashReq, req.SandboxDir, layerMeta); err != nil {
				resp.ExitCode = 1
				resp.Output = fmt.Sprintf("Failed to write output: %v", err)
			}
			return resp
		}
	}

	// Compute hash and layer metadata
	if req.Verbosity > 1 {
		if inputDigest != "" {
			fmt.Fprintf(os.Stderr, "[request %d] Cache miss for input %s (digest: %s), computing hash\n", req.RequestId, hashReq.input, inputDigest)
		} else {
			fmt.Fprintf(os.Stderr, "[request %d] Computing hash for input %s (no digest available)\n", req.RequestId, hashReq.input)
		}
	}

	var hashBytes []byte
	var layerMeta *layerMetadata

	if hashReq.layerMeta {
		// Compute both hashes in a single pass for layer metadata
		var err error
		hashBytes, layerMeta, err = computeLayerHashes(hashReq.input, hashReq.digest, req.SandboxDir)
		if err != nil {
			resp.ExitCode = 1
			resp.Output = fmt.Sprintf("Failed to compute layer hashes: %v", err)
			return resp
		}

		// Cache the diffID, format, and size if we have a digest
		if inputDigest != "" && hashReq.digest == "sha256" {
			ph.diffIDMutex.Lock()
			ph.diffIDCache[inputDigest] = layerMeta.diffID
			ph.diffIDMutex.Unlock()

			ph.layerFormatMutex.Lock()
			ph.layerFormatCache[inputDigest] = string(layerMeta.layerFormat)
			ph.layerFormatMutex.Unlock()

			ph.compressedSizeMutex.Lock()
			ph.compressedSizeCache[inputDigest] = layerMeta.compressedSize
			ph.compressedSizeMutex.Unlock()

			if req.Verbosity > 1 {
				fmt.Fprintf(os.Stderr, "[request %d] Cached layer metadata (diffID, format, size) for digest %s\n", req.RequestId, inputDigest)
			}
		}
	} else {
		// Just compute the hash
		var err error
		hashBytes, err = computeHash(hashReq.input, hashReq.digest, req.SandboxDir)
		if err != nil {
			resp.ExitCode = 1
			resp.Output = fmt.Sprintf("Failed to compute hash: %v", err)
			return resp
		}
	}

	// Cache the result if we have a digest
	if inputDigest != "" {
		// Store in the appropriate cache based on digest algorithm
		switch hashReq.digest {
		case "sha256":
			ph.sha256Mutex.Lock()
			ph.sha256Cache[inputDigest] = hashBytes
			ph.sha256Mutex.Unlock()
		case "sha512":
			ph.sha512Mutex.Lock()
			ph.sha512Cache[inputDigest] = hashBytes
			ph.sha512Mutex.Unlock()
		}
		if req.Verbosity > 1 {
			fmt.Fprintf(os.Stderr, "[request %d] Cached result for digest %s\n", req.RequestId, inputDigest)
		}
	}

	// Write output
	if err := writeHashOutput(hashBytes, hashReq, req.SandboxDir, layerMeta); err != nil {
		resp.ExitCode = 1
		resp.Output = fmt.Sprintf("Failed to write output: %v", err)
		return resp
	}

	return resp
}
