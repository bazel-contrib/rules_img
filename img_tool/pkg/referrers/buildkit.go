package referrers

import (
	"encoding/json"
	"fmt"
	"slices"
)

// BuildKit (docker buildx build --sbom, --provenance) stores the attestations of an image (SBOMs,
// SLSA provenance) in the image index, not as referrers: after the runnable manifests, an
// attestation manifest per platform manifest, with the platform unknown/unknown and annotations
// naming what it is and which manifest it is about. Each of its layers is an in-toto statement
// (application/vnd.in-toto+json) whose predicate type the layer's in-toto.io/predicate-type
// annotation names. With OCI artifacts (BuildKit's default since compatibility version 0.31), the
// attestation manifest also has the artifact type application/vnd.docker.attestation.manifest.v1+json
// and a subject, so a registry with the referrers API lists it as a referrer too; with
// oci-artifact=false it has neither, and only the index ties it to its manifest. See
// https://github.com/moby/buildkit/blob/master/docs/attestations/attestation-storage.md.

const (
	// BuildKitAttestationType is the pseudo artifact type with which Discover finds the BuildKit
	// attestation manifests that Options.Index lists for a manifest.
	BuildKitAttestationType = "buildkit:attestation"

	// BuildKitAttestationArtifactType is the artifact type of a BuildKit attestation manifest
	// stored as an OCI artifact.
	BuildKitAttestationArtifactType = "application/vnd.docker.attestation.manifest.v1+json"

	buildKitReferenceType       = "vnd.docker.reference.type"
	buildKitReferenceDigest     = "vnd.docker.reference.digest"
	buildKitAttestationManifest = "attestation-manifest"
)

// buildKitEntries returns the attestation manifests an image index lists, mapped to the digest of
// the manifest each is about. An index without any, or a manifest that isn't an index, has none.
func buildKitEntries(index []byte) (map[string]string, error) {
	if len(index) == 0 {
		return nil, nil
	}
	var parsed struct {
		Manifests []struct {
			Digest      string            `json:"digest"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(index, &parsed); err != nil {
		return nil, fmt.Errorf("parsing the image index: %w", err)
	}
	entries := map[string]string{}
	for _, m := range parsed.Manifests {
		// "If any other value is specified, the entire manifest should be ignored."
		if m.Annotations[buildKitReferenceType] != buildKitAttestationManifest {
			continue
		}
		if subject := m.Annotations[buildKitReferenceDigest]; subject != "" {
			entries[m.Digest] = subject
		}
	}
	return entries, nil
}

// BuildKitAttestations returns the sorted digests of the BuildKit attestation manifests that the
// image index (raw) lists for the manifest subject.
func BuildKitAttestations(index []byte, subject string) ([]string, error) {
	entries, err := buildKitEntries(index)
	if err != nil {
		return nil, err
	}
	found := []string{}
	for digest, about := range entries {
		if about == subject {
			found = append(found, digest)
		}
	}
	slices.Sort(found)
	return found, nil
}

// BuildKitAttestationSubject returns the manifest that the image index (raw) lists the BuildKit
// attestation manifest digest for, and whether it lists it at all. It attributes an attestation
// manifest without a subject field (BuildKit's oci-artifact=false format).
func BuildKitAttestationSubject(index []byte, digest string) (string, bool, error) {
	entries, err := buildKitEntries(index)
	if err != nil {
		return "", false, err
	}
	subject, ok := entries[digest]
	return subject, ok, nil
}
