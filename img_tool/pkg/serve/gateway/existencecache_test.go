package gateway

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"
)

// The blob existence cache has two kinds of test here: the ones below exercise
// the data structure directly (keys, expiry, eviction, the memory bound and the
// promise that none of it allocates), and the handler-level ones in
// existencecache_serve_test.go check what a client actually observes.

const (
	testCacheRegistry   = "registry.test"
	testCacheRepository = "team/service/app"
	testCacheDigest     = "sha256:6b0f2e1a4c3d5e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f7"
)

// fakeClock is a manually advanced clock, so the TTL tests neither sleep nor
// depend on how long the rest of the test took.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// newTestCache builds a cache sized to exactly entries, with a clock the test
// drives.
func newTestCache(t *testing.T, ttl time.Duration, entries int64) (*blobExistenceCache, *fakeClock) {
	t.Helper()
	c := newBlobExistenceCache(ttl, entries*entryCost)
	if c == nil {
		t.Fatalf("newBlobExistenceCache(%v, room for %d) returned a disabled cache", ttl, entries)
	}
	if c.capacity != entries {
		t.Fatalf("capacity = %d, want %d", c.capacity, entries)
	}
	clock := &fakeClock{t: c.base}
	c.now = clock.now
	return c, clock
}

// blobDigest is a distinct, digest-shaped string per index.
func blobDigest(i int) string {
	return fmt.Sprintf("sha256:%064x", i)
}

func TestBlobCacheStoreLookup(t *testing.T) {
	c, _ := newTestCache(t, time.Hour, 64)

	if _, ok := c.lookup(testCacheRegistry, testCacheRepository, testCacheDigest); ok {
		t.Fatal("empty cache reported a hit")
	}
	c.store(testCacheRegistry, testCacheRepository, testCacheDigest, 4096)
	length, ok := c.lookup(testCacheRegistry, testCacheRepository, testCacheDigest)
	if !ok {
		t.Fatal("stored blob was not found")
	}
	if length != 4096 {
		t.Fatalf("content length = %d, want 4096", length)
	}

	// A registry that reported no Content-Length is stored as such, so the
	// replayed response omits the header rather than claiming a zero-byte blob.
	c.store(testCacheRegistry, testCacheRepository, blobDigest(1), -1)
	if length, ok := c.lookup(testCacheRegistry, testCacheRepository, blobDigest(1)); !ok || length != -1 {
		t.Fatalf("lookup of a blob with no content length = (%d, %v), want (-1, true)", length, ok)
	}
}

// TestBlobCacheKeyIsAllThreeParts is the correctness property the whole feature
// rests on: a blob is present in *a repository of a registry*, so a hit must
// require all three parts of the key to match.
func TestBlobCacheKeyIsAllThreeParts(t *testing.T) {
	c, _ := newTestCache(t, time.Hour, 64)
	c.store(testCacheRegistry, testCacheRepository, testCacheDigest, 1)

	for _, tc := range []struct {
		name                         string
		registry, repository, digest string
	}{
		{"other registry", "other.test", testCacheRepository, testCacheDigest},
		{"other repository", testCacheRegistry, "team/service/other", testCacheDigest},
		{"other digest", testCacheRegistry, testCacheRepository, blobDigest(2)},
		{"registry with a port", testCacheRegistry + ":5000", testCacheRepository, testCacheDigest},
		{"repository prefix", testCacheRegistry, "team/service", testCacheDigest},
		// The three parts are concatenated in the arena, so a naive comparison of
		// the joined bytes would confuse these two with the stored key.
		{"shifted boundary", testCacheRegistry + "team", "/service/app", testCacheDigest},
		{"shifted the other way", "registry", ".testteam/service/app", testCacheDigest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := c.lookup(tc.registry, tc.repository, tc.digest); ok {
				t.Errorf("lookup(%q, %q, %q) hit the entry stored for (%q, %q, %q)",
					tc.registry, tc.repository, tc.digest, testCacheRegistry, testCacheRepository, testCacheDigest)
			}
		})
	}
}

