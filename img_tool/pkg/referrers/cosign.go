package referrers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// Cosign's tag scheme, which predates OCI referrers (and is still what cosign
// writes by default without --registry-referrers-mode=oci-1-1): the signatures
// of a manifest are the layers of the manifest tagged sha256-<hex>.sig in the
// same repository, its attestations those of sha256-<hex>.att. The manifests
// have no subject: nothing but the tag name points from the artifact to the
// image, and nothing points from the image to the artifact.
//
// Discover finds them for the pseudo artifact types CosignTagType("sig") and
// CosignTagType("att").
const (
	cosignTagTypePrefix = "cosign:"

	// CosignSignatures selects the cosign signatures of a manifest (its .sig tag).
	CosignSignatures = "sig"
	// CosignAttestations selects the cosign attestations of a manifest (its .att tag).
	CosignAttestations = "att"

	mediaTypeSimpleSigning = "application/vnd.dev.cosign.simplesigning.v1+json"
	mediaTypeDSSE          = "application/vnd.dsse.envelope.v1+json"
	payloadTypeInToto      = "application/vnd.in-toto+json"

	// maxCosignLayerSize bounds a layer of a cosign artifact read to check what
	// it refers to. An attestation embeds its predicate, an SBOM, say.
	maxCosignLayerSize = 64 << 20
)

// CosignTagType returns the pseudo artifact type with which Discover finds the
// cosign artifacts tagged with suffix (CosignSignatures or CosignAttestations).
func CosignTagType(suffix string) string { return cosignTagTypePrefix + suffix }

// cosignTagSuffix returns the tag suffix of a pseudo artifact type made by
// CosignTagType.
func cosignTagSuffix(artifactType string) (string, bool) {
	suffix, ok := strings.CutPrefix(artifactType, cosignTagTypePrefix)
	if !ok {
		return "", false
	}
	return suffix, true
}

// IsCosignTagType reports whether artifactType is a pseudo artifact type made
// by CosignTagType.
func IsCosignTagType(artifactType string) bool {
	_, ok := cosignTagSuffix(artifactType)
	return ok
}

// CosignTag returns cosign's tag for the artifacts of subject with suffix: the
// digest with ":" replaced by "-", a ".", and the suffix.
func CosignTag(subject, suffix string) string {
	return strings.ReplaceAll(subject, ":", "-") + "." + suffix
}

// discoverCosign returns the digest of the manifest tagged with cosign's tag
// for the artifacts of subject with suffix, or "" if there is no such tag.
//
// As the tag is all that relates it to subject, the manifest has to prove it:
// every layer is read, and has to be what cosign writes under that tag, naming
// subject in its signed payload -- a simple signing payload's
// docker-manifest-digest for a signature, an in-toto statement's subject for
// an attestation. Anything else is an error, rather than being skipped: it's
// either not cosign's, or not about subject.
func discoverCosign(ctx context.Context, repo name.Repository, subject, suffix string, options Options) (string, error) {
	if suffix != CosignSignatures && suffix != CosignAttestations {
		return "", fmt.Errorf("unknown cosign tag suffix %q (want %q or %q)", suffix, CosignSignatures, CosignAttestations)
	}
	tag := CosignTag(subject, suffix)
	descriptor, err := remote.Get(repo.Tag(tag), append(slices.Clone(options.Remote), remote.WithContext(ctx))...)
	var terr *transport.Error
	if errors.As(err, &terr) && terr.StatusCode == http.StatusNotFound {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("pulling the cosign tag %s: %w", tag, err)
	}
	artifact, err := ReadCosignArtifact(descriptor.Manifest, RemoteLayers(ctx, repo, options))
	if err != nil {
		return "", fmt.Errorf("the cosign tag %s (%s): %w", tag, descriptor.Digest, err)
	}
	if artifact.Suffix != suffix {
		return "", fmt.Errorf("the cosign tag %s (%s) holds a cosign .%s artifact", tag, descriptor.Digest, artifact.Suffix)
	}
	if !slices.Contains(artifact.Subjects, subject) {
		return "", fmt.Errorf("the cosign tag %s (%s): its signed payloads don't name %s (but %v)", tag, descriptor.Digest, subject, artifact.Subjects)
	}
	return descriptor.Digest.String(), nil
}

// CosignArtifact describes one of cosign's tag-scheme artifacts.
type CosignArtifact struct {
	// Suffix is the suffix of the tag cosign keeps it under: CosignSignatures
	// or CosignAttestations.
	Suffix string
	// Subjects are the manifests that the signed payload of every layer names,
	// sorted: the digest a signature's simple signing payload names, or the
	// subjects of an attestation's in-toto statement.
	Subjects []string
}

