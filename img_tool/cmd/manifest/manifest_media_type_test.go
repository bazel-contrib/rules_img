package manifest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	specv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// buildManifest runs ManifestProcess with args plus output paths in dir and
// returns the raw manifest and its parsed descriptor.
func buildManifest(t *testing.T, dir string, args ...string) ([]byte, specv1.Descriptor) {
	t.Helper()
	resetManifestFlags()
	layerFromMetadataArgs = nil
	annotations = stringMap{}

	manifestPath := filepath.Join(dir, "manifest.json")
	descriptorPath := filepath.Join(dir, "descriptor.json")
	args = append(args, "--manifest", manifestPath, "--descriptor", descriptorPath)
	ManifestProcess(context.Background(), args)

	manifestRaw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	descriptorRaw, err := os.ReadFile(descriptorPath)
	if err != nil {
		t.Fatal(err)
	}
	var descriptor specv1.Descriptor
	if err := json.Unmarshal(descriptorRaw, &descriptor); err != nil {
		t.Fatalf("parsing descriptor: %v", err)
	}
	return manifestRaw, descriptor
}

func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOmitMediaType(t *testing.T) {
	inputs := t.TempDir()
	helmConfig := writeTestFile(t, inputs, "helm_config.json", `{"name":"test-chart","version":"1.0.0"}`)
	subject := writeTestFile(t, inputs, "subject.json", `{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:`+fmt.Sprintf("%064x", 1)+`","size":123}`)

	tests := []struct {
		name string
		args []string
	}{
		{
			name: "helm chart",
			args: []string{"--config-media-type=application/vnd.cncf.helm.config.v1+json", "--config-fragment=" + helmConfig},
		},
		{
			// Allowed for an ordinary image too: the caller owns the choice.
			name: "container image",
			args: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"--annotation=org.opencontainers.image.title=test", "--subject-descriptor=" + subject}, tt.args...)
			withRaw, withDescriptor := buildManifest(t, t.TempDir(), args...)
			omittedRaw, omittedDescriptor := buildManifest(t, t.TempDir(), append(args, "--omit-media-type")...)

			with := decodeObject(t, withRaw)
			omitted := decodeObject(t, omittedRaw)
			if got := with["mediaType"]; got != specv1.MediaTypeImageManifest {
				t.Errorf("default manifest mediaType = %v, want %s", got, specv1.MediaTypeImageManifest)
			}
			if _, ok := omitted["mediaType"]; ok {
				t.Errorf("manifest with --omit-media-type has a top-level mediaType:\n%s", omittedRaw)
			}

			// Nothing but the field differs.
			delete(with, "mediaType")
			withoutField, err := json.Marshal(with)
			if err != nil {
				t.Fatal(err)
			}
			omittedCanonical, err := json.Marshal(omitted)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(withoutField, omittedCanonical) {
				t.Errorf("manifests differ beyond mediaType:\nwith:    %s\nomitted: %s", withoutField, omittedCanonical)
			}

			for name, c := range map[string]struct {
				raw        []byte
				descriptor specv1.Descriptor
			}{"default": {withRaw, withDescriptor}, "omitted": {omittedRaw, omittedDescriptor}} {
				if c.descriptor.MediaType != specv1.MediaTypeImageManifest {
					t.Errorf("%s descriptor mediaType = %q, want %s", name, c.descriptor.MediaType, specv1.MediaTypeImageManifest)
				}
				if want := fmt.Sprintf("sha256:%x", sha256.Sum256(c.raw)); c.descriptor.Digest.String() != want {
					t.Errorf("%s descriptor digest = %s, want %s", name, c.descriptor.Digest, want)
				}
				if c.descriptor.Size != int64(len(c.raw)) {
					t.Errorf("%s descriptor size = %d, want %d", name, c.descriptor.Size, len(c.raw))
				}
			}
		})
	}
}

func decodeObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parsing manifest: %v", err)
	}
	return m
}