// TestBlobCacheForget covers the one thing that unmakes an entry before its TTL:
// the blob left the repository, and the cache was told.
func TestBlobCacheForget(t *testing.T) {
	c, _ := newTestCache(t, time.Hour, 64)

	// Forgetting what the cache never held is not an error and costs nothing.
	c.forget(testCacheRegistry, testCacheRepository, testCacheDigest)
	if stats := c.stats(); stats.entries != 0 || stats.evictedDeleted != 0 {
		t.Fatalf("forgetting an absent key: entries = %d, deleted evictions = %d; want 0 and 0", stats.entries, stats.evictedDeleted)
	}

	c.store(testCacheRegistry, testCacheRepository, testCacheDigest, 4096)
	c.store(testCacheRegistry, testCacheRepository, blobDigest(1), 8192)
	c.forget(testCacheRegistry, testCacheRepository, testCacheDigest)

	if _, ok := c.lookup(testCacheRegistry, testCacheRepository, testCacheDigest); ok {
		t.Error("a forgotten blob is still reported as present")
	}
	// Only that one key: forgetting is not a flush.
	if _, ok := c.lookup(testCacheRegistry, testCacheRepository, blobDigest(1)); !ok {
		t.Error("forgetting one blob dropped another")
	}
	if stats := c.stats(); stats.entries != 1 || stats.evictedDeleted != 1 {
		t.Errorf("after forgetting one of two: entries = %d, deleted evictions = %d; want 1 and 1", stats.entries, stats.evictedDeleted)
	}
	// The slot went back into circulation rather than leaking, and the blob can be
	// stored again — a delete is not a tombstone.
	checkIntegrity(t, c)
	c.store(testCacheRegistry, testCacheRepository, testCacheDigest, 4096)
	if length, ok := c.lookup(testCacheRegistry, testCacheRepository, testCacheDigest); !ok || length != 4096 {
		t.Errorf("re-storing a forgotten blob: length = %d, ok = %v; want 4096 and true", length, ok)
	}
	checkIntegrity(t, c)
}

// TestBlobCacheForgetOnDisabledCache: a nil cache tolerates every method, which is
// what keeps the handler free of a second code path for the cache being off.
func TestBlobCacheForgetOnDisabledCache(t *testing.T) {
	var c *blobExistenceCache
	c.forget(testCacheRegistry, testCacheRepository, testCacheDigest)
}

// TestBlobCacheHashCollisionMisses forces two keys to share a hash, which is the
// case the stored key bytes exist to settle.
func TestBlobCacheHashCollisionMisses(t *testing.T) {
	c, _ := newTestCache(t, time.Hour, 64)
	c.store(testCacheRegistry, testCacheRepository, testCacheDigest, 7)

	// Plant the stored entry's hash on the key we are about to look up.
	stored := c.hash(testCacheRegistry, testCacheRepository, testCacheDigest)
	shard := &c.shards[stored>>c.shardShift]
	idx := shard.find(stored, testCacheRegistry, testCacheRepository, testCacheDigest)
	if idx < 0 {
		t.Fatal("stored entry not found in its shard")
	}
	if got := shard.find(stored, testCacheRegistry, testCacheRepository, blobDigest(3)); got >= 0 {
		t.Fatalf("a colliding hash returned slot %d for a different digest", got)
	}
	if got := shard.find(stored, testCacheRegistry, testCacheRepository, testCacheDigest); got != idx {
		t.Fatalf("find = %d, want %d for the key that is really stored", got, idx)
	}
}

