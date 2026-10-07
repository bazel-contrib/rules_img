package deploy

import (
	"fmt"
	"io"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/push"
)

// The kinds of reference `img deploy` writes to stdout, one per line. Every
// line belongs to exactly one kind, so --print selects whole lines and never
// reformats them.
const (
	// printKindTag is a tag reference (<registry>/<repository>:<tag>): the
	// operation's own tags plus any added with --tag, and the tags written by
	// registry_tag operations.
	printKindTag = "tag"
	// printKindManifest is the digest reference
	// (<registry>/<repository>@sha256:...) of a manifest or index the deploy
	// pushed in its own right.
	printKindManifest = "manifest"
	// printKindReferrer is the digest reference of a manifest the deploy
	// pushed as an attachment (an OCI referrer) of another manifest, such as
	// an SBOM or a provenance artifact.
	printKindReferrer = "referrer"
	// printKindAll selects every kind. It is the default, and wins over any
	// other value in the same list.
	printKindAll = "all"
	// printKindNone selects nothing, silencing stdout.
	printKindNone = "none"
)

// printKinds lists the selectable kinds in the order they are reported in
// error messages.
var printKinds = []string{printKindTag, printKindManifest, printKindReferrer}

// printFilter decides which of the references a deploy produced are printed.
// The zero value prints all of them, which is the default and keeps any caller
// that does not configure one at the historical behavior.
type printFilter struct {
	// selected holds the chosen kinds. A nil map means "every kind", so that
	// the zero printFilter prints everything.
	selected map[string]bool
}

// parsePrintFilter turns a --print value into a filter. An empty list keeps
// the default of printing every kind.
func parsePrintFilter(list []string) (printFilter, error) {
	if len(list) == 0 {
		return printFilter{}, nil
	}
	selected := map[string]bool{}
	for _, kind := range list {
		switch kind {
		case printKindAll:
			// "all" is the widest selection, so nothing after it can narrow it.
			return printFilter{}, nil
		case printKindNone:
			// Selects nothing; a kind listed alongside it still applies.
		case printKindTag, printKindManifest, printKindReferrer:
			selected[kind] = true
		default:
			return printFilter{}, fmt.Errorf("invalid --print value %q: want a comma-separated list of %s, or %q/%q", kind, strings.Join(printKinds, ", "), printKindAll, printKindNone)
		}
	}
	return printFilter{selected: selected}, nil
}

// wants reports whether references of the given kind are printed.
func (f printFilter) wants(kind string) bool {
	return f.selected == nil || f.selected[kind]
}

// printRefs writes refs to w, one per line, when kind is selected. It is used
// for the reference lists that are all of one kind: the tags of registry_tag
// operations, and the references a --sink captured.
func (f printFilter) printRefs(w io.Writer, kind string, refs []string) {
	if !f.wants(kind) {
		return
	}
	for _, ref := range refs {
		fmt.Fprintln(w, ref)
	}
}

// printPushed writes the references a push wrote to w, one per line, keeping
// only the selected kinds and the order the push reported them in.
func (f printFilter) printPushed(w io.Writer, refs []push.PushedReference) {
	for _, ref := range refs {
		if f.wants(printKindOf(ref)) {
			fmt.Fprintln(w, ref.Ref)
		}
	}
}

// printKindOf classifies a reference a push wrote. A referrer's tags, which it
// only has when --tag added one, count as tags: --print selects by what a line
// names, not by which operation produced it.
func printKindOf(ref push.PushedReference) string {
	switch {
	case ref.Tag:
		return printKindTag
	case ref.Referrer:
		return printKindReferrer
	default:
		return printKindManifest
	}
}
