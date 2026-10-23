package layer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlaceFilesSpecPlace(t *testing.T) {
	tests := []struct {
		name      string
		spec      placeFilesSpec
		shortPath string
		count     int
		want      string
		wantErr   bool
	}{
		{
			name:      "single output collapses onto the path key",
			spec:      placeFilesSpec{Mode: "package_relative", Dest: "app", Anchor: "_main/pkg"},
			shortPath: "_main/pkg/config/properties.yaml",
			count:     1,
			want:      "app",
		},
		{
			name:      "single output collapses onto the path key when flattening",
			spec:      placeFilesSpec{Mode: "flatten", Dest: "app", Anchor: "_main/pkg"},
			shortPath: "_main/pkg/config/properties.yaml",
			count:     1,
			want:      "app",
		},
		{
			// A trailing slash is an explicit directory key: one output is laid
			// out inside it exactly like several would be.
			name:      "single output under a directory key stays package-relative",
			spec:      placeFilesSpec{Mode: "package_relative", Dest: "app/", Anchor: "_main/pkg"},
			shortPath: "_main/pkg/config/properties.yaml",
			count:     1,
			want:      "app/config/properties.yaml",
		},
		{
			name:      "multiple outputs under a directory key stay package-relative",
			spec:      placeFilesSpec{Mode: "package_relative", Dest: "app/", Anchor: "_main/pkg"},
			shortPath: "_main/pkg/config/properties.yaml",
			count:     2,
			want:      "app/config/properties.yaml",
		},
		{
			name:      "single output under a directory key flattens to the basename",
			spec:      placeFilesSpec{Mode: "flatten", Dest: "app/", Anchor: "_main/pkg"},
			shortPath: "_main/pkg/config/properties.yaml",
			count:     1,
			want:      "app/properties.yaml",
		},
		{
			name:      "multiple outputs flatten to the basename",
			spec:      placeFilesSpec{Mode: "flatten", Dest: "app", Anchor: "_main/pkg"},
			shortPath: "_main/pkg/config/properties.yaml",
			count:     2,
			want:      "app/properties.yaml",
		},
		{
			name:      "relative mode never collapses a single output",
			spec:      placeFilesSpec{Mode: "relative", Dest: "app", Anchor: "_main/pkg/bin"},
			shortPath: "_main/pkg/bin/data.txt",
			count:     1,
			want:      "app/data.txt",
		},
		{
			name:      "escaping the image root fails",
			spec:      placeFilesSpec{Mode: "package_relative", Dest: "", Anchor: "_main/pkg/deep"},
			shortPath: "_main/other.txt",
			count:     2,
			wantErr:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.spec.place(tc.shortPath, tc.count)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("place(%q, %d) = %q, want error", tc.shortPath, tc.count, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("place(%q, %d) unexpected error: %v", tc.shortPath, tc.count, err)
			}
			if got != tc.want {
				t.Errorf("place(%q, %d) = %q, want %q", tc.shortPath, tc.count, got, tc.want)
			}
		})
	}
}

func TestParsePlaceFilesHeader(t *testing.T) {
	tests := []struct {
		name         string
		line         string
		wantErr      bool
		wantOnEscape string
	}{
		{
			name:         "header without an escape policy defaults to error",
			line:         "relative\x00app/bin\x00_main/pkg/bin\x00_main/pkg/bin/launcher",
			wantOnEscape: onEscapeError,
		},
		{
			name:         "empty escape policy defaults to error",
			line:         "relative\x00app/bin\x00_main/pkg/bin\x00_main/pkg/bin/launcher\x00",
			wantOnEscape: onEscapeError,
		},
		{
			name:         "explicit skip policy",
			line:         "relative\x00\x00_main/pkg/bin\x00_main/pkg/bin/launcher\x00skip",
			wantOnEscape: onEscapeSkip,
		},
		{
			name:    "unknown escape policy is rejected",
			line:    "relative\x00\x00_main/pkg/bin\x00_main/pkg/bin/launcher\x00best_effort",
			wantErr: true,
		},
		{
			name:    "too few fields",
			line:    "relative\x00app/bin\x00_main/pkg/bin",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := parsePlaceFilesHeader(tc.line)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parsePlaceFilesHeader(%q) = %+v, want error", tc.line, spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePlaceFilesHeader(%q) unexpected error: %v", tc.line, err)
			}
			if spec.OnEscape != tc.wantOnEscape {
				t.Errorf("parsePlaceFilesHeader(%q).OnEscape = %q, want %q", tc.line, spec.OnEscape, tc.wantOnEscape)
			}
		})
	}
}

// A wrapper rule such as bazel-lib's platform_transition_binary symlinks the
// executable into a subdirectory of its own package, so every other default
// output sits above the executable's directory. With the executable at the
// image root those outputs have nowhere to go: best-effort placement drops
// them, strict placement fails.
func TestReadPlaceFilesParamFileEscapePolicy(t *testing.T) {
	source := filepath.Join(t.TempDir(), "content")
	if err := os.WriteFile(source, []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		"", // placeholder for the header
		"_main/pkg/launcher_/launcher\x00f" + source,
		"_main/pkg/launcher/assets/logo.txt\x00f" + source,
	}

	writeParamFile := func(t *testing.T, header string) string {
		t.Helper()
		lines[0] = header
		paramFile := filepath.Join(t.TempDir(), "place.params")
		if err := os.WriteFile(paramFile, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return paramFile
	}

	// The executable is "_main/pkg/launcher/launcher", placed at "launcher".
	const header = "relative\x00\x00_main/pkg/launcher\x00_main/pkg/launcher/launcher\x00%s"

	t.Run("skip", func(t *testing.T) {
		got, err := readPlaceFilesParamFile(writeParamFile(t, strings.Replace(header, "%s", onEscapeSkip, 1)))
		if err != nil {
			t.Fatalf("readPlaceFilesParamFile() unexpected error: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("readPlaceFilesParamFile() = %+v, want exactly the placeable file", got)
		}
		if got[0].PathInImage != "assets/logo.txt" {
			t.Errorf("placed at %q, want %q", got[0].PathInImage, "assets/logo.txt")
		}
	})

	t.Run("error", func(t *testing.T) {
		if _, err := readPlaceFilesParamFile(writeParamFile(t, strings.Replace(header, "%s", onEscapeError, 1))); err == nil {
			t.Fatal("readPlaceFilesParamFile() = nil error, want error")
		}
	})
}