func TestBlobCacheExpiry(t *testing.T) {
	const ttl = 6 * time.Hour
	c, clock := newTestCache(t, ttl, 64)
	c.store(testCacheRegistry, testCacheRepository, testCacheDigest, 1)

	clock.advance(ttl - time.Nanosecond)
	if _, ok := c.lookup(testCacheRegistry, testCacheRepository, testCacheDigest); !ok {
		t.Fatal("entry expired before its TTL was up")
	}
	clock.advance(time.Nanosecond)
	if _, ok := c.lookup(testCacheRegistry, testCacheRepository, testCacheDigest); ok {
		t.Fatal("entry survived its TTL")
	}
	// The expired entry is dropped rather than left to occupy its slot, and is
	// reported as such.
	if stats := c.stats(); stats.entries != 0 || stats.evictedExpired != 1 {
		t.Fatalf("after expiry: entries = %d, expired evictions = %d; want 0 and 1", stats.entries, stats.evictedExpired)
	}

	// Storing it again starts a fresh TTL, and refreshing an entry that is still
	// live extends it rather than adding a second one.
	c.store(testCacheRegistry, testCacheRepository, testCacheDigest, 1)
	clock.advance(ttl / 2)
	c.store(testCacheRegistry, testCacheRepository, testCacheDigest, 1)
	clock.advance(ttl - time.Nanosecond)
	if _, ok := c.lookup(testCacheRegistry, testCacheRepository, testCacheDigest); !ok {
		t.Fatal("a refreshed entry expired on the original deadline")
	}
	if entries := c.stats().entries; entries != 1 {
		t.Fatalf("entries = %d after refreshing one blob, want 1", entries)
	}
	// The slot the expired entry occupied went back into circulation, rather than
	// leaking or being counted twice.
	checkIntegrity(t, c)
}

// TestBlobCacheEvictsLeastRecentlyUsed uses a single-shard cache so the eviction
// order is the whole cache's, not one shard's.
func TestBlobCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c, _ := newTestCache(t, time.Hour, 4)
	if len(c.shards) != 1 {
		t.Fatalf("a 4-entry cache has %d shards, want 1", len(c.shards))
	}
	for i := range 4 {
		c.store(testCacheRegistry, testCacheRepository, blobDigest(i), int64(i))
	}
	// Touch the oldest entry, so it is no longer the one to go.
	if _, ok := c.lookup(testCacheRegistry, testCacheRepository, blobDigest(0)); !ok {
		t.Fatal("blob 0 was not cached")
	}
	c.store(testCacheRegistry, testCacheRepository, blobDigest(4), 4)

	for _, i := range []int{0, 2, 3, 4} {
		if _, ok := c.lookup(testCacheRegistry, testCacheRepository, blobDigest(i)); !ok {
			t.Errorf("blob %d was evicted, want it kept", i)
		}
	}
	if _, ok := c.lookup(testCacheRegistry, testCacheRepository, blobDigest(1)); ok {
		t.Error("blob 1 survived, want the least recently used entry evicted")
	}
	if stats := c.stats(); stats.entries != 4 || stats.evictedCapacity != 1 {
		t.Fatalf("entries = %d, capacity evictions = %d; want 4 and 1", stats.entries, stats.evictedCapacity)
	}
	checkIntegrity(t, c)
}

// TestBlobCacheNeverExceedsCapacity stores far more blobs than fit, over every
// shard, and checks the structure is still coherent and still bounded.
func TestBlobCacheNeverExceedsCapacity(t *testing.T) {
	c, _ := newTestCache(t, time.Hour, 512)
	for i := range 10_000 {
		c.store(testCacheRegistry, testCacheRepository, blobDigest(i), int64(i))
	}
	stats := c.stats()
	if stats.entries > stats.capacity {
		t.Fatalf("entries = %d, above the capacity of %d", stats.entries, stats.capacity)
	}
	if stats.evictedCapacity == 0 {
		t.Fatal("no capacity evictions after storing 10000 blobs in a 512-entry cache")
	}
	// The most recently stored blobs are the ones a fleet is working on, so they
	// are the ones that must have survived. Only the last shard-full is
	// guaranteed: each shard evicts its own least recently used entry.
	found := 0
	for i := 10_000 - int(stats.capacity)/len(c.shards); i < 10_000; i++ {
		if _, ok := c.lookup(testCacheRegistry, testCacheRepository, blobDigest(i)); ok {
			found++
		}
	}
	if want := int(stats.capacity) / len(c.shards) / 2; found < want {
		t.Errorf("only %d of the most recent blobs survived, want at least %d", found, want)
	}
	checkIntegrity(t, c)
}

