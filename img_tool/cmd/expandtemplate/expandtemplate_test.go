package expandtemplate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestUnixToRFC3339(t *testing.T) {
	tests := []struct {
		name    string
		value   any
		want    string
		wantErr bool
	}{
		{name: "string seconds", value: "1750000000", want: "2025-06-15T15:06:40Z"},
		{name: "string with whitespace", value: " 1750000000\n", want: "2025-06-15T15:06:40Z"},
		{name: "int seconds", value: 1750000000, want: "2025-06-15T15:06:40Z"},
		{name: "float seconds", value: float64(1750000000), want: "2025-06-15T15:06:40Z"},
		{name: "epoch", value: "0", want: "1970-01-01T00:00:00Z"},
		{name: "empty string", value: "", want: ""},
		{name: "missing value", value: nil, want: ""},
		{name: "not a number", value: "yesterday", wantErr: true},
		{name: "unsupported type", value: true, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := unixToRFC3339(tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("unixToRFC3339(%v) = %q, want error", tc.value, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unixToRFC3339(%v) failed: %v", tc.value, err)
			}
			if got != tc.want {
				t.Errorf("unixToRFC3339(%v) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

func TestDeriveBuildTimestampRFC3339(t *testing.T) {
	tests := []struct {
		name     string
		settings buildSettings
		want     string
	}{
		{
			name:     "derived from BUILD_TIMESTAMP",
			settings: buildSettings{buildTimestampKey: buildSetting{value: "1750000000"}},
			want:     "2025-06-15T15:06:40Z",
		},
		{
			name:     "no stamp values",
			settings: buildSettings{},
			want:     "",
		},
		{
			name:     "malformed BUILD_TIMESTAMP is ignored",
			settings: buildSettings{buildTimestampKey: buildSetting{value: "not-a-timestamp"}},
			want:     "",
		},
		{
			name: "explicit value wins",
			settings: buildSettings{
				buildTimestampKey:        buildSetting{value: "1750000000"},
				buildTimestampRFC3339Key: buildSetting{value: "2000-01-01T00:00:00Z"},
			},
			want: "2000-01-01T00:00:00Z",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deriveBuildTimestampRFC3339(tc.settings)
			got, ok := tc.settings[buildTimestampRFC3339Key]
			if !ok {
				t.Fatalf("%s not defined", buildTimestampRFC3339Key)
			}
			if got.value != tc.want {
				t.Errorf("%s = %v, want %q", buildTimestampRFC3339Key, got.value, tc.want)
			}
		})
	}
}

// TestExpandTemplatesBuildTimestamp covers the end-to-end path used by the
// image_manifest rule: a stamp file provides BUILD_TIMESTAMP, and templates
// reference the derived RFC 3339 variable (and the rfc3339 function).
func TestExpandTemplatesBuildTimestamp(t *testing.T) {
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "request.json")
	outputPath := filepath.Join(dir, "expanded.json")
	stampPath := filepath.Join(dir, "volatile-status.txt")

	request := `{
		"templates": {
			"created": "{{.BUILD_TIMESTAMP_RFC3339}}",
			"annotations": {
				"org.opencontainers.image.created": "{{.BUILD_TIMESTAMP_RFC3339}}",
				"custom.created": "{{ rfc3339 .MY_TIMESTAMP }}"
			}
		},
		"build_settings": {}
	}`
	if err := os.WriteFile(inputPath, []byte(request), 0o644); err != nil {
		t.Fatalf("writing request: %v", err)
	}
	if err := os.WriteFile(stampPath, []byte("BUILD_TIMESTAMP 1750000000\nMY_TIMESTAMP 1700000000\n"), 0o644); err != nil {
		t.Fatalf("writing stamp file: %v", err)
	}

	if err := expandTemplates(inputPath, outputPath, []string{stampPath}, nil, nil, nil); err != nil {
		t.Fatalf("expandTemplates failed: %v", err)
	}

	var output struct {
		Created     string            `json:"created"`
		Annotations map[string]string `json:"annotations"`
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	if err := json.Unmarshal(data, &output); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if want := "2025-06-15T15:06:40Z"; output.Created != want {
		t.Errorf("created = %q, want %q", output.Created, want)
	}
	if want := "2025-06-15T15:06:40Z"; output.Annotations["org.opencontainers.image.created"] != want {
		t.Errorf("org.opencontainers.image.created = %q, want %q", output.Annotations["org.opencontainers.image.created"], want)
	}
	if want := "2023-11-14T22:13:20Z"; output.Annotations["custom.created"] != want {
		t.Errorf("custom.created = %q, want %q", output.Annotations["custom.created"], want)
	}
}

// TestExpandTemplatesWithoutStamp asserts that a template referencing
// BUILD_TIMESTAMP_RFC3339 expands to the empty string (not "<no value>") when no
// stamp file is available, so that the image config's created field stays unset.
func TestExpandTemplatesWithoutStamp(t *testing.T) {
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "request.json")
	outputPath := filepath.Join(dir, "expanded.json")

	request := `{"templates": {"created": "{{.BUILD_TIMESTAMP_RFC3339}}"}, "build_settings": {}}`
	if err := os.WriteFile(inputPath, []byte(request), 0o644); err != nil {
		t.Fatalf("writing request: %v", err)
	}

	if err := expandTemplates(inputPath, outputPath, nil, nil, nil, nil); err != nil {
		t.Fatalf("expandTemplates failed: %v", err)
	}

	var output struct {
		Created string `json:"created"`
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	if err := json.Unmarshal(data, &output); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if output.Created != "" {
		t.Errorf("created = %q, want empty", output.Created)
	}
}
