package load

import (
	"reflect"
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
			name:      "duplicates removed and sorted",
			op:        indexedLoadOp("", "", []string{"b:2", "a:1"}),
			extraTags: []string{"a:1"},
			want:      []string{"a:1", "b:2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &loader{extraTags: tc.extraTags}
			got := l.tags(tc.op)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("tags() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLoaderTagsDoesNotMutateOp guards against aliasing the operation's Tags
// backing array when appending extra tags.
func TestLoaderTagsDoesNotMutateOp(t *testing.T) {
	op := indexedLoadOp("", "", []string{"my-app:latest"})
	l := &loader{extraTags: []string{"extra:tag"}}
	_ = l.tags(op)
	if !reflect.DeepEqual(op.Tags, []string{"my-app:latest"}) {
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
			got := l.tags(tc.op)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("tags() = %v, want %v", got, tc.want)
			}
		})
	}
}