// ReadCosignArtifact reads the manifest raw as one of cosign's tag-scheme
// artifacts, reading its layers with read. It fails if raw isn't one: an image
// manifest whose layers are all simple signing payloads (a signature), or all
// DSSE envelopes of in-toto statements (an attestation). The signatures
// themselves are not verified: that is up to the policy that consumes the
// artifact.
func ReadCosignArtifact(raw []byte, read func(v1.Descriptor) ([]byte, error)) (CosignArtifact, error) {
	var manifest v1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return CosignArtifact{}, fmt.Errorf("parsing the manifest: %w", err)
	}
	mediaType := manifest.MediaType
	if mediaType == "" && manifest.Config.Digest.Hex != "" {
		// The mediaType field is optional: a manifest with a config is an image
		// manifest (the Starlark side infers it the same way).
		mediaType = types.OCIManifestSchema1
	}
	if mediaType != types.OCIManifestSchema1 && mediaType != types.DockerManifestSchema2 {
		return CosignArtifact{}, fmt.Errorf("it is a %q, not an image manifest", manifest.MediaType)
	}
	if len(manifest.Layers) == 0 {
		return CosignArtifact{}, fmt.Errorf("it has no layers")
	}
	var artifact CosignArtifact
	for i, layer := range manifest.Layers {
		if layer.Size > maxCosignLayerSize {
			return CosignArtifact{}, fmt.Errorf("layer %s is larger than %d bytes", layer.Digest, maxCosignLayerSize)
		}
		data, err := read(layer)
		if err != nil {
			return CosignArtifact{}, fmt.Errorf("reading layer %s: %w", layer.Digest, err)
		}
		suffix, subjects, err := payloadSubjects(string(layer.MediaType), data)
		if err != nil {
			return CosignArtifact{}, fmt.Errorf("layer %s: %w", layer.Digest, err)
		}
		if i == 0 {
			artifact = CosignArtifact{Suffix: suffix, Subjects: subjects}
			continue
		}
		if suffix != artifact.Suffix {
			return CosignArtifact{}, fmt.Errorf("it mixes cosign signatures and attestations")
		}
		artifact.Subjects = slices.DeleteFunc(artifact.Subjects, func(s string) bool { return !slices.Contains(subjects, s) })
	}
	slices.Sort(artifact.Subjects)
	artifact.Subjects = slices.Compact(artifact.Subjects)
	return artifact, nil
}

// payloadSubjects returns the tag suffix of a layer of a cosign artifact, and
// the manifests its signed payload names.
func payloadSubjects(mediaType string, data []byte) (string, []string, error) {
	switch mediaType {
	case mediaTypeSimpleSigning:
		var payload struct {
			Critical struct {
				Image struct {
					DockerManifestDigest string `json:"docker-manifest-digest"`
				} `json:"image"`
			} `json:"critical"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			return "", nil, fmt.Errorf("parsing the simple signing payload: %w", err)
		}
		digest := payload.Critical.Image.DockerManifestDigest
		if digest == "" {
			return "", nil, fmt.Errorf("the simple signing payload names no manifest")
		}
		return CosignSignatures, []string{digest}, nil
	case mediaTypeDSSE:
		var envelope struct {
			PayloadType string `json:"payloadType"`
			Payload     string `json:"payload"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			return "", nil, fmt.Errorf("parsing the DSSE envelope: %w", err)
		}
		if envelope.PayloadType != payloadTypeInToto {
			return "", nil, fmt.Errorf("the DSSE envelope has the payload type %q, not %q", envelope.PayloadType, payloadTypeInToto)
		}
		payload, err := base64.StdEncoding.DecodeString(envelope.Payload)
		if err != nil {
			if payload, err = base64.RawURLEncoding.DecodeString(envelope.Payload); err != nil {
				return "", nil, fmt.Errorf("decoding the DSSE payload: %w", err)
			}
		}
		var statement struct {
			Subject []struct {
				Digest map[string]string `json:"digest"`
			} `json:"subject"`
		}
		if err := json.NewDecoder(bytes.NewReader(payload)).Decode(&statement); err != nil {
			return "", nil, fmt.Errorf("parsing the in-toto statement: %w", err)
		}
		var subjects []string
		for _, s := range statement.Subject {
			for algorithm, encoded := range s.Digest {
				subjects = append(subjects, algorithm+":"+encoded)
			}
		}
		return CosignAttestations, subjects, nil
	}
	return "", nil, fmt.Errorf("a layer of media type %q is not one of cosign's signatures or attestations", mediaType)
}

// RemoteLayers reads the layers of a manifest in repo from the registry, for
// ReadCosignArtifact and AttributeCosignArtifact.
func RemoteLayers(ctx context.Context, repo name.Repository, options Options) func(v1.Descriptor) ([]byte, error) {
	return func(layer v1.Descriptor) ([]byte, error) {
		blob, err := remote.Layer(repo.Digest(layer.Digest.String()), append(slices.Clone(options.Remote), remote.WithContext(ctx))...)
		if err != nil {
			return nil, err
		}
		rc, err := blob.Compressed()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		data, err := io.ReadAll(io.LimitReader(rc, maxCosignLayerSize+1))
		if err != nil {
			return nil, err
		}
		// The read stops at the limit, before the digest is verified: a blob
		// larger than its descriptor says must be caught here.
		if int64(len(data)) != layer.Size {
			return nil, fmt.Errorf("blob %s doesn't have the %d bytes its descriptor says", layer.Digest, layer.Size)
		}
		return data, nil
	}
}

// AttributeCosignArtifact returns the manifest of tree that the cosign
// tag-scheme artifact raw (whose layers read returns) is about: the one its
// signed payloads name. It fails if they name none of tree, or several.
func AttributeCosignArtifact(raw []byte, read func(v1.Descriptor) ([]byte, error), tree []string) (CosignArtifact, string, error) {
	artifact, err := ReadCosignArtifact(raw, read)
	if err != nil {
		return CosignArtifact{}, "", err
	}
	var inTree []string
	for _, subject := range artifact.Subjects {
		if slices.Contains(tree, subject) {
			inTree = append(inTree, subject)
		}
	}
	switch len(inTree) {
	case 1:
		return artifact, inTree[0], nil
	case 0:
		return CosignArtifact{}, "", fmt.Errorf("its signed payloads name %v, none of which is part of the image (%v)", artifact.Subjects, tree)
	default:
		return CosignArtifact{}, "", fmt.Errorf("its signed payloads name several manifests of the image: %v", inTree)
	}
}
