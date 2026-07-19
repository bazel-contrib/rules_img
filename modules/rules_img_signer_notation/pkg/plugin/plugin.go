// Package plugin provides the shared entrypoint and OCI-layout helpers for the
// rules_img notation signer plugin. It is a self-contained copy (no dependency
// on rules_img) so this module can be released independently.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/bazel-contrib/rules_img_signer_notation/pkg/signerapi"
)

// Subcommand is the verb `img deploy` invokes signer plugins with.
const Subcommand = "sign-oci-artifact"

// Run reads the subject descriptor from stdin, signs it, and writes the OCI
// image layout tar of the signature artifact to stdout.
func Run(ctx context.Context, signer signerapi.OCIArtifactSigner, stdin io.Reader, stdout io.Writer) error {
	var subject v1.Descriptor
	if err := json.NewDecoder(stdin).Decode(&subject); err != nil {
		return fmt.Errorf("decoding subject descriptor from stdin: %w", err)
	}
	if subject.Digest.Hex == "" {
		return fmt.Errorf("subject descriptor has no digest")
	}
	img, err := signer.Sign(ctx, subject)
	if err != nil {
		return fmt.Errorf("signing subject %s: %w", subject.Digest, err)
	}
	if img == nil {
		return fmt.Errorf("signer returned no artifact")
	}
	if err := WriteArtifact(stdout, []v1.Image{img}); err != nil {
		return fmt.Errorf("writing signature OCI layout: %w", err)
	}
	return nil
}

// Dispatch is a convenience for plugin main functions: it requires the
// sign-oci-artifact subcommand, builds a signer from the remaining args, and
// runs the protocol over stdin/stdout.
func Dispatch(ctx context.Context, args []string, newSigner func(args []string) (signerapi.OCIArtifactSigner, error)) error {
	if len(args) == 0 || args[0] != Subcommand {
		return fmt.Errorf("expected %q subcommand", Subcommand)
	}
	signer, err := newSigner(args[1:])
	if err != nil {
		return err
	}
	return Run(ctx, signer, os.Stdin, os.Stdout)
}
