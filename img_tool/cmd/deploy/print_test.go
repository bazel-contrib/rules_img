package deploy

import (
	"slices"
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/push"
)

func TestParsePrintFilter(t *testing.T) {
	// An empty --print, "all", and listing every kind are all the same
	// selection, so none of them can silently drop a line.
	allRefs := []push.PushedReference{
		{Ref: "reg/repo@sha256:aa"},
		{Ref: "reg/repo:latest", Tag: true},
		{Ref: "reg/repo@sha256:bb", Referrer: true},
	}

	cases := []struct {
		value string
		want  []string
	}{
		{"", []string{"reg/repo@sha256:aa", "reg/repo:latest", "reg/repo@sha256:bb"}},
		{"all", []string{"reg/repo@sha256:aa", "reg/repo:latest", "reg/repo@sha256:bb"}},
		{"tag,manifest,referrer", []string{"reg/repo@sha256:aa", "reg/repo:latest", "reg/repo@sha256:bb"}},
		{"manifest", []string{"reg/repo@sha256:aa"}},
		{"tag", []string{"reg/repo:latest"}},
		{"referrer", []string{"reg/repo@sha256:bb"}},
		{"tag,manifest", []string{"reg/repo@sha256:aa", "reg/repo:latest"}},
		{"none", nil},
		// "all" is the widest selection, so it wins wherever it appears.
		{"manifest,all", []string{"reg/repo@sha256:aa", "reg/repo:latest", "reg/repo@sha256:bb"}},
		// Whitespace and empty elements are tolerated, as for --sign_targets.
		{" manifest , tag ,", []string{"reg/repo@sha256:aa", "reg/repo:latest"}},
	}

	for _, tc := range cases {
		filter, err := parsePrintFilter(splitCommaList(tc.value))
		if err != nil {
			t.Errorf("parsePrintFilter(%q) returned error: %v", tc.value, err)
			continue
		}
		var out strings.Builder
		filter.printPushed(&out, allRefs)
		got := lines(out.String())
		if !slices.Equal(got, tc.want) {
			t.Errorf("--print=%q printed %q, want %q", tc.value, got, tc.want)
		}
	}
}

func TestParsePrintFilterRejectsUnknownKind(t *testing.T) {
	for _, value := range []string{"bogus", "tag,bogus", "manifests", "ALL"} {
		if _, err := parsePrintFilter(splitCommaList(value)); err == nil {
			t.Errorf("parsePrintFilter(%q) = nil error, want one", value)
		}
	}
}

func TestPrintFilterPrintRefs(t *testing.T) {
	refs := []string{"reg/repo:a", "reg/repo:b"}

	for _, tc := range []struct {
		value string
		want  []string
	}{
		{"", refs},
		{"tag", refs},
		{"manifest", nil},
		{"none", nil},
	} {
		filter, err := parsePrintFilter(splitCommaList(tc.value))
		if err != nil {
			t.Fatalf("parsePrintFilter(%q) returned error: %v", tc.value, err)
		}
		var out strings.Builder
		filter.printRefs(&out, printKindTag, refs)
		got := lines(out.String())
		if !slices.Equal(got, tc.want) {
			t.Errorf("--print=%q printed %q, want %q", tc.value, got, tc.want)
		}
	}
}

// TestPrintFilterZeroValuePrintsEverything pins the zero value, which every
// caller that does not configure a selection relies on.
func TestPrintFilterZeroValuePrintsEverything(t *testing.T) {
	var filter printFilter
	for _, kind := range append(printKinds, "anything-else") {
		if !filter.wants(kind) {
			t.Errorf("zero printFilter does not want %q", kind)
		}
	}
}

func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
