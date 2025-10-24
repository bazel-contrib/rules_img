package blobstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStore_WriteSmall(t *testing.T) {
	tempDir := t.TempDir()
	store := New(filepath.Join(tempDir, "blobs"))

	if err := store.Init(); err != nil {
		t.Fatalf("Failed to initialize store: %v", err)
	}

	// Test writing a small blob
	data := []byte("test blob content")
	digest, err := store.WriteSmall(data)
	if err != nil {
		t.Fatalf("Failed to write small blob: %v", err)
	}

	// Verify digest format
	if !strings.HasPrefix(digest, "sha256:") {
		t.Errorf("Expected digest to start with 'sha256:', got %s", digest)
	}

	// Verify blob exists
	if !store.Exists(digest) {
		t.Errorf("Blob should exist after writing")
	}

	// Verify file was created
	expectedPath := store.Path(digest)
	if _, err := os.Stat(expectedPath); err != nil {
		t.Errorf("Blob file should exist at %s: %v", expectedPath, err)
	}

	// Write the same blob again - should not error
	digest2, err := store.WriteSmall(data)
	if err != nil {
		t.Fatalf("Failed to write duplicate blob: %v", err)
	}
	if digest2 != digest {
		t.Errorf("Expected same digest for same content, got %s and %s", digest, digest2)
	}
}

func TestStore_WriteSmallWithDigest(t *testing.T) {
	tempDir := t.TempDir()
	store := New(filepath.Join(tempDir, "blobs"))

	if err := store.Init(); err != nil {
		t.Fatalf("Failed to initialize store: %v", err)
	}

	// Calculate expected digest
	data := []byte("test blob content")
	hasher := sha256.New()
	hasher.Write(data)
	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))

	// Write with correct digest
	if err := store.WriteSmallWithDigest(digest, data); err != nil {
		t.Fatalf("Failed to write blob with digest: %v", err)
	}

	// Verify blob exists
	if !store.Exists(digest) {
		t.Errorf("Blob should exist after writing")
	}

	// Write with incorrect digest - should error
	wrongDigest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if err := store.WriteSmallWithDigest(wrongDigest, data); err == nil {
		t.Errorf("Expected error when writing with wrong digest")
	}

	// Write the same blob again - should not error
	if err := store.WriteSmallWithDigest(digest, data); err != nil {
		t.Fatalf("Failed to write duplicate blob: %v", err)
	}
}

func TestStore_WriteLarge(t *testing.T) {
	tempDir := t.TempDir()
	store := New(filepath.Join(tempDir, "blobs"))

	if err := store.Init(); err != nil {
		t.Fatalf("Failed to initialize store: %v", err)
	}

	// Create a large blob
	data := bytes.Repeat([]byte("test"), 1024*256) // 1MB
	hasher := sha256.New()
	hasher.Write(data)
	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))

	// Write large blob
	reader := bytes.NewReader(data)
	if err := store.WriteLarge(digest, reader); err != nil {
		t.Fatalf("Failed to write large blob: %v", err)
	}

	// Verify blob exists
	if !store.Exists(digest) {
		t.Errorf("Blob should exist after writing")
	}

	// Write with incorrect digest - should error
	reader2 := bytes.NewReader(data)
	wrongDigest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if err := store.WriteLarge(wrongDigest, reader2); err == nil {
		t.Errorf("Expected error when writing with wrong digest")
	}

	// Verify wrong blob was not created
	if store.Exists(wrongDigest) {
		t.Errorf("Blob with wrong digest should not exist")
	}

	// Write the same blob again - should not error and should consume reader
	reader3 := bytes.NewReader(data)
	if err := store.WriteLarge(digest, reader3); err != nil {
		t.Fatalf("Failed to write duplicate blob: %v", err)
	}
}

