package deploy

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	registrytypes "github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/signer"
)

// TestMain turns the test binary into a signer plugin when GO_WANT_CHAIN_PLUGIN
// is set, so the chain tests run real subprocesses through forEachSignature.
func TestMain(m *testing.M) {
	if os.Getenv("GO_WANT_CHAIN_PLUGIN") == "1" {
		os.Exit(runChainPlugin())
	}
	os.Exit(m.Run())
}

// runChainPlugin creates an artifact named after $CHAIN_NAME for whatever it is
// asked to attach to, recording what it was told in the manifest annotations.
func runChainPlugin() int {
	if os.Getenv("CHAIN_FAIL") == "1" {
		fmt.Fprintln(os.Stderr, "chain plugin: forced failure")
		return 1
	}
	raw, _ := io.ReadAll(os.Stdin)
	var req signer.Request
	_ = json.Unmarshal(raw, &req)
	annotations := map[string]string{"name": os.Getenv("CHAIN_NAME")}
	var subject registryv1.Descriptor
	if req.ProtocolVersion == signer.ProtocolRequest {
		subject = req.Subject
		annotations["protocol"] = "2"
		annotations["role"] = req.Role
		annotations["subject_artifact_type"] = req.Subject.ArtifactType
		annotations["tags"] = strings.Join(req.Destination.Tags, ",")
		if req.Parent != nil {
			annotations["parent"] = req.Parent.Digest.String()
		}
	} else if err := json.Unmarshal(raw, &subject); err != nil {
		return 2
	}

	h := func(b []byte) registryv1.Hash {
		sum := sha256.Sum256(b)
		return registryv1.Hash{Algorithm: "sha256", Hex: hex.EncodeToString(sum[:])}
	}
	config, layer := []byte("{}"), []byte(os.Getenv("CHAIN_NAME"))
	manifest := registryv1.Manifest{
		SchemaVersion: 2,
		MediaType:     registrytypes.OCIManifestSchema1,
		ArtifactType:  "application/vnd.test." + os.Getenv("CHAIN_NAME"),
		Config:        registryv1.Descriptor{MediaType: "application/vnd.oci.empty.v1+json", Digest: h(config), Size: int64(len(config))},
		Layers:        []registryv1.Descriptor{{MediaType: "application/octet-stream", Digest: h(layer), Size: int64(len(layer))}},
		Subject:       &subject,
		Annotations:   annotations,
	}
	rawManifest, _ := json.Marshal(manifest)
	mh := h(rawManifest)
	index, _ := json.Marshal(registryv1.IndexManifest{SchemaVersion: 2, MediaType: registrytypes.OCIImageIndex, Manifests: []registryv1.Descriptor{{
		MediaType: registrytypes.OCIManifestSchema1, Digest: mh, Size: int64(len(rawManifest)), ArtifactType: manifest.ArtifactType,
	}}})
	tw := tar.NewWriter(os.Stdout)
	for _, f := range []struct {
		name string
		data []byte
	}{
		{"oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)},
		{"index.json", index},
		{"blobs/sha256/" + mh.Hex, rawManifest},
		{"blobs/sha256/" + manifest.Config.Digest.Hex, config},
		{"blobs/sha256/" + manifest.Layers[0].Digest.Hex, layer},
	} {
		_ = tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.data))})
		_, _ = tw.Write(f.data)
	}
	_ = tw.Close()
	return 0
}

// chainStep describes one plugin of a test chain.
type chainStep struct {
	name     string
	targets  []string
	protocol int
	fail     bool
}

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// chainFixture writes the plugins' sign_setting files and returns a store that
// finds them plus the steps referencing them.
func chainFixture(t *testing.T, steps []chainStep) (*signer.SettingStore, []api.SignStep) {
	t.Helper()
	dir := t.TempDir()
	var files []string
	var out []api.SignStep
	for _, s := range steps {
		cfg := signer.SignSettingConfig{
			SchemaVersion: 1, Mode: "command", Tool: os.Args[0], Protocol: s.protocol,
			Env: map[string]string{"GO_WANT_CHAIN_PLUGIN": "1", "CHAIN_NAME": s.name},
		}
		if s.fail {
			cfg.Env["CHAIN_FAIL"] = "1"
		}
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, s.name+".json")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, path)
		out = append(out, api.SignStep{
			Setting: &api.Descriptor{MediaType: api.SignSettingMediaType, Digest: digestOf(string(data)), Size: int64(len(data))},
			Targets: s.targets,
		})
	}
	store, err := signer.Discover(nil, files, "")
	if err != nil {
		t.Fatal(err)
	}
	return store, out
}

