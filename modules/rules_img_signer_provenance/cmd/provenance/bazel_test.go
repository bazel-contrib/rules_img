package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/bazel-contrib/rules_img_signer_provenance/pkg/plugin"
	"github.com/bazel-contrib/rules_img_signer_provenance/pkg/signerapi"
)

// signBazel runs the plugin end to end and returns the statement as generic JSON.
func signBazel(t *testing.T, req any, args ...string) map[string]any {
	t.Helper()
	s, err := newSigner(append([]string{"--definitions=bazel"}, args...))
	if err != nil {
		t.Fatalf("newSigner(%v): %v", args, err)
	}
	in, _ := json.Marshal(req)
	var out bytes.Buffer
	if err := plugin.Run(context.Background(), s, bytes.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(layerFromLayout(t, out.Bytes()), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// at digs into generic JSON along a path of keys (and array indexes).
func at(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch key := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("%v: not an object at %q (%T)", path, key, v)
			}
			v = m[key]
		case int:
			a, ok := v.([]any)
			if !ok || key >= len(a) {
				t.Fatalf("%v: no element %d (%T)", path, key, v)
			}
			v = a[key]
		}
	}
	return v
}

func predicateOf(t *testing.T, st map[string]any) map[string]any {
	t.Helper()
	return at(t, st, "predicate").(map[string]any)
}

func setGitHubEnv(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "push", "GITHUB_EVENT_PATH": "/nonexistent/event.json",
		"GITHUB_ACTOR": "someone", "GITHUB_ACTOR_ID": "1951866", "GITHUB_TRIGGERING_ACTOR": "someone",
		"GITHUB_REF": "refs/heads/main", "GITHUB_REF_NAME": "main", "GITHUB_REF_PROTECTED": "true", "GITHUB_REF_TYPE": "branch",
		"GITHUB_REPOSITORY": "bazel-contrib/rules_img", "GITHUB_REPOSITORY_ID": "286865579",
		"GITHUB_REPOSITORY_OWNER": "bazel-contrib", "GITHUB_REPOSITORY_OWNER_ID": "1234",
		"GITHUB_JOB": "push-ghcr", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_RUN_ID": "18715517508", "GITHUB_RUN_NUMBER": "617",
		"GITHUB_SERVER_URL": "https://github.com", "GITHUB_WORKFLOW": "Push Service Images",
		"GITHUB_WORKFLOW_REF": "bazel-contrib/rules_img/.github/workflows/push_services.yaml@refs/heads/main",
		"GITHUB_WORKFLOW_SHA": "55f1937b77c46a5dfb8cc4f8c4938b8e729c05a1",
		"ImageOS":             "ubuntu24", "ImageVersion": "20250929.60.1", "RUNNER_OS": "Linux", "RUNNER_ARCH": "X64",
		"RUNNER_ENVIRONMENT": "github-hosted", "RUNNER_TRACKING_ID": "github_7bd1d852", "RUNNER_NAME": "GitHub Actions 1000091440",
	} {
		t.Setenv(k, v)
	}
}

func TestDefinitionsDefaultToBuildKit(t *testing.T) {
	clearCIEnv(t)
	_, _, st, _ := sign(t)
	if st.Predicate.BuildDefinition.BuildType != buildKitBuildType {
		t.Errorf("buildType = %q", st.Predicate.BuildDefinition.BuildType)
	}
}