// TestBlobCacheRespectsMemoryBound checks the promise the flag makes: the cache
// allocates its whole bound at startup, and not a byte more.
func TestBlobCacheRespectsMemoryBound(t *testing.T) {
	for _, maxBytes := range []int64{
		MinBlobExistenceCacheBytes,
		64 << 10,
		1 << 20,
		64 << 20,
		//

		// A bound that is not a multiple of the entry cost, so the rounding is
		// exercised too.
		12345678,
	} {
		t.Run(fmt.Sprint(maxBytes), func(t *testing.T) {
			c := newBlobExistenceCache(time.Hour, maxBytes)
			if c == nil {
				t.Fatalf("newBlobExistenceCache(1h, %d) is disabled", maxBytes)
			}
			var buckets int64
			for i := range c.shards {
				buckets += int64(len(c.shards[i].buckets))
			}
			allocated := c.capacity*entryBytes + c.capacity*slotBytes + buckets*4
			if allocated > maxBytes {
				t.Errorf("allocated %d bytes for a bound of %d", allocated, maxBytes)
			}
			// Fill it, so anything that would grow at runtime has to.
			for i := range int(c.capacity) * 2 {
				c.store(testCacheRegistry, testCacheRepository, blobDigest(i), int64(i))
			}
			if entries := c.stats().entries; entries > c.capacity {
				t.Errorf("entries = %d, above the capacity of %d", entries, c.capacity)
			}
			checkIntegrity(t, c)
		})
	}
}

func TestBlobCacheDisabledConfigurations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ttl      time.Duration
		maxBytes int64
	}{
		{"zero ttl", 0, 64 << 20},
		{"negative ttl", -time.Hour, 64 << 20},
		{"zero memory", time.Hour, 0},
		{"negative memory", time.Hour, -1},
		{"too little memory for one entry", time.Hour, MinBlobExistenceCacheBytes - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newBlobExistenceCache(tc.ttl, tc.maxBytes)
			if c != nil {
				t.Fatalf("newBlobExistenceCache(%v, %d) built a cache, want it disabled", tc.ttl, tc.maxBytes)
			}
			// A disabled cache is a nil one, and every method has to tolerate it so
			// the gateway needs no second code path.
			c.store(testCacheRegistry, testCacheRepository, testCacheDigest, 1)
			if _, ok := c.lookup(testCacheRegistry, testCacheRepository, testCacheDigest); ok {
				t.Error("a disabled cache reported a hit")
			}
			if stats := c.stats(); stats != (cacheStats{}) {
				t.Errorf("stats of a disabled cache = %+v, want the zero value", stats)
			}
		})
	}
}

// TestBlobCacheSkipsOversizeKeys covers the one case a store is dropped: a key
// too long for the fixed arena slice an entry owns.
func TestBlobCacheSkipsOversizeKeys(t *testing.T) {
	c, _ := newTestCache(t, time.Hour, 64)
	long := strings.Repeat("a", entryBytes)

	c.store(testCacheRegistry, long, testCacheDigest, 1)
	if _, ok := c.lookup(testCacheRegistry, long, testCacheDigest); ok {
		t.Error("an oversize key was cached")
	}
	if entries := c.stats().entries; entries != 0 {
		t.Errorf("entries = %d after an oversize store, want 0", entries)
	}

	// A key of exactly the arena slice length still fits.
	fits := strings.Repeat("b", entryBytes-len(testCacheRegistry)-len(testCacheDigest))
	c.store(testCacheRegistry, fits, testCacheDigest, 2)
	if _, ok := c.lookup(testCacheRegistry, fits, testCacheDigest); !ok {
		t.Error("a key filling the arena slice exactly was not cached")
	}
}

