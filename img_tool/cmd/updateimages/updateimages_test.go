package updateimages

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

	imgreferrers "github.com/bazel-contrib/rules_img/img_tool/pkg/referrers"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, content := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestScanWorkspace(t *testing.T) {
	root := writeFiles(t, map[string]string{
		"MODULE.bazel": `module(name = "example")

bazel_dep(name = "rules_img", version = "0.3.26", repo_name = "img")

pull = use_repo_rule("@img//img:pull.bzl", "pull")

pull(
    name = "alpine",
    repository = "library/alpine",
    tag = "3.23",
    unsafe_allow_tag_without_digest = True,
)

other = use_repo_rule("@other//:pull.bzl", "pull")

other(name = "not_an_image")

include("//images:images.MODULE.bazel")
`,
		"images/images.MODULE.bazel": `DIGEST = "sha256:` + strings.Repeat("a", 64) + `"

images = use_extension("@@rules_img+//img:extensions.bzl", "images", dev_dependency = True)
images.pull(
    name = "nginx",
    digest = DIGEST,
    registries = ["mirror.gcr.io", "index.docker.io"],
    repository = "library/nginx",
    tag = "stable",
)
images.referrers(
    manifest = DIGEST,
    referrer_digests = ["sha256:` + strings.Repeat("b", 64) + `"],
)
images.settings(credential_helper = "helper", config = ":config.json")
use_repo(images, "rules_img_images.bzl", nginx = "img_` + strings.Repeat("a", 64) + `")
`,
	})
	ws, err := ScanWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"MODULE.bazel", "images/images.MODULE.bazel"}; !reflect.DeepEqual(ws.Files, want) {
		t.Errorf("Files = %v, want %v", ws.Files, want)
	}
	var got []string
	for _, entry := range ws.Entries {
		got = append(got, string(entry.Kind)+" "+entry.DisplayName()+" "+entry.Where())
	}
	want := []string{
		"pull alpine MODULE.bazel:7",
		"images.pull nginx images/images.MODULE.bazel:4",
		"images.referrers sha256:" + strings.Repeat("a", 64) + " images/images.MODULE.bazel:11",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %q, want %q", got, want)
	}
	alpine, nginx := ws.Entries[0], ws.Entries[1]
	if !alpine.UnsafeAllowTagWithoutDigest || alpine.Ref() != "index.docker.io/library/alpine:3.23" {
		t.Errorf("alpine = %+v (ref %s)", alpine, alpine.Ref())
	}
	if !nginx.DevDependency || nginx.proxy != "images" || nginx.Digest != "sha256:"+strings.Repeat("a", 64) {
		t.Errorf("nginx = %+v", nginx)
	}
	if nginx.Editable("digest") || !nginx.Editable("tag") || !nginx.Editable("referrer_digests") {
		t.Errorf("digest (a variable) must not be editable, tag and an unset attribute must be")
	}
	if got := nginx.Sources(); len(got) != 2 || got[0].Registry != "mirror.gcr.io" {
		t.Errorf("sources = %v", got)
	}
	if nginx.Target() != "//images/images.MODULE.bazel:%4" {
		t.Errorf("target = %s", nginx.Target())
	}
	if ws.Settings.CredentialHelper != "helper" || ws.Settings.Config != "images/config.json" {
		t.Errorf("settings = %+v", ws.Settings)
	}
	if len(ws.UseRepos) != 1 || ws.UseRepos[0].Keywords["nginx"] != "img_"+strings.Repeat("a", 64) || ws.UseRepos[0].Line != 16 {
		t.Errorf("use_repo = %+v", *ws.UseRepos[0])
	}
}

func TestScanWorkspaceRulesImgItself(t *testing.T) {
	root := writeFiles(t, map[string]string{
		"MODULE.bazel": `module(name = "rules_img")
images = use_extension("//img:extensions.bzl", "images")
images.pull(repository = "distroless/static", registry = "gcr.io", digest = "sha256:` + strings.Repeat("c", 64) + `")
`,
	})
	ws, err := ScanWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws.Entries) != 1 || ws.Entries[0].DisplayName() != "distroless/static" {
		t.Errorf("entries = %+v", ws.Entries)
	}
}

