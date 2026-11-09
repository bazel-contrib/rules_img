package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/bazel-contrib/rules_img_signer_provenance/pkg/plugin"
	"github.com/bazel-contrib/rules_img_signer_provenance/pkg/signerapi"
)

const testDigest = "sha256:5d3f6e0cc0a0c4e5ba9a2b7aa1a3b1ae2d3b2ab3ba0b1ad10cd8b5b4a6a8c7aa"

func testSubject() v1.Descriptor {
	h, _ := v1.NewHash(testDigest)
	return v1.Descriptor{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Size:      1054,
		Digest:    h,
		Platform:  &v1.Platform{OS: "linux", Architecture: "arm64"},
	}
}

func clearCIEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("SOURCE_DATE_EPOCH", "")
	t.Setenv("RULES_IMG_PROVENANCE_BUILDER_ID", "")
	t.Setenv("RULES_IMG_PROVENANCE_INVOCATION_ID", "")
	t.Setenv("BUILD_ID", "")
}

// sign runs newSigner+Sign and returns the decoded manifest and statement.
func sign(t *testing.T, args ...string) (manifest v1.Manifest, rawStatement []byte, st statement, img v1.Image) {
	t.Helper()
	s, err := newSigner(args)
	if err != nil {
		t.Fatalf("newSigner(%v): %v", args, err)
	}
	img, err = s.Sign(context.Background(), testSubject())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	m, err := img.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	layers, err := img.Layers()
	if err != nil || len(layers) != 1 {
		t.Fatalf("want exactly 1 layer, got %d (err %v)", len(layers), err)
	}
	rc, err := layers[0].Compressed()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	rawStatement, err = io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawStatement, &st); err != nil {
		t.Fatalf("statement is not valid JSON: %v", err)
	}
	return *m, rawStatement, st, img
}

// The manifest must be byte-compatible in shape with BuildKit's oci-artifact
// attestation manifest.
func TestArtifactShapeMatchesBuildKit(t *testing.T) {
	clearCIEnv(t)
	m, _, st, _ := sign(t, "--subject-name", "ghcr.io/foo/bar:v2")

	if m.ArtifactType != "application/vnd.docker.attestation.manifest.v1+json" {
		t.Errorf("artifactType = %q", m.ArtifactType)
	}
	if string(m.Config.MediaType) != "application/vnd.oci.empty.v1+json" ||
		m.Config.Digest.String() != "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a" ||
		m.Config.Size != 2 || string(m.Config.Data) != "{}" {
		t.Errorf("config is not the OCI empty descriptor: %+v", m.Config)
	}
	if m.Subject == nil || m.Subject.Digest.String() != testDigest || m.Subject.Size != 1054 ||
		string(m.Subject.MediaType) != "application/vnd.oci.image.manifest.v1+json" {
		t.Fatalf("manifest subject = %+v", m.Subject)
	}
	if m.Subject.Platform != nil {
		t.Errorf("manifest subject must not carry a platform (BuildKit does not set one): %+v", m.Subject.Platform)
	}
	if len(m.Annotations) != 0 {
		t.Errorf("BuildKit sets no manifest annotations, got %v", m.Annotations)
	}
	l := m.Layers[0]
	if string(l.MediaType) != "application/vnd.in-toto+json" ||
		l.Annotations["in-toto.io/predicate-type"] != "https://slsa.dev/provenance/v1" || len(l.Annotations) != 1 {
		t.Errorf("layer = %+v", l)
	}

	if st.Type != "https://in-toto.io/Statement/v1" || st.PredicateType != "https://slsa.dev/provenance/v1" {
		t.Errorf("statement type/predicateType = %q / %q", st.Type, st.PredicateType)
	}
	if len(st.Subject) != 1 || st.Subject[0].Digest["sha256"] != strings.TrimPrefix(testDigest, "sha256:") {
		t.Fatalf("statement subject does not bind to the image digest: %+v", st.Subject)
	}
	if got, want := st.Subject[0].Name, "pkg:docker/ghcr.io/foo/bar@v2?platform=linux%2Farm64"; got != want {
		t.Errorf("subject name = %q, want %q", got, want)
	}
	if got := st.Predicate.BuildDefinition.BuildType; got != buildKitBuildType {
		t.Errorf("buildType = %q", got)
	}
	if got := st.Predicate.BuildDefinition.InternalParameters["targetPlatform"]; got != "linux/arm64" {
		t.Errorf("targetPlatform = %v", got)
	}
}