func TestBazelDefinitionsShape(t *testing.T) {
	clearCIEnv(t)
	req := testRequest()
	req.Subject.Platform = &v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
	st := signBazel(t, req,
		"--builder-id", "https://example.com/builder",
		"--source-uri", "https://user:secret@example.com/org/repo",
		"--source-ref", "refs/heads/main",
		"--source-commit", strings.Repeat("ab", 20),
		"--source-path", "modules/app",
		"--target", "//app:push", "--target", "//other:push", "--target", "//app:push",
		"--parameter", "config=sign", "--parameter", "bazel_version=9.3.1",
		"--material", "pkg:docker/alpine@3.21@sha256:abcd",
		"--reproducible",
	)

	if got := at(t, st, "predicateType"); got != "https://slsa.dev/provenance/v1" {
		t.Errorf("predicateType = %v", got)
	}
	p := predicateOf(t, st)
	if got := at(t, p, "buildDefinition", "buildType"); got != bazelBuildType {
		t.Errorf("buildType = %v", got)
	}

	ws := at(t, p, "buildDefinition", "externalParameters", "workspace")
	if !reflect.DeepEqual(ws, map[string]any{"repository": "https://xxxxx:xxxxx@example.com/org/repo", "ref": "refs/heads/main", "path": "modules/app"}) {
		t.Errorf("workspace = %v", ws)
	}
	if got := at(t, p, "buildDefinition", "externalParameters", "targets"); !reflect.DeepEqual(got, []any{"//app:push", "//other:push"}) {
		t.Errorf("targets must be in order, without duplicates: %v", got)
	}
	if got := at(t, p, "buildDefinition", "externalParameters", "parameters"); !reflect.DeepEqual(got, map[string]any{"config": "sign", "bazel_version": "9.3.1"}) {
		t.Errorf("parameters = %v", got)
	}

	internal := at(t, p, "buildDefinition", "internalParameters").(map[string]any)
	if internal["targetPlatform"] != "linux/arm64" || internal["pushStrategy"] != "eager" || internal["reproducible"] != true || internal["builderPlatform"] == "" {
		t.Errorf("internalParameters = %v", internal)
	}

	source := at(t, p, "buildDefinition", "resolvedDependencies", 0)
	if !reflect.DeepEqual(source, map[string]any{
		"name": "source", "uri": "git+https://xxxxx:xxxxx@example.com/org/repo@refs/heads/main",
		"digest": map[string]any{"gitCommit": strings.Repeat("ab", 20)},
	}) {
		t.Errorf("source dependency = %v", source)
	}
	if material := at(t, p, "buildDefinition", "resolvedDependencies", 1); !reflect.DeepEqual(material, map[string]any{
		"uri": "pkg:docker/alpine@3.21", "digest": map[string]any{"sha256": "abcd"},
	}) {
		t.Errorf("material = %v", material)
	}

	if got := at(t, p, "runDetails", "builder"); !reflect.DeepEqual(got, map[string]any{"id": "https://example.com/builder", "version": map[string]any{"rules_img": "0.3.25"}}) {
		t.Errorf("builder = %v", got)
	}
	if got := at(t, p, "runDetails", "metadata", "invocationId"); got != "11111111-2222-4333-8444-555555555555" {
		t.Errorf("invocationId = %v", got)
	}
}

// Nothing specific to BuildKit leaks into the Bazel definitions.
func TestBazelDefinitionsHaveNothingFromBuildKit(t *testing.T) {
	clearCIEnv(t)
	setGitHubEnv(t)
	st := signBazel(t, testRequest(), "--source-uri", "https://example.com/org/repo", "--source-commit", strings.Repeat("ab", 20))
	raw, _ := json.Marshal(st)
	for _, leaked := range []string{"buildkit", "configSource", "request", "completeness", "hermetic", "github_event_payload", "github_", "frontend"} {
		if strings.Contains(strings.ToLower(string(raw)), strings.ToLower(leaked)) {
			t.Errorf("the bazel definitions must not contain %q: %s", leaked, raw)
		}
	}
}

// min/max is BuildKit's way to scrub build arguments; it means nothing here.
func TestBazelDefinitionsKeepAllParameters(t *testing.T) {
	clearCIEnv(t)
	st := signBazel(t, testRequest(), "--parameter", "build-arg:A=1", "--parameter", "label:b=2")
	got := at(t, predicateOf(t, st), "buildDefinition", "externalParameters", "parameters")
	if !reflect.DeepEqual(got, map[string]any{"build-arg:A": "1", "label:b": "2"}) {
		t.Errorf("parameters = %v, want both kept", got)
	}
}

