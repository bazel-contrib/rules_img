// Package plugin provides the shared entrypoint and OCI-layout helpers for the
// rules_img provenance signer plugin. It is a self-contained copy (no dependency
// on rules_img) so this module can be released independently.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/bazel-contrib/rules_img_signer_provenance/pkg/signerapi"
)

// Subcommand is the verb `img deploy` invokes signer plugins with.
const Subcommand = "sign-oci-artifact"

// Run reads the signing request from stdin, signs it, and writes the OCI image
// layout tar of the resulting artifact to stdout.
//
// stdin is either the bare subject descriptor (the original protocol) or, when
// the signing_config sets protocol = 2, a Request. A Request is recognized by its
// non-zero protocol_version, which a descriptor never has. A signer that does not
// implement signerapi.RequestSigner only gets the subject of a Request.
func Run(ctx context.Context, signer signerapi.OCIArtifactSigner, stdin io.Reader, stdout io.Writer) error {
	raw, err := io.ReadAll(io.LimitReader(stdin, maxStdin))
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	var probe struct {
		ProtocolVersion int `json:"protocol_version"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("decoding stdin: %w", err)
	}

	var (
		subject v1.Descriptor
		req     *signerapi.Request
	)
	if probe.ProtocolVersion == 0 {
		if err := json.Unmarshal(raw, &subject); err != nil {
			return fmt.Errorf("decoding subject descriptor from stdin: %w", err)
		}
	} else {
		if probe.ProtocolVersion != signerapi.ProtocolRequest {
			return fmt.Errorf("unsupported request protocol version %d (this plugin speaks %d); upgrade the plugin", probe.ProtocolVersion, signerapi.ProtocolRequest)
		}
		req = &signerapi.Request{}
		if err := json.Unmarshal(raw, req); err != nil {
			return fmt.Errorf("decoding signing request from stdin: %w", err)
		}
		subject = req.Subject
	}
	if subject.Digest.Hex == "" {
		return fmt.Errorf("subject descriptor has no digest")
	}

	var img v1.Image
	if rs, ok := signer.(signerapi.RequestSigner); ok && req != nil {
		img, err = rs.SignRequest(ctx, *req)
	} else {
		img, err = signer.Sign(ctx, subject)
	}
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

// maxStdin bounds the request a plugin reads; a request is a few KiB, but a
// GitHub event payload is not part of it.
const maxStdin = 1 << 20

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
