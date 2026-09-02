// Copyright 2026 The rules_img Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package registry

import (
	"sync"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// A Collector evicts what a registry no longer needs, using tracing garbage
// collection rather than a per-reference timer.
//
// Expiring references one at a time cannot express that an index needs the
// manifests it lists: independent timers relate them to nothing, so they expire
// on their own schedules and a live index can end up naming digests that have
// already been swept. Here, reachability decides. Roots are tags -- which never
// expire unless a TagTTL says otherwise -- and any manifest or blob used within
// the TTL. Everything reachable from a root survives, however long ago it was
// last touched itself.
//
// The object graph:
//
//	tag       -> the manifest it resolves to
//	manifest  -> its config and layer blobs, plus its subject
//	index     -> the manifests it lists, plus its subject
//	blob      -> nothing; blobs are opaque leaves
//
// plus one back edge: a manifest that declares a subject is a referrer of it,
// and referrers stay alive as long as their subject does. That is what keeps a
// signature or an attestation from being swept out from under the image it
// describes.
//
// Edges are re-derived by parsing manifests during a sweep rather than recorded
// alongside them. containerd has to store its edges as labels
// (containerd.io/gc.ref.content.*) because its content store holds opaque bytes;
// ours holds self-describing JSON, so parsing keeps one source of truth. Kind,
// recorded when a manifest is stored, says how to parse it.
//
// A nil *Collector is a Collector that never collects: every method below
// tolerates a nil receiver, so a registry without eviction pays nothing and
// needs no checks at its call sites.
type Collector struct {
	store Store

	ttl      time.Duration
	tagTTL   time.Duration
	interval time.Duration
	now      func() time.Time

	// sweeps excludes a sweep from a write that takes more than one call to
	// the Store. A writer holds it for reading, a sweep for writing.
	sweeps        sync.RWMutex
	lock          sync.Mutex
	lastSweep     time.Time
	manifests     map[manifestKey]time.Time
	tags          map[tagKey]time.Time
	blobs         map[v1.Hash]*blobNode
	blobCollected []func(repo string, digest v1.Hash)
}

// CollectorConfig configures a Collector.
type CollectorConfig struct {
	// TTL is how long a manifest or blob is kept after it was last used.
	// Anything reachable from a live root outlives its own TTL. Zero or
	// negative tracks the object graph without ever collecting, which is what
	// a caller wants when it only needs the live set (see RangeLiveBlobs).
	TTL time.Duration

	// TagTTL is how long a tag is kept after it was last pushed or read. Zero
	// or negative makes tags permanent roots, so nothing a tag reaches is ever
	// collected -- which bounds nothing for a registry whose clients always
	// push a tag. Callers that have a way to tell an unset setting from an
	// explicit zero, such as a command line, should default it to TTL.
	TagTTL time.Duration

	// Interval is the shortest gap between two sweeps triggered by
	// MaybeCollect. Zero defaults to a tenth of TTL. Requests in between are
	// served from what the last sweep left behind, so an object can outlive
	// its TTL by up to one interval.
	Interval time.Duration

	// Clock reads the current time. Zero defaults to time.Now.
	Clock func() time.Time
}

// CollectStats counts what a sweep removed.
type CollectStats struct {
	Manifests int
	Tags      int
	Blobs     int
}

// LiveBlob is a blob a Collector considers reachable.
type LiveBlob struct {
	// Repo is a repository the blob was seen in. Blobs are content addressed
	// and every blob store this registry ships ignores the repository, so this
	// is a plausible repository rather than the only one.
	Repo string
	// Digest identifies the blob.
	Digest v1.Hash
	// Size is the blob's size in bytes, or 0 if it was never learned.
	Size int64
	// LastUsed is when the registry last served or stored the blob. It is the
	// zero time for a blob that is only known from a manifest's descriptors.
	LastUsed time.Time
}

type manifestKey struct {
	repo   string
	digest v1.Hash
}

type tagKey struct {
	repo string
	tag  string
}

type blobNode struct {
	repo     string
	size     int64
	lastUsed time.Time
}

// NewCollector returns a Collector that evicts from store according to cfg.
func NewCollector(store Store, cfg CollectorConfig) *Collector {
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = cfg.TTL / 10
		if interval <= 0 {
			interval = time.Minute
		}
	}
	return &Collector{
		store:     store,
		ttl:       cfg.TTL,
		tagTTL:    cfg.TagTTL,
		interval:  interval,
		now:       now,
		lastSweep: now(),
		manifests: make(map[manifestKey]time.Time),
		tags:      make(map[tagKey]time.Time),
		blobs:     make(map[v1.Hash]*blobNode),
	}
}

