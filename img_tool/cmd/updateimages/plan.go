package updateimages

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
)

// Status says what came of an entry.
type Status string

const (
	StatusListed   Status = "listed"
	StatusChanged  Status = "changed"
	StatusUpToDate Status = "up-to-date"
	StatusSkipped  Status = "skipped"
	StatusError    Status = "error"
)

// result is what a command found out about one entry, and the changes it
// proposes.
type result struct {
	Entry  *Entry `json:"entry"`
	Status Status `json:"status"`

	NewDigest    string    `json:"new_digest,omitempty"`
	NewTag       string    `json:"new_tag,omitempty"`
	NewManifest  string    `json:"new_manifest,omitempty"`
	RemoveUnsafe bool      `json:"remove_unsafe_allow_tag_without_digest,omitempty"`
	AddReferrers []string  `json:"add_referrer_digests,omitempty"`
	SetReferrers *[]string `json:"set_referrer_digests,omitempty"`

	Referrers []Referrer `json:"referrers,omitempty"`
	Notes     []string   `json:"notes,omitempty"`
	Error     string     `json:"error,omitempty"`
	Commands  []string   `json:"commands,omitempty"`

	// referrersChecked tells whether Referrers is the outcome of discovering
	// referrers (and so complete, even if empty).
	referrersChecked bool

	// oldTree and newTree are the manifests of the image before and after
	// a change of its digest, if they were fetched.
	oldTree, newTree []subject
}

func (r *result) fail(format string, args ...any) *result {
	r.Status = StatusError
	r.Error = fmt.Sprintf(format, args...)
	return r
}

func (r *result) skip(format string, args ...any) *result {
	r.Status = StatusSkipped
	r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
	return r
}

func (r *result) note(format string, args ...any) {
	r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
}

func (r *result) changed() bool {
	return r.NewDigest != "" || r.NewTag != "" || r.NewManifest != "" || r.RemoveUnsafe || len(r.AddReferrers) > 0 || r.SetReferrers != nil
}

// planner computes the results of a command.
type planner struct {
	client *registryClient
	// all are the entries of every module; selected the ones to work on.
	all      []*Entry
	selected []*Entry
	jobs     int
	// pinCommand is how to run the pin command, for hints.
	pinCommand string

	treesMu sync.Mutex
	trees   map[string][]subject
	// indexes are the raw image indexes among the trees, by digest.
	indexes map[string][]byte
}

func (p *planner) tree(ctx context.Context, sources []source, digest string) ([]subject, error) {
	p.treesMu.Lock()
	cached, ok := p.trees[digest]
	p.treesMu.Unlock()
	if ok {
		return cached, nil
	}
	tree, index, err := p.client.tree(ctx, sources, digest)
	if err != nil {
		return nil, err
	}
	p.treesMu.Lock()
	if p.trees == nil {
		p.trees = map[string][]subject{}
		p.indexes = map[string][]byte{}
	}
	p.trees[digest] = tree
	if index != nil {
		p.indexes[digest] = index
	}
	p.treesMu.Unlock()
	return tree, nil
}

// index returns the raw image index digest of a tree fetched before, or nil if
// it is a manifest (or wasn't fetched).
func (p *planner) index(digest string) []byte {
	p.treesMu.Lock()
	defer p.treesMu.Unlock()
	return p.indexes[digest]
}

// mapping maps the tree of oldDigest to that of newDigest, both fetched before.
func (p *planner) mapping(oldDigest string, oldTree []subject, newDigest string, newTree []subject) subjectMapping {
	return subjectMapping{old: oldTree, new: newTree, oldIndex: p.index(oldDigest), newIndex: p.index(newDigest)}
}

// forEach runs fn for every selected entry (of the given kinds), jobs at a
// time, and returns the results in the order of the entries.
func (p *planner) forEach(ctx context.Context, kinds []Kind, fn func(context.Context, *Entry) *result) []*result {
	var entries []*Entry
	for _, entry := range p.selected {
		if slices.Contains(kinds, entry.Kind) {
			entries = append(entries, entry)
		}
	}
	results := make([]*result, len(entries))
	jobs := max(p.jobs, 1)
	semaphore := make(chan struct{}, jobs)
	var wg sync.WaitGroup
	for i, entry := range entries {
		wg.Add(1)
		semaphore <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-semaphore }()
			results[i] = fn(ctx, entry)
		}()
	}
	wg.Wait()
	return results
}

