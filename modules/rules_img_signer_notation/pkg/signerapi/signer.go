// Package signerapi holds a local, dependency-free copy of the OCIArtifactSigner
// interface. It is intentionally decoupled from rules_img so this plugin module
// can be built and released independently.
package signerapi

import (
	"context"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// OCIArtifactSigner abstracts the creation of OCI-native signature artifacts.
//
// Sign generates a signature for the given target artifact. The subject
// descriptor carries the digest, size, and mediaType of the target. It returns
// a v1.Image representing the OCI signature artifact, linked via the OCI 1.1
// subject field, ready to be pushed to a registry as a referrer.
type OCIArtifactSigner interface {
	Sign(ctx context.Context, subject v1.Descriptor) (v1.Image, error)
}
