package main

import (
	"fmt"
	"sort"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// checker runs assertions against a registry, accumulating human-readable
// failures rather than stopping at the first one.
type checker struct {
	c    *registryClient
	errs []string
}

func (ck *checker) errf(format string, args ...any) {
	ck.errs = append(ck.errs, fmt.Sprintf(format, args...))
}

// checkImage runs every assertion configured for a single tagged image.
func (ck *checker) checkImage(img ImageAssertion) {
	ref := fmt.Sprintf("%s:%s", img.Repository, img.Tag)
	m, err := ck.c.getManifest(img.Repository, img.Tag)
	if err != nil {
		ck.errf("%s: could not resolve tag: %v", ref, err)
		return
	}

	if img.Structure != nil {
		ck.checkStructure(ref, img.Repository, m, img.Structure)
	}
	if len(img.Aliases) > 0 {
		ck.checkAliases(img.Repository, img.Tag, m.digest, img.Aliases)
	}
	if img.ClosureIntact {
		ck.checkClosure(img.Repository, img.Tag)
	}
	if len(img.Referrers) > 0 {
		ck.checkReferrers(img.Repository, m.digest, img.Referrers)
	}
}

func (ck *checker) checkStructure(ref, repo string, m *fetchedManifest, s *StructureAssertion) {
	kind := "manifest"
	if m.isIndex() {
		kind = "index"
	}
	if s.Kind != "" && s.Kind != kind {
		ck.errf("%s: expected kind %q, got %q", ref, s.Kind, kind)
	}

	if m.isIndex() {
		idx, err := m.asIndex()
		if err != nil {
			ck.errf("%s: %v", ref, err)
			return
		}
		if len(s.Platforms) > 0 {
			if diff := platformDiff(s.Platforms, idx); diff != "" {
				ck.errf("%s: platforms mismatch: %s", ref, diff)
			}
		}
		if s.Layers != nil {
			ck.errf("%s: 'layers' assertion is only valid for single-arch manifests, not an index", ref)
		}
		ck.checkAnnotations(ref, idx.Annotations, s.Annotations)
		if s.ArtifactType != "" && idx.ArtifactType != s.ArtifactType {
			ck.errf("%s: expected artifact_type %q, got %q", ref, s.ArtifactType, idx.ArtifactType)
		}
		if len(s.Labels) > 0 {
			ck.errf("%s: 'labels' assertion is only valid for single-arch manifests, not an index", ref)
		}
		return
	}

	man, err := m.asManifest()
	if err != nil {
		ck.errf("%s: %v", ref, err)
		return
	}
	if len(s.Platforms) > 0 {
		ck.errf("%s: 'platforms' assertion is only valid for an index, not a single-arch manifest", ref)
	}
	if s.Layers != nil && *s.Layers != len(man.Layers) {
		ck.errf("%s: expected %d layers, got %d", ref, *s.Layers, len(man.Layers))
	}
	if s.ConfigMediaType != "" && string(man.Config.MediaType) != s.ConfigMediaType {
		ck.errf("%s: expected config media type %q, got %q", ref, s.ConfigMediaType, man.Config.MediaType)
	}
	if s.ArtifactType != "" && man.ArtifactType != s.ArtifactType {
		ck.errf("%s: expected artifact_type %q, got %q", ref, s.ArtifactType, man.ArtifactType)
	}
	ck.checkAnnotations(ref, man.Annotations, s.Annotations)
	if len(s.Labels) > 0 {
		labels, err := ck.c.configLabels(repo, man.Config)
		if err != nil {
			ck.errf("%s: reading config labels: %v", ref, err)
		} else {
			for k, want := range s.Labels {
				if got, ok := labels[k]; !ok || got != want {
					ck.errf("%s: expected label %q=%q, got %q (present=%t)", ref, k, want, got, ok)
				}
			}
		}
	}
}

func (ck *checker) checkAnnotations(ref string, got, want map[string]string) {
	for k, v := range want {
		if actual, ok := got[k]; !ok || actual != v {
			ck.errf("%s: expected annotation %q=%q, got %q (present=%t)", ref, k, v, actual, ok)
		}
	}
}

func (ck *checker) checkAliases(repo, tag, tagDigest string, aliases []string) {
	for _, alias := range aliases {
		aliasDigest, err := ck.c.headDigest(repo, alias)
		if err != nil {
			ck.errf("%s:%s alias %q: could not resolve: %v", repo, tag, alias, err)
			continue
		}
		if aliasDigest != tagDigest {
			ck.errf("%s: tag %q (%s) and alias %q (%s) resolve to different digests", repo, tag, tagDigest, alias, aliasDigest)
		}
	}
}

// checkClosure verifies that every descriptor reachable from ref — child
// manifests of an index, config + layer blobs of a manifest, and any referrers
// (recursively) — is actually present in the registry.
func (ck *checker) checkClosure(repo, ref string) {
	visited := map[string]bool{}
	var walk func(r string)
	walk = func(r string) {
		m, err := ck.c.getManifest(repo, r)
		if err != nil {
			ck.errf("%s@%s: closure: %v", repo, r, err)
			return
		}
		if visited[m.digest] {
			return
		}
		visited[m.digest] = true

		if m.isIndex() {
			idx, err := m.asIndex()
			if err != nil {
				ck.errf("%s: %v", repo, err)
				return
			}
			for _, d := range idx.Manifests {
				walk(d.Digest.String())
			}
		} else {
			man, err := m.asManifest()
			if err != nil {
				ck.errf("%s: %v", repo, err)
				return
			}
			ck.requireBlob(repo, man.Config.Digest, "config")
			for i, l := range man.Layers {
				ck.requireBlob(repo, l.Digest, fmt.Sprintf("layer %d", i))
			}
		}

		// Any referrers of this manifest (and their transitive closures) must
		// also be intact.
		if idx, err := ck.c.referrers(repo, m.digest); err == nil {
			for _, d := range idx.Manifests {
				walk(d.Digest.String())
			}
		}
	}
	walk(ref)
}

func (ck *checker) requireBlob(repo string, digest v1.Hash, what string) {
	ok, err := ck.c.blobExists(repo, digest.String())
	if err != nil {
		ck.errf("%s: checking %s blob %s: %v", repo, what, digest, err)
		return
	}
	if !ok {
		ck.errf("%s: %s blob %s missing from registry (broken closure)", repo, what, digest)
	}
}

func (ck *checker) checkReferrers(repo, digest string, expected []ReferrerAssertion) {
	idx, err := ck.c.referrers(repo, digest)
	if err != nil {
		ck.errf("%s@%s: listing referrers: %v", repo, digest, err)
		return
	}
	for _, want := range expected {
		var matches []v1.Descriptor
		for _, d := range idx.Manifests {
			if want.ArtifactType != "" && ck.c.effectiveArtifactType(repo, d) != want.ArtifactType {
				continue
			}
			if want.Kind != "" && descriptorKind(d) != want.Kind {
				continue
			}
			matches = append(matches, d)
		}
		if want.Count != nil && len(matches) != *want.Count {
			ck.errf("%s@%s: expected %d referrer(s) with artifact_type %q, found %d",
				repo, digest, *want.Count, want.ArtifactType, len(matches))
		} else if want.Count == nil && len(matches) == 0 {
			ck.errf("%s@%s: expected at least one referrer with artifact_type %q, found none",
				repo, digest, want.ArtifactType)
		}
		if len(want.Annotations) > 0 {
			if !anyDescriptorHasAnnotations(matches, want.Annotations) {
				ck.errf("%s@%s: no referrer with artifact_type %q carries annotations %v",
					repo, digest, want.ArtifactType, want.Annotations)
			}
		}
	}
}

func toSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, v := range values {
		set[v] = true
	}
	return set
}