func TestBazelDefinitionsMarkModifiedSourceByAnnotation(t *testing.T) {
	clearCIEnv(t)
	t.Setenv("GO_WANT_STATUS", "1")
	st := signBazel(t, testRequest(), statusArgs("--status-key", "STABLE_TEAM")...)
	p := predicateOf(t, st)

	source := at(t, p, "buildDefinition", "resolvedDependencies", 0).(map[string]any)
	if !reflect.DeepEqual(source["annotations"], map[string]any{"modified": true}) {
		t.Errorf("source = %v", source)
	}
	// The digest stays the commit: the dirty marker is not glued to it.
	if source["digest"].(map[string]any)["gitCommit"] != strings.Repeat("cd", 20) {
		t.Errorf("digest = %v", source["digest"])
	}
	ws := at(t, p, "buildDefinition", "externalParameters", "workspace").(map[string]any)
	if ws["path"] != "app/BUILD.bazel" || ws["repository"] != "https://xxxxx:xxxxx@example.com/org/repo.git" {
		t.Errorf("workspace = %v", ws)
	}
	// What the status command printed is external: the script belongs to the workspace.
	if got := at(t, p, "buildDefinition", "externalParameters", "workspaceStatus"); !reflect.DeepEqual(got, map[string]any{"STABLE_TEAM": "platform team"}) {
		t.Errorf("workspaceStatus = %v", got)
	}
}

func TestBazelDefinitionsCI(t *testing.T) {
	clearCIEnv(t)
	setGitHubEnv(t)
	st := signBazel(t, signerapi.Request{ProtocolVersion: signerapi.ProtocolRequest, Subject: testSubject()},
		"--source-uri", "https://github.com/bazel-contrib/rules_img", "--source-commit", strings.Repeat("ab", 20))
	p := predicateOf(t, st)

	want := map[string]any{
		"system": "github-actions", "eventName": "push", "actorId": "1951866", "repositoryId": "286865579",
		"repositoryOwnerId": "1234", "refType": "branch", "refProtected": "true",
		"runner": map[string]any{"environment": "github-hosted", "os": "Linux", "arch": "X64", "imageOs": "ubuntu24", "imageVersion": "20250929.60.1"},
	}
	if got := at(t, p, "buildDefinition", "internalParameters", "ci"); !reflect.DeepEqual(got, want) {
		t.Errorf("ci = %v\nwant %v", got, want)
	}
	// The ref of the run is what the workspace was at, and the run is the invocation.
	if got := at(t, p, "buildDefinition", "externalParameters", "workspace", "ref"); got != "refs/heads/main" {
		t.Errorf("workspace.ref = %v", got)
	}
	if got := at(t, p, "buildDefinition", "resolvedDependencies", 0, "uri"); got != "git+https://github.com/bazel-contrib/rules_img@refs/heads/main" {
		t.Errorf("source uri = %v", got)
	}
	if got := at(t, p, "runDetails", "metadata", "invocationId"); got != "https://github.com/bazel-contrib/rules_img/actions/runs/18715517508/attempts/1" {
		t.Errorf("invocationId = %v", got)
	}

	// --ci=none leaves the environment out.
	st = signBazel(t, testRequest(), "--ci", "none")
	if _, ok := at(t, predicateOf(t, st), "buildDefinition", "internalParameters").(map[string]any)["ci"]; ok {
		t.Error("--ci=none must not record the CI environment")
	}
}