func TestNewerVersionTag(t *testing.T) {
	tags := []string{
		"latest", "3", "3.22", "3.23", "3.23.1", "3.24", "3.24-rc1", "4.0", "edge",
		"v1.36.3", "v1.36.10", "v1.37.0", "v2.0.0", "1.37.1",
		"1.27-alpine", "1.28-alpine", "1.29-bookworm",
		"13.0.0-tensorrt-runtime-ubuntu24.04", "13.1.0-tensorrt-runtime-ubuntu24.04", "13.1.0-runtime-ubuntu24.04",
	}
	for _, tc := range []struct {
		current string
		bump    bump
		want    string
	}{
		{"3.23", bumpMajor, "4.0"},
		{"3.23", bumpMinor, "3.24"},
		{"3.23", bumpPatch, ""},
		{"3.23", bumpNone, ""},
		{"3.24", bumpMinor, ""},
		{"v1.36.3", bumpMajor, "v2.0.0"},
		{"v1.36.3", bumpMinor, "v1.37.0"},
		{"v1.36.3", bumpPatch, "v1.36.10"},
		{"1.27-alpine", bumpMajor, "1.28-alpine"},
		{"13.0.0-tensorrt-runtime-ubuntu24.04", bumpMajor, "13.1.0-tensorrt-runtime-ubuntu24.04"},
		{"latest", bumpMajor, ""},
		{"edge", bumpMajor, ""},
	} {
		got, ok := newerVersionTag(tc.current, tags, tc.bump)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("newerVersionTag(%q, %d) = %q, %v; want %q", tc.current, tc.bump, got, ok, tc.want)
		}
	}
}

func TestPlatformMatches(t *testing.T) {
	for _, tc := range []struct {
		spec, platform string
		want           bool
	}{
		{"linux/amd64", "linux/amd64", true},
		{"linux/arm64", "linux/arm64/v8", true},
		{"linux/arm64/v8", "linux/arm64", true},
		{"linux/arm/v7", "linux/arm/v6", false},
		{"linux/arm", "linux/arm/v6", true},
		{"linux/x86_64", "linux/amd64", true},
		{"windows/amd64", "linux/amd64", false},
	} {
		if got := platformMatches(tc.spec, tc.platform); got != tc.want {
			t.Errorf("platformMatches(%q, %q) = %v, want %v", tc.spec, tc.platform, got, tc.want)
		}
	}
}

// handlerTransport serves requests in-process: binding sockets is not possible
// in every environment the tests run in.
type handlerTransport struct{ handler http.Handler }

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body == nil {
		// A server always sees a body; the in-memory registry relies on it.
		req = req.Clone(req.Context())
		req.Body = http.NoBody
	}
	recorder := httptest.NewRecorder()
	t.handler.ServeHTTP(recorder, req)
	resp := recorder.Result()
	resp.Request = req
	return resp, nil
}

type rawManifest struct{ raw []byte }

func (m rawManifest) RawManifest() ([]byte, error)        { return m.raw, nil }
func (m rawManifest) MediaType() (types.MediaType, error) { return types.OCIManifestSchema1, nil }

const sbomType = "application/vnd.example.sbom"

type fakeRegistry struct {
	t       *testing.T
	repo    name.Repository
	options []remote.Option
}

// pushIndex pushes an index with a linux/amd64 child and an SBOM referrer
// for the child, under tag, and returns the digests of the index, the child
// and the SBOM.
func (r *fakeRegistry) pushIndex(tag string) (string, string, string) {
	r.t.Helper()
	img, err := random.Image(64, 1)
	if err != nil {
		r.t.Fatal(err)
	}
	index := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{
		Add:        img,
		Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}},
	})
	if err := remote.WriteIndex(r.repo.Tag(tag), index, r.options...); err != nil {
		r.t.Fatal(err)
	}
	indexDigest, _ := index.Digest()
	child, _ := img.Digest()
	size, _ := img.Size()
	sbom := r.pushReferrer(v1.Descriptor{MediaType: types.OCIManifestSchema1, Digest: child, Size: size}, sbomType, tag)
	return indexDigest.String(), child.String(), sbom
}

func (r *fakeRegistry) pushReferrer(subject v1.Descriptor, artifactType, marker string) string {
	r.t.Helper()
	emptyDescriptor := map[string]any{"mediaType": "application/vnd.oci.empty.v1+json", "digest": "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a", "size": 2}
	raw, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     string(types.OCIManifestSchema1),
		"artifactType":  artifactType,
		// The in-memory registry reports the config's media type as the
		// artifact type.
		"config":      map[string]any{"mediaType": artifactType, "digest": emptyDescriptor["digest"], "size": 2},
		"layers":      []any{emptyDescriptor},
		"subject":     map[string]any{"mediaType": string(subject.MediaType), "digest": subject.Digest.String(), "size": subject.Size},
		"annotations": map[string]string{"marker": marker},
	})
	if err != nil {
		r.t.Fatal(err)
	}
	h, _, _ := v1.SHA256(bytes.NewReader(raw))
	if err := remote.Put(r.repo.Digest(h.String()), rawManifest{raw: raw}, r.options...); err != nil {
		r.t.Fatal(err)
	}
	return h.String()
}