// Store returns the store the Collector evicts from.
func (c *Collector) Store() Store {
	if c == nil {
		return nil
	}
	return c.store
}

// OnBlobCollected registers a callback for blobs a sweep found unreachable.
// Callbacks run outside the Collector's lock and must not call back into it.
func (c *Collector) OnBlobCollected(fn func(repo string, digest v1.Hash)) {
	if c == nil {
		return
	}
	c.lock.Lock()
	defer c.lock.Unlock()
	c.blobCollected = append(c.blobCollected, fn)
}

// TouchManifest records that a manifest was just stored or served.
func (c *Collector) TouchManifest(repo string, digest v1.Hash) {
	if c == nil {
		return
	}
	c.lock.Lock()
	defer c.lock.Unlock()
	c.manifests[manifestKey{repo: repo, digest: digest}] = c.now()
}

// TouchTag records that a tag was just written or resolved.
func (c *Collector) TouchTag(repo, tag string) {
	if c == nil {
		return
	}
	c.lock.Lock()
	defer c.lock.Unlock()
	c.tags[tagKey{repo: repo, tag: tag}] = c.now()
}

// TouchBlob records that a blob was just stored, served, or named by a manifest
// the registry accepted. A non-positive size leaves an already known size
// alone, since a manifest's descriptor may be the only place it is stated.
func (c *Collector) TouchBlob(repo string, digest v1.Hash, size int64) {
	if c == nil {
		return
	}
	c.lock.Lock()
	defer c.lock.Unlock()

	node, ok := c.blobs[digest]
	if !ok {
		node = &blobNode{repo: repo}
		c.blobs[digest] = node
	}
	node.lastUsed = c.now()
	if size > 0 {
		node.size = size
	}
	if node.repo == "" {
		node.repo = repo
	}
}

// ForgetManifest drops a deleted manifest's bookkeeping.
func (c *Collector) ForgetManifest(repo string, digest v1.Hash) {
	if c == nil {
		return
	}
	c.lock.Lock()
	defer c.lock.Unlock()
	delete(c.manifests, manifestKey{repo: repo, digest: digest})
}

// ForgetTag drops a deleted tag's bookkeeping.
func (c *Collector) ForgetTag(repo, tag string) {
	if c == nil {
		return
	}
	c.lock.Lock()
	defer c.lock.Unlock()
	delete(c.tags, tagKey{repo: repo, tag: tag})
}

// ForgetBlob drops a deleted blob's bookkeeping.
func (c *Collector) ForgetBlob(digest v1.Hash) {
	if c == nil {
		return
	}
	c.lock.Lock()
	defer c.lock.Unlock()
	delete(c.blobs, digest)
}

// writing announces a write that a sweep must not run in the middle of, and
// returns the function that ends it.
//
// Storing a manifest and then the tag that names it is two calls to a Store
// that offers no transactions, and in between them the manifest is a manifest
// nothing points at. One sweep landing there is harmless -- it adopts what it
// has not seen before, so the first sweep never collects. A second one finds
// that adopted manifest past its TTL with no root reaching it and collects it,
// out from under the tag that is about to be written. What is left is a tag
// resolving to a manifest that is gone: a 404 for a reference the client was
// told, moments later, that the registry had created.
//
// Holding this for the whole write is what makes the pair atomic as far as the
// Collector is concerned. It is the outer of the two locks: a writer takes it
// before touching anything, and lock only afterwards.
func (c *Collector) writing() func() {
	if c == nil {
		return func() {}
	}
	c.sweeps.RLock()
	return c.sweeps.RUnlock
}

// MaybeCollect sweeps if the configured interval has passed since the last
// sweep. Handlers call it before serving a request, so a request arriving after
// something expired does not see it.
func (c *Collector) MaybeCollect() {
	if c == nil || (c.ttl <= 0 && c.tagTTL <= 0) {
		return
	}

	c.lock.Lock()
	due := c.now().Sub(c.lastSweep) >= c.interval
	c.lock.Unlock()
	if !due {
		return
	}
	c.Collect()
}