func TestBazelDefinitionsBuildInfo(t *testing.T) {
	clearCIEnv(t)
	path := filepath.Join(t.TempDir(), "info.json")
	info := `{"builder_id":"https://b","build_type":"https://example.com/type","targets":["//a:push"],"source":{"uri":"https://example.com/r","ref":"refs/tags/v1","commit":"` +
		strings.Repeat("ee", 20) + `"},"materials":[{"name":"base","uri":"pkg:docker/busybox@latest","digest":{"sha256":"aa"}}],"internal_parameters":{"custom":"1"}}`
	if err := os.WriteFile(path, []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}
	st := signBazel(t, testRequest(), "--build-info", path)
	p := predicateOf(t, st)
	if got := at(t, p, "buildDefinition", "buildType"); got != "https://example.com/type" {
		t.Errorf("buildType = %v", got)
	}
	if got := at(t, p, "buildDefinition", "externalParameters", "targets"); !reflect.DeepEqual(got, []any{"//a:push"}) {
		t.Errorf("targets = %v", got)
	}
	if got := at(t, p, "buildDefinition", "resolvedDependencies", 1, "name"); got != "base" {
		t.Errorf("a material can be named by its role: %v", got)
	}
	if got := at(t, p, "buildDefinition", "resolvedDependencies", 0, "uri"); got != "git+https://example.com/r@refs/tags/v1" {
		t.Errorf("source uri = %v", got)
	}
	if got := at(t, p, "buildDefinition", "internalParameters", "custom"); got != "1" {
		t.Errorf("custom internal parameter = %v", got)
	}
}

func TestDefinitionsFlagRules(t *testing.T) {
	clearCIEnv(t)
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"unknown definitions", []string{"--definitions=slsa"}},
		{"bazel with --frontend", []string{"--definitions=bazel", "--frontend=x"}},
		{"bazel with --mode", []string{"--definitions=bazel", "--mode=max"}},
		{"bazel with --github-event-payload", []string{"--definitions=bazel", "--github-event-payload=false"}},
		{"buildkit with --target", []string{"--target=//a:b"}},
		{"buildkit with --source-ref", []string{"--source-ref=refs/heads/main"}},
		{"bazel with frontend in build info", []string{"--definitions=bazel", "--build-info", write("f.json", `{"frontend":"x"}`)}},
		{"bazel with completeness in build info", []string{"--definitions=bazel", "--build-info", write("c.json", `{"completeness":{"request":true}}`)}},
		{"buildkit with targets in build info", []string{"--build-info", write("t.json", `{"targets":["//a:b"]}`)}},
		{"bazel reusing a defined internal parameter", []string{"--definitions=bazel", "--build-info", write("i.json", `{"internal_parameters":{"targetPlatform":"x"}}`)}},
	} {
		if _, err := newSigner(tc.args); err == nil {
			t.Errorf("%s: newSigner(%v) should fail", tc.name, tc.args)
		}
	}
	for _, args := range [][]string{{"--definitions=buildkit", "--mode=max", "--frontend=x"}, {"--definitions=bazel", "--target=//a:b", "--source-ref=refs/heads/x"}} {
		if _, err := newSigner(args); err != nil {
			t.Errorf("newSigner(%v): %v", args, err)
		}
	}
}

func TestGitURI(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/org/repo":       "git+https://github.com/org/repo@refs/heads/main",
		"git+https://github.com/org/repo":   "git+https://github.com/org/repo@refs/heads/main",
		"ssh://git@github.com/org/repo.git": "git+ssh://git@github.com/org/repo.git@refs/heads/main",
		"git@github.com:org/repo.git":       "git+ssh://git@github.com/org/repo.git@refs/heads/main",
	} {
		if got := gitURI(in, "refs/heads/main"); got != want {
			t.Errorf("gitURI(%q) = %q, want %q", in, got, want)
		}
	}
	if got := gitURI("https://github.com/org/repo", ""); got != "git+https://github.com/org/repo" {
		t.Errorf("without a ref: %q", got)
	}
}

// --- the document and the code agree ---