func newFakeRegistry(t *testing.T) (*fakeRegistry, *registryClient) {
	transport := handlerTransport{registry.New(registry.WithReferrersSupport(true))}
	repo, err := name.NewRepository("registry.example/app")
	if err != nil {
		t.Fatal(err)
	}
	options := []remote.Option{remote.WithTransport(transport)}
	client := &registryClient{
		remote:    options,
		referrers: imgreferrers.Options{Transport: transport, Remote: options},
	}
	return &fakeRegistry{t: t, repo: repo, options: options}, client
}

func TestUpdatePinAndReferrers(t *testing.T) {
	ctx := context.Background()
	reg, client := newFakeRegistry(t)
	oldIndex, oldChild, oldSBOM := reg.pushIndex("1.0")
	newIndex, newChild, newSBOM := reg.pushIndex("1.1")
	if err := remote.Tag(reg.repo.Tag("latest"), mustGet(t, reg, newIndex), reg.options...); err != nil {
		t.Fatal(err)
	}
	// A signature of the new child, which is not pinned anywhere yet.
	signature := reg.pushReferrer(v1.Descriptor{MediaType: types.OCIManifestSchema1, Digest: mustHash(t, newChild)}, "application/vnd.example.signature", "sig")

	root := writeFiles(t, map[string]string{
		"MODULE.bazel": `module(name = "example")

images = use_extension("@rules_img//img:extensions.bzl", "images")
images.pull(
    name = "app",
    digest = "` + oldIndex + `",
    referrer_digests = ["` + oldSBOM + `"],
    registry = "registry.example",
    repository = "app",
    tag = "1.0",
)
images.pull(
    name = "floating",
    registry = "registry.example",
    repository = "app",
    tag = "1.1",
    unsafe_allow_tag_without_digest = True,
)
images.referrers(
    manifest = "` + oldChild + `",
    referrer_digests = ["` + oldSBOM + `"],
)
use_repo(images, "` + imageRepo(oldIndex) + `", app = "` + imageRepo(oldIndex) + `")
`,
	})
	ws, err := ScanWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	p := &planner{client: client, all: ws.Entries, selected: ws.Entries, jobs: 2}

	t.Run("update", func(t *testing.T) {
		results := p.update(ctx, updateOptions{maxBump: bumpMajor, latestTag: "latest"})
		commands := commandStrings(addCommands(ws, results))
		want := []string{
			`set digest "` + newIndex + `"|//MODULE.bazel:%4`,
			`set tag "1.1"|//MODULE.bazel:%4`,
			`set referrer_digests ["` + newSBOM + `"]|//MODULE.bazel:%4`,
			`set app "` + imageRepo(newIndex) + `"|//MODULE.bazel:%23`,
			`use_repo_remove images ` + imageRepo(oldIndex) + `|//MODULE.bazel:all`,
			`use_repo_add images ` + imageRepo(newIndex) + `|//MODULE.bazel:all`,
			`set manifest "` + newChild + `"|//MODULE.bazel:%19`,
			`set referrer_digests ["` + newSBOM + `"]|//MODULE.bazel:%19`,
		}
		if !reflect.DeepEqual(commands, want) {
			t.Errorf("commands:\n%s\nwant:\n%s", strings.Join(commands, "\n"), strings.Join(want, "\n"))
		}
		for _, r := range results {
			if r.Status == StatusError {
				t.Errorf("%s: %s", r.Entry.DisplayName(), r.Error)
			}
		}
	})

	t.Run("update keeping tags", func(t *testing.T) {
		results := p.update(ctx, updateOptions{maxBump: bumpNone, latestTag: "latest"})
		for _, r := range results {
			if r.Status != StatusUpToDate {
				t.Errorf("%s: status %s (new tag %q, new digest %q), want up to date: the tag 1.0 did not move", r.Entry.DisplayName(), r.Status, r.NewTag, r.NewDigest)
			}
		}
	})

	t.Run("pin", func(t *testing.T) {
		commands := commandStrings(addCommands(ws, p.pin(ctx)))
		want := []string{
			`set digest "` + newIndex + `"|//MODULE.bazel:%12`,
			`remove unsafe_allow_tag_without_digest|//MODULE.bazel:%12`,
		}
		if !reflect.DeepEqual(commands, want) {
			t.Errorf("commands = %q, want %q", commands, want)
		}
	})

	t.Run("referrers", func(t *testing.T) {
		app := &planner{client: client, all: ws.Entries, selected: ws.Entries[:1], jobs: 1}
		results := app.referrers(ctx, newTypeFilter([]string{"application/vnd.example.*"}))
		if len(results) != 1 {
			t.Fatalf("got %d results", len(results))
		}
		r := results[0]
		if len(r.Referrers) != 1 || r.Referrers[0].Digest != oldSBOM || !r.Referrers[0].Pinned || r.Referrers[0].SubjectPlatform != "linux/amd64" || r.Referrers[0].Annotations["marker"] != "1.0" {
			t.Errorf("referrers of the old image = %+v", r.Referrers)
		}
		if r.Status != StatusUpToDate || len(r.AddReferrers) != 0 {
			t.Errorf("status %s, add %v: the only referrer is pinned", r.Status, r.AddReferrers)
		}

		// The referrers tag names the old child; point it at the new one.
		moved := *ws.Entries[2]
		moved.Manifest = newChild
		moved.ReferrerDigests = []string{newSBOM}
		tagged := &planner{client: client, all: append(slices.Clone(ws.Entries), &Entry{Kind: KindImagesPull, Registry: "registry.example", Repository: "app", Digest: newIndex}), selected: []*Entry{&moved}, jobs: 1}
		for _, tc := range []struct {
			filter []string
			want   []string
		}{
			{nil, []string{signature}},
			{[]string{"application/vnd.example.signature"}, []string{signature}},
			{[]string{sbomType}, nil},
			{[]string{"cosign:*"}, nil},
		} {
			results := tagged.referrers(ctx, newTypeFilter(tc.filter))
			if got := results[0]; !reflect.DeepEqual(got.AddReferrers, tc.want) || got.Error != "" {
				t.Errorf("filter %v: add %v (error %q), want %v", tc.filter, got.AddReferrers, got.Error, tc.want)
			}
		}
	})
}

