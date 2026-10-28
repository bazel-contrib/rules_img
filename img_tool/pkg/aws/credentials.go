package aws

import (
	"context"
	"errors"
	"sync"
	"time"
)

// expiryWindow is how long before its actual expiry a credential is
// considered stale and refreshed.
const expiryWindow = 5 * time.Minute

// Credentials are the keys used to sign a request.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// Source describes where the credentials came from, for diagnostics.
	Source string
	// Expires is when the credentials stop working. The zero value means they
	// never expire.
	Expires time.Time
}

// HasKeys reports whether both the access key ID and secret are set.
func (c Credentials) HasKeys() bool {
	return c.AccessKeyID != "" && c.SecretAccessKey != ""
}

func (c Credentials) expiring(now time.Time) bool {
	return !c.Expires.IsZero() && !now.Add(expiryWindow).Before(c.Expires)
}

// Provider retrieves credentials.
type Provider interface {
	Retrieve(ctx context.Context) (Credentials, error)
}

// ProviderFunc adapts a function to a [Provider].
type ProviderFunc func(ctx context.Context) (Credentials, error)

// Retrieve implements [Provider].
func (f ProviderFunc) Retrieve(ctx context.Context) (Credentials, error) { return f(ctx) }

// StaticProvider returns a provider that always returns c.
func StaticProvider(c Credentials) Provider {
	return ProviderFunc(func(context.Context) (Credentials, error) { return c, nil })
}

// cachedProvider memoizes a provider until its credentials are about to
// expire. Concurrent callers share one refresh.
type cachedProvider struct {
	inner Provider
	now   func() time.Time

	mu    sync.Mutex
	creds Credentials
	ok    bool
}

// NewCachedProvider wraps p so credentials are reused until shortly before
// they expire.
func NewCachedProvider(p Provider) Provider {
	if c, ok := p.(*cachedProvider); ok {
		return c
	}
	return &cachedProvider{inner: p, now: time.Now}
}

func (c *cachedProvider) Retrieve(ctx context.Context) (Credentials, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok && !c.creds.expiring(c.now()) {
		return c.creds, nil
	}
	creds, err := c.inner.Retrieve(ctx)
	if err != nil {
		return Credentials{}, err
	}
	if !creds.HasKeys() {
		return Credentials{}, errors.New("aws: credential provider returned empty credentials")
	}
	c.creds, c.ok = creds, true
	return creds, nil
}
