package deploy

import (
	"fmt"
	"reflect"
	"testing"

	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/signer"
)

func candidate(protocol int, registry, repo, digest string, tags []string, bestEffort bool) signCandidate {
	return signCandidate{
		registry: registry, repository: repo,
		decision: signDecision{sign: true, bestEffort: bestEffort},
		cfg:      signer.SignSettingConfig{SchemaVersion: 1, Mode: "command", Tool: "t", Protocol: protocol},
		subject:  signSubject{desc: api.Descriptor{Digest: digest}, role: signer.RoleRoot},
		tags:     tags,
	}
}

func TestMergeSignJobsRequestProtocol(t *testing.T) {
	jobs := mergeSignJobs([]signCandidate{
		candidate(2, "ghcr.io", "ex/app", "sha256:a", []string{"v1", "latest"}, true),
		candidate(2, "ghcr.io", "ex/other", "sha256:a", []string{"v1"}, false),        // other repository
		candidate(2, "ghcr.io", "ex/app", "sha256:b", []string{"v1"}, false),          // other digest
		candidate(2, "ghcr.io", "ex/app", "sha256:a", []string{"latest", "x"}, false), // merges into #1
	})
	if len(jobs) != 3 {
		t.Fatalf("want 3 jobs (one per repository and digest), got %d", len(jobs))
	}
	if want := []string{"latest", "v1", "x"}; !reflect.DeepEqual(jobs[0].tags, want) {
		t.Errorf("merged tags = %v, want %v", jobs[0].tags, want)
	}
	if jobs[0].decision.bestEffort {
		t.Error("a merged job is best effort only if every operation is")
	}
}

func TestMergeSignJobsDescriptorProtocolIsNotMerged(t *testing.T) {
	jobs := mergeSignJobs([]signCandidate{
		candidate(1, "ghcr.io", "ex/app", "sha256:a", []string{"v1"}, false),
		candidate(0, "ghcr.io", "ex/app", "sha256:a", []string{"latest"}, false),
	})
	if len(jobs) != 2 {
		t.Fatalf("the original protocol must keep one call per operation and subject, got %d jobs", len(jobs))
	}
}

func TestMergeSignJobsDifferentSettingsAreSeparate(t *testing.T) {
	a := candidate(2, "ghcr.io", "ex/app", "sha256:a", nil, false)
	b := candidate(2, "ghcr.io", "ex/app", "sha256:a", nil, false)
	b.cfg.Args = []string{"--other"}
	if got := len(mergeSignJobs([]signCandidate{a, b})); got != 2 {
		t.Fatalf("different plugin configs must not share a job, got %d", got)
	}
}

func TestMergeSignJobsRootWinsOverChild(t *testing.T) {
	child := candidate(2, "ghcr.io", "ex/app", "sha256:a", nil, false)
	parent := api.Descriptor{Digest: "sha256:idx"}
	child.subject = signSubject{desc: api.Descriptor{Digest: "sha256:a"}, role: signer.RoleChildManifest, parent: &parent}
	root := candidate(2, "ghcr.io", "ex/app", "sha256:a", nil, false)
	jobs := mergeSignJobs([]signCandidate{child, root})
	if len(jobs) != 1 || jobs[0].subject.role != signer.RoleRoot || jobs[0].subject.parent != nil {
		t.Fatalf("a manifest pushed on its own and inside an index is a root: %+v", jobs[0].subject)
	}
}

func TestCollectSubjectsRolesAndParent(t *testing.T) {
	op := api.IndexedPushDeployOperation{}
	op.RootKind = "index"
	op.Root = api.Descriptor{Digest: "sha256:idx", MediaType: "application/vnd.oci.image.index.v1+json"}
	op.Manifests = []api.ManifestDeployInfo{
		{Descriptor: api.Descriptor{Digest: "sha256:m1"}},
		{Descriptor: api.Descriptor{Digest: "sha256:m2"}},
	}
	got := collectSubjects(op, map[string]bool{api.SignTargetRoots: true, api.SignTargetChildManifests: true})
	if len(got) != 3 || got[0].role != signer.RoleRoot || got[1].role != signer.RoleChildManifest || got[1].parent.Digest != "sha256:idx" {
		t.Fatalf("subjects = %+v", got)
	}
	if got := collectSubjects(op, map[string]bool{api.SignTargetRoots: true}); len(got) != 1 {
		t.Fatalf("roots only must not include children: %+v", got)
	}
}

type fakeImages map[string]registryv1.Image

func (f fakeImages) Image(h registryv1.Hash) (registryv1.Image, error) {
	img, ok := f[h.String()]
	if !ok {
		return nil, fmt.Errorf("no image %s", h)
	}
	return img, nil
}

func TestPlatformOf(t *testing.T) {
	img, err := mutate.ConfigFile(empty.Image, &registryv1.ConfigFile{OS: "linux", Architecture: "arm64", Variant: "v8"})
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := img.Digest()
	images := fakeImages{digest.String(): img}
	manifest := api.Descriptor{Digest: digest.String(), MediaType: "application/vnd.oci.image.manifest.v1+json"}

	if got := platformOf(images, manifest); got == nil || got.OS != "linux" || got.Architecture != "arm64" || got.Variant != "v8" {
		t.Errorf("platform = %+v", got)
	}
	if got := platformOf(nil, manifest); got != nil {
		t.Error("no image source means no platform")
	}
	if got := platformOf(images, api.Descriptor{Digest: digest.String(), MediaType: "application/vnd.oci.image.index.v1+json"}); got != nil {
		t.Error("an index has no platform")
	}
	if got := platformOf(images, api.Descriptor{Digest: "sha256:" + string(make([]byte, 0)) + "0000000000000000000000000000000000000000000000000000000000000000", MediaType: manifest.MediaType}); got != nil {
		t.Error("an unreadable image must not fail signing")
	}
}
