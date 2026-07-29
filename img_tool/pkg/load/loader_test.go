package load

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

func indexedLoadOp(registry, repository string, tags []string) api.IndexedLoadDeployOperation {
	return api.IndexedLoadDeployOperation{
		LoadDeployOperation: api.LoadDeployOperation{
			Registry:   registry,
			Repository: repository,
			Tags:       tags,
			Daemon:     "docker",
		},
	}
}

func TestLoaderTags(t *testing.T) {
	for _, tc := range []struct {
		name      string
		op        api.IndexedLoadDeployOperation
		extraTags []string
		want      []string
	}{
		{
			name: "backwards-compatible full references",
			op:   indexedLoadOp("", "", []string{"my-app:latest", "my-app:v1"}),
			want: []string{"my-app:latest", "my-app:v1"},
		},
		{
			name: "registry and repository reconstruct names",
			op:   indexedLoadOp("gcr.io", "proj/app", []string{"latest", "v1"}),
			want: []string{"gcr.io/proj/app:latest", "gcr.io/proj/app:v1"},
		},
		{
			name:      "extra tags are treated as full references",
			op:        indexedLoadOp("gcr.io", "proj/app", []string{"latest"}),
			extraTags: []string{"local/name:dev"},
			want:      []string{"gcr.io/proj/app:latest", "local/name:dev"},
		},
		{
			name:      "split mode duplicate with extra tag is removed",
			op:        indexedLoadOp("gcr.io", "proj/app", []string{"latest"}),
			extraTags: []string{"gcr.io/proj/app:latest"},
			want:      []string{"gcr.io/proj/app:latest"},
		},
		{
			name:      "duplicates removed and sorted",
			op:        indexedLoadOp("", "", []string{"bb:2", "aa:1"}),
			extraTags: []string{"aa:1"},
			want:      []string{"aa:1", "bb:2"},
		},
		{
			name: "registry with port keeps its port",
			op:   indexedLoadOp("docker.mycompany.tld:1234", "foo", []string{"latest"}),
			want: []string{"docker.mycompany.tld:1234/foo:latest"},
		},
		{
			name: "full reference with a port is used verbatim",
			op:   indexedLoadOp("", "", []string{"docker.mycompany.tld:1234/foo:latest"}),
			want: []string{"docker.mycompany.tld:1234/foo:latest"},
		},
		{
			name:      "no registry is invented for short names",
			op:        indexedLoadOp("", "", []string{"my-app:latest"}),
			extraTags: []string{"localhost:5000/my-app:dev"},
			want:      []string{"localhost:5000/my-app:dev", "my-app:latest"},
		},
		{
			name: "untagged reference gets the default tag",
			op:   indexedLoadOp("", "", []string{"my-app"}),
			want: []string{"my-app:latest"},
		},
		{
			name: "default tag deduplicates against the explicit one",
			op:   indexedLoadOp("", "", []string{"my-app", "my-app:latest"}),
			want: []string{"my-app:latest"},
		},
		{
			name: "digest reference is preserved",
			op:   indexedLoadOp("", "", []string{"my-app@sha256:" + strings.Repeat("a", 64)}),
			want: []string{"my-app@sha256:" + strings.Repeat("a", 64)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &loader{extraTags: tc.extraTags}
			got, err := l.tags(tc.op)
			if err != nil {
				t.Fatalf("tags() returned error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("tags() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLoaderTagsInvalidReference covers references that cannot name an image:
// they are reported instead of being handed to a daemon.
func TestLoaderTagsInvalidReference(t *testing.T) {
	for _, tc := range []struct {
		name      string
		op        api.IndexedLoadDeployOperation
		extraTags []string
	}{
		{
			name: "uppercase repository",
			op:   indexedLoadOp("", "", []string{"MyApp:latest"}),
		},
		{
			name: "tag expanded to the empty string",
			op:   indexedLoadOp("gcr.io", "proj/app", []string{""}),
		},
		{
			name: "registry is not a valid host",
			op:   indexedLoadOp("not a host", "proj/app", []string{"latest"}),
		},
		{
			name:      "invalid extra tag",
			op:        indexedLoadOp("gcr.io", "proj/app", []string{"latest"}),
			extraTags: []string{"gcr.io/proj/app:no spaces allowed"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &loader{extraTags: tc.extraTags}
			got, err := l.tags(tc.op)
			if err == nil {
				t.Fatalf("tags() = %v, want error", got)
			}
		})
	}
}

// TestLoaderTagsDoesNotMutateOp guards against aliasing the operation's Tags
// backing array when appending extra tags.
func TestLoaderTagsDoesNotMutateOp(t *testing.T) {
	op := indexedLoadOp("", "", []string{"my-app:latest"})
	l := &loader{extraTags: []string{"extra:tag"}}
	if _, err := l.tags(op); err != nil {
		t.Fatalf("tags() returned error: %v", err)
	}
	if !reflect.DeepEqual(op.Tags, []string{"my-app:latest"}) {
		t.Fatalf("tags() mutated op.Tags: %v", op.Tags)
	}
}

// TestLoaderTagsWithOverridesDoesNotMutateOp guards against aliasing the
// operation's Tags backing array in split mode when overrides are applied.
func TestLoaderTagsWithOverridesDoesNotMutateOp(t *testing.T) {
	op := indexedLoadOp("gcr.io", "proj/app", []string{"latest"})
	l := &loader{overrideRegistry: "reg.example.com", overrideRepository: "team/app"}
	if _, err := l.tags(op); err != nil {
		t.Fatalf("tags() returned error: %v", err)
	}
	if !reflect.DeepEqual(op.Tags, []string{"latest"}) {
		t.Fatalf("tags() mutated op.Tags: %v", op.Tags)
	}
}

func TestLoaderTagsWithOverrides(t *testing.T) {
	for _, tc := range []struct {
		name               string
		op                 api.IndexedLoadDeployOperation
		overrideRegistry   string
		overrideRepository string
		want               []string
	}{
		{
			name:               "both overrides in split mode",
			op:                 indexedLoadOp("gcr.io", "proj/app", []string{"latest"}),
			overrideRegistry:   "reg.example.com",
			overrideRepository: "team/app",
			want:               []string{"reg.example.com/team/app:latest"},
		},
		{
			name:             "registry-only override in split mode",
			op:               indexedLoadOp("gcr.io", "proj/app", []string{"latest", "v1"}),
			overrideRegistry: "reg.example.com",
			want:             []string{"reg.example.com/proj/app:latest", "reg.example.com/proj/app:v1"},
		},
		{
			name:               "repository-only override in split mode",
			op:                 indexedLoadOp("gcr.io", "proj/app", []string{"latest"}),
			overrideRepository: "team/app",
			want:               []string{"gcr.io/team/app:latest"},
		},
		{
			name:               "overrides ignored in rules_oci fallback (empty registry/repository)",
			op:                 indexedLoadOp("", "", []string{"my-app:latest"}),
			overrideRegistry:   "reg.example.com",
			overrideRepository: "team/app",
			want:               []string{"my-app:latest"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &loader{overrideRegistry: tc.overrideRegistry, overrideRepository: tc.overrideRepository}
			got, err := l.tags(tc.op)
			if err != nil {
				t.Fatalf("tags() returned error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("tags() = %v, want %v", got, tc.want)
			}
		})
	}
}
