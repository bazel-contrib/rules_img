package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	// attestationArtifactType is the artifactType of BuildKit-style attestation
	// manifests, which the provenance signer plugin attaches as referrers.
	attestationArtifactType = "application/vnd.docker.attestation.manifest.v1+json"
	inTotoMediaType         = "application/vnd.in-toto+json"
	emptyConfigMediaType    = "application/vnd.oci.empty.v1+json"
	inTotoStatementV1       = "https://in-toto.io/Statement/v1"
	slsaProvenanceV1        = "https://slsa.dev/provenance/v1"
)

// verifyProvenance checks that every platform image manifest of repo:tag has
// exactly one provenance attestation referrer, and that its content is bound to
// that manifest. There is no upstream CLI that verifies an unsigned BuildKit
// attestation, so this reads the in-toto statement directly.
func (sc *signatureChecker) verifyProvenance(reg *registryClient, a *ProvenanceAssertion) {
	sc.provenanceRuns = nil
	root, err := reg.getManifest(a.Repository, a.Tag)
	if err != nil {
		sc.errf("provenance: resolving %s:%s: %v", a.Repository, a.Tag, err)
		return
	}

	type target struct {
		digest   string
		platform string
	}
	var targets []target
	if root.isIndex() {
		idx, err := root.asIndex()
		if err != nil {
			sc.errf("provenance: %v", err)
			return
		}
		for _, d := range idx.Manifests {
			// Skip non-image children (e.g. BuildKit-style attestations
			// listed in the index itself).
			if d.Platform == nil || d.Platform.OS == "unknown" {
				continue
			}
			p := normalizedPlatform(d.Platform.OS, d.Platform.Architecture, d.Platform.Variant)
			targets = append(targets, target{digest: d.Digest.String(), platform: p})
		}
	} else {
		targets = append(targets, target{digest: root.digest})
	}
	if len(targets) == 0 {
		sc.errf("provenance: %s:%s has no platform manifests to attest", a.Repository, a.Tag)
		return
	}

	for _, t := range targets {
		sc.verifyProvenanceFor(reg, a, t.digest, t.platform)
	}

	// Every attestation of one deploy shares its invocation and start time, like
	// the platforms of one BuildKit build do.
	for i := 1; i < len(sc.provenanceRuns); i++ {
		if r := sc.provenanceRuns[i]; r != sc.provenanceRuns[0] {
			sc.errf("provenance: attestations of one deploy must share invocationId and startedOn, got %+v and %+v", sc.provenanceRuns[0], r)
			break
		}
	}
}

// normalizedPlatform spells a platform the way BuildKit records it (containerd's
// normalization): the default variant is dropped, so an index entry for
// linux/arm64/v8 is attested as linux/arm64.
func normalizedPlatform(os, arch, variant string) string {
	switch arch {
	case "arm64":
		if variant == "v8" || variant == "8" {
			variant = ""
		}
	case "amd64":
		if variant == "v1" {
			variant = ""
		}
	}
	p := os + "/" + arch
	if variant != "" {
		p += "/" + variant
	}
	return p
}

