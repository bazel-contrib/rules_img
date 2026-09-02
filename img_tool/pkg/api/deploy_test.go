package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestQualifyLoadTags(t *testing.T) {
	for _, tc := range []struct {
		name       string
		registry   string
		repository string
		tags       []string
		want       []string
	}{
		{
			name: "no registry or repository returns tags verbatim",
			tags: []string{"my-app:latest", "docker.io/library/foo:v1"},
			want: []string{"my-app:latest", "docker.io/library/foo:v1"},
		},
		{
			name:       "registry and repository reconstruct full names",
			registry:   "gcr.io",
			repository: "my-project/my-app",
			tags:       []string{"latest", "v1.0.0"},
			want:       []string{"gcr.io/my-project/my-app:latest", "gcr.io/my-project/my-app:v1.0.0"},
		},
		{
			name:       "registry without repository falls back to verbatim",
			registry:   "gcr.io",
			repository: "",
			tags:       []string{"already/full:latest"},
			want:       []string{"already/full:latest"},
		},
		{
			name:       "repository without registry falls back to verbatim",
			registry:   "",
			repository: "my-app",
			tags:       []string{"already/full:latest"},
			want:       []string{"already/full:latest"},
		},
		{
			name:       "no tags with registry yields no names",
			registry:   "gcr.io",
			repository: "my-app",
			tags:       nil,
			want:       []string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := QualifyLoadTags(tc.registry, tc.repository, tc.tags)
			if len(got) != len(tc.want) {
				t.Fatalf("QualifyLoadTags() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("QualifyLoadTags()[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestValidateLoadDestination(t *testing.T) {
	for _, tc := range []struct {
		name       string
		registry   string
		repository string
		wantErr    bool
	}{
		{name: "both empty is valid (verbatim mode)", wantErr: false},
		{name: "both set is valid (split mode)", registry: "gcr.io", repository: "proj/app", wantErr: false},
		{name: "registry only is an error", registry: "gcr.io", wantErr: true},
		{name: "repository only is an error", repository: "proj/app", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateLoadDestination(tc.registry, tc.repository)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateLoadDestination(%q, %q) error = %v, wantErr %v", tc.registry, tc.repository, err, tc.wantErr)
			}
		})
	}
}

// TestQualifyLoadTagsReturnsFreshSlice guards the documented contract that the// returned slice is always a fresh copy, so callers may append to it without
// clobbering the operation's own Tags backing array.
func TestQualifyLoadTagsReturnsFreshSlice(t *testing.T) {
	tags := []string{"my-app:latest"}
	got := QualifyLoadTags("", "", tags)
	got = append(got, "mutated")
	if tags[0] != "my-app:latest" {
		t.Fatalf("QualifyLoadTags aliased the input slice: %v", tags)
	}
}

func TestNormalizeLoadReference(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)

	for _, tc := range []struct {
		name string
		ref  string
		want string
	}{
		{
			name: "fully-qualified reference is unchanged",
			ref:  "gcr.io/my-project/my-app:latest",
			want: "gcr.io/my-project/my-app:latest",
		},
		{
			name: "registry with a port is not mistaken for a tag",
			ref:  "docker.mycompany.tld:1234/foo:latest",
			want: "docker.mycompany.tld:1234/foo:latest",
		},
		{
			name: "registry with a port and no tag only gains the default tag",
			ref:  "docker.mycompany.tld:1234/foo",
			want: "docker.mycompany.tld:1234/foo:latest",
		},
		{
			name: "short name keeps its shape: no registry, no library namespace",
			ref:  "my-app:latest",
			want: "my-app:latest",
		},
		{
			name: "multi-component short name is not prefixed with a registry",
			ref:  "my-team/my-app:latest",
			want: "my-team/my-app:latest",
		},
		{
			name: "explicit docker.io is preserved, not rewritten to index.docker.io",
			ref:  "docker.io/library/foo:v1",
			want: "docker.io/library/foo:v1",
		},
		{
			name: "untagged reference gains the default tag",
			ref:  "my-app",
			want: "my-app:latest",
		},
		{
			name: "localhost with a port",
			ref:  "localhost:5000/my-app:dev",
			want: "localhost:5000/my-app:dev",
		},
		{
			name: "IPv6 host with a port",
			ref:  "[::1]:5000/my-app:dev",
			want: "[::1]:5000/my-app:dev",
		},
		{
			name: "digest reference is untouched",
			ref:  "gcr.io/my-project/my-app@" + digest,
			want: "gcr.io/my-project/my-app@" + digest,
		},
		{
			name: "uppercase tag is allowed",
			ref:  "my-app:LATEST",
			want: "my-app:LATEST",
		},
		{
			// The distribution grammar allows a one-character path component,
			// and go-containerregistry accepts one since v0.22.0.
			name: "single-character repository",
			ref:  "a:1",
			want: "a:1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeLoadReference(tc.ref)
			if err != nil {
				t.Fatalf("NormalizeLoadReference(%q) error = %v", tc.ref, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeLoadReference(%q) = %q, want %q", tc.ref, got, tc.want)
			}
		})
	}
}

func TestNormalizeLoadReferenceErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		ref  string
	}{
		{name: "empty reference", ref: ""},
		{name: "empty tag", ref: "gcr.io/proj/app:"},
		{name: "uppercase repository", ref: "MyApp:latest"},
		{name: "space in the tag", ref: "my-app:no spaces"},
		{name: "registry is not a valid host", ref: "not a host/my-app:latest"},
		{
			// The shape the old hand-rolled normalization used to produce for a
			// registry with a port.
			name: "doubly-qualified name",
			ref:  "docker.io/library/docker.mycompany.tld:1234/foo:latest",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeLoadReference(tc.ref)
			if err == nil {
				t.Fatalf("NormalizeLoadReference(%q) = %q, want error", tc.ref, got)
			}
		})
	}
}

