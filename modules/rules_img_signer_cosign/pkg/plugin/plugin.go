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

	"github.com/bazel-contrib/rules_img_signer_cosign/pkg/signerapi"
)

// Subcommand is the verb `img deploy` invokes signer plugins with to sign.
const Subcommand = "sign-oci-artifact"

// AttestSubcommand is the verb `img deploy` invokes signer plugins with to make
// an attestation (of an SBOM, say) about an image.
const AttestSubcommand = "attest-oci-artifact"

// AttestRequest is what the attest-oci-artifact subcommand reads from stdin.
type AttestRequest struct {
	// Subject is the descriptor of the manifest the attestation is about.
	Subject v1.Descriptor `json:"subject"`
	// PredicateType is the type of Predicate, such as
	// https://cyclonedx.org/bom.
	PredicateType string `json:"predicate_type"`
	// Predicate is recorded in the in-toto statement as it is.
	Predicate json.RawMessage `json:"predicate"`
}

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

// RunAttest reads an AttestRequest from stdin, attests its predicate about its
// subject, and writes the OCI image layout tar of the attestation artifact to
// stdout.
func RunAttest(ctx context.Context, attester signerapi.OCIArtifactAttester, stdin io.Reader, stdout io.Writer) error {
	var request AttestRequest
	if err := json.NewDecoder(stdin).Decode(&request); err != nil {
		return fmt.Errorf("decoding the attestation request from stdin: %w", err)
	}
	switch {
	case request.Subject.Digest.Hex == "":
		return fmt.Errorf("the attestation request has no subject digest")
	case request.PredicateType == "":
		return fmt.Errorf("the attestation request has no predicate type")
	case len(request.Predicate) == 0:
		return fmt.Errorf("the attestation request has no predicate")
	}
	img, err := attester.Attest(ctx, request.Subject, request.PredicateType, request.Predicate)
	if err != nil {
		return fmt.Errorf("attesting %s about %s: %w", request.PredicateType, request.Subject.Digest, err)
	}
	if img == nil {
		return fmt.Errorf("attester returned no artifact")
	}
	if err := WriteArtifact(stdout, []v1.Image{img}); err != nil {
		return fmt.Errorf("writing attestation OCI layout: %w", err)
	}
	return nil
}

// Dispatch is a convenience for plugin main functions: it requires the
// sign-oci-artifact (or attest-oci-artifact) subcommand, builds a signer from
// the remaining args, and runs the protocol over stdin/stdout.
func Dispatch(ctx context.Context, args []string, newSigner func(args []string) (signerapi.OCIArtifactSigner, error)) error {
	if len(args) == 0 || args[0] != Subcommand && args[0] != AttestSubcommand {
		return fmt.Errorf("expected %q or %q subcommand", Subcommand, AttestSubcommand)
	}
	signer, err := newSigner(args[1:])
	if err != nil {
		return err
	}
	if args[0] == AttestSubcommand {
		attester, ok := signer.(signerapi.OCIArtifactAttester)
		if !ok {
			return fmt.Errorf("this plugin can't make attestations (%s)", AttestSubcommand)
		}
		return RunAttest(ctx, attester, os.Stdin, os.Stdout)
	}
	return Run(ctx, signer, os.Stdin, os.Stdout)
}