func TestPredicateJSONFieldNames(t *testing.T) {
	clearCIEnv(t)
	_, raw, _, _ := sign(t,
		"--builder-id", "https://example.com/builder",
		"--frontend", "bazel",
		"--source-uri", "https://user:secret@example.com/org/repo.git",
		"--source-commit", strings.Repeat("ab", 20),
		"--source-path", "BUILD.bazel",
		"--material", "pkg:docker/alpine@3.17?platform=linux%2Famd64@sha256:abcdef",
		"--parameter", "target=//app:image",
		"--reproducible",
	)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	pred := got["predicate"].(map[string]any)
	bd := pred["buildDefinition"].(map[string]any)
	ext := bd["externalParameters"].(map[string]any)
	cs := ext["configSource"].(map[string]any)
	if cs["uri"] != "https://xxxxx:xxxxx@example.com/org/repo.git" || cs["path"] != "BUILD.bazel" ||
		cs["digest"].(map[string]any)["sha1"] != strings.Repeat("ab", 20) {
		t.Errorf("configSource = %v", cs)
	}
	req := ext["request"].(map[string]any)
	if req["frontend"] != "bazel" || req["args"].(map[string]any)["target"] != "//app:image" {
		t.Errorf("request = %v", req)
	}
	deps := bd["resolvedDependencies"].([]any)
	d0 := deps[0].(map[string]any)
	if d0["uri"] != "pkg:docker/alpine@3.17?platform=linux%2Famd64" || d0["digest"].(map[string]any)["sha256"] != "abcdef" {
		t.Errorf("resolvedDependencies[0] = %v", d0)
	}
	if _, ok := bd["internalParameters"].(map[string]any)["builderPlatform"]; !ok {
		t.Errorf("internalParameters lacks builderPlatform: %v", bd["internalParameters"])
	}
	rd := pred["runDetails"].(map[string]any)
	if rd["builder"].(map[string]any)["id"] != "https://example.com/builder" {
		t.Errorf("builder = %v", rd["builder"])
	}
	md := rd["metadata"].(map[string]any)
	vcs := md["buildkit_metadata"].(map[string]any)["vcs"].(map[string]any)
	if vcs["source"] != "https://xxxxx:xxxxx@example.com/org/repo.git" || vcs["revision"] != strings.Repeat("ab", 20) {
		t.Errorf("vcs = %v", vcs)
	}
	comp := md["buildkit_completeness"].(map[string]any)
	if comp["request"] != false || comp["resolvedDependencies"] != false {
		t.Errorf("a post-hoc emitter must not claim completeness by default: %v", comp)
	}
	if md["buildkit_reproducible"] != true {
		t.Errorf("buildkit_reproducible = %v", md["buildkit_reproducible"])
	}
	if _, ok := md["buildkit_hermetic"]; ok {
		t.Error("buildkit_hermetic must be omitted: the plugin cannot observe hermeticity")
	}
	if strings.Contains(string(raw), "secret") {
		t.Error("credentials leaked into the statement")
	}
}

func TestDeterministicByDefault(t *testing.T) {
	clearCIEnv(t)
	args := []string{"--builder-id", "x", "--parameter", "b=2", "--parameter", "a=1"}
	_, raw1, _, img1 := sign(t, args...)
	_, raw2, _, img2 := sign(t, args...)
	if !bytes.Equal(raw1, raw2) {
		t.Error("statement differs between runs")
	}
	d1, _ := img1.Digest()
	d2, _ := img2.Digest()
	if d1 != d2 {
		t.Errorf("manifest digest differs: %s vs %s", d1, d2)
	}
	if strings.Contains(string(raw1), "startedOn") || strings.Contains(string(raw1), "finishedOn") {
		t.Errorf("timestamps recorded without being requested: %s", raw1)
	}
}

func TestTimestamps(t *testing.T) {
	clearCIEnv(t)

	t.Setenv("SOURCE_DATE_EPOCH", "1700000000")
	_, _, st, _ := sign(t)
	md := st.Predicate.RunDetails.Metadata
	if md.StartedOn != "2023-11-14T22:13:20Z" || md.FinishedOn != "2023-11-14T22:13:20Z" {
		t.Errorf("SOURCE_DATE_EPOCH not honored: %+v", md)
	}

	_, _, st, _ = sign(t, "--started-on", "2023-11-14T22:00:00Z")
	if got := st.Predicate.RunDetails.Metadata.StartedOn; got != "2023-11-14T22:00:00Z" {
		t.Errorf("startedOn = %q", got)
	}
	if _, err := newSigner([]string{"--started-on", "2023-11-14T23:00:00Z"}); err == nil {
		t.Error("--started-on after the finish time must fail")
	}

	t.Setenv("SOURCE_DATE_EPOCH", "")
	_, _, st, _ = sign(t, "--record-timestamps")
	md = st.Predicate.RunDetails.Metadata
	if md.FinishedOn == "" || md.StartedOn != md.FinishedOn {
		t.Errorf("--record-timestamps: %+v", md)
	}

	t.Setenv("SOURCE_DATE_EPOCH", "garbage")
	if _, err := newSigner(nil); err == nil {
		t.Error("invalid SOURCE_DATE_EPOCH must fail")
	}
}

func TestModeMinScrubsBuildArgsAndLabels(t *testing.T) {
	clearCIEnv(t)
	args := []string{"--parameter", "build-arg:SECRET=1", "--parameter", "label:foo=bar", "--parameter", "target=x"}

	_, _, st, _ := sign(t, args...)
	if got := st.Predicate.BuildDefinition.ExternalParameters.Request.Args; len(got) != 1 || got["target"] != "x" {
		t.Errorf("min mode args = %v", got)
	}

	_, _, st, _ = sign(t, append([]string{"--mode", "max"}, args...)...)
	if got := st.Predicate.BuildDefinition.ExternalParameters.Request.Args; len(got) != 3 {
		t.Errorf("max mode args = %v", got)
	}
}

