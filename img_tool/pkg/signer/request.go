package signer

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/version"
)

// Signer plugin protocols, selected by SignSettingConfig.Protocol.
const (
	// ProtocolDescriptor is the original protocol: stdin carries just the JSON
	// of the subject's v1.Descriptor, and the plugin is called once per push
	// operation and subject. It is what an unset Protocol means.
	ProtocolDescriptor = 1
	// ProtocolRequest is the opt-in protocol: stdin carries a JSON Request with
	// the subject plus where it is being pushed and facts about the deploy. The
	// plugin is called once per registry, repository and subject digest, with
	// the tags of every operation that pushes it there.
	ProtocolRequest = 2
)

// Roles of a Request's subject.
const (
	// RoleRoot marks the root of a push operation: a single-platform image
	// manifest or an image index.
	RoleRoot = "root"
	// RoleChildManifest marks a manifest of an image index, signed when the
	// operation selects the child_manifests target. Request.Parent is the index.
	RoleChildManifest = "child_manifest"
	// RolePluginArtifact marks a manifest that a plugin earlier in the chain
	// created, offered to a plugin whose targets include plugin_artifacts. The
	// subject's ArtifactType says what it is and Parent is what it is attached to.
	RolePluginArtifact = "plugin_artifact"
)

// Request is what a ProtocolRequest plugin receives on stdin. Fields may be
// added in later versions of the protocol; a plugin must ignore the ones it does
// not know and may refuse a ProtocolVersion it does not support.
type Request struct {
	// ProtocolVersion is always ProtocolRequest. A plugin tells a Request from
	// the bare descriptor of ProtocolDescriptor by this field being non-zero.
	ProtocolVersion int `json:"protocol_version"`
	// Subject is the descriptor to sign. For a single-platform image manifest
	// its Platform is read from the image config; it is nil for image indexes
	// and whenever the config is not available.
	Subject v1.Descriptor `json:"subject"`
	// Role is RoleRoot or RoleChildManifest.
	Role string `json:"role"`
	// Parent is the image index a RoleChildManifest subject belongs to.
	Parent *v1.Descriptor `json:"parent,omitempty"`
	// Destination is where the subject is pushed.
	Destination Destination `json:"destination"`
	// Deploy describes the deploy this call is part of.
	Deploy Deploy `json:"deploy"`
}

// Destination is the repository a subject is pushed to, after any
// --registry/--repository overrides of the deploy.
type Destination struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	// Tags are the tags the subject is pushed under, merged over every push
	// operation that pushes it to this repository (sorted, unique). Empty for a
	// push by digest. Tags attached by separate registry_tag operations are not
	// included.
	Tags []string `json:"tags,omitempty"`
	// References are the full image names: registry/repository:tag for each tag,
	// or registry/repository@digest when there are no tags.
	References []string `json:"references"`
}

// Deploy carries facts that are the same for every call of one deploy, so that
// plugins attesting several subjects agree on them.
type Deploy struct {
	// RulesImgVersion is the version of the rules_img `img` tool running the
	// deploy ("dev" for unreleased builds).
	RulesImgVersion string `json:"rules_img_version"`
	// InvocationID identifies this deploy (a random UUID).
	InvocationID string `json:"invocation_id"`
	// StartedAt is when the deploy started, RFC 3339 in UTC. It honors
	// $SOURCE_DATE_EPOCH.
	StartedAt string `json:"started_at"`
	// PushStrategy is the push strategy of the deploy (e.g. "eager").
	PushStrategy string `json:"push_strategy,omitempty"`
}

// NewDeploy captures the per-deploy facts once, to be shared by every Request
// of the deploy.
func NewDeploy(pushStrategy string) (Deploy, error) {
	start := time.Now()
	if sde := os.Getenv("SOURCE_DATE_EPOCH"); sde != "" {
		secs, err := strconv.ParseInt(sde, 10, 64)
		if err != nil {
			return Deploy{}, fmt.Errorf("invalid SOURCE_DATE_EPOCH %q: %w", sde, err)
		}
		start = time.Unix(secs, 0)
	}
	id, err := newUUID()
	if err != nil {
		return Deploy{}, err
	}
	return Deploy{
		RulesImgVersion: version.Version,
		InvocationID:    id,
		StartedAt:       start.UTC().Format(time.RFC3339),
		PushStrategy:    pushStrategy,
	}, nil
}

// newUUID returns a random (version 4) UUID.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating invocation id: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

// References returns the full image names for a destination: one per tag, or the
// digest reference when there are no tags.
func References(registry, repository string, tags []string, digest string) []string {
	base := registry + "/" + repository
	if len(tags) == 0 {
		return []string{base + "@" + digest}
	}
	refs := make([]string, 0, len(tags))
	for _, t := range tags {
		refs = append(refs, base+":"+t)
	}
	return refs
}
