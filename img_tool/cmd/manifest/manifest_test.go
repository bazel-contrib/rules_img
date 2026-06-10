package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// descriptorJSON returns a minimal OCI descriptor JSON with the given digest hex.
func descriptorJSON(t *testing.T, hexDigest string) string {
	t.Helper()
	m := map[string]any{
		"mediaType": "application/vnd.oci.image.manifest.v1+json",
		"digest":    "sha256:" + hexDigest,
		"size":      1024,
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal descriptor: %v", err)
	}
	return string(b)
}

// writeTempDescriptor writes a descriptor JSON file and returns its path.
func writeTempDescriptor(t *testing.T, hexDigest string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "descriptor.json")
	if err := os.WriteFile(path, []byte(descriptorJSON(t, hexDigest)), 0o600); err != nil {
		t.Fatalf("write temp descriptor: %v", err)
	}
	return path
}

func TestAnnotationsFromBaseImageDescriptorFile(t *testing.T) {
	const (
		fakeDigest      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		baseDigestKey   = "org.opencontainers.image.base.digest"
		expectedDigest  = "sha256:" + fakeDigest
	)

	tests := []struct {
		name        string
		filePath    func(t *testing.T) string // returns "" or a real temp path
		annotations map[string]string
		wantErr     bool
		// wantAnnotations is the full expected return value; nil means the return should be nil.
		wantAnnotations map[string]string
	}{
		{
			name:            "no_file_nil_annotations",
			filePath:        func(*testing.T) string { return "" },
			annotations:     nil,
			wantAnnotations: nil,
		},
		{
			// This is the bug case: existing annotations must survive when there is no base
			// descriptor file.  Before the fix the function returns nil, discarding them.
			name:     "no_file_with_annotations",
			filePath: func(*testing.T) string { return "" },
			annotations: map[string]string{
				"foo": "bar",
			},
			wantAnnotations: map[string]string{
				"foo": "bar",
			},
		},
		{
			name:            "with_descriptor_nil_annotations",
			filePath:        func(t *testing.T) string { return writeTempDescriptor(t, fakeDigest) },
			annotations:     nil,
			wantAnnotations: map[string]string{baseDigestKey: expectedDigest},
		},
		{
			name:     "with_descriptor_existing_annotations",
			filePath: func(t *testing.T) string { return writeTempDescriptor(t, fakeDigest) },
			annotations: map[string]string{
				"foo": "bar",
			},
			wantAnnotations: map[string]string{
				"foo":          "bar",
				baseDigestKey: expectedDigest,
			},
		},
		{
			// When base.digest is already present it must not be overwritten.
			name:     "with_descriptor_digest_already_set",
			filePath: func(t *testing.T) string { return writeTempDescriptor(t, fakeDigest) },
			annotations: map[string]string{
				baseDigestKey: "sha256:existing",
			},
			wantAnnotations: map[string]string{
				baseDigestKey: "sha256:existing",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := annotationsFromBaseImageDescriptorFile(tc.filePath(t), tc.annotations)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(got) != len(tc.wantAnnotations) {
				t.Fatalf("len(got)=%d, len(want)=%d\ngot:  %v\nwant: %v", len(got), len(tc.wantAnnotations), got, tc.wantAnnotations)
			}
			for k, wantV := range tc.wantAnnotations {
				if gotV, ok := got[k]; !ok {
					t.Errorf("key %q missing from result", k)
				} else if gotV != wantV {
					t.Errorf("key %q: got %q, want %q", k, gotV, wantV)
				}
			}
		})
	}
}