func TestNormalizeLoadReferences(t *testing.T) {
	got, err := NormalizeLoadReferences([]string{"my-app", "gcr.io/proj/app:v1"})
	if err != nil {
		t.Fatalf("NormalizeLoadReferences() error = %v", err)
	}
	if want := []string{"my-app:latest", "gcr.io/proj/app:v1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("NormalizeLoadReferences() = %v, want %v", got, want)
	}

	if got, err := NormalizeLoadReferences(nil); err != nil || got != nil {
		t.Fatalf("NormalizeLoadReferences(nil) = %v, %v; want nil, nil", got, err)
	}

	if _, err := NormalizeLoadReferences([]string{"my-app:latest", "MyApp:latest"}); err == nil {
		t.Fatal("NormalizeLoadReferences() with an invalid reference: want error")
	}
}

// TestLoadDeployOperationOmitsEmptyDestination verifies that a load operation
// carrying only tags serializes without registry/repository keys (the
// rules_oci-compatible mode), while a fully-qualified one emits them.
func TestLoadDeployOperationOmitsEmptyDestination(t *testing.T) {
	tagsOnly := LoadDeployOperation{
		BaseCommandOperation: BaseCommandOperation{Command: "load"},
		Tags:                 []string{"my-app:latest"},
		Daemon:               "docker",
	}
	data, err := json.Marshal(tagsOnly)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "registry") || strings.Contains(string(data), "repository") {
		t.Fatalf("tags-only load op should omit registry/repository, got %s", data)
	}

	full := LoadDeployOperation{
		BaseCommandOperation: BaseCommandOperation{Command: "load"},
		Registry:             "gcr.io",
		Repository:           "my-project/my-app",
		Tags:                 []string{"latest"},
		Daemon:               "docker",
	}
	data, err = json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"registry":"gcr.io"`) || !strings.Contains(string(data), `"repository":"my-project/my-app"`) {
		t.Fatalf("qualified load op should emit registry/repository, got %s", data)
	}

	if got := full.ImageNames(); !reflect.DeepEqual(got, []string{"gcr.io/my-project/my-app:latest"}) {
		t.Fatalf("ImageNames() = %v", got)
	}
}