// platformDiff compares the expected platform list against an index's child
// platforms and returns "" when they match exactly, else a human-readable
// difference. Matching is strict and variant-sensitive: a spec entry must be the
// full "os/arch" or "os/arch/variant" of the child descriptor (e.g. an arm64
// image with variant v8 must be spelled "linux/arm64/v8"). Non-platform children
// (e.g. the unknown/unknown attestation placeholder) are ignored.
func platformDiff(want []string, idx *v1.IndexManifest) string {
	var actual []string
	for _, d := range idx.Manifests {
		if d.Platform == nil || d.Platform.OS == "" || d.Platform.OS == "unknown" {
			continue
		}
		p := fmt.Sprintf("%s/%s", d.Platform.OS, d.Platform.Architecture)
		if d.Platform.Variant != "" {
			p += "/" + d.Platform.Variant
		}
		actual = append(actual, p)
	}

	wantSet := toSet(want)
	gotSet := toSet(actual)
	var missing, extra []string
	for _, w := range want {
		if !gotSet[w] {
			missing = append(missing, w)
		}
	}
	for _, a := range actual {
		if !wantSet[a] {
			extra = append(extra, a)
		}
	}

	if len(missing) == 0 && len(extra) == 0 {
		return ""
	}
	sort.Strings(missing)
	sort.Strings(extra)
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, "missing ["+strings.Join(missing, ", ")+"]")
	}
	if len(extra) > 0 {
		parts = append(parts, "unexpected ["+strings.Join(extra, ", ")+"]")
	}
	return strings.Join(parts, "; ")
}

func descriptorKind(d v1.Descriptor) string {
	if d.MediaType.IsIndex() {
		return "index"
	}
	return "manifest"
}

func anyDescriptorHasAnnotations(descs []v1.Descriptor, want map[string]string) bool {
	for _, d := range descs {
		match := true
		for k, v := range want {
			if got, ok := d.Annotations[k]; !ok || got != v {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
