package hash

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fileopener"
)

// layerMetadata holds precomputed layer information.
type layerMetadata struct {
	diffID            []byte            // set when "diff_id" mode is active
	diffIDAnnotations map[string][]byte // set when "diff_id_annotation:<name>" modes are active
	compressedSize    int64
	layerFormat       api.LayerFormat
}

const diffIDAnnotationPrefix = "diff_id_annotation:"

// digestModesNeedDiffID returns true if any of the modes require computing the diff ID.
func digestModesNeedDiffID(modes []string) bool {
	for _, mode := range modes {
		if mode == "diff_id" || len(mode) > len(diffIDAnnotationPrefix) && mode[:len(diffIDAnnotationPrefix)] == diffIDAnnotationPrefix {
			return true
		}
	}
	return false
}

// buildLayerMetadata constructs a layerMetadata from raw cached values and digest modes.
func buildLayerMetadata(diffID []byte, layerFormat api.LayerFormat, compressedSize int64, digestModes []string) *layerMetadata {
	meta := &layerMetadata{
		compressedSize: compressedSize,
		layerFormat:    layerFormat,
	}
	for _, mode := range digestModes {
		switch {
		case mode == "diff_id":
			meta.diffID = diffID
		case len(mode) > len(diffIDAnnotationPrefix) && mode[:len(diffIDAnnotationPrefix)] == diffIDAnnotationPrefix:
			name := mode[len(diffIDAnnotationPrefix):]
			if meta.diffIDAnnotations == nil {
				meta.diffIDAnnotations = make(map[string][]byte)
			}
			meta.diffIDAnnotations[name] = diffID
		}
	}
	return meta
}

// computeHash computes the hash of the input file.
// If sandboxDir is non-empty, it is used as a prefix for the input path.
func computeHash(inputPath, digestAlg, sandboxDir string) ([]byte, error) {
	var h hash.Hash
	switch digestAlg {
	case "sha256":
		h = sha256.New()
	case "sha512":
		h = sha512.New()
	default:
		return nil, fmt.Errorf("unsupported digest algorithm: %s", digestAlg)
	}

	// Apply sandbox prefix if provided
	actualPath := inputPath
	if sandboxDir != "" {
		actualPath = filepath.Join(sandboxDir, inputPath)
	}

	file, err := os.Open(actualPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open input file %s: %w", actualPath, err)
	}
	defer file.Close()

	if _, err := io.Copy(h, file); err != nil {
		return nil, fmt.Errorf("failed to hash input file: %w", err)
	}

	return h.Sum(nil), nil
}

// computeLayerHashes computes hashes according to the requested digest modes.
// Supported modes:
//   - "digest": sha256 of the file as-is (the blob/compressed digest).
//   - "diff_id": sha256 of the uncompressed content (the OCI diff ID), stored in DiffID.
//   - "diff_id_annotation:<name>": same as diff_id but stored as annotation <name>.
//
// Returns (compressedHash, layerMetadata, error).
func computeLayerHashes(inputPath, digestAlg, sandboxDir string, digestModes []string) ([]byte, *layerMetadata, error) {
	if digestAlg != "sha256" {
		return nil, nil, fmt.Errorf("layer metadata only supports sha256, got: %s", digestAlg)
	}

	// Parse modes
	needsDiffID := digestModesNeedDiffID(digestModes)
	for _, mode := range digestModes {
		switch {
		case mode == "digest":
			// always computed
		case mode == "diff_id":
			// handled below
		case len(mode) > len(diffIDAnnotationPrefix) && mode[:len(diffIDAnnotationPrefix)] == diffIDAnnotationPrefix:
			// handled below
		default:
			return nil, nil, fmt.Errorf("unsupported digest mode: %s", mode)
		}
	}

	// Apply sandbox prefix if provided
	actualPath := inputPath
	if sandboxDir != "" {
		actualPath = filepath.Join(sandboxDir, inputPath)
	}

	file, err := os.Open(actualPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open input file %s: %w", actualPath, err)
	}
	defer file.Close()

	// Get file size for compressed size
	fileInfo, err := file.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to stat layer file: %w", err)
	}
	compressedSize := fileInfo.Size()

	if !needsDiffID {
		// Only need the blob digest — no format detection or decompression required.
		h := sha256.New()
		if _, err := io.Copy(h, file); err != nil {
			return nil, nil, fmt.Errorf("failed to hash input file: %w", err)
		}
		blobHash := h.Sum(nil)
		return blobHash, buildLayerMetadata(nil, "", compressedSize, digestModes), nil
	}

	// Need diff ID — detect layer format to decompress.
	layerFormat, err := fileopener.LearnLayerFormat(file)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to determine layer format: %w", err)
	}

	// Reset file pointer
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, nil, fmt.Errorf("failed to seek to start of layer file: %w", err)
	}

	// Create hashers
	compressedHasher := sha256.New()
	uncompressedHasher := sha256.New()

	// Compute hashes in a single pass
	var compressedHash, uncompressedHash []byte
	if layerFormat == api.TarLayer {
		// For uncompressed tar, both hashes are the same
		if _, err := io.Copy(io.MultiWriter(compressedHasher, uncompressedHasher), file); err != nil {
			return nil, nil, fmt.Errorf("failed to hash uncompressed tar: %w", err)
		}
		compressedHash = compressedHasher.Sum(nil)
		uncompressedHash = compressedHash
	} else {
		// For compressed layers, hash both compressed and uncompressed content
		teeReader := io.TeeReader(file, compressedHasher)
		decompressReader, err := fileopener.CompressionReaderWithFormat(teeReader, layerFormat.CompressionAlgorithm())
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create decompression reader: %w", err)
		}

		if _, err := io.Copy(uncompressedHasher, decompressReader); err != nil {
			return nil, nil, fmt.Errorf("failed to hash uncompressed content: %w", err)
		}

		compressedHash = compressedHasher.Sum(nil)
		uncompressedHash = uncompressedHasher.Sum(nil)
	}

	return compressedHash, buildLayerMetadata(uncompressedHash, layerFormat, compressedSize, digestModes), nil
}

