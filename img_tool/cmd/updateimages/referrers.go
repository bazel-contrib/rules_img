package updateimages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	imgreferrers "github.com/bazel-contrib/rules_img/img_tool/pkg/referrers"
)

// Referrer is an artifact that applies to a pulled manifest: a referrer in the
// sense of the OCI distribution spec (found with the referrers API or the
// referrers tag schema), or one of cosign's tag-scheme signatures and
// attestations.
type Referrer struct {
	Digest string `json:"digest"`
	// Subject is the manifest the referrer applies to.
	Subject         string `json:"subject"`
	SubjectPlatform string `json:"subject_platform,omitempty"`
	// Type is what --artifact-type filters on: the artifact type (the
	// artifactType of the manifest, or else the media type of its config),
	// cosign:sig / cosign:att for cosign's tag-scheme artifacts, or
	// buildkit:attestation for the BuildKit attestation manifests an index
	// lists.
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
	// Source is how the referrer was found: referrers-api, tag-schema,
	// cosign-tag, or buildkit-index.
	Source string `json:"source"`
	// CosignTag is the tag of a cosign tag-scheme artifact.
	CosignTag   string            `json:"cosign_tag,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Layers      []Layer           `json:"layers,omitempty"`
	// Pinned tells whether referrer_digests already lists it.
	Pinned bool `json:"pinned"`
}

// Layer describes a layer (blob) of a referrer.
type Layer struct {
	MediaType   string            `json:"media_type"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

const (
	sourceCosignTag     = "cosign-tag"
	sourceBuildKitIndex = "buildkit-index"
)

var cosignSuffixes = []string{imgreferrers.CosignSignatures, imgreferrers.CosignAttestations}

// typeFilter selects referrers by type. Patterns may contain * wildcards. An
// empty filter selects everything.
type typeFilter struct {
	patterns []string
	regexps  []*regexp.Regexp
}

func newTypeFilter(patterns []string) typeFilter {
	f := typeFilter{patterns: patterns}
	for _, pattern := range patterns {
		quoted := strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*")
		f.regexps = append(f.regexps, regexp.MustCompile("^"+quoted+"$"))
	}
	return f
}

func (f typeFilter) matches(t string) bool {
	if len(f.regexps) == 0 {
		return true
	}
	for _, re := range f.regexps {
		if re.MatchString(t) {
			return true
		}
	}
	return false
}

// isPseudoType tells whether t is a pseudo artifact type, which finds
// artifacts without asking the referrers API.
func isPseudoType(t string) bool {
	return imgreferrers.IsCosignTagType(t) || t == imgreferrers.BuildKitAttestationType
}

// wantsOCI tells whether the filter can match any referrer other than
// cosign's tag-scheme artifacts and the BuildKit attestations of an index.
func (f typeFilter) wantsOCI() bool {
	if len(f.patterns) == 0 {
		return true
	}
	for _, pattern := range f.patterns {
		if !isPseudoType(pattern) {
			return true
		}
	}
	return false
}

// apiFilter is the artifact type to pass to the referrers API, if the filter
// selects exactly one.
func (f typeFilter) apiFilter() []string {
	if len(f.patterns) == 1 && !strings.Contains(f.patterns[0], "*") && !isPseudoType(f.patterns[0]) {
		return f.patterns
	}
	return nil
}

// discover lists the referrers of every subject. Referrers whose type the
// filter doesn't select are dropped. Sources are tried in order until one has
// any: a mirror may well answer, but without the referrers of the original.
// index is the raw image index the subjects belong to (nil if none), whose
// entries name their BuildKit attestations.
func (c *registryClient) discover(ctx context.Context, sources []source, subjects []subject, filter typeFilter, index []byte) ([]Referrer, error) {
	var errs []error
	answered := false
	for _, src := range sources {
		repo, err := src.repository()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", src, err))
			continue
		}
		var all []Referrer
		for _, s := range subjects {
			var referrers []Referrer
			referrers, err = c.discoverFrom(ctx, repo, s, filter, index)
			if err != nil {
				break
			}
			all = append(all, referrers...)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", src, err))
			continue
		}
		if len(all) > 0 {
			return all, nil
		}
		answered = true
	}
	if answered {
		return nil, nil
	}
	return nil, errors.Join(errs...)
}