func chainOp(i int, root string, tags []string, steps []api.SignStep, bestEffort bool) api.IndexedPushDeployOperation {
	op := api.IndexedPushDeployOperation{I: i}
	op.RootKind = "manifest"
	op.Root = api.Descriptor{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: digestOf(root), Size: 10}
	op.Registry, op.Repository, op.Tags = "r.io", "app", tags
	op.Sign = &api.SignConfig{Steps: steps, BestEffort: bestEffort}
	return op
}

// call is what one plugin invocation did, as reported through emit.
type call struct {
	plugin     string
	subject    string // digest the artifact was attached to
	role       string
	parent     string
	subjectTyp string
	tags       string
	artifact   string // digest of the artifact the plugin created
}

func runChain(t *testing.T, store *signer.SettingStore, ops []api.IndexedPushDeployOperation) ([]call, error) {
	t.Helper()
	var calls []call
	emit := func(_ context.Context, _ api.IndexedPushDeployOperation, subject api.Descriptor, imgs []registryv1.Image) error {
		for _, img := range imgs {
			m, err := img.Manifest()
			if err != nil {
				return err
			}
			d, _ := img.Digest()
			calls = append(calls, call{
				plugin: m.Annotations["name"], subject: subject.Digest, role: m.Annotations["role"],
				parent: m.Annotations["parent"], subjectTyp: m.Annotations["subject_artifact_type"],
				tags: m.Annotations["tags"], artifact: d.String(),
			})
		}
		return nil
	}
	err := forEachSignature(context.Background(), ops, api.DeploySettings{PushStrategy: "eager"}, store, nil, signOptions{}, nil, emit)
	return calls, err
}

func names(calls []call) string {
	var parts []string
	for _, c := range calls {
		parts = append(parts, c.plugin)
	}
	return strings.Join(parts, " ")
}

func findCall(t *testing.T, calls []call, plugin, subject string) call {
	t.Helper()
	for _, c := range calls {
		if c.plugin == plugin && c.subject == subject {
			return c
		}
	}
	t.Fatalf("no call of %s on %s in %+v", plugin, subject, calls)
	return call{}
}

func TestChainStepsOnlySeeEarlierArtifacts(t *testing.T) {
	for _, protocol := range []int{signer.ProtocolDescriptor, signer.ProtocolRequest} {
		t.Run(fmt.Sprintf("protocol%d", protocol), func(t *testing.T) {
			store, steps := chainFixture(t, []chainStep{
				{name: "A", protocol: protocol},                                                     // roots
				{name: "B", protocol: protocol, targets: []string{"plugin_artifacts"}},              // roots + A's artifacts
				{name: "C", protocol: protocol},                                                     // roots only: sees nothing created
				{name: "D", protocol: protocol, targets: []string{"plugin_artifacts", "referrers"}}, // sees A, B, C
			})
			root := digestOf("root")
			calls, err := runChain(t, store, []api.IndexedPushDeployOperation{chainOp(0, "root", []string{"latest"}, steps, false)})
			if err != nil {
				t.Fatal(err)
			}

			// A: root. B: root, then A's artifact. C: root. D: root, then A's, B's (both of
			// B's, since B also attached to A's), C's artifacts. Nothing sees its own output.
			if got, want := names(calls), "A B B C D D D D D"; got != want {
				t.Fatalf("calls = %q, want %q\n%+v", got, want, calls)
			}
			aRoot := findCall(t, calls, "A", root)
			bOnRoot := findCall(t, calls, "B", root)
			bOnA := findCall(t, calls, "B", aRoot.artifact)
			cRoot := findCall(t, calls, "C", root)
			for _, c := range calls {
				if c.plugin == "A" && c.subject != root || c.plugin == "C" && c.subject != root {
					t.Errorf("%s has no plugin_artifacts target and must only see the root, got %+v", c.plugin, c)
				}
			}
			// D sees everything created by A, B and C before it, and nothing of its own.
			for _, artifact := range []string{aRoot.artifact, bOnRoot.artifact, bOnA.artifact, cRoot.artifact} {
				findCall(t, calls, "D", artifact)
			}
			findCall(t, calls, "D", root)
			for _, c := range calls {
				if c.plugin != "D" {
					for _, d := range calls {
						if d.plugin == "D" && d.artifact == c.subject {
							t.Errorf("a plugin processed its own output: %+v", c)
						}
					}
				}
			}
			if protocol == signer.ProtocolRequest {
				if bOnA.role != signer.RolePluginArtifact || bOnA.parent != root || bOnA.subjectTyp != "application/vnd.test.A" {
					t.Errorf("B on A's artifact should be told role/parent/artifactType: %+v", bOnA)
				}
				if bOnA.tags != "" || bOnRoot.tags != "latest" {
					t.Errorf("artifacts are attached by digest (no tags), the root keeps its tags: %+v %+v", bOnA, bOnRoot)
				}
				if bOnRoot.role != signer.RoleRoot {
					t.Errorf("root role = %q", bOnRoot.role)
				}
			}
		})
	}
}