// encodeHash encodes the hash bytes according to the specified encoding.
func encodeHash(hashBytes []byte, encoding, digest string) ([]byte, error) {
	switch encoding {
	case "raw":
		return hashBytes, nil
	case "hex":
		return []byte(hex.EncodeToString(hashBytes)), nil
	case "sri":
		b64Hash := base64.StdEncoding.EncodeToString(hashBytes)
		return fmt.Appendf(nil, "%s-%s", digest, b64Hash), nil
	case "oci-digest":
		hexHash := hex.EncodeToString(hashBytes)
		return fmt.Appendf(nil, "%s:%s", digest, hexHash), nil
	default:
		return nil, fmt.Errorf("unsupported encoding: %s", encoding)
	}
}

// writeHashOutput writes the hash to the output file with the specified encoding.
// If sandboxDir is non-empty, it is used as a prefix for the output path.
// For layer metadata, layerMeta must be provided.
func writeHashOutput(hashBytes []byte, req *hashRequest, sandboxDir string, layerMeta *layerMetadata) error {
	// Apply sandbox prefix if provided
	outputPath := req.output
	if sandboxDir != "" {
		outputPath = filepath.Join(sandboxDir, req.output)
	}

	// Handle layer-metadata encoding specially
	if req.layerMeta {
		if layerMeta == nil {
			return fmt.Errorf("layer metadata encoding requires layer metadata to be computed")
		}
		return writeLayerMetadata(hashBytes, layerMeta, req, outputPath)
	}

	outputData, err := encodeHash(hashBytes, req.encoding, req.digest)
	if err != nil {
		return err
	}

	if err := os.WriteFile(outputPath, outputData, 0o644); err != nil {
		return fmt.Errorf("failed to write output file %s: %w", outputPath, err)
	}

	return nil
}

// writeLayerMetadata writes layer metadata to a JSON file using precomputed data.
func writeLayerMetadata(compressedHash []byte, meta *layerMetadata, req *hashRequest, outputPath string) (retErr error) {
	// Build layer name
	layerName := req.name
	if layerName == "" {
		layerName = fmt.Sprintf("sha256:%x", compressedHash)
	}

	// Use override media type (e.g. for Helm charts) or infer from layer format
	mediaType := req.mediaType
	if mediaType == "" {
		mediaType = string(meta.layerFormat)
	}

	// Build DiffID — only set when "diff_id" mode was requested
	diffID := ""
	if meta.diffID != nil {
		diffID = fmt.Sprintf("sha256:%x", meta.diffID)
	}

	// Merge diff_id_annotation values into annotations
	mergedAnnotations := req.annotations
	if len(meta.diffIDAnnotations) > 0 {
		if mergedAnnotations == nil {
			mergedAnnotations = make(map[string]string, len(meta.diffIDAnnotations))
		}
		for name, hash := range meta.diffIDAnnotations {
			mergedAnnotations[name] = fmt.Sprintf("sha256:%x", hash)
		}
	}

	// Create descriptor
	descriptor := api.Descriptor{
		Name:        layerName,
		DiffID:      diffID,
		MediaType:   mediaType,
		Digest:      fmt.Sprintf("sha256:%x", compressedHash),
		Size:        meta.compressedSize,
		Annotations: mergedAnnotations,
	}

	// Write JSON output
	outputFile, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("failed to open output file %s: %w", outputPath, err)
	}
	defer func() {
		if err := outputFile.Close(); retErr == nil && err != nil {
			retErr = fmt.Errorf("closing output file: %w", err)
		}
	}()

	encoder := json.NewEncoder(outputFile)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(descriptor); err != nil {
		return fmt.Errorf("failed to encode layer metadata: %w", err)
	}

	return nil
}
