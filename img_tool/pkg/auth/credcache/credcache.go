// Package credcache memoizes credential lookups with single-flight semantics.
//
// It is meant for credential sources whose answer depends only on the key they
// are asked about, typically a registry host: a credential helper, or a cloud
// provider's token endpoint. Such a source is commonly slow (an exec, an STS
// round trip) and rate limited, and a client resolving many repositories of one
// registry at once would otherwise ask it the same question once per repository.
//
// Do not put a source in front of this cache under a coarser key than it looks
// up by. A Docker config, for example, may hold different credentials for two
// repositories of the same registry, so caching it per registry would hand one
// repository's credentials to the other.
package credcache

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Fetch looks up the credential for one key. A non-zero expiresAt is when the
// credential stops being usable; a zero one leaves that to the cache's TTL.
type Fetch[V any] func(ctx context.Context) (value V, expiresAt time.Time, err error)

// Cache memoizes credentials per key. Concurrent lookups of the same key share
// one fetch, and a successful answer is reused until it expires: at the expiry
// the fetch reported, or ttl after it returned if it reported none. Failures are
// not cached, so the next lookup after one fetches again.
//
// The zero value is not usable; create one with [New].
type Cache[V any] struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	entries map[string]*entry[V]
}

// entry is one fetch of one key. done is closed once the fetch has returned;
// the other fields are written before that and only read after it.
type entry[V any] struct {
	done      chan struct{}
	value     V
	expiresAt time.Time
	err       error
}

// New returns an empty cache that keeps a credential whose fetch reported no
// expiry for ttl.
func New[V any](ttl time.Duration) *Cache[V] {
	return &Cache[V]{
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[string]*entry[V]),
	}
}

// Outcome is how a lookup was answered.
type Outcome int

const (
	// Hit means an unexpired cached credential answered the lookup.
	Hit Outcome = iota
	// Shared means the lookup waited for a fetch another lookup had started.
	Shared
	// Fetched means the lookup ran the fetch itself.
	Fetched
)

func (o Outcome) String() string {
	switch o {
	case Hit:
		return "hit"
	case Shared:
		return "shared"
	case Fetched:
		return "fetched"
	}
	return "unknown"
}

// Get returns the credential for key, calling fetch only if no unexpired
// credential is cached and no other lookup of key is already fetching one.
//
// The fetch runs under the context of the lookup that started it. A lookup that
// waits on someone else's fetch stops waiting when its own ctx ends, and fetches
// again itself if that fetch failed only because its starter's context ended.
func (c *Cache[V]) Get(ctx context.Context, key string, fetch Fetch[V]) (V, time.Time, error) {
	value, expiresAt, _, err := c.GetOutcome(ctx, key, fetch)
	return value, expiresAt, err
}

// GetOutcome is [Cache.Get], also reporting how the lookup was answered.
func (c *Cache[V]) GetOutcome(ctx context.Context, key string, fetch Fetch[V]) (V, time.Time, Outcome, error) {
	for {
		c.mu.Lock()
		e, ok := c.entries[key]
		if ok {
			select {
			case <-e.done:
				if c.now().Before(e.expiresAt) {
					c.mu.Unlock()
					return e.value, e.expiresAt, Hit, nil
				}
				// Expired: replace it below.
				ok = false
			default:
			}
		}
		if ok {
			c.mu.Unlock()
			select {
			case <-e.done:
			case <-ctx.Done():
				var zero V
				return zero, time.Time{}, Shared, ctx.Err()
			}
			if e.err != nil && isContextError(e.err) && ctx.Err() == nil {
				continue
			}
			return e.value, e.expiresAt, Shared, e.err
		}

		e = &entry[V]{done: make(chan struct{})}
		c.entries[key] = e
		c.mu.Unlock()

		e.value, e.expiresAt, e.err = fetch(ctx)
		if e.err == nil && e.expiresAt.IsZero() {
			e.expiresAt = c.now().Add(c.ttl)
		}
		if e.err != nil {
			c.mu.Lock()
			if c.entries[key] == e {
				delete(c.entries, key)
			}
			c.mu.Unlock()
		}
		close(e.done)
		return e.value, e.expiresAt, Fetched, e.err
	}
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
