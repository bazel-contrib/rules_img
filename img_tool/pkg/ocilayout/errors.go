package ocilayout

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// errNoBlobContent is returned when a Blob has no in-memory or streaming
// content (a Path blob handed to the streaming reader).
var errNoBlobContent = errors.New("ocilayout: blob has no readable content")

func errBlobNotFound(hexDigest string) error {
	return fmt.Errorf("ocilayout: blob %s not found", hexDigest)
}

// Output-group names used in MissingBlobsError to select the rules_img hint
// text. They mirror the Bazel output groups that request each format.
const (
	OutputGroupOCILayout = "oci_layout"
	OutputGroupTarball   = "tarball"
)

// MissingBlobsError reports layer blobs that were required but not supplied.
// It consolidates the two identical copies that previously lived in
// cmd/ocilayout and cmd/dockersave. OutputGroup selects the rules_img-specific
// hint text shown when RULES_IMG=1.
type MissingBlobsError struct {
	MissingBlobs []string
	// OutputGroup is "oci_layout" or "tarball"; it selects the hint wording.
	OutputGroup string
}

func (e *MissingBlobsError) Error() string {
	if os.Getenv("RULES_IMG") == "1" {
		group := e.OutputGroup
		if group == "" {
			group = OutputGroupOCILayout
		}
		what := "OCI image layouts"
		if group == OutputGroupTarball {
			what = "Docker save tarballs"
		}
		return fmt.Sprintf(
			`Missing layer blobs %s
%q output group requested with shallow base image. You probably want to add the "layer_handling" attribute to the pull rule of your base image (choose "lazy" or "eager", but NOT "shallow").
If you explicitly want to opt in to %s with missing blobs, use the "--@rules_img//img/settings:shallow_oci_layout=i_know_what_i_am_doing" flag.
`,
			strings.Join(e.MissingBlobs, ", "),
			group,
			what,
		)
	}
	return fmt.Sprintf("missing blobs: %s", strings.Join(e.MissingBlobs, ", "))
}
