package main

import (
	"strings"
	"testing"
)

func TestSetModuleVersionRewritesOnlyTheVersion(t *testing.T) {
	patched, err := setModuleVersion([]byte(testModuleBazel), "0.3.20-20260811-fa8b7de")
	if err != nil {
		t.Fatal(err)
	}

	want := strings.Replace(testModuleBazel, `version = "0.3.19"`, `version = "0.3.20-20260811-fa8b7de"`, 1)
	if string(patched) != want {
		t.Errorf("patched MODULE.bazel:\n%s\nwant:\n%s", patched, want)
	}
}

func TestSetModuleVersionRejectsFilesWithoutAModuleVersion(t *testing.T) {
	for name, content := range map[string]string{
		"no module call":  "bazel_dep(name = \"platforms\", version = \"1.0.0\")\n",
		"no version":      "module(name = \"rules_img\")\n",
		"computed module": "module(name = \"rules_img\", version = VERSION)\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := setModuleVersion([]byte(content), "1.0.0"); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestUnifiedDiffApplies(t *testing.T) {
	for name, tc := range map[string]struct {
		original string
		modified string
	}{
		"one line in the middle": {
			original: "a\nb\nc\nd\ne\nf\ng\nh\n",
			modified: "a\nb\nc\nD\ne\nf\ng\nh\n",
		},
		"first line": {
			original: "a\nb\nc\n",
			modified: "A\nb\nc\n",
		},
		"last line": {
			original: "a\nb\nc\n",
			modified: "a\nb\nC\n",
		},
		"more lines than before": {
			original: "a\nb\nc\n",
			modified: "a\nb1\nb2\nb3\nc\n",
		},
		"fewer lines than before": {
			original: "a\nb1\nb2\nb3\nc\n",
			modified: "a\nb\nc\n",
		},
		"a module file": {
			original: testModuleBazel,
			modified: strings.Replace(testModuleBazel, "0.3.19", "0.3.20-20260811-fa8b7de", 1),
		},
	} {
		t.Run(name, func(t *testing.T) {
			diff, err := unifiedDiff("MODULE.bazel", []byte(tc.original), []byte(tc.modified))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(diff), "--- a/MODULE.bazel\n+++ b/MODULE.bazel\n@@ ") {
				t.Errorf("diff is not a patch -p1 compatible unified diff:\n%s", diff)
			}
			applied, err := applyUnifiedDiff([]byte(tc.original), diff)
			if err != nil {
				t.Fatalf("applying the diff: %v\n%s", err, diff)
			}
			if string(applied) != tc.modified {
				t.Errorf("applying the diff gave:\n%s\nwant:\n%s", applied, tc.modified)
			}
		})
	}
}

func TestUnifiedDiffRejectsIdenticalContent(t *testing.T) {
	if _, err := unifiedDiff("MODULE.bazel", []byte("a\n"), []byte("a\n")); err == nil {
		t.Error("expected an error when there is nothing to patch")
	}
}

func TestApplyUnifiedDiffRejectsMismatchedContext(t *testing.T) {
	diff, err := unifiedDiff("MODULE.bazel", []byte("a\nb\nc\n"), []byte("a\nB\nc\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyUnifiedDiff([]byte("x\ny\nz\n"), diff); err == nil {
		t.Error("expected an error when the patch does not match the file")
	}
}

func TestValidatePrereleaseVersion(t *testing.T) {
	for name, tc := range map[string]struct {
		publish  string
		declared string
		wantErr  bool
	}{
		"prerelease of the next patch":     {publish: "0.3.20-20260811-fa8b7de", declared: "0.3.19"},
		"prerelease of the next minor":     {publish: "0.4.0-20260811-fa8b7de", declared: "0.3.19"},
		"prerelease of the release itself": {publish: "0.3.19-20260811-fa8b7de", declared: "0.3.19", wantErr: true},
		"prerelease of an older version":   {publish: "0.3.18-20260811-fa8b7de", declared: "0.3.19", wantErr: true},
		"no prerelease part":               {publish: "0.3.20", declared: "0.3.19", wantErr: true},
		"not a version":                    {publish: "main-fa8b7de", declared: "0.3.19", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			err := validatePrereleaseVersion(tc.publish, tc.declared)
			if tc.wantErr && err == nil {
				t.Errorf("validatePrereleaseVersion(%q, %q) = nil, want an error", tc.publish, tc.declared)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validatePrereleaseVersion(%q, %q) = %v", tc.publish, tc.declared, err)
			}
		})
	}
}