// Without the target, a plugin never sees what the others created, even when
// they ran before it.
func TestChainWithoutPluginArtifactsTargetSeesNothing(t *testing.T) {
	store, steps := chainFixture(t, []chainStep{{name: "A"}, {name: "B"}})
	calls, err := runChain(t, store, []api.IndexedPushDeployOperation{chainOp(0, "root", nil, steps, false)})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(calls); got != "A B" {
		t.Fatalf("calls = %q, want A B", got)
	}
}

// A plugin offered an operation's artifacts only sees those of that operation.
// This uses the original protocol on purpose: under the request protocol, merging
// would hide an artifact being offered to the wrong operation.
func TestChainArtifactsAreScopedToTheirOperation(t *testing.T) {
	store, steps := chainFixture(t, []chainStep{{name: "A"}, {name: "B", targets: []string{"plugin_artifacts"}}})
	calls, err := runChain(t, store, []api.IndexedPushDeployOperation{
		chainOp(0, "x", nil, steps, false),
		chainOp(1, "y", nil, steps, false),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Each of the two images: A on the root; B on the root and on A's artifact. If
	// B were offered the other operation's artifact too, it would run six times.
	if got := names(calls); got != "A A B B B B" {
		t.Fatalf("calls = %q, want \"A A B B B B\"\n%+v", got, calls)
	}
	for _, image := range []string{"x", "y"} {
		aArtifact := findCall(t, calls, "A", digestOf(image)).artifact
		seen := 0
		for _, c := range calls {
			if c.plugin == "B" && c.subject == aArtifact {
				seen++
			}
		}
		if seen != 1 {
			t.Errorf("A's artifact of %s was offered to B %d times, want once", image, seen)
		}
	}
}

// With the request protocol, operations that push the same digest to the same
// repository share one call per step, and the artifacts they cause are shared too.
func TestChainMergesOperationsUnderTheRequestProtocol(t *testing.T) {
	store, steps := chainFixture(t, []chainStep{
		{name: "A", protocol: signer.ProtocolRequest},
		{name: "B", protocol: signer.ProtocolRequest, targets: []string{"plugin_artifacts"}},
	})
	calls, err := runChain(t, store, []api.IndexedPushDeployOperation{
		chainOp(0, "root", []string{"latest"}, steps, false),
		chainOp(1, "root", []string{"v1"}, steps, false),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(calls); got != "A B B" {
		t.Fatalf("one attestation per image, not per operation: calls = %q\n%+v", got, calls)
	}
	if got := findCall(t, calls, "A", digestOf("root")).tags; got != "latest,v1" {
		t.Errorf("tags = %q, want them merged", got)
	}
}

func TestChainFailures(t *testing.T) {
	// A required plugin that fails aborts the deploy before the next one runs.
	store, steps := chainFixture(t, []chainStep{{name: "A", fail: true}, {name: "B"}})
	calls, err := runChain(t, store, []api.IndexedPushDeployOperation{chainOp(0, "root", nil, steps, false)})
	if err == nil || len(calls) != 0 {
		t.Fatalf("a failing step must abort: calls=%+v err=%v", calls, err)
	}

	// Best effort: the failure is a warning, and later plugins still run, with the
	// artifact of the failed one simply missing.
	store, steps = chainFixture(t, []chainStep{{name: "A", fail: true}, {name: "B", targets: []string{"plugin_artifacts"}}})
	calls, err = runChain(t, store, []api.IndexedPushDeployOperation{chainOp(0, "root", nil, steps, true)})
	if err != nil {
		t.Fatalf("best effort must not fail the deploy: %v", err)
	}
	if got := names(calls); got != "B" {
		t.Fatalf("calls = %q, want just B on the root", got)
	}
}

// The runtime --sign_targets override replaces the targets of every step.
func TestChainRuntimeTargetOverride(t *testing.T) {
	store, steps := chainFixture(t, []chainStep{{name: "A"}, {name: "B"}})
	var calls []call
	emit := func(_ context.Context, _ api.IndexedPushDeployOperation, subject api.Descriptor, imgs []registryv1.Image) error {
		for _, img := range imgs {
			m, _ := img.Manifest()
			calls = append(calls, call{plugin: m.Annotations["name"], subject: subject.Digest})
		}
		return nil
	}
	err := forEachSignature(context.Background(),
		[]api.IndexedPushDeployOperation{chainOp(0, "root", nil, steps, false)},
		api.DeploySettings{}, store, nil, signOptions{targetOverride: []string{"plugin_artifacts"}}, nil, emit)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(calls); got != "A B B" {
		t.Fatalf("override applies to every step: calls = %q", got)
	}
}