func newResult(entry *Entry) *result {
	r := &result{Entry: entry, Status: StatusUpToDate}
	if len(entry.unresolved) > 0 {
		r.note("could not evaluate %s (not a literal or a simple variable)", strings.Join(entry.unresolved, ", "))
	}
	return r
}

func (p *planner) list() []*result {
	var results []*result
	for _, entry := range p.selected {
		r := newResult(entry)
		r.Status = StatusListed
		results = append(results, r)
	}
	return results
}

// pin adds a digest to images that only have a tag.
func (p *planner) pin(ctx context.Context) []*result {
	return p.forEach(ctx, []Kind{KindPull, KindImagesPull}, func(ctx context.Context, entry *Entry) *result {
		r := newResult(entry)
		switch {
		case entry.Digest != "":
			return r
		case entry.Tag == "":
			return r.fail("the image has neither a tag nor a digest")
		case !entry.Editable("digest"):
			if entry.Root {
				return r.fail("digest is set, but not to a literal; edit it by hand")
			}
		}
		digest, _, err := p.client.resolveTag(ctx, entry.Sources(), entry.Tag)
		if err != nil {
			return r.fail("resolving tag %q: %v", entry.Tag, err)
		}
		r.Status = StatusChanged
		r.NewDigest = digest
		r.RemoveUnsafe = entry.UnsafeAllowTagWithoutDigest && entry.Editable("unsafe_allow_tag_without_digest")
		if len(entry.ReferrerDigests) > 0 {
			r.note("referrer_digests were pinned for whatever the tag pointed to; check that they apply to %s", digest)
		}
		return r
	})
}

// updateOptions configures update.
type updateOptions struct {
	maxBump   bump
	latestTag string
}

// update moves images to newer versions: an image with a version tag to the
// highest tag of the same shape, an image with a digest only to what
// latestTag points to, and an image with any other tag (and a digest) to what
// its tag points to now.
func (p *planner) update(ctx context.Context, opts updateOptions) []*result {
	results := p.forEach(ctx, []Kind{KindPull, KindImagesPull}, func(ctx context.Context, entry *Entry) *result {
		return p.updateImage(ctx, entry, opts)
	})
	return append(results, p.moveReferrersTags(ctx, results)...)
}

func (p *planner) updateImage(ctx context.Context, entry *Entry, opts updateOptions) *result {
	r := newResult(entry)
	sources := entry.Sources()
	tag := entry.Tag
	switch {
	case entry.Tag == "" && entry.Digest == "":
		return r.fail("the image has neither a tag nor a digest")
	case entry.Tag == "":
		tag = opts.latestTag
	default:
		if _, ok := parseVersionTag(entry.Tag); ok && opts.maxBump != bumpNone {
			tags, _, err := p.client.listTags(ctx, sources)
			if err != nil {
				return r.fail("listing tags: %v", err)
			}
			if newer, ok := newerVersionTag(entry.Tag, tags, opts.maxBump); ok {
				r.NewTag = newer
				tag = newer
			}
		}
	}
	if entry.Digest == "" && r.NewTag == "" {
		// A floating tag without a newer version: nothing to pin.
		return r
	}
	digest, _, err := p.client.resolveTag(ctx, sources, tag)
	if err != nil {
		return r.fail("resolving tag %q: %v", tag, err)
	}
	if entry.Digest != "" && digest != entry.Digest {
		r.NewDigest = digest
	}
	if !r.changed() {
		return r
	}
	for attr, changes := range map[string]bool{"digest": r.NewDigest != "", "tag": r.NewTag != ""} {
		if changes && entry.Root && !entry.Editable(attr) {
			return r.fail("%s is set, but not to a literal; edit it by hand", attr)
		}
	}
	r.Status = StatusChanged
	if r.NewDigest == "" {
		return r
	}
	if entry.HasReferrerArtifacts {
		r.note("referrer_artifacts may name subjects of the old digest; check them")
	}
	r.oldTree, err = p.tree(ctx, sources, entry.Digest)
	if err != nil {
		return r.fail("fetching the current image %s: %v", entry.Digest, err)
	}
	r.newTree, err = p.tree(ctx, sources, r.NewDigest)
	if err != nil {
		return r.fail("fetching the new image %s: %v", r.NewDigest, err)
	}
	if len(entry.ReferrerDigests) > 0 {
		if entry.Root && !entry.Editable("referrer_digests") {
			return r.fail("referrer_digests is set, but not to a literal; edit it by hand")
		}
		repinned, notes, err := p.client.repin(ctx, sources, entry.ReferrerDigests, nil, p.mapping(entry.Digest, r.oldTree, r.NewDigest, r.newTree))
		if err != nil {
			return r.fail("finding the referrers of the new image that correspond to referrer_digests: %v", err)
		}
		r.SetReferrers = &repinned
		r.Notes = append(r.Notes, notes...)
	}
	return r
}