func TestBuildInfoAndCompleteness(t *testing.T) {
	clearCIEnv(t)
	path := filepath.Join(t.TempDir(), "info.json")
	info := `{"builder_id":"https://b","frontend":"bazel","materials":[{"uri":"pkg:docker/busybox@latest","digest":{"sha256":"aa"}}],
	"parameters":{"k":"v"},"internal_parameters":{"custom":"1"},"completeness":{"request":true,"resolvedDependencies":true}}`
	if err := os.WriteFile(path, []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, st, _ := sign(t, "--build-info", path, "--mode", "max", "--builder-id", "https://flag", "--material", "https://example.com/f.tar")
	pred := st.Predicate
	if pred.RunDetails.Builder.ID != "https://flag" {
		t.Errorf("flag must override build info: %q", pred.RunDetails.Builder.ID)
	}
	if len(pred.BuildDefinition.ResolvedDependencies) != 2 {
		t.Errorf("materials should append: %+v", pred.BuildDefinition.ResolvedDependencies)
	}
	if pred.BuildDefinition.InternalParameters["custom"] != "1" {
		t.Errorf("internal parameters = %v", pred.BuildDefinition.InternalParameters)
	}
	if c := pred.RunDetails.Metadata.Completeness; !c.Request || !c.ResolvedDependencies {
		t.Errorf("max mode must keep asserted completeness: %+v", c)
	}

	_, _, st, _ = sign(t, "--build-info", path) // min
	if c := st.Predicate.RunDetails.Metadata.Completeness; c.Request || !c.ResolvedDependencies {
		t.Errorf("min mode scrubs args and so must not claim a complete request: %+v", c)
	}

	collide := filepath.Join(t.TempDir(), "collide.json")
	if err := os.WriteFile(collide, []byte(`{"internal_parameters":{"builderPlatform":"x"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newSigner([]string{"--build-info", collide}); err == nil {
		t.Error("custom internal parameter colliding with a built-in key must fail")
	}
}

func TestGitHubActionsContext(t *testing.T) {
	clearCIEnv(t)
	event := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(event, []byte(`{"ref":"refs/heads/main"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "push", "GITHUB_EVENT_PATH": event,
		"GITHUB_REPOSITORY": "org/repo", "GITHUB_REPOSITORY_ID": "1", "GITHUB_REPOSITORY_OWNER": "org", "GITHUB_REPOSITORY_OWNER_ID": "2",
		"GITHUB_JOB": "j", "GITHUB_RUN_ATTEMPT": "3", "GITHUB_RUN_ID": "42", "GITHUB_RUN_NUMBER": "7",
		"GITHUB_SERVER_URL": "https://github.com", "GITHUB_WORKFLOW": "ci", "GITHUB_WORKFLOW_REF": "org/repo/.github/workflows/ci.yml@refs/heads/main", "GITHUB_WORKFLOW_SHA": "abc",
		"GITHUB_ACTOR": "me", // incomplete group: must be dropped, like buildx
	} {
		t.Setenv(k, v)
	}

	_, _, st, _ := sign(t)
	ip := st.Predicate.BuildDefinition.InternalParameters
	if ip["github_event_name"] != "push" || ip["github_repository"] != "org/repo" || ip["github_run_id"] != "42" {
		t.Errorf("internalParameters = %v", ip)
	}
	if _, ok := ip["github_actor"]; ok {
		t.Error("partial group github_actor must not be emitted")
	}
	if _, ok := ip["github_event_payload"]; !ok {
		t.Error("event payload missing")
	}
	if got, want := st.Predicate.RunDetails.Metadata.InvocationID, "https://github.com/org/repo/actions/runs/42/attempts/3"; got != want {
		t.Errorf("invocationId = %q, want %q", got, want)
	}

	_, _, st, _ = sign(t, "--github-event-payload=false")
	if _, ok := st.Predicate.BuildDefinition.InternalParameters["github_event_payload"]; ok {
		t.Error("payload must be omitted with --github-event-payload=false")
	}
	_, _, st, _ = sign(t, "--ci", "none")
	if _, ok := st.Predicate.BuildDefinition.InternalParameters["github_event_name"]; ok {
		t.Error("--ci=none must not record the CI context")
	}
	_, _, st, _ = sign(t, "--invocation-id", "mine")
	if st.Predicate.RunDetails.Metadata.InvocationID != "mine" {
		t.Error("--invocation-id must win over the derived value")
	}
}

// Mirrors BuildKit util/purl/image_test.go.
func TestRefToPURL(t *testing.T) {
	const dgst = "sha256:9a7b7c3e2d6b8c1f2e1d0c9b8a7f6e5d4c3b2a190807f6e5d4c3b2a1908f7e6d"
	amd64 := &v1.Platform{OS: "linux", Architecture: "amd64"}
	for _, tc := range []struct {
		ref      string
		platform *v1.Platform
		want     string
	}{
		{"alpine", nil, "pkg:docker/alpine@latest"},
		{"alpine:3.15", nil, "pkg:docker/alpine@3.15"},
		{"docker.io/library/alpine", nil, "pkg:docker/alpine@latest"},
		{"alpine:latest@" + dgst, nil, "pkg:docker/alpine@latest?digest=" + dgst},
		{"alpine@" + dgst, nil, "pkg:docker/alpine?digest=" + dgst},
		{"user/test:v2", nil, "pkg:docker/user/test@v2"},
		{"ghcr.io/foo/bar:v2", nil, "pkg:docker/ghcr.io/foo/bar@v2"},
		{"ghcr.io/foo/bar", nil, "pkg:docker/ghcr.io/foo/bar@latest"},
		{"localhost:5000/foo/bar", nil, "pkg:docker/localhost:5000/foo/bar@latest"},
		{"busybox", amd64, "pkg:docker/busybox@latest?platform=linux%2Famd64"},
		{"busybox@" + dgst, amd64, "pkg:docker/busybox?digest=" + dgst + "&platform=linux%2Famd64"},
		{"busybox", &v1.Platform{OS: "linux", Architecture: "arm", Variant: "v7"}, "pkg:docker/busybox@latest?platform=linux%2Farm%2Fv7"},
		// BuildKit normalizes: the image index says arm64/v8, the purl says arm64.
		{"busybox", &v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}, "pkg:docker/busybox@latest?platform=linux%2Farm64"},
	} {
		got, err := refToPURL(tc.ref, tc.platform)
		if err != nil {
			t.Errorf("refToPURL(%q): %v", tc.ref, err)
			continue
		}
		if got != tc.want {
			t.Errorf("refToPURL(%q) = %q, want %q", tc.ref, got, tc.want)
		}
	}
	for _, bad := range []string{"", "UPPER/case", "alpine@notadigest"} {
		if _, err := refToPURL(bad, nil); err == nil {
			t.Errorf("refToPURL(%q) should fail", bad)
		}
	}
}

func TestParseMaterial(t *testing.T) {
	for _, tc := range []struct {
		in     string
		uri    string
		digest map[string]string
	}{
		{"https://example.com/a.tar", "https://example.com/a.tar", nil},
		{"https://example.com/a.tar@sha256:ab12", "https://example.com/a.tar", map[string]string{"sha256": "ab12"}},
		{"pkg:docker/alpine@3.17", "pkg:docker/alpine@3.17", nil}, // '@' of a purl is not a digest
		{"pkg:docker/alpine@3.17@sha256:ab12", "pkg:docker/alpine@3.17", map[string]string{"sha256": "ab12"}},
	} {
		got, err := parseMaterial(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if got.URI != tc.uri || len(got.Digest) != len(tc.digest) || (tc.digest != nil && got.Digest["sha256"] != tc.digest["sha256"]) {
			t.Errorf("parseMaterial(%q) = %+v", tc.in, got)
		}
	}
}

func TestFlagValidation(t *testing.T) {
	clearCIEnv(t)
	for _, args := range [][]string{
		{"--mode", "full"},
		{"--ci", "maybe"},
		{"--parameter", "novalue"},
		{"--source-commit", "XYZ"},
		{"--source-commit", "abcd"},
		{"--build-info", "/nonexistent/info.json"},
		{"extra-positional"},
	} {
		if _, err := newSigner(args); err == nil {
			t.Errorf("newSigner(%v) should fail", args)
		}
	}
}

// The host reads the plugin's stdout as an OCI layout (img_tool/pkg/signer
// ReadArtifactLayout): verify the same invariants here, since this module
// cannot depend on rules_img.
func TestOCILayoutRoundTrip(t *testing.T) {
	clearCIEnv(t)
	_, _, _, img := sign(t)
	var buf bytes.Buffer
	if err := plugin.WriteArtifact(&buf, []v1.Image{img}); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		data, _ := io.ReadAll(tr)
		files[h.Name] = data
	}
	if _, ok := files["oci-layout"]; !ok {
		t.Fatal("missing oci-layout")
	}
	var idx v1.IndexManifest
	if err := json.Unmarshal(files["index.json"], &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Manifests) != 1 || idx.Manifests[0].ArtifactType != attestationArtifactType {
		t.Fatalf("index.json = %+v", idx)
	}
	for name, data := range files {
		if !strings.HasPrefix(name, "blobs/sha256/") {
			continue
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != strings.TrimPrefix(name, "blobs/sha256/") {
			t.Errorf("blob %s does not match its digest", name)
		}
	}
	want, _ := img.Digest()
	if _, ok := files["blobs/sha256/"+want.Hex]; !ok {
		t.Errorf("manifest blob %s missing", want)
	}
}

func TestRunProtocol(t *testing.T) {
	clearCIEnv(t)
	s, err := newSigner([]string{"--subject-name", "alpine"})
	if err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(testSubject())
	var out bytes.Buffer
	if err := plugin.Run(context.Background(), s, bytes.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 {
		t.Fatal("no layout written to stdout")
	}
	if err := plugin.Run(context.Background(), s, strings.NewReader(`{}`), &out); err == nil {
		t.Error("a subject without a digest must be rejected")
	}
}

// --- request protocol ---

func testRequest() signerapi.Request {
	return signerapi.Request{
		ProtocolVersion: signerapi.ProtocolRequest,
		Subject:         testSubject(),
		Role:            signerapi.RoleChildManifest,
		Destination: signerapi.Destination{
			Registry: "ghcr.io", Repository: "example/app", Tags: []string{"latest", "v1"},
			References: []string{"ghcr.io/example/app:latest", "ghcr.io/example/app:v1"},
		},
		Deploy: signerapi.Deploy{
			RulesImgVersion: "0.3.25",
			InvocationID:    "11111111-2222-4333-8444-555555555555",
			StartedAt:       "2023-11-14T22:00:00Z",
			PushStrategy:    "eager",
		},
	}
}

// signRequest runs the plugin end to end through plugin.Run with a request on
// stdin and returns the statement.
func signRequest(t *testing.T, req any, args ...string) (statement, error) {
	t.Helper()
	s, err := newSigner(args)
	if err != nil {
		t.Fatalf("newSigner(%v): %v", args, err)
	}
	in, _ := json.Marshal(req)
	var out bytes.Buffer
	if err := plugin.Run(context.Background(), s, bytes.NewReader(in), &out); err != nil {
		return statement{}, err
	}
	return statementFromLayout(t, out.Bytes()), nil
}

// layerFromLayout returns the bytes of the attestation layer of an OCI layout tar.
func layerFromLayout(t *testing.T, layout []byte) []byte {
	t.Helper()
	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(layout))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeDir {
			files[h.Name], _ = io.ReadAll(tr)
		}
	}
	var idx v1.IndexManifest
	if err := json.Unmarshal(files["index.json"], &idx); err != nil {
		t.Fatal(err)
	}
	var m v1.Manifest
	if err := json.Unmarshal(files["blobs/sha256/"+idx.Manifests[0].Digest.Hex], &m); err != nil {
		t.Fatal(err)
	}
	return files["blobs/sha256/"+m.Layers[0].Digest.Hex]
}

// statementFromLayout extracts the (BuildKit layout) statement from an OCI layout tar.
func statementFromLayout(t *testing.T, layout []byte) statement {
	t.Helper()
	var st statement
	if err := json.Unmarshal(layerFromLayout(t, layout), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestRequestProtocolRecordsDestinationPlatformAndVersion(t *testing.T) {
	clearCIEnv(t)
	st, err := signRequest(t, testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Subject) != 2 {
		t.Fatalf("one subject name per reference, got %+v", st.Subject)
	}
	for i, want := range []string{
		"pkg:docker/ghcr.io/example/app@latest?platform=linux%2Farm64",
		"pkg:docker/ghcr.io/example/app@v1?platform=linux%2Farm64",
	} {
		if st.Subject[i].Name != want || st.Subject[i].Digest["sha256"] != strings.TrimPrefix(testDigest, "sha256:") {
			t.Errorf("subject[%d] = %+v, want name %q", i, st.Subject[i], want)
		}
	}
	if got := st.Predicate.BuildDefinition.InternalParameters["targetPlatform"]; got != "linux/arm64" {
		t.Errorf("targetPlatform = %v", got)
	}
	rd := st.Predicate.RunDetails
	if rd.Builder.Version["rules_img"] != "0.3.25" {
		t.Errorf("builder.version = %v", rd.Builder.Version)
	}
	if rd.Metadata.InvocationID != "11111111-2222-4333-8444-555555555555" {
		t.Errorf("invocationId = %q", rd.Metadata.InvocationID)
	}
	if rd.Metadata.StartedOn != "" || rd.Metadata.FinishedOn != "" {
		t.Errorf("timestamps must stay off by default, got %+v", rd.Metadata)
	}
}

func TestRequestProtocolSubjectNameFlagOverridesDestination(t *testing.T) {
	clearCIEnv(t)
	st, err := signRequest(t, testRequest(), "--subject-name", "docker.io/library/alpine:3")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Subject) != 1 || st.Subject[0].Name != "pkg:docker/alpine@3?platform=linux%2Farm64" {
		t.Errorf("subject = %+v", st.Subject)
	}
}

func TestRequestProtocolDeployStartAndInvocation(t *testing.T) {
	clearCIEnv(t)
	t.Setenv("SOURCE_DATE_EPOCH", "1700000100") // 2023-11-14T22:15:00Z
	st, err := signRequest(t, testRequest())
	if err != nil {
		t.Fatal(err)
	}
	md := st.Predicate.RunDetails.Metadata
	if md.StartedOn != "2023-11-14T22:00:00Z" || md.FinishedOn != "2023-11-14T22:15:00Z" {
		t.Errorf("startedOn comes from the deploy: %+v", md)
	}

	st, err = signRequest(t, testRequest(), "--started-on", "2023-11-14T21:00:00Z", "--invocation-id", "mine")
	if err != nil {
		t.Fatal(err)
	}
	md = st.Predicate.RunDetails.Metadata
	if md.StartedOn != "2023-11-14T21:00:00Z" || md.InvocationID != "mine" {
		t.Errorf("explicit values must win over the deploy's: %+v", md)
	}

	// A deploy start after the finish time (clock skew) must not produce a
	// negative duration.
	req := testRequest()
	req.Deploy.StartedAt = "2023-11-15T00:00:00Z"
	st, err = signRequest(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Predicate.RunDetails.Metadata.StartedOn; got != "2023-11-14T22:15:00Z" {
		t.Errorf("startedOn = %q, want it clamped to finishedOn", got)
	}
}

func TestRunRejectsUnknownProtocolVersionAndEmptySubject(t *testing.T) {
	clearCIEnv(t)
	req := testRequest()
	req.ProtocolVersion = 99
	if _, err := signRequest(t, req); err == nil || !strings.Contains(err.Error(), "unsupported request protocol version 99") {
		t.Errorf("err = %v", err)
	}
	req = testRequest()
	req.Subject = v1.Descriptor{}
	if _, err := signRequest(t, req); err == nil {
		t.Error("a request without a subject digest must be rejected")
	}
}

// A plugin that does not understand requests must keep working unchanged when
// the host starts sending them, so the original protocol stays supported.
func TestLegacyDescriptorStillWorks(t *testing.T) {
	clearCIEnv(t)
	st, err := signRequest(t, testSubject(), "--subject-name", "ghcr.io/o/r:t")
	if err != nil {
		t.Fatal(err)
	}
	if st.Subject[0].Name != "pkg:docker/ghcr.io/o/r@t?platform=linux%2Farm64" {
		t.Errorf("subject = %+v", st.Subject)
	}
	if st.Predicate.RunDetails.Builder.Version != nil {
		t.Error("no request means no rules_img version")
	}
}

// --- workspace status ---

// TestHelperStatusProcess is not a real test: with GO_WANT_STATUS=1 it acts as a
// workspace status command.
func TestHelperStatusProcess(t *testing.T) {
	switch os.Getenv("GO_WANT_STATUS") {
	case "":
		return
	case "fail":
		fmt.Fprintln(os.Stderr, "boom")
		os.Exit(3)
	}
	fmt.Println("BUILD_SCM_REVISION " + strings.Repeat("cd", 20))
	fmt.Println("BUILD_SCM_URL https://token:secret@example.com/org/repo.git")
	fmt.Println("BUILD_SCM_PATH app/BUILD.bazel")
	fmt.Println("BUILD_SCM_STATUS modified")
	fmt.Println()
	fmt.Println("STABLE_TEAM platform team") // value with a space
	fmt.Println("STABLE_SECRET hunter2")
	fmt.Println("STABLE_EMPTY")
	os.Exit(0)
}

func statusArgs(extra ...string) []string {
	return append([]string{"--status-command", os.Args[0], "--status-arg=-test.run=TestHelperStatusProcess"}, extra...)
}

func TestParseStatus(t *testing.T) {
	got, err := parseStatus(strings.NewReader("A 1\n\nB two words\r\nC\nA 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := workspaceStatus{"A": "3", "B": "two words", "C": ""}
	if len(got) != len(want) || got["A"] != "3" || got["B"] != "two words" || got["C"] != "" {
		t.Errorf("parseStatus = %v, want %v", got, want)
	}
	if _, err := parseStatus(strings.NewReader(" no-key\n")); err == nil {
		t.Error("a line without a key must be rejected")
	}
}

func TestStatusCommandDerivesSourceInformation(t *testing.T) {
	clearCIEnv(t)
	t.Setenv("GO_WANT_STATUS", "1")
	_, raw, st, _ := sign(t, statusArgs("--status-key", "STABLE_TEAM", "--status-key", "STABLE_EMPTY", "--status-key", "STABLE_MISSING")...)

	cs := st.Predicate.BuildDefinition.ExternalParameters.ConfigSource
	if cs.URI != "https://xxxxx:xxxxx@example.com/org/repo.git" || cs.Path != "app/BUILD.bazel" || cs.Digest["sha1"] != strings.Repeat("cd", 20) {
		t.Errorf("configSource = %+v", cs)
	}
	vcs := st.Predicate.RunDetails.Metadata.BuildKitMetadata.VCS
	if vcs["revision"] != strings.Repeat("cd", 20)+"-dirty" || vcs["source"] != cs.URI {
		t.Errorf("vcs = %v (a modified tree marks the revision -dirty, but not configSource.digest)", vcs)
	}
	ws, _ := st.Predicate.BuildDefinition.InternalParameters["workspace_status"].(map[string]any)
	if len(ws) != 2 || ws["STABLE_TEAM"] != "platform team" || ws["STABLE_EMPTY"] != "" {
		t.Errorf("workspace_status = %v", ws)
	}
	if strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "secret@") {
		t.Error("keys that were not allowlisted, and credentials, must not be recorded")
	}
}

func TestStatusCommandPrecedence(t *testing.T) {
	clearCIEnv(t)
	t.Setenv("GO_WANT_STATUS", "1")
	info := filepath.Join(t.TempDir(), "info.json")
	if err := os.WriteFile(info, []byte(`{"source":{"uri":"https://info.example/repo","commit":"`+strings.Repeat("ee", 20)+`","path":"info.bazel"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// status > build info
	_, _, st, _ := sign(t, statusArgs("--build-info", info)...)
	if got := st.Predicate.BuildDefinition.ExternalParameters.ConfigSource; got.Path != "app/BUILD.bazel" || got.Digest["sha1"] != strings.Repeat("cd", 20) {
		t.Errorf("the status command must win over build info: %+v", got)
	}

	// flag > status; and a flag-pinned revision is not qualified by the status
	_, _, st, _ = sign(t, statusArgs("--source-commit", strings.Repeat("ab", 20), "--source-uri", "https://flag.example/repo")...)
	cs := st.Predicate.BuildDefinition.ExternalParameters.ConfigSource
	if cs.URI != "https://flag.example/repo" || cs.Digest["sha1"] != strings.Repeat("ab", 20) {
		t.Errorf("flags must win over the status command: %+v", cs)
	}
	if rev := st.Predicate.RunDetails.Metadata.BuildKitMetadata.VCS["revision"]; rev != strings.Repeat("ab", 20) {
		t.Errorf("revision = %q, must not be marked dirty by a status that did not supply it", rev)
	}

	// build info alone is not marked dirty either
	_, _, st, _ = sign(t, "--build-info", info)
	if rev := st.Predicate.RunDetails.Metadata.BuildKitMetadata.VCS["revision"]; rev != strings.Repeat("ee", 20) {
		t.Errorf("revision = %q", rev)
	}
}

func TestStatusCommandFromEnvAndCleanTree(t *testing.T) {
	clearCIEnv(t)
	t.Setenv("GO_WANT_STATUS", "1")
	t.Setenv("RULES_IMG_PROVENANCE_STATUS_COMMAND", os.Args[0])
	_, _, st, _ := sign(t, "--status-arg=-test.run=TestHelperStatusProcess")
	if st.Predicate.BuildDefinition.ExternalParameters.ConfigSource.Path != "app/BUILD.bazel" {
		t.Error("$RULES_IMG_PROVENANCE_STATUS_COMMAND not honored")
	}
	if workspaceStatus(map[string]string{"BUILD_SCM_STATUS": "clean"}).dirty() || workspaceStatus(map[string]string{"BUILD_SCM_STATUS": "CLEAN"}).dirty() || workspaceStatus(nil).dirty() {
		t.Error("clean, CLEAN and absent are not dirty")
	}
}

func TestStatusCommandFailureAndMisuse(t *testing.T) {
	clearCIEnv(t)
	t.Setenv("GO_WANT_STATUS", "fail")
	if _, err := newSigner(statusArgs()); err == nil || !strings.Contains(err.Error(), "status command") {
		t.Errorf("a failing status command must fail signing, got %v", err)
	}
	if _, err := newSigner([]string{"--status-command", "/nonexistent/status-script"}); err == nil {
		t.Error("a missing status command must fail signing")
	}
	if _, err := newSigner([]string{"--status-key", "A"}); err == nil {
		t.Error("--status-key without --status-command is a mistake")
	}
}

func TestNormalizePlatform(t *testing.T) {
	for _, tc := range []struct{ in, want v1.Platform }{
		{v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}, v1.Platform{OS: "linux", Architecture: "arm64"}},
		{v1.Platform{OS: "linux", Architecture: "aarch64"}, v1.Platform{OS: "linux", Architecture: "arm64"}},
		{v1.Platform{OS: "linux", Architecture: "amd64", Variant: "v1"}, v1.Platform{OS: "linux", Architecture: "amd64"}},
		{v1.Platform{OS: "linux", Architecture: "amd64", Variant: "v3"}, v1.Platform{OS: "linux", Architecture: "amd64", Variant: "v3"}},
		{v1.Platform{OS: "linux", Architecture: "x86_64"}, v1.Platform{OS: "linux", Architecture: "amd64"}},
		{v1.Platform{OS: "linux", Architecture: "arm"}, v1.Platform{OS: "linux", Architecture: "arm", Variant: "v7"}},
		{v1.Platform{OS: "linux", Architecture: "armel"}, v1.Platform{OS: "linux", Architecture: "arm", Variant: "v6"}},
		{v1.Platform{OS: "macos", Architecture: "arm64"}, v1.Platform{OS: "darwin", Architecture: "arm64"}},
	} {
		if got := normalizePlatform(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("normalizePlatform(%+v) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

// The platform an image index declares is what the host reads from the image
// config, so a real arm64 image arrives as arm64/v8 and must be recorded as arm64.
func TestRequestProtocolNormalizesPlatform(t *testing.T) {
	clearCIEnv(t)
	req := testRequest()
	req.Subject.Platform = &v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
	st, err := signRequest(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Predicate.BuildDefinition.InternalParameters["targetPlatform"]; got != "linux/arm64" {
		t.Errorf("targetPlatform = %v, want linux/arm64", got)
	}
	if got := st.Subject[0].Name; !strings.HasSuffix(got, "?platform=linux%2Farm64") {
		t.Errorf("subject name = %q", got)
	}
}

func TestArtifactTypeDefaultsToBuildKits(t *testing.T) {
	clearCIEnv(t)
	m, _, _, _ := sign(t)
	if m.ArtifactType != "application/vnd.docker.attestation.manifest.v1+json" {
		t.Errorf("default artifactType = %q", m.ArtifactType)
	}
}

func TestArtifactTypeIsConfigurable(t *testing.T) {
	clearCIEnv(t)
	const custom = "application/vnd.example.provenance.v1+json"
	m, _, st, img := sign(t, "--artifact-type", custom)
	if m.ArtifactType != custom {
		t.Errorf("artifactType = %q, want %q", m.ArtifactType, custom)
	}
	// Only the artifactType changes: the layer and the statement are the same.
	if string(m.Layers[0].MediaType) != "application/vnd.in-toto+json" || st.PredicateType != "https://slsa.dev/provenance/v1" {
		t.Errorf("a custom artifactType must not change the content: %+v / %q", m.Layers[0], st.PredicateType)
	}

	// The OCI layout the host reads advertises it too, so referrers can be
	// filtered by it.
	var buf bytes.Buffer
	if err := plugin.WriteArtifact(&buf, []v1.Image{img}); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			t.Fatal("no index.json in the layout")
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name != "index.json" {
			continue
		}
		var idx v1.IndexManifest
		data, _ := io.ReadAll(tr)
		if err := json.Unmarshal(data, &idx); err != nil {
			t.Fatal(err)
		}
		if idx.Manifests[0].ArtifactType != custom {
			t.Errorf("index.json artifactType = %q", idx.Manifests[0].ArtifactType)
		}
		break
	}
}

func TestArtifactTypeIsValidated(t *testing.T) {
	clearCIEnv(t)
	for _, bad := range []string{"", "nomediatype", "/subtype", "type/", "a/b/c", "has space/x"} {
		if _, err := newSigner([]string{"--artifact-type", bad}); err == nil {
			t.Errorf("--artifact-type %q should be rejected", bad)
		}
	}
	for _, good := range []string{"application/vnd.example+json", "application/x-foo"} {
		if _, err := newSigner([]string{"--artifact-type", good}); err != nil {
			t.Errorf("--artifact-type %q: %v", good, err)
		}
	}
}

// The invocation ID: what the caller says, else the Bazel invocation, else the CI
// run, else what img deploy generated. The first three are the same for every call
// of one deploy, so the attestations of one deploy still agree.
func TestInvocationIDPrecedence(t *testing.T) {
	const (
		hostID   = "11111111-2222-4333-8444-555555555555"
		bazelID  = "678b36de-a1fd-41eb-a7b8-2466660ca18c"
		ghURL    = "https://github.com/bazel-contrib/rules_img/actions/runs/18715517508/attempts/1"
		explicit = "my-own-id"
	)
	invocation := func(args ...string) string {
		t.Helper()
		st, err := signRequest(t, testRequest(), append([]string{"--github-event-payload=false"}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		return st.Predicate.RunDetails.Metadata.InvocationID
	}

	// The deploy's own ID is the last resort.
	if got := invocation(); got != hostID {
		t.Errorf("no other source: %q, want the deploy's %q", got, hostID)
	}

	// The CI run beats it: the run is what a person can look up.
	setGitHubEnv(t)
	if got := invocation(); got != ghURL {
		t.Errorf("GitHub Actions: %q, want the run URL", got)
	}

	// The Bazel invocation, which bazel run exports as $BUILD_ID, beats the CI run.
	t.Setenv("BUILD_ID", bazelID)
	if got := invocation(); got != bazelID {
		t.Errorf("$BUILD_ID: %q, want %q", got, bazelID)
	}

	// An explicit ID beats all of them.
	if got := invocation("--invocation-id", explicit); got != explicit {
		t.Errorf("--invocation-id: %q", got)
	}
	t.Setenv("RULES_IMG_PROVENANCE_INVOCATION_ID", "from-env")
	if got := invocation(); got != "from-env" {
		t.Errorf("$RULES_IMG_PROVENANCE_INVOCATION_ID: %q", got)
	}
}

// Without a request (the original protocol) $BUILD_ID still applies.
func TestBuildIDWithoutRequest(t *testing.T) {
	clearCIEnv(t)
	t.Setenv("BUILD_ID", "678b36de-a1fd-41eb-a7b8-2466660ca18c")
	_, _, st, _ := sign(t)
	if got := st.Predicate.RunDetails.Metadata.InvocationID; got != "678b36de-a1fd-41eb-a7b8-2466660ca18c" {
		t.Errorf("invocationId = %q", got)
	}
}

// The Bazel definitions use the same ID.
func TestBazelDefinitionsUseBuildID(t *testing.T) {
	clearCIEnv(t)
	t.Setenv("BUILD_ID", "678b36de-a1fd-41eb-a7b8-2466660ca18c")
	st := signBazel(t, testRequest())
	if got := at(t, predicateOf(t, st), "runDetails", "metadata", "invocationId"); got != "678b36de-a1fd-41eb-a7b8-2466660ca18c" {
		t.Errorf("invocationId = %v", got)
	}
}