// TestBlobCacheDoesNotAllocate is the load-bearing test for the cache's design:
// the memory is taken once, at startup, so serving from it must not put anything
// on the heap however long the process runs.
func TestBlobCacheDoesNotAllocate(t *testing.T) {
	c, _ := newTestCache(t, time.Hour, 64)
	c.store(testCacheRegistry, testCacheRepository, testCacheDigest, 4096)

	if allocs := testing.AllocsPerRun(200, func() {
		c.lookup(testCacheRegistry, testCacheRepository, testCacheDigest)
	}); allocs != 0 {
		t.Errorf("lookup allocated %v objects per call, want 0", allocs)
	}
	// Storing past capacity, so every call evicts and reuses a slot. The digests
	// are built up front: formatting one would be the measured allocation.
	digests := make([]string, 256)
	for i := range digests {
		digests[i] = blobDigest(i)
	}
	i := 0
	if allocs := testing.AllocsPerRun(200, func() {
		i = (i + 1) % len(digests)
		c.store(testCacheRegistry, testCacheRepository, digests[i], int64(i))
	}); allocs != 0 {
		t.Errorf("store allocated %v objects per call, want 0", allocs)
	}
}

// TestBlobCacheConcurrent hammers the cache from every direction at once. Run
// with -race, it is what says the sharded locking is right; the integrity check
// afterwards is what says no interleaving corrupted a list.
func TestBlobCacheConcurrent(t *testing.T) {
	c, _ := newTestCache(t, time.Hour, 1024)
	const (
		goroutines = 16
		iterations = 2000
		keyspace   = 4096
	)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(g)))
			for range iterations {
				digest := blobDigest(rng.Intn(keyspace))
				repository := fmt.Sprintf("team/service%d", rng.Intn(4))
				if rng.Intn(2) == 0 {
					c.store(testCacheRegistry, repository, digest, 1)
					continue
				}
				c.lookup(testCacheRegistry, repository, digest)
			}
		}()
	}
	wg.Wait()

	stats := c.stats()
	if stats.entries > stats.capacity {
		t.Fatalf("entries = %d, above the capacity of %d", stats.entries, stats.capacity)
	}
	checkIntegrity(t, c)

	// An entry stored after the storm is still found, so the structure is not
	// merely uncorrupted but still usable.
	c.store(testCacheRegistry, testCacheRepository, testCacheDigest, 99)
	if length, ok := c.lookup(testCacheRegistry, testCacheRepository, testCacheDigest); !ok || length != 99 {
		t.Fatalf("after concurrent use, lookup = (%d, %v), want (99, true)", length, ok)
	}
}

func TestShardCount(t *testing.T) {
	// Whatever the machine, the shard count is a power of two, at least one, and
	// never so high that a shard holds fewer than minSlotsPerShard entries.
	for _, capacity := range []int64{1, 63, 64, 128, 1024, 178_481, 1 << 22} {
		shards := shardCount(capacity)
		if shards < 1 || shards&(shards-1) != 0 {
			t.Errorf("shardCount(%d) = %d, want a power of two of at least 1", capacity, shards)
		}
		if shards > 1 && capacity/int64(shards) < minSlotsPerShard {
			t.Errorf("shardCount(%d) = %d, leaving %d slots per shard (want at least %d)",
				capacity, shards, capacity/int64(shards), minSlotsPerShard)
		}
		if shards > maxShards {
			t.Errorf("shardCount(%d) = %d, above the cap of %d", capacity, shards, maxShards)
		}
	}
}