func (c *registryClient) discoverFrom(ctx context.Context, repo name.Repository, s subject, filter typeFilter, index []byte) ([]Referrer, error) {
	var found []Referrer
	if filter.wantsOCI() {
		descriptors, listing, err := imgreferrers.List(ctx, repo, s.Digest, filter.apiFilter(), c.referrers)
		if err != nil {
			return nil, err
		}
		for _, descriptor := range descriptors {
			// The referrers API is trusted to report the artifact type; the
			// tag schema is not, since clients maintain it.
			if listing == imgreferrers.SourceAPI && !filter.matches(descriptor.ArtifactType) {
				continue
			}
			got, err := c.getManifestFrom(ctx, repo, descriptor.Digest.String())
			if err != nil {
				return nil, fmt.Errorf("fetching referrer %s of %s: %w", descriptor.Digest, s.Digest, err)
			}
			if listing == imgreferrers.SourceTagSchema && (got.manifest.Subject == nil || got.manifest.Subject.Digest.String() != s.Digest) {
				continue
			}
			referrer := newReferrer(descriptor.Digest.String(), s, got, string(listing))
			if listing == imgreferrers.SourceAPI && descriptor.ArtifactType != "" {
				referrer.Type = descriptor.ArtifactType
			}
			if !filter.matches(referrer.Type) || slices.ContainsFunc(found, func(r Referrer) bool { return r.Digest == referrer.Digest }) {
				continue
			}
			found = append(found, referrer)
		}
	}
	for _, suffix := range cosignSuffixes {
		if !filter.matches(imgreferrers.CosignTagType(suffix)) {
			continue
		}
		referrer, ok, err := c.cosignArtifact(ctx, repo, s, suffix)
		if err != nil {
			return nil, err
		}
		if ok {
			found = append(found, referrer)
		}
	}
	if filter.matches(imgreferrers.BuildKitAttestationType) {
		attestations, err := imgreferrers.BuildKitAttestations(index, s.Digest)
		if err != nil {
			return nil, err
		}
		for _, digest := range attestations {
			if slices.ContainsFunc(found, func(r Referrer) bool { return r.Digest == digest }) {
				// Also listed as an OCI artifact, by the referrers API.
				continue
			}
			got, err := c.getManifestFrom(ctx, repo, digest)
			if err != nil {
				return nil, fmt.Errorf("fetching the BuildKit attestation %s of %s: %w", digest, s.Digest, err)
			}
			referrer := newReferrer(digest, s, got, sourceBuildKitIndex)
			referrer.Type = imgreferrers.BuildKitAttestationType
			found = append(found, referrer)
		}
	}
	return found, nil
}

// cosignArtifact reads cosign's tag-scheme artifact of subject with suffix.
// Like the images extension, it checks that every layer names the subject in
// its signed payload, so what it finds can be pinned.
func (c *registryClient) cosignArtifact(ctx context.Context, repo name.Repository, s subject, suffix string) (Referrer, bool, error) {
	tag := imgreferrers.CosignTag(s.Digest, suffix)
	descriptor, err := remote.Get(repo.Tag(tag), c.options(ctx)...)
	if isNotFound(err) {
		return Referrer{}, false, nil
	} else if err != nil {
		return Referrer{}, false, fmt.Errorf("pulling the cosign tag %s: %w", tag, err)
	}
	artifact, err := imgreferrers.ReadCosignArtifact(descriptor.Manifest, imgreferrers.RemoteLayers(ctx, repo, c.referrers))
	if err != nil {
		return Referrer{}, false, fmt.Errorf("the cosign tag %s (%s): %w", tag, descriptor.Digest, err)
	}
	if artifact.Suffix != suffix || !slices.Contains(artifact.Subjects, s.Digest) {
		return Referrer{}, false, fmt.Errorf("the cosign tag %s (%s) is not a cosign .%s artifact of %s", tag, descriptor.Digest, suffix, s.Digest)
	}
	got := fetched{raw: descriptor.Manifest, size: descriptor.Size}
	if err := json.Unmarshal(descriptor.Manifest, &got.manifest); err != nil {
		return Referrer{}, false, fmt.Errorf("parsing the cosign tag %s: %w", tag, err)
	}
	referrer := newReferrer(descriptor.Digest.String(), s, got, sourceCosignTag)
	referrer.Type = imgreferrers.CosignTagType(suffix)
	referrer.CosignTag = tag
	return referrer, true, nil
}

func newReferrer(digest string, s subject, got fetched, source string) Referrer {
	referrer := Referrer{
		Digest:          digest,
		Subject:         s.Digest,
		SubjectPlatform: s.Platform,
		Type:            got.manifest.artifactType(),
		MediaType:       got.manifest.MediaType,
		Size:            got.size,
		Source:          source,
		Annotations:     got.manifest.Annotations,
	}
	for _, layer := range got.manifest.Layers {
		referrer.Layers = append(referrer.Layers, Layer{MediaType: string(layer.MediaType), Size: layer.Size, Annotations: layer.Annotations})
	}
	return referrer
}

// pinnedReferrer describes a referrer that referrer_digests pins: its type
// and the manifest of tree it applies to.
type pinnedReferrer struct {
	digest  string
	typ     string
	subject string
}