// moveReferrersTags moves the images.referrers tags that apply to a manifest
// of an image whose digest changes to the corresponding manifest of the new
// image.
func (p *planner) moveReferrersTags(ctx context.Context, results []*result) []*result {
	var moved []*result
	// Every images.referrers tag of the root module follows, whether selected
	// or not: one left behind would name a manifest that is no longer pulled.
	for _, entry := range p.all {
		if entry.Kind != KindImagesReferrers || !entry.Root || entry.Manifest == "" {
			continue
		}
		for _, image := range results {
			if image.NewDigest == "" || image.oldTree == nil || !slices.ContainsFunc(image.oldTree, func(s subject) bool { return s.Digest == entry.Manifest }) {
				continue
			}
			r := newResult(entry)
			moved = append(moved, r)
			if keeper := p.stillPulling(image.Entry.Digest, results); keeper != nil {
				r.skip("%s %s still pulls %s", keeper.Kind, keeper.DisplayName(), image.Entry.Digest)
				break
			}
			mapping := p.mapping(image.Entry.Digest, image.oldTree, image.NewDigest, image.newTree)
			target, err := mapping.mapDigest(entry.Manifest)
			if err != nil {
				r.fail("moving to the new digest of %s: %v", image.Entry.DisplayName(), err)
				break
			}
			if !entry.Editable("manifest") {
				r.fail("manifest is set, but not to a literal; edit it by hand")
				break
			}
			r.Status = StatusChanged
			r.NewManifest = target.Digest
			r.note("follows %s %s to its new digest", image.Entry.Kind, image.Entry.DisplayName())
			if len(entry.ReferrerDigests) > 0 {
				if !entry.Editable("referrer_digests") {
					r.fail("referrer_digests is set, but not to a literal; edit it by hand")
					break
				}
				var scope []string
				if entry.Manifest != image.oldTree[0].Digest {
					scope = []string{entry.Manifest}
				}
				repinned, notes, err := p.client.repin(ctx, image.Entry.Sources(), entry.ReferrerDigests, scope, mapping)
				if err != nil {
					r.fail("finding the referrers of the new manifest that correspond to referrer_digests: %v", err)
					break
				}
				r.SetReferrers = &repinned
				r.Notes = append(r.Notes, notes...)
			}
			break
		}
	}
	return moved
}

// stillPulling returns an image entry that keeps pulling digest after the
// update, if any.
func (p *planner) stillPulling(digest string, results []*result) *Entry {
	changing := map[*Entry]bool{}
	for _, r := range results {
		if r.NewDigest != "" && r.Status == StatusChanged {
			changing[r.Entry] = true
		}
	}
	for _, entry := range p.all {
		if entry.Kind != KindImagesReferrers && entry.Digest == digest && !changing[entry] {
			return entry
		}
	}
	return nil
}

