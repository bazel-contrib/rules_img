package metadata

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

// "abc" under BLAKE3.
const abcBLAKE3 = "6437b3ac38465133ffb63b75273a8db548c558465d79db03fd359c6cd5bd9d85"

func writeFixture(t *testing.T, content string, descriptor api.Descriptor) (blobPath, metadataPath string) {
	t.Helper()
	dir := t.TempDir()
	blobPath = filepath.Join(dir, "layer.tgz")
	metadataPath = filepath.Join(dir, "layer.json")
	if err := os.WriteFile(blobPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return blobPath, metadataPath
}

func readDescriptor(t *testing.T, path string) api.Descriptor {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var descriptor api.Descriptor
	if err := json.Unmarshal(data, &descriptor); err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func TestRecordCASDigest(t *testing.T) {
	original := api.Descriptor{
		DiffID:      "sha256:diff",
		MediaType:   "application/vnd.oci.image.layer.v1.tar+gzip",
		Digest:      "sha256:abc",
		Size:        3,
		Annotations: map[string]string{"k": "v"},
		History:     []api.History{{CreatedBy: "test"}},
	}
	blob, metadata := writeFixture(t, "abc", original)

	if err := RecordCASDigest("blake3", blob, metadata); err != nil {
		t.Fatal(err)
	}

	got := readDescriptor(t, metadata)
	if got.BLAKE3Hex() != abcBLAKE3 {
		t.Errorf("recorded blake3 = %q, want %q", got.BLAKE3Hex(), abcBLAKE3)
	}
	// The SHA256 side of the metadata, the part registries see, is untouched.
	got.CASDigests = nil
	want, _ := json.Marshal(original)
	gotJSON, _ := json.Marshal(got)
	if string(gotJSON) != string(want) {
		t.Errorf("metadata changed besides cas_digests:\n got %s\nwant %s", gotJSON, want)
	}
}

func TestRecordCASDigestKeepsOtherDigests(t *testing.T) {
	blob, metadata := writeFixture(t, "abc", api.Descriptor{Digest: "sha256:abc", Size: 3, CASDigests: map[string]string{"other": "1234"}})
	if err := RecordCASDigest("blake3", blob, metadata); err != nil {
		t.Fatal(err)
	}
	got := readDescriptor(t, metadata).CASDigests
	if got["other"] != "1234" || got["blake3"] != abcBLAKE3 {
		t.Errorf("cas_digests = %v", got)
	}
}

func TestRecordCASDigestNoFunctionDoesNothing(t *testing.T) {
	blob, metadata := writeFixture(t, "abc", api.Descriptor{Digest: "sha256:abc", Size: 3})
	before, _ := os.ReadFile(metadata)
	if err := RecordCASDigest("", blob, metadata); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(metadata); string(after) != string(before) {
		t.Errorf("metadata rewritten: %s", after)
	}
}

func TestRecordCASDigestRejectsAMismatchedBlob(t *testing.T) {
	blob, metadata := writeFixture(t, "abc", api.Descriptor{Digest: "sha256:abc", Size: 99})
	err := RecordCASDigest("blake3", blob, metadata)
	if err == nil || !strings.Contains(err.Error(), "describes a blob of 99 bytes") {
		t.Fatalf("RecordCASDigest error = %v, want a size mismatch", err)
	}
	if readDescriptor(t, metadata).CASDigests != nil {
		t.Error("a digest was recorded for the wrong blob")
	}
}

func TestValidateCASDigest(t *testing.T) {
	for function, wantErr := range map[string]bool{"": false, "blake3": false, "sha1": true, "sha256": true} {
		if err := ValidateCASDigest(function); (err != nil) != wantErr {
			t.Errorf("ValidateCASDigest(%q) = %v, want error: %v", function, err, wantErr)
		}
	}
}