func commandStrings(commands []command) []string {
	var strs []string
	for _, c := range commands {
		strs = append(strs, c.String())
	}
	return strs
}

func mustHash(t *testing.T, digest string) v1.Hash {
	t.Helper()
	h, err := v1.NewHash(digest)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func mustGet(t *testing.T, reg *fakeRegistry, digest string) *remote.Descriptor {
	t.Helper()
	descriptor, err := remote.Get(reg.repo.Digest(digest), reg.options...)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func TestAddCommandsKeepsSharedImageRepos(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	first := &Entry{Kind: KindImagesPull, Root: true, File: "MODULE.bazel", Line: 2, Name: "first", Digest: digest, proxy: "images"}
	second := &Entry{Kind: KindImagesPull, Root: true, File: "MODULE.bazel", Line: 3, Name: "second", Digest: digest, proxy: "images"}
	ws := &Workspace{
		Entries:  []*Entry{first, second},
		UseRepos: []*UseRepo{{File: "MODULE.bazel", Line: 4, Proxy: "images", Keywords: map[string]string{"first": imageRepo(digest)}}},
	}
	r := &result{Entry: first, Status: StatusChanged, NewDigest: "sha256:" + strings.Repeat("b", 64)}
	commands := commandStrings(addCommands(ws, []*result{r}))
	if want := []string{`set digest "sha256:` + strings.Repeat("b", 64) + `"|//MODULE.bazel:%2`}; !reflect.DeepEqual(commands, want) {
		t.Errorf("commands = %q, want %q", commands, want)
	}
	if len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "second also pulls") {
		t.Errorf("notes = %q", r.Notes)
	}
}

func TestInventoryMerge(t *testing.T) {
	ws := &Workspace{Entries: []*Entry{{Kind: KindImagesPull, Root: true, Name: "nginx"}}}
	inv := &inventory{
		Images: []inventoryTag{
			{Module: "", IsRoot: true, Name: "nginx"},
			{Module: "", IsRoot: true, Name: "hidden"},
			{Module: "dep", Version: "1.0", Name: "base", Repository: "distroless/base", Registry: "gcr.io", Digest: "sha256:" + strings.Repeat("d", 64)},
		},
	}
	entries, warnings := inv.merge(ws)
	if len(entries) != 2 || entries[1].Where() != "dep@1.0" || entries[1].Root {
		t.Errorf("entries = %+v", entries)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], `"hidden"`) {
		t.Errorf("warnings = %q", warnings)
	}
}

