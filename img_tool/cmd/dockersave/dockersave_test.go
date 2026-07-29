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

func TestResolveRepoTags(t *testing.T) {
	for _, tc := range []struct {
		name         string
		flagTags     []string
		content      string
		wantRepoTags []string
		wantOCITags  []string
		wantErr      bool
	}{
		{
			name:         "registry with a port keeps its port",
			content:      `{"registry":"docker.mycompany.tld:1234","repository":"foo","tags":["latest"],"daemon":"docker"}`,
			wantRepoTags: []string{"docker.mycompany.tld:1234/foo:latest"},
			wantOCITags:  []string{"docker.mycompany.tld:1234/foo:latest"},
		},
		{
			name:         "short name is not expanded to docker.io/library",
			content:      `{"tags":["my-app:latest"],"daemon":"docker"}`,
			wantRepoTags: []string{"my-app:latest"},
			wantOCITags:  []string{"my-app:latest"},
		},
		{
			name:         "untagged reference gains the default tag",
			content:      `{"tags":["my-app"],"daemon":"docker"}`,
			wantRepoTags: []string{"my-app:latest"},
			wantOCITags:  []string{"my-app:latest"},
		},
		{
			name:         "flags win over the configuration file",
			flagTags:     []string{"from-flag:v1"},
			content:      `{"tags":["from-config:v1"],"daemon":"docker"}`,
			wantRepoTags: []string{"from-flag:v1"},
			wantOCITags:  []string{"from-flag:v1"},
		},
		{
			name:         "no tags anywhere falls back to the default repo tag",
			content:      `{"daemon":"docker"}`,
			wantRepoTags: []string{"image:latest"},
			wantOCITags:  nil,
		},
		{
			name:    "invalid reference from the configuration file",
			content: `{"tags":["MyApp:latest"],"daemon":"docker"}`,
			wantErr: true,
		},
		{
			name:     "invalid reference from a flag",
			flagTags: []string{"docker.io/library/docker.mycompany.tld:1234/foo:latest"},
			content:  `{"daemon":"docker"}`,
			wantErr:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			repoTags, ociTags, err := resolveRepoTags(tc.flagTags, path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveRepoTags() = %v, %v; want error", repoTags, ociTags)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveRepoTags(): %v", err)
			}
			if !reflect.DeepEqual(repoTags, tc.wantRepoTags) {
				t.Fatalf("repoTags = %v, want %v", repoTags, tc.wantRepoTags)
			}
			if !reflect.DeepEqual(ociTags, tc.wantOCITags) {
				t.Fatalf("ociTags = %v, want %v", ociTags, tc.wantOCITags)
			}
		})
	}
}

// TestResolveRepoTagsNoConfigFile covers the plain CLI use: only --repo-tag
// flags, no load configuration file.
func TestResolveRepoTagsNoConfigFile(t *testing.T) {
	repoTags, ociTags, err := resolveRepoTags([]string{"my/image"}, "")
	if err != nil {
		t.Fatalf("resolveRepoTags(): %v", err)
	}
	if want := []string{"my/image:latest"}; !reflect.DeepEqual(repoTags, want) || !reflect.DeepEqual(ociTags, want) {
		t.Fatalf("resolveRepoTags() = %v, %v; want %v", repoTags, ociTags, want)
	}
}