// docFixture writes the request every documentation test signs.
func docRequest() signerapi.Request {
	return signerapi.Request{
		ProtocolVersion: signerapi.ProtocolRequest,
		Subject: v1.Descriptor{
			MediaType: "application/vnd.oci.image.manifest.v1+json", Size: 1054,
			Digest:   v1.Hash{Algorithm: "sha256", Hex: "5d3f6e0cc0a0c4e5ba9a2b7aa1a3b1ae2d3b2ab3ba0b1ad10cd8b5b4a6a8c7aa"},
			Platform: &v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"},
		},
		Role: signerapi.RoleChildManifest,
		Destination: signerapi.Destination{
			Registry: "ghcr.io", Repository: "bazel-contrib/rules_img/img", Tags: []string{"1.2.3"},
			References: []string{"ghcr.io/bazel-contrib/rules_img/img:1.2.3"},
		},
		Deploy: signerapi.Deploy{RulesImgVersion: "0.3.25", StartedAt: "2023-11-14T22:13:20Z", PushStrategy: "eager"},
	}
}

func docExample(t *testing.T) map[string]any {
	t.Helper()
	clearCIEnv(t)
	setGitHubEnv(t)
	t.Setenv("SOURCE_DATE_EPOCH", "1700000100")
	return signBazel(t, docRequest(),
		"--builder-id", "https://github.com/bazel-contrib/rules_img/.github/workflows/push_services.yaml@refs/heads/main",
		"--source-uri", "https://github.com/bazel-contrib/rules_img",
		"--source-path", "modules/rules_img_services",
		"--source-commit", "55f1937b77c46a5dfb8cc4f8c4938b8e729c05a1",
		"--target", "//services/img:push",
		"--parameter", "config=sign",
		"--builder-platform", "linux/amd64",
		"--reproducible", "--record-timestamps",
	)
}

// flattenPaths lists the dotted paths of a JSON document. Array elements share a
// path ending in [], and the keys under parameters and workspaceStatus, which the
// user chooses, are not listed.
func flattenPaths(prefix string, v any, out map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			out[path] = true
			if k == "parameters" || k == "workspaceStatus" {
				continue
			}
			flattenPaths(path, child, out)
		}
	case []any:
		for _, child := range x {
			flattenPaths(prefix+"[]", child, out)
		}
	}
}

func readDoc(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../slsa-definitions.md")
	if err != nil {
		t.Fatalf("reading slsa-definitions.md: %v", err)
	}
	return string(data)
}

// Every field the bazel definitions can emit is defined in the document.
func TestDocumentDefinesEveryField(t *testing.T) {
	doc := readDoc(t)

	example := docExample(t)
	paths := map[string]bool{}
	flattenPaths("", predicateOf(t, example), paths)

	// A second statement reaches the fields the example does not: a modified source
	// resolved from a status command, a named material, and the status keys.
	t.Setenv("GO_WANT_STATUS", "1")
	other := signBazel(t, docRequest(), statusArgs("--status-key", "STABLE_TEAM", "--material", "pkg:docker/alpine@3.21@sha256:aa")...)
	flattenPaths("", predicateOf(t, other), paths)

	var missing []string
	for path := range paths {
		if !strings.Contains(doc, "`"+path+"`") {
			missing = append(missing, path)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("slsa-definitions.md does not define these fields (write them as `path`):\n  %s", strings.Join(missing, "\n  "))
	}
}

// The example in the document is what the plugin writes.
func TestDocumentExampleIsCurrent(t *testing.T) {
	doc := readDoc(t)
	const start, end = "<!-- example:start -->\n```json\n", "\n```\n<!-- example:end -->"
	i, j := strings.Index(doc, start), strings.Index(doc, end)
	if i < 0 || j < i {
		t.Fatal("slsa-definitions.md has no <!-- example:start --> block")
	}

	example := docExample(t)
	want, err := json.MarshalIndent(example, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var documented map[string]any
	if err := json.Unmarshal([]byte(doc[i+len(start):j]), &documented); err == nil && reflect.DeepEqual(documented, example) {
		return
	}
	if os.Getenv("UPDATE_SLSA_DOC") == "1" {
		updated := doc[:i+len(start)] + string(want) + doc[j:]
		if err := os.WriteFile("../../slsa-definitions.md", []byte(updated), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Errorf("the example in slsa-definitions.md is out of date; run `UPDATE_SLSA_DOC=1 go test ./cmd/provenance -run TestDocumentExampleIsCurrent` or use:\n%s", want)
}