// Collect sweeps unconditionally and reports what it removed.
func (c *Collector) Collect() CollectStats {
	if c == nil {
		return CollectStats{}
	}

	c.sweeps.Lock()
	c.lock.Lock()
	live := c.markLocked()
	stats, collectedBlobs := c.sweepLocked(live)
	c.lastSweep = c.now()
	c.lock.Unlock()
	c.sweeps.Unlock()

	c.reportCollectedBlobs(collectedBlobs)
	return stats
}

// RangeLiveBlobs calls fn for every reachable blob, stopping early if fn
// returns false. It marks without sweeping, so callers that only need the live
// set -- keeping blobs alive in an external cache, for instance -- do not have
// to evict to learn it.
func (c *Collector) RangeLiveBlobs(fn func(LiveBlob) bool) {
	if c == nil {
		return
	}

	c.lock.Lock()
	live := c.markLocked()
	blobs := make([]LiveBlob, 0, len(live.blobs))
	for digest, blob := range live.blobs {
		blobs = append(blobs, LiveBlob{
			Repo:     blob.repo,
			Digest:   digest,
			Size:     blob.size,
			LastUsed: blob.lastUsed,
		})
	}
	c.lock.Unlock()

	for _, blob := range blobs {
		if !fn(blob) {
			return
		}
	}
}

// liveSet is the result of a mark pass.
type liveSet struct {
	// manifests are the reachable manifests.
	manifests map[manifestKey]struct{}
	// blobs are the reachable blobs, with the best size and last-used time
	// known for each.
	blobs map[v1.Hash]blobNode
	// staleTags are tags whose own TagTTL has elapsed. They did not act as
	// roots and the sweep removes them.
	staleTags map[tagKey]struct{}
	// untrackedManifests and untrackedTags are objects the store holds that the
	// Collector had no node for -- a push that raced this sweep, say. They are
	// adopted rather than collected, so nothing is ever swept on the very sweep
	// that first sees it.
	untrackedManifests map[manifestKey]struct{}
	untrackedTags      map[tagKey]struct{}
}

// markLocked walks the object graph from its roots. It must be called with the
// lock held.
func (c *Collector) markLocked() liveSet {
	now := c.now()
	live := liveSet{
		manifests:          make(map[manifestKey]struct{}),
		blobs:              make(map[v1.Hash]blobNode),
		staleTags:          make(map[tagKey]struct{}),
		untrackedManifests: make(map[manifestKey]struct{}),
		untrackedTags:      make(map[tagKey]struct{}),
	}

	// Build the graph: every manifest's references, and the inverse of the
	// subject edge so marking a manifest can mark its referrers.
	graph := make(map[manifestKey]references)
	referrers := make(map[manifestKey][]v1.Hash)
	var worklist []manifestKey
	c.store.RangeRepos(func(repo string) bool {
		c.store.RangeManifests(repo, func(digest v1.Hash, manifest Manifest) bool {
			key := manifestKey{repo: repo, digest: digest}
			refs := parseReferences(manifest)
			graph[key] = refs
			if refs.subject != nil {
				subject := manifestKey{repo: repo, digest: *refs.subject}
				referrers[subject] = append(referrers[subject], digest)
			}
			lastUsed, tracked := c.manifests[key]
			switch {
			case !tracked:
				live.untrackedManifests[key] = struct{}{}
				worklist = append(worklist, key)
			case c.fresh(lastUsed, now, c.ttl):
				worklist = append(worklist, key)
			}
			return true
		})
		c.store.RangeTags(repo, func(tag string, digest v1.Hash) bool {
			key := tagKey{repo: repo, tag: tag}
			lastUsed, tracked := c.tags[key]
			if !tracked {
				live.untrackedTags[key] = struct{}{}
			} else if !c.fresh(lastUsed, now, c.tagTTL) {
				live.staleTags[key] = struct{}{}
				return true
			}
			worklist = append(worklist, manifestKey{repo: repo, digest: digest})
			return true
		})
		return true
	})

	// Blobs used recently enough are roots in their own right: a layer pushed
	// before the manifest that will name it must not be swept in between.
	for digest, node := range c.blobs {
		if c.fresh(node.lastUsed, now, c.ttl) {
			live.blobs[digest] = *node
		}
	}

	// Mark, breadth first. The visited set is what makes cycles -- an index
	// that lists itself, or a pair that lists each other -- terminate, and what
	// keeps a manifest shared by several indexes from being walked twice.
	for len(worklist) > 0 {
		key := worklist[len(worklist)-1]
		worklist = worklist[:len(worklist)-1]
		if _, seen := live.manifests[key]; seen {
			continue
		}
		refs, ok := graph[key]
		if !ok {
			// A tag pointing at a digest we do not hold, or a subject that was
			// never pushed. Nothing to mark.
			continue
		}
		live.manifests[key] = struct{}{}

		for _, child := range refs.manifests {
			worklist = append(worklist, manifestKey{repo: key.repo, digest: child})
		}
		for _, referrer := range referrers[key] {
			worklist = append(worklist, manifestKey{repo: key.repo, digest: referrer})
		}
		for _, blob := range refs.blobs {
			live.blobs[blob.digest] = c.blobFromDescriptor(key.repo, blob)
		}
	}
	return live
}