// inspectPinned finds out what a pinned referrer is: a referrer with a
// subject, a BuildKit attestation manifest that index (the raw image index of
// tree, or nil) lists for one of its manifests, or a cosign tag-scheme
// artifact, which is attributed to the manifest of tree that its signed
// payloads name.
func (c *registryClient) inspectPinned(ctx context.Context, sources []source, digest string, tree []string, index []byte) (pinnedReferrer, error) {
	return firstSourceValue(sources, func(_ source, repo name.Repository) (pinnedReferrer, error) {
		got, err := c.getManifestFrom(ctx, repo, digest)
		if err != nil {
			return pinnedReferrer{}, err
		}
		if got.manifest.Subject != nil {
			return pinnedReferrer{digest: digest, typ: got.manifest.artifactType(), subject: got.manifest.Subject.Digest.String()}, nil
		}
		// A BuildKit attestation manifest without a subject (oci-artifact=false):
		// the index names the manifest it is about.
		if subject, ok, err := imgreferrers.BuildKitAttestationSubject(index, digest); err != nil {
			return pinnedReferrer{}, err
		} else if ok && slices.Contains(tree, subject) {
			return pinnedReferrer{digest: digest, typ: imgreferrers.BuildKitAttestationType, subject: subject}, nil
		}
		artifact, attributed, err := imgreferrers.AttributeCosignArtifact(got.raw, imgreferrers.RemoteLayers(ctx, repo, c.referrers), tree)
		if err != nil {
			return pinnedReferrer{}, fmt.Errorf("referrer %s has no subject and is not a cosign signature or attestation of the image: %w", digest, err)
		}
		return pinnedReferrer{digest: digest, typ: imgreferrers.CosignTagType(artifact.Suffix), subject: attributed}, nil
	})
}

func firstSourceValue[T any](sources []source, op func(source, name.Repository) (T, error)) (T, error) {
	value, _, err := firstSource(sources, op)
	return value, err
}

// subjectMapping maps the manifests of an image's old tree to the
// corresponding manifests of its new tree: the root to the root, and a child
// to the child of the same platform.
type subjectMapping struct {
	old, new []subject
	// oldIndex and newIndex are the raw roots of the trees if they are image
	// indexes, whose entries name the BuildKit attestations of the children.
	oldIndex, newIndex []byte
}

func (m subjectMapping) mapDigest(digest string) (subject, error) {
	if digest == m.old[0].Digest {
		return m.new[0], nil
	}
	var platform string
	for _, s := range m.old[1:] {
		if s.Digest == digest {
			platform = s.Platform
		}
	}
	if platform == "" {
		return subject{}, fmt.Errorf("%s is not part of the image", digest)
	}
	if countPlatform(m.old[1:], platform) > 1 {
		return subject{}, fmt.Errorf("the image has more than one %s manifest", platform)
	}
	var matches []subject
	for _, s := range m.new[1:] {
		if s.Platform == platform {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 0:
		return subject{}, fmt.Errorf("the new image has no %s manifest", platform)
	case 1:
		return matches[0], nil
	}
	return subject{}, fmt.Errorf("the new image has more than one %s manifest", platform)
}

func countPlatform(subjects []subject, platform string) int {
	n := 0
	for _, s := range subjects {
		if s.Platform == platform {
			n++
		}
	}
	return n
}

func (m subjectMapping) oldDigests() []string {
	digests := make([]string, 0, len(m.old))
	for _, s := range m.old {
		digests = append(digests, s.Digest)
	}
	return digests
}

// repin finds the referrers of the new tree that correspond to the pinned
// referrers of the old tree: for each pinned referrer, the referrers of the
// same type that apply to the corresponding manifest. It returns the new
// digests (sorted), and notes about what has no counterpart.
func (c *registryClient) repin(ctx context.Context, sources []source, pinned []string, scope []string, mapping subjectMapping) ([]string, []string, error) {
	var notes []string
	wanted := map[subject][]string{}
	for _, digest := range pinned {
		info, err := c.inspectPinned(ctx, sources, digest, mapping.oldDigests(), mapping.oldIndex)
		if err != nil {
			return nil, nil, err
		}
		if len(scope) > 0 && !slices.Contains(scope, info.subject) {
			return nil, nil, fmt.Errorf("pinned referrer %s applies to %s, which is not part of %v", digest, info.subject, scope)
		}
		target, err := mapping.mapDigest(info.subject)
		if err != nil {
			notes = append(notes, fmt.Sprintf("dropping pinned %s referrer %s: %v", info.typ, digest, err))
			continue
		}
		if !slices.Contains(wanted[target], info.typ) {
			wanted[target] = append(wanted[target], info.typ)
		}
	}
	var digests []string
	for target, types := range wanted {
		found, err := c.discover(ctx, sources, []subject{target}, newTypeFilter(types), mapping.newIndex)
		if err != nil {
			return nil, nil, fmt.Errorf("discovering referrers of %s: %w", target.Digest, err)
		}
		for _, typ := range types {
			if !slices.ContainsFunc(found, func(r Referrer) bool { return r.Type == typ }) {
				notes = append(notes, fmt.Sprintf("the new %s has no %s referrer; dropping the pinned one", target.label(), typ))
			}
		}
		for _, referrer := range found {
			if !slices.Contains(digests, referrer.Digest) {
				digests = append(digests, referrer.Digest)
			}
		}
	}
	sort.Strings(digests)
	sort.Strings(notes)
	return digests, notes, nil
}
