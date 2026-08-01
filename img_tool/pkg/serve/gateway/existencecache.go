package gateway

import (
	"hash/maphash"
	"math/bits"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// This file implements the blob existence cache: a memo of the fact that a blob
// is in a repository, which is the answer 200 to HEAD /v2/<name>/blobs/<digest>.
//
// That one request dominates a build farm's registry traffic. Every push begins
// by probing whether each layer is already upstream, so a fleet re-pushing the
// same base image asks the same question thousands of times, and each answer
// costs a round trip the client waits on. The answer is also the most cacheable
// one in the protocol: a blob is immutable and content-addressed, so "this
// digest is present in this repository" cannot become true-then-false-then-true
// — it can only stop being true, when the blob leaves the repository. That
// single failure mode is what the TTL bounds, for the one way it happens out of
// sight: a registry garbage-collecting the blob. The other way — a client
// deleting it — travels through the gateway, which drops the entry there and
// then (see Handler.forgetDeletedBlob).
//
// A probe is not the only request that establishes the fact, and the others fill
// the cache from the side the fleet is already paying for: a read the registry
// serves under the digest asked for, and an upload it carries through to a
// commit. Whichever client loses the race to push a layer has then already
// answered every probe that follows, so the fleet pays for neither the re-upload
// nor the first probe. The commit response is the earliest point at which that is
// true, and gateway.go is careful to admit nothing before it (see
// Handler.rememberBlob).
//
// Manifests and tags are deliberately *not* cached. A tag is mutable by
// definition, and a manifest HEAD is how a client discovers that a tag now
// points somewhere else, so memoizing either would hand out stale answers about
// data that is meant to change.
//
// Each instance holds its own cache, and what it learns is broadcast to its peers
// (see replication.go): the memory stays local — a shared cache would put a
// network round trip in front of every probe, which is the very cost this exists
// to remove — while the *fact* travels, so a fleet pays for one upstream probe
// per blob rather than one per instance.
//
// The implementation is one preallocated block of memory, sharded for
// concurrency:
//
//   - Capacity is fixed at startup from the configured memory bound. The slot
//     table, the hash buckets and the key arena are three allocations made once;
//     nothing in a lookup or a store allocates, and the cache's contribution to
//     RSS cannot grow past the bound no matter what traffic arrives.
//   - Each entry owns a fixed slice of the arena for its key, so the arena never
//     fragments and evicting an entry is an overwrite rather than a free.
//   - Every link is an index into the slot table rather than a pointer, so the
//     whole structure holds no pointers and the garbage collector never scans
//     it.
//   - Keys are spread over independently locked shards, which is what lets the
//     cache scale with cores: a lookup takes one shard's mutex and holds it for a
//     hash-bucket walk and an LRU splice, both O(1) (see BenchmarkBlobCache).
//
// The one thing this deliberately does not do is collapse concurrent misses for
// the same blob into a single upstream request. A registry answers a blob HEAD
// in milliseconds, so the duplicate probes a burst can produce are cheap, while
// sharing one in-flight request would tie one client's answer to another
// client's context and cancellation.

const (
	// entryBytes is the arena slice one entry gets for its key: the upstream
	// registry, the repository, and the digest, concatenated. It leaves room for a
	// key far longer than a registry serves — a sha256 digest is 71 bytes of it, so
	// a 249-byte registry and repository still fit, and even a sha512 digest leaves
	// 185 — and a key that does not fit is simply not cached.
	entryBytes = 320

	// slotBytes is the size of the fixed-size part of an entry. unsafe.Sizeof is
	// a compile-time constant — this file does no pointer arithmetic — and using
	// it keeps the memory accounting exact rather than a number to maintain by
	// hand.
	slotBytes = int64(unsafe.Sizeof(slot{}))

	// bucketBytes is an entry's share of the hash bucket table, which is sized to
	// the next power of two at or above a shard's slot count and therefore holds
	// fewer than two int32 per slot.
	bucketBytes = 2 * 4

	// entryCost is what one entry takes out of the configured memory bound. The
	// three tables are the cache's only allocations, so capacity x entryCost is
	// its total footprint.
	entryCost = entryBytes + slotBytes + bucketBytes

	// maxShards caps the shard count, and shardsPerProc is how many shards each
	// runnable goroutine gets. Both come from measurement (BenchmarkBlobCache):
	// the cost under load is dominated by cores contending for the same shard, so
	// spreading keys widely is what makes the cache scale, and past this cap the
	// curve is flat and shards only cost LRU quality — each shard evicts its own
	// least recently used entry, not the cache's.
	maxShards     = 256
	shardsPerProc = 16

	// minSlotsPerShard keeps sharding from turning a small cache into a set of
	// tiny LRU lists that evict entries a single list would have kept.
	minSlotsPerShard = 64

	// maxCapacity bounds the slot table, so that a slot index always fits in the
	// int32 links and the table sizes cannot overflow an int on a 32-bit platform.
	// A larger memory bound is honoured up to this many entries and no further,
	// which is not a limit any real deployment reaches: it is about 1.5 GiB worth
	// of blob digests.
	maxCapacity = 1 << 22
)

// MinBlobExistenceCacheBytes is the smallest memory bound that holds one entry.
// A smaller one disables the cache, so a command validates against it rather
// than starting up with a cache that silently never stores anything.
const MinBlobExistenceCacheBytes = entryCost

// slot is the fixed-size part of a cache entry.
//
// Every link is an int32 index into the owning shard's slot table, with -1 for
// "none", which is what keeps the whole structure free of pointers for the
// garbage collector to trace.
type slot struct {
	// hash is the full 64-bit hash of the key, so walking a bucket only compares
	// key bytes for a slot whose hash already matches.
	hash uint64
	// expires is the deadline, in nanoseconds since the cache's base time. It is
	// zero in a slot that holds no entry.
	expires int64
	// contentLength is the Content-Length the registry reported for the blob, or
	// -1 when it reported none.
	contentLength int64
	// prev and next chain the LRU list, most recently used first. next doubles as
	// the free-list link in a slot that holds no entry.
	prev, next int32
	// hprev and hnext chain the slots that share a hash bucket. The head of a
	// bucket has hprev == -1; its bucket index is recomputed from hash, so it
	// does not have to be stored.
	hprev, hnext int32
	// The lengths of the three key parts in the slot's arena slice, which is laid
	// out as registry, then repository, then digest.
	regLen, repoLen, digestLen uint16
}

// blobExistenceCache memoizes successful blob existence checks.
//
// A nil *blobExistenceCache is a disabled cache: every method tolerates it, so
// the gateway needs no second code path for the cache being turned off.
type blobExistenceCache struct {
	shards []cacheShard
	// shardShift takes a shard index from the top bits of a key's hash. The
	// bucket index comes from the bottom bits, so the two are independent.
	shardShift uint
	// seed is this process's hash seed. maphash randomizes it per process, which
	// is what stops a client from choosing digests that collide into one bucket.
	seed maphash.Seed
	ttl  time.Duration
	// base is the reference point deadlines are measured from. Storing an offset
	// from it rather than a wall-clock instant means both readings carry the
	// monotonic clock, so a clock step (NTP, a suspended VM) cannot stretch or
	// cut short a TTL.
	base time.Time
	// now is the clock, replaced in tests.
	now func() time.Time
	// capacity is the number of slots, summed over the shards.
	capacity int64
}

// cacheShard is one independently locked partition of the cache. Everything it
// owns is a slice of the cache's three preallocated tables.
type cacheShard struct {
	mu sync.Mutex
	// slots, buckets and arena are this shard's slices of the three tables. Slot
	// i owns arena[i*entryBytes:(i+1)*entryBytes].
	slots   []slot
	buckets []int32
	arena   []byte
	// mask turns a hash into a bucket index; buckets is a power of two long.
	mask uint64
	// head and tail are the ends of the LRU list, -1 when it is empty.
	head, tail int32
	// free heads the list of slots whose entry was dropped, chained through
	// slot.next. used counts the slots that have ever held an entry, so the table
	// is filled without an initialization pass over every slot.
	free int32
	used int32

	// The counters below are read without the lock, by the metric callbacks.
	live            atomic.Int64
	evictedCapacity atomic.Int64
	evictedExpired  atomic.Int64
	evictedDeleted  atomic.Int64
}

// newBlobExistenceCache builds a cache that treats a blob it has seen as present
// for ttl, within maxBytes of memory allocated up front.
//
// It returns nil — a disabled cache — when ttl is not positive or when maxBytes
// is too small to hold a single entry.
func newBlobExistenceCache(ttl time.Duration, maxBytes int64) *blobExistenceCache {
	if ttl <= 0 || maxBytes < entryCost {
		return nil
	}
	capacity := maxBytes / entryCost
	if capacity > maxCapacity {
		capacity = maxCapacity
	}
	shards := shardCount(capacity)
	perShard := int(capacity) / shards
	// One power-of-two bucket table per shard, at a load factor of at most 1: an
	// average bucket then holds about one entry, so a lookup that misses walks a
	// single link and one that hits walks one or two.
	perShardBuckets := 1 << bits.Len(uint(perShard-1))
	total := perShard * shards

	c := &blobExistenceCache{
		shards:     make([]cacheShard, shards),
		shardShift: uint(64 - bits.TrailingZeros(uint(shards))),
		seed:       maphash.MakeSeed(),
		ttl:        ttl,
		base:       time.Now(),
		now:        time.Now,
		capacity:   int64(total),
	}
	// The three tables, allocated once. None of them holds a pointer, so the
	// runtime hands them out as fresh (already zeroed) pages: the arena and the slot
	// table cost nothing in RSS until entries are stored in them, and only the
	// bucket table — the smallest of the three — is touched here, to write its empty
	// sentinel.
	slots := make([]slot, total)
	buckets := make([]int32, perShardBuckets*shards)
	arena := make([]byte, total*entryBytes)
	for i := range buckets {
		buckets[i] = -1
	}
	for i := range c.shards {
		s := &c.shards[i]
		s.slots = slots[i*perShard : (i+1)*perShard]
		s.buckets = buckets[i*perShardBuckets : (i+1)*perShardBuckets]
		s.arena = arena[i*perShard*entryBytes : (i+1)*perShard*entryBytes]
		s.mask = uint64(perShardBuckets - 1)
		s.head, s.tail, s.free = -1, -1, -1
	}
	return c
}

// shardCount picks a power-of-two shard count: enough that concurrent lookups of
// different blobs rarely queue behind each other, but not so many that each
// shard's LRU list is too short to be a useful one.
func shardCount(capacity int64) int {
	n := 1
	for n < maxShards && n < shardsPerProc*runtime.GOMAXPROCS(0) && int64(n*2*minSlotsPerShard) <= capacity {
		n *= 2
	}
	return n
}

// lookup reports whether the blob is known to exist in the repository, and the
// Content-Length the registry gave for it (-1 if it gave none).
//
// A hit moves the entry to the head of its shard's LRU list. An entry past its
// deadline is dropped and reported as a miss, so the caller asks the registry
// again.
func (c *blobExistenceCache) lookup(registry, repository, digest string) (contentLength int64, ok bool) {
	if c == nil {
		return 0, false
	}
	hash := c.hash(registry, repository, digest)
	s := &c.shards[hash>>c.shardShift]
	now := c.elapsed()

	s.mu.Lock()
	defer s.mu.Unlock()

	idx := s.find(hash, registry, repository, digest)
	if idx < 0 {
		return 0, false
	}
	if s.slots[idx].expires <= now {
		s.drop(idx)
		s.evictedExpired.Add(1)
		return 0, false
	}
	s.touch(idx)
	return s.slots[idx].contentLength, true
}

// store records that the blob exists in the repository, for the cache's TTL,
// evicting the least recently used entry of its shard if that shard is full.
// Storing a key that is already present refreshes its deadline in place.
func (c *blobExistenceCache) store(registry, repository, digest string, contentLength int64) {
	if c == nil {
		return
	}
	c.insert(registry, repository, digest, contentLength, c.ttl)
}

// insert is store with an explicit lifetime, which is what a replicated entry
// needs: an instance that donates part of its cache to a starting peer sends what
// is *left* of each deadline, so the fact does not get a fresh TTL every time it
// is copied to another instance (see [CacheReplication.warmUp]).
//
// A contentLength below zero means "unknown", and never overwrites a length the
// cache already holds: a blob is content-addressed, so its size cannot have
// changed, and a peer that learned of the blob from an upload knows no size while
// this instance may have served it and know exactly.
//
// A key too long for an entry's arena slice is not stored. It is the one case
// that is skipped silently: no registry serves a name that long, and the
// alternative — a variable-length arena — would fragment and could not offer a
// hard memory bound.
func (c *blobExistenceCache) insert(registry, repository, digest string, contentLength int64, lifetime time.Duration) {
	if c == nil || lifetime <= 0 || len(registry)+len(repository)+len(digest) > entryBytes {
		return
	}
	hash := c.hash(registry, repository, digest)
	s := &c.shards[hash>>c.shardShift]
	expires := c.elapsed() + lifetime.Nanoseconds()

	s.mu.Lock()
	defer s.mu.Unlock()

	if idx := s.find(hash, registry, repository, digest); idx >= 0 {
		s.slots[idx].expires = expires
		if contentLength >= 0 {
			s.slots[idx].contentLength = contentLength
		}
		s.touch(idx)
		return
	}

	idx := s.claim()
	region := s.arena[int(idx)*entryBytes:][:entryBytes]
	n := copy(region, registry)
	n += copy(region[n:], repository)
	copy(region[n:], digest)

	e := &s.slots[idx]
	e.hash = hash
	e.expires = expires
	e.contentLength = contentLength
	e.regLen = uint16(len(registry))
	e.repoLen = uint16(len(repository))
	e.digestLen = uint16(len(digest))

	s.link(idx)
	s.pushFront(idx)
	s.live.Add(1)
}

// forget drops the entry for a blob, so that the next probe for it asks the
// registry again. Forgetting a key the cache does not hold does nothing.
//
// Other than an entry evicted to make room for another blob, this is the only way
// one goes before its TTL. The cache's one failure mode is holding a blob that has
// since gone, and a delete travelling through the gateway is the one moment it can
// know that happened.
func (c *blobExistenceCache) forget(registry, repository, digest string) {
	if c == nil {
		return
	}
	hash := c.hash(registry, repository, digest)
	s := &c.shards[hash>>c.shardShift]

	s.mu.Lock()
	defer s.mu.Unlock()

	if idx := s.find(hash, registry, repository, digest); idx >= 0 {
		s.drop(idx)
		s.evictedDeleted.Add(1)
	}
}

// cacheEntry is one entry copied out of the cache, which is how an instance
// hands part of its cache to a peer that is starting up.
type cacheEntry struct {
	registry, repository, digest string
	// contentLength is the size the registry reported for the blob, or -1.
	contentLength int64
	// lifetime is what is left of the entry's deadline. It travels with the entry
	// so that copying a fact between instances does not restart its TTL: the TTL
	// bounds how long the gateway may believe a blob that has since been garbage
	// collected upstream, and that bound has to survive the copy.
	lifetime time.Duration
}

// maxSnapshotEntries is the ceiling on one call to [blobExistenceCache.hottest],
// and so on the memory a snapshot allocates up front. It is enforced there rather
// than left to the caller because limit reaches it from a peer's request: the
// bound has to hold whatever anyone asks for.
//
// At 64k entries a snapshot is a few megabytes, which is a large farm's hot
// working set several times over — a donation is a head start, not a copy of
// everything a big cache holds.
const maxSnapshotEntries = 1 << 16

// hottest copies out up to limit of the cache's most recently used live entries,
// which is what a starting instance asks a peer for so that its first probes are
// not all misses (see [CacheReplication.warmUp]).
//
// Entries are taken from the head of every shard's LRU list, an equal share from
// each: the cache has no global LRU order — that is the concurrency the sharding
// buys — so "the hottest entries of each shard" is the closest thing to it, and
// the difference does not matter for a set that is a hint either way.
//
// A shard's lock is held only for the copy of its key bytes; the strings are built
// after it is released. A donating instance is a serving instance, and a request
// hashing to the same shard must not wait for a donation to be marshalled.
func (c *blobExistenceCache) hottest(limit int) []cacheEntry {
	if c == nil || limit <= 0 {
		return nil
	}
	// Two ceilings, both of which have to be applied before anything is sized from
	// limit: there is nothing to copy beyond what the cache can hold, and
	// maxSnapshotEntries is what keeps the allocations below bounded no matter what
	// a peer asked for.
	if int64(limit) > c.capacity {
		limit = int(c.capacity)
	}
	if limit > maxSnapshotEntries {
		limit = maxSnapshotEntries
	}
	perShard := (limit + len(c.shards) - 1) / len(c.shards)
	now := c.elapsed()

	// keys accumulates the key bytes of every entry taken, and spans records how
	// to cut them apart again.
	keys := make([]byte, 0, limit*entrySampleBytes)
	spans := make([]entrySpan, 0, limit)
	for i := range c.shards {
		if len(spans) >= limit {
			break
		}
		s := &c.shards[i]
		s.mu.Lock()
		taken := 0
		for idx := s.head; idx >= 0 && taken < perShard && len(spans) < limit; idx = s.slots[idx].next {
			e := &s.slots[idx]
			if e.expires <= now {
				// Expired but not yet evicted: it teaches a peer nothing.
				continue
			}
			end := int(e.regLen) + int(e.repoLen) + int(e.digestLen)
			keys = append(keys, s.arena[int(idx)*entryBytes:][:end]...)
			spans = append(spans, entrySpan{
				regLen:        e.regLen,
				repoLen:       e.repoLen,
				digestLen:     e.digestLen,
				contentLength: e.contentLength,
				lifetime:      e.expires - now,
			})
			taken++
		}
		s.mu.Unlock()
	}

	entries := make([]cacheEntry, 0, len(spans))
	at := 0
	for _, span := range spans {
		key := keys[at:][:int(span.regLen)+int(span.repoLen)+int(span.digestLen)]
		at += len(key)
		regEnd := int(span.regLen)
		repoEnd := regEnd + int(span.repoLen)
		entries = append(entries, cacheEntry{
			registry:      string(key[:regEnd]),
			repository:    string(key[regEnd:repoEnd]),
			digest:        string(key[repoEnd:]),
			contentLength: span.contentLength,
			lifetime:      time.Duration(span.lifetime),
		})
	}
	return entries
}

// entrySampleBytes is how much room per entry [blobExistenceCache.hottest]
// reserves for key bytes up front. It is a guess at a typical key — a sha256
// digest is 71 bytes of it — not a limit: a longer key simply grows the buffer.
const entrySampleBytes = 128

// entrySpan is the fixed-size part of an entry copied out under a shard's lock,
// paired with its key bytes in the buffer alongside.
type entrySpan struct {
	regLen, repoLen, digestLen uint16
	contentLength              int64
	lifetime                   int64
}

// hash mixes the three parts of a key without concatenating them, so a lookup
// allocates nothing.
func (c *blobExistenceCache) hash(registry, repository, digest string) uint64 {
	h := maphash.String(c.seed, registry)
	h = mixHash(h, maphash.String(c.seed, repository))
	return mixHash(h, maphash.String(c.seed, digest))
}

// mixHash combines two 64-bit hashes so that every input bit reaches every
// output bit — splitmix64's finalizer over the two. That matters here because
// the shard index comes from the top of the result and the bucket index from the
// bottom: a weaker combiner would correlate the two and pile keys into a corner
// of one shard.
func mixHash(a, b uint64) uint64 {
	x := a ^ (b * 0x9e3779b97f4a7c15)
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// elapsed is the current time as a nanosecond offset from the cache's base.
func (c *blobExistenceCache) elapsed() int64 {
	return int64(c.now().Sub(c.base))
}

// cacheStats is a snapshot of the cache's occupancy for the metric callbacks. It
// is read from atomics rather than under the shard locks, so exporting metrics
// never blocks a lookup.
type cacheStats struct {
	// entries counts the entries held, including any that have expired but have
	// not been looked up or evicted since.
	entries int64
	// capacity is the number of entries the memory bound allows.
	capacity int64
	// evictedCapacity counts entries dropped to make room, evictedExpired those
	// dropped because their TTL had passed, and evictedDeleted those dropped
	// because a blob delete travelled through the gateway.
	evictedCapacity int64
	evictedExpired  int64
	evictedDeleted  int64
}

func (c *blobExistenceCache) stats() cacheStats {
	if c == nil {
		return cacheStats{}
	}
	st := cacheStats{capacity: c.capacity}
	for i := range c.shards {
		s := &c.shards[i]
		st.entries += s.live.Load()
		st.evictedCapacity += s.evictedCapacity.Load()
		st.evictedExpired += s.evictedExpired.Load()
		st.evictedDeleted += s.evictedDeleted.Load()
	}
	return st
}

// find returns the slot holding the key, or -1. The stored key bytes are
// compared even though the hash already matched: a 64-bit collision is
// improbable, but the cost of one would be telling a client a blob exists in a
// repository it was never seen in.
func (s *cacheShard) find(hash uint64, registry, repository, digest string) int32 {
	for idx := s.buckets[hash&s.mask]; idx >= 0; idx = s.slots[idx].hnext {
		if s.slots[idx].hash == hash && s.matches(idx, registry, repository, digest) {
			return idx
		}
	}
	return -1
}

// matches compares an entry's stored key with the three parts of a lookup key.
// The comparisons are against the arena bytes directly: the compiler turns
// string(b) == s into a comparison that materializes no string, so this
// allocates nothing.
func (s *cacheShard) matches(idx int32, registry, repository, digest string) bool {
	e := &s.slots[idx]
	if int(e.regLen) != len(registry) || int(e.repoLen) != len(repository) || int(e.digestLen) != len(digest) {
		return false
	}
	regEnd := int(e.regLen)
	repoEnd := regEnd + int(e.repoLen)
	key := s.arena[int(idx)*entryBytes:][:repoEnd+int(e.digestLen)]
	return string(key[:regEnd]) == registry &&
		string(key[regEnd:repoEnd]) == repository &&
		string(key[repoEnd:]) == digest
}

// claim returns a slot to write a new entry into: one freed earlier, then one
// that has never been used, and finally the least recently used entry, which is
// evicted to make room.
func (s *cacheShard) claim() int32 {
	if s.free >= 0 {
		idx := s.free
		s.free = s.slots[idx].next
		return idx
	}
	if int(s.used) < len(s.slots) {
		idx := s.used
		s.used++
		return idx
	}
	idx := s.tail
	s.unlinkLRU(idx)
	s.unlink(idx)
	s.evictedCapacity.Add(1)
	s.live.Add(-1)
	return idx
}

// drop removes an entry and returns its slot to the free list.
func (s *cacheShard) drop(idx int32) {
	s.unlinkLRU(idx)
	s.unlink(idx)
	s.slots[idx].expires = 0
	// The free list is chained through next, so this has to come after unlinkLRU,
	// which writes that field as part of unlinking.
	s.slots[idx].next = s.free
	s.free = idx
	s.live.Add(-1)
}

// touch moves an entry to the head of the LRU list. An entry that is already the
// head needs no work at all, which is the common case for the blob a fleet is
// currently hammering.
func (s *cacheShard) touch(idx int32) {
	if s.head == idx {
		return
	}
	s.unlinkLRU(idx)
	s.pushFront(idx)
}

func (s *cacheShard) pushFront(idx int32) {
	e := &s.slots[idx]
	e.prev, e.next = -1, s.head
	if s.head >= 0 {
		s.slots[s.head].prev = idx
	}
	s.head = idx
	if s.tail < 0 {
		s.tail = idx
	}
}

func (s *cacheShard) unlinkLRU(idx int32) {
	e := &s.slots[idx]
	if e.prev >= 0 {
		s.slots[e.prev].next = e.next
	} else {
		s.head = e.next
	}
	if e.next >= 0 {
		s.slots[e.next].prev = e.prev
	} else {
		s.tail = e.prev
	}
	e.prev, e.next = -1, -1
}

// link adds an entry to its hash bucket.
func (s *cacheShard) link(idx int32) {
	bucket := s.slots[idx].hash & s.mask
	head := s.buckets[bucket]
	e := &s.slots[idx]
	e.hprev, e.hnext = -1, head
	if head >= 0 {
		s.slots[head].hprev = idx
	}
	s.buckets[bucket] = idx
}

// unlink removes an entry from its hash bucket. The bucket is recomputed from
// the entry's hash, which is why a slot does not store its bucket index.
func (s *cacheShard) unlink(idx int32) {
	e := &s.slots[idx]
	if e.hprev >= 0 {
		s.slots[e.hprev].hnext = e.hnext
	} else {
		s.buckets[e.hash&s.mask] = e.hnext
	}
	if e.hnext >= 0 {
		s.slots[e.hnext].hprev = e.hprev
	}
	e.hprev, e.hnext = -1, -1
}
