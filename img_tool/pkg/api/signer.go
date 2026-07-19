package api

import (
	"context"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// OCIArtifactSigner abstracts the creation of OCI-native signature artifacts.
//
// It is the single seam between `img deploy` and any signing implementation.
// `img deploy` itself carries exactly one implementation, which delegates to an
// external signer plugin over a subprocess RPC (see img_tool/pkg/signer). Plugin
// authors implement this same interface in-process and wrap it with the shared
// runner in img_tool/signer-plugins/pkg/plugin.
type OCIArtifactSigner interface {
	// Sign generates a cryptographic signature for the given target artifact.
	// The 'subject' descriptor contains the digest, size, and mediaType of the
	// target. It returns a v1.Image representing the OCI signature artifact
	// itself, properly linked via the OCI 1.1 subject field, ready to be pushed
	// to a registry.
	Sign(ctx context.Context, subject v1.Descriptor) (v1.Image, error)
}
