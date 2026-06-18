package blobstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Store manages blob storage on disk with content-addressable naming
type Store struct {
	// blobDir is the root directory for blob storage (e.g., "outputDir/blobs")
	blobDir string
}

// New creates a new blob store with the given root directory
// The directory structure will be: blobDir/sha256/{hash}
func New(blobDir string) *Store {
	return &Store{
		blobDir: blobDir,
	}
}

// Init ensures the blob directory structure exists
func (s *Store) Init() error {
	return os.MkdirAll(filepath.Join(s.blobDir, "sha256"), 0o755)
}

// Exists checks if a blob with the given digest exists in the store
func (s *Store) Exists(digest string) bool {
	path := s.blobPath(digest)
	_, err := os.Stat(path)
	return err == nil
}

// WriteSmall writes a small blob to the store if it doesn't already exist
// Returns the digest of the written blob
func (s *Store) WriteSmall(data []byte) (string, error) {
	// Calculate digest
	hasher := sha256.New()
	hasher.Write(data)
	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))

	// Check if already exists
	if s.Exists(digest) {
		return digest, nil
	}

	// Write to disk
	path := s.blobPath(digest)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("writing blob %s: %w", digest, err)
	}

	return digest, nil
}

// WriteSmallWithDigest writes a small blob with a known digest if it doesn't exist
// It validates that the data matches the expected digest
func (s *Store) WriteSmallWithDigest(digest string, data []byte) error {
	// Check if already exists
	if s.Exists(digest) {
		return nil
	}

	// Validate digest
	hasher := sha256.New()
	hasher.Write(data)
	actualDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))

	if actualDigest != digest {
		return fmt.Errorf("digest mismatch: expected %s, got %s", digest, actualDigest)
	}

	// Write to disk
	path := s.blobPath(digest)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing blob %s: %w", digest, err)
	}

	return nil
}

// WriteLarge consumes an io.Reader and writes a large blob to the store if it doesn't exist
// The digest must be provided as we don't want to buffer the entire content in memory
func (s *Store) WriteLarge(digest string, r io.Reader) error {
	// Check if already exists
	if s.Exists(digest) {
		// Still need to consume the reader to avoid broken pipes
		_, _ = io.Copy(io.Discard, r)
		return nil
	}

	// Write to a temporary file first
	path := s.blobPath(digest)
	tempFile, err := os.CreateTemp(filepath.Dir(path), "blobstore_tmp")
	if err != nil {
		return fmt.Errorf("creating temp file for blob %s: %w", digest, err)
	}
	tempPath := tempFile.Name()

	defer func() {
		_ = tempFile.Close()
		_ = os.Remove(tempPath) // Clean up temp file if it still exists
	}()

	// Calculate digest while writing
	hasher := sha256.New()
	w := io.MultiWriter(tempFile, hasher)

	// Copy data from the reader to the writer
	if _, err := io.Copy(w, r); err != nil {
		return fmt.Errorf("writing blob %s: %w", digest, err)
	}

	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("closing temp file for blob %s: %w", digest, err)
	}

	// Validate digest
	actualDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if actualDigest != digest {
		return fmt.Errorf("digest mismatch for blob: expected %s, got %s", digest, actualDigest)
	}

	// Atomically rename to final location
	// if the OS supports it (Windows doesn't really).
	if err := os.Rename(tempPath, path); err != nil {
		// if renaming fails, check if the destination is already correct.
		if s.Exists(digest) {
			return nil
		}
		return fmt.Errorf("renaming blob %s to final location: %w", digest, err)
	}

	return nil
}

// ReadSmall reads a small blob from the store as a byte slice
// Returns an error if the blob doesn't exist
func (s *Store) ReadSmall(digest string) ([]byte, error) {
	path := s.blobPath(digest)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("blob %s not found", digest)
		}
		return nil, fmt.Errorf("reading blob %s: %w", digest, err)
	}

	// Validate digest
	hasher := sha256.New()
	hasher.Write(data)
	actualDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))

	if actualDigest != digest {
		// Remove corrupted blob
		os.Remove(path)
		return nil, fmt.Errorf("digest mismatch for blob %s: expected %s, got %s", path, digest, actualDigest)
	}

	return data, nil
}

// Open opens a blob for reading, returning an io.ReadCloser
// The caller is responsible for closing the reader
func (s *Store) Open(digest string) (io.ReadCloser, error) {
	path := s.blobPath(digest)
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("blob %s not found", digest)
		}
		return nil, fmt.Errorf("opening blob %s: %w", digest, err)
	}

	// We return a validatingReader that will check the digest on close
	return &validatingReader{
		file:           file,
		path:           path,
		expectedDigest: digest,
		hasher:         sha256.New(),
	}, nil
}

// Path returns the filesystem path for a given digest
// This can be useful for operations that need direct file access
func (s *Store) Path(digest string) string {
	return s.blobPath(digest)
}

// blobPath constructs the filesystem path for a blob with the given digest
func (s *Store) blobPath(digest string) string {
	// Remove "sha256:" prefix if present
	sha256sum := strings.TrimPrefix(digest, "sha256:")
	return filepath.Join(s.blobDir, "sha256", sha256sum)
}

// validatingReader wraps a file and validates its digest when fully read
type validatingReader struct {
	file           *os.File
	path           string
	expectedDigest string
	hasher         io.Writer
	tee            io.Reader
	initialized    bool
}

func (v *validatingReader) Read(p []byte) (int, error) {
	if !v.initialized {
		v.tee = io.TeeReader(v.file, v.hasher)
		v.initialized = true
	}

	n, err := v.tee.Read(p)
	if err == io.EOF {
		// Validate digest when we reach EOF
		actualDigest := "sha256:" + hex.EncodeToString(v.hasher.(interface{ Sum([]byte) []byte }).Sum(nil))
		if actualDigest != v.expectedDigest {
			return n, fmt.Errorf("digest mismatch for blob %s: expected %s, got %s", v.path, v.expectedDigest, actualDigest)
		}
	}
	return n, err
}

func (v *validatingReader) Close() error {
	return v.file.Close()
}