func TestParseArgs(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		command  string
		patterns []string
		keepTags bool
	}{
		{[]string{"update", "--keep-tags"}, "update", nil, true},
		{[]string{"update", "nginx", "--keep-tags", "alpine*"}, "update", []string{"nginx", "alpine*"}, true},
		{[]string{"referrers", "--bazel-target", "@rules_img//img/pull:referrers", "--inventory", "inv.json", "nginx"}, "referrers", []string{"nginx"}, false},
		{[]string{"list"}, "list", nil, false},
		// After the command, every argument is a pattern.
		{[]string{"update", "list"}, "update", []string{"list"}, false},
	} {
		opts, err := parseArgs(tc.args, io.Discard, io.Discard)
		if err != nil {
			t.Errorf("parseArgs(%q): %v", tc.args, err)
			continue
		}
		if opts.command != tc.command || !reflect.DeepEqual(opts.patterns, tc.patterns) || opts.keepTags != tc.keepTags {
			t.Errorf("parseArgs(%q) = command %q, patterns %q, keep-tags %v; want %q, %q, %v", tc.args, opts.command, opts.patterns, opts.keepTags, tc.command, tc.patterns, tc.keepTags)
		}
	}
	for _, args := range [][]string{
		nil,
		{"bogus"},
		// Flags of other commands are rejected, rather than ignored.
		{"referrers", "--keep-tags"},
		{"list", "--mode=apply"},
		{"pin", "--root-only"},
	} {
		if _, err := parseArgs(args, io.Discard, io.Discard); err == nil {
			t.Errorf("parseArgs(%q) succeeded", args)
		}
	}
}

func TestApplyCommand(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{
			[]string{"update", "--bazel-target", "@rules_img//img/pull:update", "--inventory", "/tmp/inventory.json"},
			"bazel run @rules_img//img/pull:update -- --mode=apply",
		},
		{
			[]string{"update", "--bazel-target", "@rules_img//img/pull:update", "--inventory", "/tmp/inventory.json", "--mode=report", "distroless*", "--keep-tags", "--max-bump", "minor", "nginx"},
			"bazel run @rules_img//img/pull:update -- --mode=apply --keep-tags --max-bump=minor 'distroless*' nginx",
		},
		{
			[]string{"referrers", "--bazel-target", "@rules_img//img/pull:referrers", "--artifact-type=cosign:att", "--artifact-type", "application/vnd.dev.sigstore.*"},
			"bazel run @rules_img//img/pull:referrers -- --mode=apply --artifact-type=cosign:att '--artifact-type=application/vnd.dev.sigstore.*'",
		},
		{
			[]string{"pin", "it's"},
			`img update-images pin --mode=apply 'it'\''s'`,
		},
	} {
		opts, err := parseArgs(tc.args, io.Discard, io.Discard)
		if err != nil {
			t.Fatalf("parseArgs(%q): %v", tc.args, err)
		}
		if got := opts.applyCommand(); got != tc.want {
			t.Errorf("applyCommand() for %q =\n  %s\nwant\n  %s", tc.args, got, tc.want)
		}
	}
}

func TestCommandHelp(t *testing.T) {
	var help strings.Builder
	_, err := parseArgs([]string{"update", "--bazel-target", "@rules_img//img/pull:update", "--inventory", "inv.json", "--help"}, &help, io.Discard)
	if err != flag.ErrHelp {
		t.Fatalf("parseArgs(--help) = %v, want flag.ErrHelp", err)
	}
	for _, want := range []string{
		"Usage: bazel run @rules_img//img/pull:update -- [FLAGS] [PATTERN...]",
		"--keep-tags",
		"--mode string",
		`(default "report")`,
		"@rules_img//img/pull:pin",
	} {
		if !strings.Contains(help.String(), want) {
			t.Errorf("help does not contain %q:\n%s", want, help.String())
		}
	}
	for _, unwanted := range []string{"--artifact-type", "--inventory", "--bazel-target", "img update-images"} {
		if strings.Contains(help.String(), unwanted) {
			t.Errorf("help contains %q:\n%s", unwanted, help.String())
		}
	}
}

type rawIndex struct{ raw []byte }

