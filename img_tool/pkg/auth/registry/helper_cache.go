package registry

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/credcache"
	"github.com/google/go-containerregistry/pkg/authn"
)

// cachingHelper puts a [credcache.Cache] in front of an [authn.Helper].
//
// [authn.NewKeychainFromHelper] only ever asks a helper about a registry host,
// never a repository, so this caches per registry, which is exactly what the
// helper looks up by. The helper reports no expiry, so a credential is reused
// for the cache's TTL.
type cachingHelper struct {
	name  string
	inner authn.Helper
	cache *credcache.Cache[helperCredential]
	// debugLog, if set, receives a line for every lookup saying how the cache
	// answered it.
	debugLog io.Writer
}

type helperCredential struct {
	username, secret string
}

func newCachingHelper(name string, inner authn.Helper, ttl time.Duration, debugLog io.Writer) *cachingHelper {
	return &cachingHelper{name: name, inner: inner, cache: credcache.New[helperCredential](ttl), debugLog: debugLog}
}

func (c *cachingHelper) Get(serverURL string) (string, string, error) {
	cred, expiresAt, outcome, err := c.cache.GetOutcome(context.Background(), serverURL, func(context.Context) (helperCredential, time.Time, error) {
		if c.debugLog != nil {
			fmt.Fprintf(c.debugLog, "%s cache: miss for %s, fetching\n", c.name, serverURL)
		}
		username, secret, err := c.inner.Get(serverURL)
		return helperCredential{username, secret}, time.Time{}, err
	})
	if c.debugLog != nil {
		switch {
		case outcome == credcache.Hit:
			fmt.Fprintf(c.debugLog, "%s cache: hit for %s, cached until %s\n", c.name, serverURL, expiresAt.Format(time.RFC3339))
		case outcome == credcache.Shared && err != nil:
			fmt.Fprintf(c.debugLog, "%s cache: miss for %s, the fetch already in flight failed: %v\n", c.name, serverURL, err)
		case outcome == credcache.Shared:
			fmt.Fprintf(c.debugLog, "%s cache: miss for %s, used the fetch already in flight, cached until %s\n", c.name, serverURL, expiresAt.Format(time.RFC3339))
		case err != nil:
			fmt.Fprintf(c.debugLog, "%s cache: fetch for %s failed, not cached: %v\n", c.name, serverURL, err)
		default:
			fmt.Fprintf(c.debugLog, "%s cache: fetched %s, cached until %s\n", c.name, serverURL, expiresAt.Format(time.RFC3339))
		}
	}
	return cred.username, cred.secret, err
}
