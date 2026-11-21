package pull

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `img cosign-subjects` attributes a pinned cosign attestation from the files
// the repository rule downloaded, without a registry.
func TestCosignSubjects(t *testing.T) {
	dir := t.TempDir()
	child := "sha256:" + strings.Repeat("2", 64)
	statement, _ := json.Marshal(map[string]any{
		"_type":   "https://in-toto.io/Statement/v0.1",
		"subject": []any{map[string]any{"digest": map[string]string{"sha256": strings.Repeat("2", 64)}}},
	})
	envelope, _ := json.Marshal(map[string]string{"payloadType": "application/vnd.in-toto+json", "payload": base64.StdEncoding.EncodeToString(statement)})
	layer := "sha256:" + strings.Repeat("a", 64)
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	manifest, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": "sha256:" + strings.Repeat("b", 64), "size": 2},
		"layers":        []any{map[string]any{"mediaType": "application/vnd.dsse.envelope.v1+json", "digest": layer, "size": len(envelope)}},
	})
	input, _ := json.Marshal(CosignSubjectsInput{
		Tree: []string{"sha256:" + strings.Repeat("1", 64), child},
		Artifacts: []CosignSubjectsArtifact{{
			Digest:   "sha256:" + strings.Repeat("c", 64),
			Manifest: write("manifest.json", manifest),
			Blobs:    map[string]string{layer: write("layer.json", envelope)},
		}},
	})
	output := filepath.Join(dir, "out.json")
	if err := cosignSubjects(context.Background(), write("in.json", input), output); err != nil {
		t.Fatalf("cosignSubjects() = %v", err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]CosignSubject
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if got := result["sha256:"+strings.Repeat("c", 64)]; got != (CosignSubject{Subject: child, Tag: "att"}) {
		t.Errorf("cosignSubjects() attributed the attestation as %+v, want an attestation of %s", got, child)
	}
}
