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

// ProtocolRequest is the protocol version of Request.
const ProtocolRequest = 2

// Role values of a Request's subject.
const (
	RoleRoot          = "root"
	RoleChildManifest = "child_manifest"
)

// Request is what `img deploy` writes to the plugin's stdin when the plugin's
// signing_config sets protocol = 2. It is a local copy of the host's type, so
// this module stays independent of rules_img. Unknown fields are ignored, which
// lets the host add fields without breaking older plugins.
type Request struct {
	ProtocolVersion int `json:"protocol_version"`
	// Subject is the descriptor to attach the artifact to. For a single-platform
	// image its Platform is set from the image config.
	Subject v1.Descriptor `json:"subject"`
	// Role is RoleRoot or RoleChildManifest.
	Role string `json:"role"`
	// Parent is the image index a RoleChildManifest subject belongs to.
	Parent      *v1.Descriptor `json:"parent,omitempty"`
	Destination Destination    `json:"destination"`
	Deploy      Deploy         `json:"deploy"`
}

// Destination is the repository a subject is pushed to.
type Destination struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	// Tags the subject is pushed under (merged over all push operations to this
	// repository). Empty for a push by digest.
	Tags []string `json:"tags,omitempty"`
	// References are the full image names: registry/repository:tag for each tag,
	// or registry/repository@digest without tags.
	References []string `json:"references"`
}

// Deploy carries facts that are shared by every call of one deploy.
type Deploy struct {
	RulesImgVersion string `json:"rules_img_version"`
	InvocationID    string `json:"invocation_id"`
	// StartedAt is when the deploy started, RFC 3339 in UTC.
	StartedAt    string `json:"started_at"`
	PushStrategy string `json:"push_strategy,omitempty"`
	// BuildInfoFile is the path of a JSON file naming the Bazel targets the deploy
	// was built from. Present only if the signing_config sets build_info.
	BuildInfoFile string `json:"build_info_file,omitempty"`
}

// RequestSigner is implemented by signers that use the context of a Request. A
// signer that does not implement it is called with just the subject descriptor,
// so existing signers work unchanged under protocol = 2.
type RequestSigner interface {
	OCIArtifactSigner
	SignRequest(ctx context.Context, req Request) (v1.Image, error)
}