func (m rawIndex) RawManifest() ([]byte, error)        { return m.raw, nil }
func (m rawIndex) MediaType() (types.MediaType, error) { return types.OCIImageIndex, nil }

// pushBuildKitIndex pushes an index under tag as `docker buildx build --sbom`
// with oci-artifact=false writes it: a linux/amd64 child, and an attestation
// manifest for it without a subject, which only the index ties to the child.
// It returns the digests of the index, the child and the attestation.
func (r *fakeRegistry) pushBuildKitIndex(tag string) (string, string, string) {
	r.t.Helper()
	img, err := random.Image(64, 1)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := remote.Write(r.repo.Tag(tag+"-child"), img, r.options...); err != nil {
		r.t.Fatal(err)
	}
	child, _ := img.Digest()
	childSize, _ := img.Size()
	emptyDescriptor := map[string]any{"mediaType": "application/vnd.oci.empty.v1+json", "digest": "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a", "size": 2}
	attestationRaw, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     string(types.OCIManifestSchema1),
		"config":        emptyDescriptor,
		"layers":        []any{emptyDescriptor},
		"annotations":   map[string]string{"marker": tag},
	})
	if err != nil {
		r.t.Fatal(err)
	}
	attestation, attestationSize, _ := v1.SHA256(bytes.NewReader(attestationRaw))
	if err := remote.Put(r.repo.Digest(attestation.String()), rawManifest{raw: attestationRaw}, r.options...); err != nil {
		r.t.Fatal(err)
	}
	indexRaw, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     string(types.OCIImageIndex),
		"manifests": []any{
			map[string]any{"mediaType": string(types.OCIManifestSchema1), "digest": child.String(), "size": childSize, "platform": map[string]string{"os": "linux", "architecture": "amd64"}},
			map[string]any{
				"mediaType":   string(types.OCIManifestSchema1),
				"digest":      attestation.String(),
				"size":        attestationSize,
				"platform":    map[string]string{"os": "unknown", "architecture": "unknown"},
				"annotations": map[string]string{"vnd.docker.reference.type": "attestation-manifest", "vnd.docker.reference.digest": child.String()},
			},
		},
	})
	if err != nil {
		r.t.Fatal(err)
	}
	index, _, _ := v1.SHA256(bytes.NewReader(indexRaw))
	if err := remote.Put(r.repo.Tag(tag), rawIndex{raw: indexRaw}, r.options...); err != nil {
		r.t.Fatal(err)
	}
	return index.String(), child.String(), attestation.String()
}

// A pinned BuildKit attestation manifest without a subject is about the
// manifest the index lists it for: an update repins the new index's
// attestation of the corresponding manifest, and `referrers` finds it.
func TestUpdateRepinsBuildKitAttestations(t *testing.T) {
	ctx := context.Background()
	reg, client := newFakeRegistry(t)
	oldIndex, _, oldAttestation := reg.pushBuildKitIndex("1.0")
	newIndex, _, newAttestation := reg.pushBuildKitIndex("1.1")

	root := writeFiles(t, map[string]string{
		"MODULE.bazel": `module(name = "example")

images = use_extension("@rules_img//img:extensions.bzl", "images")
images.pull(
    name = "app",
    digest = "` + oldIndex + `",
    referrer_digests = ["` + oldAttestation + `"],
    registry = "registry.example",
    repository = "app",
    tag = "1.0",
)
`,
	})
	ws, err := ScanWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	p := &planner{client: client, all: ws.Entries, selected: ws.Entries, jobs: 1}
	results := p.update(ctx, updateOptions{maxBump: bumpMajor, latestTag: "latest"})
	if len(results) != 1 || results[0].Status != StatusChanged || results[0].NewDigest != newIndex {
		t.Fatalf("results = %+v, want the image moved to %s", results, newIndex)
	}
	if got := results[0].SetReferrers; got == nil || !slices.Equal(*got, []string{newAttestation}) {
		t.Errorf("repinned referrers = %v, want %v (notes %v)", got, []string{newAttestation}, results[0].Notes)
	}

	results = p.referrers(ctx, newTypeFilter([]string{"buildkit:attestation"}))
	if r := results[0]; len(r.Referrers) != 1 || r.Referrers[0].Digest != oldAttestation || !r.Referrers[0].Pinned || r.Referrers[0].SubjectPlatform != "linux/amd64" {
		t.Errorf("BuildKit attestations of the old image = %+v (error %q)", r.Referrers, r.Error)
	}
}
