package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const (
	infoImage = "5d3f6e0cc0a0c4e5ba9a2b7aa1a3b1ae2d3b2ab3ba0b1ad10cd8b5b4a6a8c7aa" // docRequest's subject
	infoIndex = "1111111111111111111111111111111111111111111111111111111111111111"
)

// buildInfoFixture writes a build info file: an index of two images, one of which
// is based on a pulled image, plus a layer that belongs to the other image only.
func buildInfoFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "build_info.json")
	doc := `{
  "schema_version": 1,
  "deploy": {"label": "@@//services/img:push", "configuration": "k8-fastbuild"},
  "images": [
    {"label": "@@//services/img:index", "configuration": "k8-fastbuild", "kind": "index",
     "digest": "sha256:` + infoIndex + `", "manifests": ["sha256:` + infoImage + `", "sha256:22"]},
    {"label": "@@//services/img:arm64", "configuration": "arm64-fastbuild", "kind": "image",
     "platform": {"os": "linux", "architecture": "arm64", "variant": "v8"},
     "digest": "sha256:` + infoImage + `", "base": "@@distroless//:base", "layers": ["sha256:aa", "sha256:bb"]},
    {"label": "@@//services/img:amd64", "configuration": "amd64-fastbuild", "kind": "image",
     "digest": "sha256:22", "layers": ["sha256:cc"]},
    {"label": "@@distroless//:base", "configuration": "k8-fastbuild", "kind": "image",
     "digest": "sha256:33", "layers": ["sha256:aa"]}
  ],
  "layers": [
    {"label": "@@//services/img:app", "configuration": "arm64-fastbuild", "digest": "sha256:bb"},
    {"label": "@@//services/img:app_amd64", "configuration": "amd64-fastbuild", "digest": "sha256:cc"},
    {"label": "@@distroless//:layer", "configuration": "k8-fastbuild", "digest": "sha256:aa"}
  ],
  "base_images": [
    {"label": "@@distroless//:base", "registries": ["gcr.io"], "repository": "distroless/cc", "tag": "latest", "digest": "sha256:33"},
    {"label": "@@other//:base", "registries": ["gcr.io"], "repository": "other", "digest": "sha256:44"}
  ]
}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func names(deps []any) []string {
	var out []string
	for _, d := range deps {
		out = append(out, d.(map[string]any)["name"].(string))
	}
	return out
}

func TestBazelInfoRecordsTheClosureOfTheSubject(t *testing.T) {
	clearCIEnv(t)
	req := docRequest()
	req.Deploy.BuildInfoFile = buildInfoFixture(t)
	p := predicateOf(t, signBazel(t, req, "--target", "//manual:target"))

	if got := at(t, p, "buildDefinition", "externalParameters", "targets"); !reflect.DeepEqual(got, []any{"//manual:target", "@@//services/img:push"}) {
		t.Errorf("targets = %v", got)
	}
	deps := at(t, p, "buildDefinition", "resolvedDependencies").([]any)
	// The pulled base image the subject is based on, the subject and the base image
	// again as images, and the layers of both -- but nothing of the amd64 image.
	want := []string{"@@distroless//:base", "@@//services/img:arm64", "@@distroless//:base", "@@distroless//:layer", "@@//services/img:app"}
	if got := names(deps); !reflect.DeepEqual(got, want) {
		t.Errorf("resolvedDependencies = %v, want %v", got, want)
	}
	if got := at(t, deps[0], "uri"); got != "pkg:docker/gcr.io/distroless/cc@latest" {
		t.Errorf("base image uri = %v", got)
	}
	if got := at(t, deps[0], "digest", "sha256"); got != "33" {
		t.Errorf("base image digest = %v", got)
	}
	if got := at(t, deps[1], "annotations", "configuration"); got != "arm64-fastbuild" {
		t.Errorf("configuration = %v", got)
	}
	if got := at(t, deps[1], "annotations", "platform"); got != "linux/arm64" {
		t.Errorf("platform = %v (the default variant is dropped)", got)
	}
	if got := at(t, deps[2], "annotations", "platform"); got != nil {
		t.Errorf("platform = %v for an image without one", got)
	}
}

func TestBazelInfoDetail(t *testing.T) {
	clearCIEnv(t)
	req := docRequest()
	req.Deploy.BuildInfoFile = buildInfoFixture(t)

	bases := at(t, predicateOf(t, signBazel(t, req, "--bazel-info=bases")), "buildDefinition", "resolvedDependencies").([]any)
	if got := names(bases); !reflect.DeepEqual(got, []string{"@@distroless//:base"}) {
		t.Errorf("bases = %v", got)
	}

	p := predicateOf(t, signBazel(t, req, "--bazel-info=none"))
	if deps := at(t, p, "buildDefinition", "resolvedDependencies"); deps != nil {
		t.Errorf("none still recorded %v", deps)
	}
	if got := at(t, p, "buildDefinition", "externalParameters", "targets"); !reflect.DeepEqual(got, []any{"@@//services/img:push"}) {
		t.Errorf("targets = %v", got)
	}
}

func TestBazelInfoIsOptional(t *testing.T) {
	clearCIEnv(t)
	p := predicateOf(t, signBazel(t, docRequest()))
	if got := at(t, p, "buildDefinition", "externalParameters", "targets"); got != nil {
		t.Errorf("targets = %v without a build info file", got)
	}
	if _, err := newSigner([]string{"--bazel-info=everything", "--definitions=bazel"}); err == nil {
		t.Error("an unknown --bazel-info was accepted")
	}
	if _, err := newSigner([]string{"--bazel-info=all"}); err == nil {
		t.Error("--bazel-info is part of the bazel definitions only")
	}
}