func TestStore_ReadSmall(t *testing.T) {
	tempDir := t.TempDir()
	store := New(filepath.Join(tempDir, "blobs"))

	if err := store.Init(); err != nil {
		t.Fatalf("Failed to initialize store: %v", err)
	}

	// Write a blob
	data := []byte("test blob content for reading")
	digest, err := store.WriteSmall(data)
	if err != nil {
		t.Fatalf("Failed to write blob: %v", err)
	}

	// Read it back
	readData, err := store.ReadSmall(digest)
	if err != nil {
		t.Fatalf("Failed to read blob: %v", err)
	}

	// Verify content
	if !bytes.Equal(data, readData) {
		t.Errorf("Read data doesn't match written data")
	}

	// Try to read non-existent blob
	fakeDigest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := store.ReadSmall(fakeDigest); err == nil {
		t.Errorf("Expected error when reading non-existent blob")
	}

	// Corrupt the blob and try to read
	corruptData := []byte("corrupted")
	if err := os.WriteFile(store.Path(digest), corruptData, 0o644); err != nil {
		t.Fatalf("Failed to corrupt blob: %v", err)
	}

	if _, err := store.ReadSmall(digest); err == nil {
		t.Errorf("Expected error when reading corrupted blob")
	}

	// Verify corrupted blob was removed
	if store.Exists(digest) {
		t.Errorf("Corrupted blob should have been removed")
	}
}

func TestStore_Open(t *testing.T) {
	tempDir := t.TempDir()
	store := New(filepath.Join(tempDir, "blobs"))

	if err := store.Init(); err != nil {
		t.Fatalf("Failed to initialize store: %v", err)
	}

	// Write a blob
	data := []byte("test blob content for opening")
	digest, err := store.WriteSmall(data)
	if err != nil {
		t.Fatalf("Failed to write blob: %v", err)
	}

	// Open and read it
	reader, err := store.Open(digest)
	if err != nil {
		t.Fatalf("Failed to open blob: %v", err)
	}
	defer reader.Close()

	readData, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("Failed to read from opened blob: %v", err)
	}

	// Verify content
	if !bytes.Equal(data, readData) {
		t.Errorf("Read data doesn't match written data")
	}

	// Try to open non-existent blob
	fakeDigest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := store.Open(fakeDigest); err == nil {
		t.Errorf("Expected error when opening non-existent blob")
	}
}

func TestStore_Path(t *testing.T) {
	tempDir := t.TempDir()
	store := New(filepath.Join(tempDir, "blobs"))

	// Test with sha256: prefix
	digest1 := "sha256:abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234"
	path1 := store.Path(digest1)
	expected1 := filepath.Join(tempDir, "blobs", "sha256", "abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234")
	if path1 != expected1 {
		t.Errorf("Expected path %s, got %s", expected1, path1)
	}

	// Test without sha256: prefix
	digest2 := "abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234"
	path2 := store.Path(digest2)
	if path2 != expected1 {
		t.Errorf("Expected path %s, got %s", expected1, path2)
	}
}

func TestStore_ConcurrentWrites(t *testing.T) {
	tempDir := t.TempDir()
	store := New(filepath.Join(tempDir, "blobs"))

	if err := store.Init(); err != nil {
		t.Fatalf("Failed to initialize store: %v", err)
	}

	// Create test data
	data := []byte("concurrent test blob")
	hasher := sha256.New()
	hasher.Write(data)
	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))

	// Run concurrent writes
	errChan := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func() {
			errChan <- store.WriteSmallWithDigest(digest, data)
		}()
	}

	// Check all writes succeeded
	for i := 0; i < 10; i++ {
		if err := <-errChan; err != nil {
			t.Errorf("Concurrent write failed: %v", err)
		}
	}

	// Verify blob exists and is correct
	readData, err := store.ReadSmall(digest)
	if err != nil {
		t.Fatalf("Failed to read blob after concurrent writes: %v", err)
	}

	if !bytes.Equal(data, readData) {
		t.Errorf("Data corrupted after concurrent writes")
	}
}