// blobFromDescriptor merges what a descriptor says about a blob with what the
// Collector already knows, preferring the tracked node's size and last-used
// time.
func (c *Collector) blobFromDescriptor(repo string, desc descriptor) blobNode {
	merged := blobNode{repo: repo, size: desc.size}
	if node, ok := c.blobs[desc.digest]; ok {
		merged.lastUsed = node.lastUsed
		if node.size > 0 {
			merged.size = node.size
		}
		if node.repo != "" {
			merged.repo = node.repo
		}
	}
	return merged
}

// sweepLocked removes everything the mark pass did not reach. It must be called
// with the lock held. The returned blobs still need their callbacks fired,
// which happens once the lock is released.
func (c *Collector) sweepLocked(live liveSet) (CollectStats, []LiveBlob) {
	var stats CollectStats
	now := c.now()

	// Adopt objects the store holds but the Collector had not seen, so they
	// survive at least until the next sweep.
	for key := range live.untrackedManifests {
		c.manifests[key] = now
	}
	for key := range live.untrackedTags {
		c.tags[key] = now
	}

	for key := range c.manifests {
		if _, reachable := live.manifests[key]; reachable {
			continue
		}
		if _, held := c.store.GetManifest(key.repo, key.digest); held {
			c.store.DeleteManifest(key.repo, key.digest)
			stats.Manifests++
		}
		delete(c.manifests, key)
	}

	for key := range c.tags {
		digest, held := c.store.ResolveTag(key.repo, key.tag)
		if !held {
			delete(c.tags, key)
			continue
		}
		_, stale := live.staleTags[key]
		if _, target := c.store.GetManifest(key.repo, digest); stale || !target {
			// Either the tag itself expired, or it points at a manifest that is
			// gone -- a dangling tag resolves to a 404 either way.
			c.store.DeleteTag(key.repo, key.tag)
			delete(c.tags, key)
			stats.Tags++
		}
	}

	var collected []LiveBlob
	for digest, node := range c.blobs {
		if _, reachable := live.blobs[digest]; reachable {
			continue
		}
		collected = append(collected, LiveBlob{
			Repo:     node.repo,
			Digest:   digest,
			Size:     node.size,
			LastUsed: node.lastUsed,
		})
		delete(c.blobs, digest)
		stats.Blobs++
	}
	return stats, collected
}

// reportCollectedBlobs fires the callbacks for swept blobs, skipping any blob a
// concurrent request has since brought back.
func (c *Collector) reportCollectedBlobs(collected []LiveBlob) {
	for _, blob := range collected {
		c.lock.Lock()
		_, resurrected := c.blobs[blob.Digest]
		callbacks := c.blobCollected
		c.lock.Unlock()
		if resurrected {
			continue
		}
		for _, fn := range callbacks {
			fn(blob.Repo, blob.Digest)
		}
	}
}

// fresh reports whether something last used at lastUsed is still within ttl. A
// non-positive ttl never expires.
func (c *Collector) fresh(lastUsed, now time.Time, ttl time.Duration) bool {
	if ttl <= 0 {
		return true
	}
	return now.Sub(lastUsed) < ttl
}