// referrers discovers the referrers of images (and of the manifests that
// images.referrers tags name), and proposes to pin those not pinned yet.
func (p *planner) referrers(ctx context.Context, filter typeFilter) []*result {
	return p.forEach(ctx, []Kind{KindPull, KindImagesPull, KindImagesReferrers}, func(ctx context.Context, entry *Entry) *result {
		r := newResult(entry)
		digest := entry.Digest
		sources := entry.Sources()
		if entry.Kind == KindImagesReferrers {
			digest = entry.Manifest
			var err error
			sources, err = p.sourcesOf(ctx, digest)
			if err != nil {
				return r.fail("%v", err)
			}
		}
		if digest == "" {
			return r.skip("not pinned to a digest; pin it first with %s", p.pinCommand)
		}
		tree, err := p.tree(ctx, sources, digest)
		if err != nil {
			return r.fail("fetching %s: %v", digest, err)
		}
		subjects := []subject{tree[0]}
		for _, child := range tree[1:] {
			if child.Platform == "unknown" || child.Platform == "unknown/unknown" {
				// Attestation manifests that buildx puts in an index.
				continue
			}
			if len(entry.Platforms) > 0 && !slices.ContainsFunc(entry.Platforms, func(spec string) bool { return platformMatches(spec, child.Platform) }) {
				continue
			}
			subjects = append(subjects, child)
		}
		found, err := p.client.discover(ctx, sources, subjects, filter, p.index(digest))
		if err != nil {
			return r.fail("discovering referrers: %v", err)
		}
		var add []string
		for i := range found {
			found[i].Pinned = slices.Contains(entry.ReferrerDigests, found[i].Digest)
			if !found[i].Pinned && !slices.Contains(add, found[i].Digest) {
				add = append(add, found[i].Digest)
			}
		}
		r.Referrers = found
		r.referrersChecked = true
		if len(add) == 0 {
			return r
		}
		sort.Strings(add)
		if entry.Root && !entry.Editable("referrer_digests") {
			return r.fail("referrer_digests is set, but not to a literal; edit it by hand")
		}
		r.Status = StatusChanged
		r.AddReferrers = add
		return r
	})
}

// sourcesOf returns the sources of the images that contain the manifest
// digest, for an images.referrers tag.
func (p *planner) sourcesOf(ctx context.Context, digest string) ([]source, error) {
	var sources []source
	addSources := func(entry *Entry) {
		for _, src := range entry.Sources() {
			if !slices.Contains(sources, src) {
				sources = append(sources, src)
			}
		}
	}
	for _, entry := range p.all {
		if entry.Kind != KindImagesReferrers && entry.Digest == digest {
			addSources(entry)
		}
	}
	if len(sources) > 0 {
		return sources, nil
	}
	for _, entry := range p.all {
		if entry.Kind != KindImagesPull || entry.Digest == "" {
			continue
		}
		tree, err := p.tree(ctx, entry.Sources(), entry.Digest)
		if err != nil {
			continue
		}
		if slices.ContainsFunc(tree, func(s subject) bool { return s.Digest == digest }) {
			addSources(entry)
		}
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("no pulled image contains the manifest %s", digest)
	}
	return sources, nil
}

// platformMatches tells whether a platform ("os/arch[/variant]") matches a
// platform spec of the platforms attribute of pull. A spec without a variant
// matches any variant.
func platformMatches(spec, platform string) bool {
	normalize := func(s string) []string {
		parts := strings.Split(s, "/")
		if len(parts) > 1 {
			switch parts[1] {
			case "x86_64", "x86-64":
				parts[1] = "amd64"
			case "aarch64":
				parts[1] = "arm64"
			}
		}
		if len(parts) == 3 && parts[1] == "arm64" && parts[2] == "v8" {
			parts = parts[:2]
		}
		return parts
	}
	want, got := normalize(spec), normalize(platform)
	if len(want) < 2 || len(got) < 2 || want[0] != got[0] || want[1] != got[1] {
		return false
	}
	if len(want) == 3 {
		return len(got) == 3 && want[2] == got[2]
	}
	return true
}

// readOnlyNote explains what to do about a change to an entry of another
// module, which can't be edited.
func readOnlyNote(entry *Entry, r *result) {
	if entry.Root || !r.changed() {
		return
	}
	switch {
	case len(r.AddReferrers) > 0 && entry.Kind == KindImagesPull:
		var quoted []string
		for _, digest := range r.AddReferrers {
			quoted = append(quoted, fmt.Sprintf("%q", digest))
		}
		r.note("declared by module %s; to pin the referrers from the root module, add: images.referrers(manifest = %q, referrer_digests = [%s])", entry.Where(), entry.Digest, strings.Join(quoted, ", "))
	default:
		r.note("declared by module %s, which only its maintainers can change", entry.Where())
	}
}