// BenchmarkBlobCache measures the cache from every core at once, which is the
// shape of the load it is built for: a build farm's workers all probing at the
// same time. The mixed case is the realistic one — a fleet pushing overlapping
// image sets hits mostly the same blobs, and refreshes them as they expire.
func BenchmarkBlobCache(b *testing.B) {
	digests := make([]string, 8192)
	for i := range digests {
		digests[i] = blobDigest(i)
	}
	for _, bc := range []struct {
		name string
		// storeEvery is how many lookups fall between two stores; 0 means never
		// store, 1 means store every time.
		storeEvery int
	}{
		{"lookup", 0},
		{"mixed", 16},
		{"store", 1},
	} {
		b.Run(bc.name, func(b *testing.B) {
			c := newBlobExistenceCache(time.Hour, 64<<20)
			for _, digest := range digests {
				c.store(testCacheRegistry, testCacheRepository, digest, 4096)
			}
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					i++
					digest := digests[i%len(digests)]
					if bc.storeEvery > 0 && i%bc.storeEvery == 0 {
						c.store(testCacheRegistry, testCacheRepository, digest, 4096)
						continue
					}
					c.lookup(testCacheRegistry, testCacheRepository, digest)
				}
			})
		})
	}
}

// checkIntegrity verifies every shard's invariants: the LRU list is a consistent
// doubly-linked list of exactly the live entries, each of those entries is
// reachable through its hash bucket, and every slot is accounted for exactly
// once (live, free, or never used).
func checkIntegrity(t *testing.T, c *blobExistenceCache) {
	t.Helper()
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		checkShardIntegrity(t, i, s)
		s.mu.Unlock()
	}
}

func checkShardIntegrity(t *testing.T, shard int, s *cacheShard) {
	t.Helper()
	seen := make(map[int32]bool, len(s.slots))
	var forward []int32
	prev := int32(-1)
	for idx := s.head; idx >= 0; idx = s.slots[idx].next {
		if seen[idx] {
			t.Fatalf("shard %d: LRU list revisits slot %d", shard, idx)
		}
		seen[idx] = true
		if s.slots[idx].prev != prev {
			t.Fatalf("shard %d: slot %d has prev = %d, want %d", shard, idx, s.slots[idx].prev, prev)
		}
		forward = append(forward, idx)
		prev = idx
	}
	if prev != s.tail {
		t.Fatalf("shard %d: LRU list ends at slot %d, but tail is %d", shard, prev, s.tail)
	}
	if live := s.live.Load(); int64(len(forward)) != live {
		t.Fatalf("shard %d: LRU list holds %d entries, but live counts %d", shard, len(forward), live)
	}
	// Every live entry must be reachable through its bucket, which is what a
	// lookup walks.
	for _, idx := range forward {
		e := &s.slots[idx]
		found := false
		for at := s.buckets[e.hash&s.mask]; at >= 0; at = s.slots[at].hnext {
			if at == idx {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("shard %d: live slot %d is not in bucket %d", shard, idx, e.hash&s.mask)
		}
		if e.expires == 0 {
			t.Fatalf("shard %d: live slot %d has no deadline", shard, idx)
		}
	}
	// ...and every bucket must hold only live entries, in a consistent chain.
	chained := 0
	for bucket := range s.buckets {
		hprev := int32(-1)
		for idx := s.buckets[bucket]; idx >= 0; idx = s.slots[idx].hnext {
			if !seen[idx] {
				t.Fatalf("shard %d: bucket %d holds slot %d, which is not a live entry", shard, bucket, idx)
			}
			if s.slots[idx].hprev != hprev {
				t.Fatalf("shard %d: slot %d has hprev = %d, want %d", shard, idx, s.slots[idx].hprev, hprev)
			}
			hprev = idx
			chained++
		}
	}
	if chained != len(forward) {
		t.Fatalf("shard %d: buckets hold %d entries, LRU list holds %d", shard, chained, len(forward))
	}
	// Free slots plus live slots plus never-used slots account for the table.
	free := 0
	for idx := s.free; idx >= 0; idx = s.slots[idx].next {
		if seen[idx] {
			t.Fatalf("shard %d: slot %d is both live and free", shard, idx)
		}
		free++
		if free > len(s.slots) {
			t.Fatalf("shard %d: the free list cycles", shard)
		}
	}
	if got := len(forward) + free; got != int(s.used) {
		t.Fatalf("shard %d: %d live + %d free = %d, but %d slots have been used", shard, len(forward), free, got, s.used)
	}
}
