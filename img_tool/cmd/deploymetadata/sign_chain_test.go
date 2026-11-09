package deploymetadata

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

func writeSettingFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// resetSignFlags restores the package-level signing flags after a test.
func resetSignFlags(t *testing.T) {
	t.Helper()
	oldFile, oldTargets, oldSteps := signSettingFile, signTargets, signSteps
	t.Cleanup(func() { signSettingFile, signTargets, signSteps = oldFile, oldTargets, oldSteps })
	signSettingFile, signTargets, signSteps = "", nil, nil
}

func TestSignChainFromStepFlagsKeepsOrderAndPerStepTargets(t *testing.T) {
	resetSignFlags(t)
	first := writeSettingFile(t, "first.json", `{"first":true}`)
	second := writeSettingFile(t, "second.json", `{"second":true}`)
	signSteps = []signStepFlag{
		{file: first, targets: []string{"roots", "child_manifests"}},
		{file: second, targets: []string{"plugin_artifacts"}},
	}

	steps, err := signChain()
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 {
		t.Fatalf("steps = %+v", steps)
	}
	firstDesc, _ := signConfigDescriptor(first)
	secondDesc, _ := signConfigDescriptor(second)
	if steps[0].Setting.Digest != firstDesc.Digest || steps[1].Setting.Digest != secondDesc.Digest {
		t.Errorf("steps are not in flag order: %+v", steps)
	}
	if !reflect.DeepEqual(steps[0].Targets, []string{"roots", "child_manifests"}) || !reflect.DeepEqual(steps[1].Targets, []string{"plugin_artifacts"}) {
		t.Errorf("targets belong to their step: %+v", steps)
	}
}

// The original single-plugin flags keep working and mean one step.
func TestSignChainFromLegacyFlags(t *testing.T) {
	resetSignFlags(t)
	signSettingFile = writeSettingFile(t, "one.json", `{"one":true}`)
	signTargets = []string{"roots", "referrers"}

	steps, err := signChain()
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || !reflect.DeepEqual(steps[0].Targets, []string{"roots", "referrers"}) {
		t.Fatalf("steps = %+v", steps)
	}

	signSteps = []signStepFlag{{file: signSettingFile}}
	if _, err := signChain(); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Errorf("mixing the two forms must be rejected, got %v", err)
	}
}

func TestSignChainNothingConfigured(t *testing.T) {
	resetSignFlags(t)
	if steps, err := signChain(); err != nil || len(steps) != 0 {
		t.Fatalf("no flags means no signing: %+v %v", steps, err)
	}
}

func TestValidSignTarget(t *testing.T) {
	for _, ok := range []string{"roots", "child_manifests", "referrers", "plugin_artifacts", "all"} {
		if err := validSignTarget(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	if err := validSignTarget("everything"); err == nil {
		t.Error("an unknown target must be rejected")
	}
}

// The referrers of the main image (an SBOM) are processed by the steps that select
// referrers, each as a root. A step keeps plugin_artifacts, so the plugins after it
// also process what earlier ones created for the SBOM.
func TestReferrerSignSteps(t *testing.T) {
	setting := func(d string) *api.Descriptor { return &api.Descriptor{Digest: d} }
	steps := []api.SignStep{
		{Setting: setting("sha256:a"), Targets: []string{"roots", "child_manifests"}},               // no referrers: skipped
		{Setting: setting("sha256:b"), Targets: []string{"roots", "referrers"}},                     // referrers
		{Setting: setting("sha256:c"), Targets: []string{"roots", "referrers", "plugin_artifacts"}}, // referrers + artifacts
		{Setting: setting("sha256:d"), Targets: []string{"all"}},                                    // all includes both
		{Setting: setting("sha256:e"), Targets: []string{"plugin_artifacts"}},                       // artifacts only: skipped
	}
	got := referrerSignSteps(steps)
	want := []api.SignStep{
		{Setting: setting("sha256:b")},
		{Setting: setting("sha256:c"), Targets: []string{"plugin_artifacts"}},
		{Setting: setting("sha256:d"), Targets: []string{"plugin_artifacts"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("referrer steps = %+v, want %+v", got, want)
	}
	if got := referrerSignSteps(nil); len(got) != 0 {
		t.Errorf("no steps, no referrer signing: %+v", got)
	}
}

func TestEffectiveSteps(t *testing.T) {
	legacy := &api.SignConfig{Setting: &api.Descriptor{Digest: "sha256:x"}, Targets: []string{"roots"}}
	if got := legacy.EffectiveSteps(); len(got) != 1 || got[0].Setting.Digest != "sha256:x" || got[0].Targets[0] != "roots" {
		t.Errorf("legacy = %+v", got)
	}
	chain := &api.SignConfig{Setting: &api.Descriptor{Digest: "sha256:ignored"}, Steps: []api.SignStep{{Setting: &api.Descriptor{Digest: "sha256:1"}}, {Setting: &api.Descriptor{Digest: "sha256:2"}}}}
	if got := chain.EffectiveSteps(); len(got) != 2 || got[0].Setting.Digest != "sha256:1" {
		t.Errorf("steps win over the single-plugin fields: %+v", got)
	}
	if got := (&api.SignConfig{}).EffectiveSteps(); len(got) != 1 || got[0].Setting != nil {
		t.Errorf("an empty config is one step using the default setting: %+v", got)
	}
}
