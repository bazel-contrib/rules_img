package dockersave

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReadTagsFromConfigFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    []string
	}{
		{
			name:    "tags only are returned verbatim",
			content: `{"tags":["my-app:latest","docker.io/library/foo:v1"],"daemon":"docker"}`,
			want:    []string{"my-app:latest", "docker.io/library/foo:v1"},
		},
		{
			name:    "registry and repository reconstruct full names",
			content: `{"registry":"gcr.io","repository":"proj/app","tags":["latest","v1"],"daemon":"docker"}`,
			want:    []string{"gcr.io/proj/app:latest", "gcr.io/proj/app:v1"},
		},
		{
			name:    "empty registry/repository keys behave like the tags-only mode",
			content: `{"registry":"","repository":"","tags":["my-app:latest"],"daemon":"docker"}`,
			want:    []string{"my-app:latest"},
		},
		{
			name:    "no tags field yields nil",
			content: `{"registry":"gcr.io","repository":"proj/app","daemon":"docker"}`,
			want:    nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := readTagsFromConfigFile(path)
			if err != nil {
				t.Fatalf("readTagsFromConfigFile: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("readTagsFromConfigFile() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReadTagsFromConfigFileEmptyPath(t *testing.T) {
	got, err := readTagsFromConfigFile("")
	if err != nil || got != nil {
		t.Fatalf("readTagsFromConfigFile(\"\") = %v, %v; want nil, nil", got, err)
	}
}

func TestReadTagsFromConfigFileLoneRegistryErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// registry set but repository empty (e.g. a template that expanded to "").
	if err := os.WriteFile(path, []byte(`{"registry":"gcr.io","repository":"","tags":["latest"],"daemon":"docker"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readTagsFromConfigFile(path); err == nil {
		t.Fatal("expected error for registry without repository, got nil")
	}
}