func (sc *signatureChecker) verifyProvenanceFor(reg *registryClient, a *ProvenanceAssertion, digest, platform string) {
	label := fmt.Sprintf("provenance: %s@%s", a.Repository, digest)
	idx, err := reg.referrers(a.Repository, digest)
	if err != nil {
		sc.errf("%s: listing referrers: %v", label, err)
		return
	}
	var found []string
	for _, d := range idx.Manifests {
		if reg.effectiveArtifactType(a.Repository, d) == attestationArtifactType {
			found = append(found, d.Digest.String())
		}
	}
	if len(found) != 1 {
		sc.errf("%s: want exactly 1 referrer with artifactType %q, got %d", label, attestationArtifactType, len(found))
		return
	}

	m, err := reg.getManifest(a.Repository, found[0])
	if err != nil {
		sc.errf("%s: %v", label, err)
		return
	}
	man, err := m.asManifest()
	if err != nil {
		sc.errf("%s: %v", label, err)
		return
	}
	if man.Subject == nil || man.Subject.Digest.String() != digest {
		sc.errf("%s: attestation manifest subject = %v, want %s", label, man.Subject, digest)
	}
	if man.Subject != nil && man.Subject.Platform != nil {
		sc.errf("%s: attestation manifest subject must not carry a platform (BuildKit does not set one)", label)
	}
	if string(man.Config.MediaType) != emptyConfigMediaType {
		sc.errf("%s: config mediaType = %q, want the OCI empty descriptor", label, man.Config.MediaType)
	}
	if len(man.Layers) != 1 {
		sc.errf("%s: want 1 layer, got %d", label, len(man.Layers))
		return
	}
	layer := man.Layers[0]
	if string(layer.MediaType) != inTotoMediaType {
		sc.errf("%s: layer mediaType = %q, want %q", label, layer.MediaType, inTotoMediaType)
	}
	if got := layer.Annotations["in-toto.io/predicate-type"]; got != slsaProvenanceV1 {
		sc.errf("%s: layer predicate-type annotation = %q, want %q", label, got, slsaProvenanceV1)
	}

	body, err := reg.getBlob(a.Repository, layer.Digest.String())
	if err != nil {
		sc.errf("%s: %v", label, err)
		return
	}
	var st struct {
		Type          string `json:"_type"`
		PredicateType string `json:"predicateType"`
		Subject       []struct {
			Name   string            `json:"name"`
			Digest map[string]string `json:"digest"`
		} `json:"subject"`
		Predicate struct {
			BuildDefinition struct {
				BuildType          string         `json:"buildType"`
				InternalParameters map[string]any `json:"internalParameters"`
			} `json:"buildDefinition"`
			RunDetails struct {
				Builder struct {
					ID      string            `json:"id"`
					Version map[string]string `json:"version"`
				} `json:"builder"`
				Metadata struct {
					InvocationID string `json:"invocationId"`
					StartedOn    string `json:"startedOn"`
					FinishedOn   string `json:"finishedOn"`
					Completeness struct {
						Request              bool `json:"request"`
						ResolvedDependencies bool `json:"resolvedDependencies"`
					} `json:"buildkit_completeness"`
				} `json:"metadata"`
			} `json:"runDetails"`
		} `json:"predicate"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		sc.errf("%s: statement is not valid JSON: %v", label, err)
		return
	}
	if st.Type != inTotoStatementV1 || st.PredicateType != slsaProvenanceV1 {
		sc.errf("%s: statement type/predicateType = %q / %q", label, st.Type, st.PredicateType)
	}
	if len(st.Subject) == 0 {
		sc.errf("%s: statement has no subject", label)
	}
	for _, s := range st.Subject {
		if s.Digest["sha256"] != strings.TrimPrefix(digest, "sha256:") {
			sc.errf("%s: statement subject digest %v is not bound to the image", label, s.Digest)
		}
		if platform != "" && s.Name != "" && !strings.HasSuffix(s.Name, "platform="+strings.ReplaceAll(platform, "/", "%2F")) {
			sc.errf("%s: subject name %q does not carry platform %s", label, s.Name, platform)
		}
	}
	if platform != "" {
		if got := st.Predicate.BuildDefinition.InternalParameters["targetPlatform"]; got != platform {
			sc.errf("%s: targetPlatform = %v, want %s", label, got, platform)
		}
	}
	if a.BuilderID != "" && st.Predicate.RunDetails.Builder.ID != a.BuilderID {
		sc.errf("%s: builder.id = %q, want %q", label, st.Predicate.RunDetails.Builder.ID, a.BuilderID)
	}
	md := st.Predicate.RunDetails.Metadata
	if st.Predicate.RunDetails.Builder.Version["rules_img"] == "" {
		sc.errf("%s: builder.version.rules_img missing: the request protocol should tell the plugin the rules_img version", label)
	}
	if md.InvocationID == "" {
		sc.errf("%s: invocationId missing", label)
	}
	for _, s := range st.Subject {
		if !strings.HasPrefix(s.Name, "pkg:docker/") || !strings.Contains(s.Name, a.Repository) {
			sc.errf("%s: subject name %q should be a pkg:docker purl of the pushed repository %s (taken from the request, not from args)", label, s.Name, a.Repository)
		}
	}
	sc.provenanceRuns = append(sc.provenanceRuns, provenanceRun{InvocationID: md.InvocationID, StartedOn: md.StartedOn})
	if a.RecordTimestamps && (md.StartedOn == "" || md.FinishedOn == "") {
		sc.errf("%s: startedOn/finishedOn missing although the signing config records timestamps", label)
	}
	if md.Completeness.Request || md.Completeness.ResolvedDependencies {
		sc.errf("%s: a post-hoc attestation must not claim completeness: %+v", label, md.Completeness)
	}
}
