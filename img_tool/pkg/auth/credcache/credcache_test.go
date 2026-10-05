package credcache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetSingleFlightsConcurrentLookups(t *testing.T) {
	c := New[string](time.Hour)
	gate := make(chan struct{})
	var calls atomic.Int32
	fetch := func(context.Context) (string, time.Time, error) {
		calls.Add(1)
		<-gate
		return "token", time.Time{}, nil
	}

	const lookups = 64
	var wg sync.WaitGroup
	var wrong atomic.Int32
	for range lookups {
		wg.Go(func() {
			if v, _, err := c.Get(t.Context(), "registry.example", fetch); err != nil || v != "token" {
				wrong.Add(1)
			}
		})
	}
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("fetch called %d times for %d concurrent lookups, want 1", got, lookups)
	}
	if n := wrong.Load(); n != 0 {
		t.Errorf("%d lookups returned the wrong value", n)
	}

	if _, _, err := c.Get(t.Context(), "other.example", fetch); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("fetch called %d times after a second key, want 2", got)
	}
}

func TestGetDoesNotCacheFailures(t *testing.T) {
	c := New[string](time.Hour)
	var calls int
	fail := true
	fetch := func(context.Context) (string, time.Time, error) {
		calls++
		if fail {
			return "", time.Time{}, errors.New("throttled")
		}
		return "token", time.Time{}, nil
	}

	if _, _, err := c.Get(t.Context(), "k", fetch); err == nil {
		t.Fatal("expected the fetch error")
	}
	fail = false
	if v, _, err := c.Get(t.Context(), "k", fetch); err != nil || v != "token" {
		t.Fatalf("Get after a failure = %q, %v; want a fresh value", v, err)
	}
	if calls != 2 {
		t.Errorf("fetch called %d times, want 2 (failure retried)", calls)
	}
}

func TestGetExpiry(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := New[string](time.Hour)
	c.now = func() time.Time { return now }

	var calls int
	var expiresAt time.Time
	fetch := func(context.Context) (string, time.Time, error) {
		calls++
		return "token", expiresAt, nil
	}
	get := func() time.Time {
		t.Helper()
		_, exp, err := c.Get(t.Context(), "k", fetch)
		if err != nil {
			t.Fatal(err)
		}
		return exp
	}

	// No reported expiry: the TTL applies.
	if exp := get(); !exp.Equal(now.Add(time.Hour)) {
		t.Errorf("expiry without a reported one = %v, want now+ttl", exp)
	}
	now = now.Add(59 * time.Minute)
	get()
	if calls != 1 {
		t.Fatalf("fetch called %d times within the TTL, want 1", calls)
	}
	now = now.Add(time.Minute)
	get()
	if calls != 2 {
		t.Fatalf("fetch called %d times after the TTL, want 2", calls)
	}

	// A reported expiry wins over the TTL, whether shorter or longer.
	for _, lifetime := range []time.Duration{time.Minute, 3 * time.Hour} {
		now = now.Add(2 * time.Hour)
		expiresAt = now.Add(lifetime)
		before := calls
		if exp := get(); !exp.Equal(expiresAt) {
			t.Errorf("expiry = %v, want the reported %v", exp, expiresAt)
		}
		now = now.Add(lifetime - time.Second)
		get()
		if calls != before+1 {
			t.Errorf("lifetime %v: refetched before the reported expiry", lifetime)
		}
		now = now.Add(time.Second)
		get()
		if calls != before+2 {
			t.Errorf("lifetime %v: not refetched at the reported expiry", lifetime)
		}
	}
}

func TestGetWaiterRetriesWhenStarterIsCanceled(t *testing.T) {
	c := New[string](time.Hour)
	started := make(chan struct{})
	starterCtx, cancel := context.WithCancel(t.Context())

	var calls atomic.Int32
	fetch := func(ctx context.Context) (string, time.Time, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return "", time.Time{}, ctx.Err()
		}
		return "token", time.Time{}, nil
	}

	starterErr := make(chan error, 1)
	go func() {
		_, _, err := c.Get(starterCtx, "k", fetch)
		starterErr <- err
	}()
	<-started

	waiter := make(chan string, 1)
	go func() {
		v, _, err := c.Get(t.Context(), "k", fetch)
		if err != nil {
			v = "error: " + err.Error()
		}
		waiter <- v
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()

	if err := <-starterErr; !errors.Is(err, context.Canceled) {
		t.Errorf("starter error = %v, want context.Canceled", err)
	}
	if v := <-waiter; v != "token" {
		t.Errorf("waiter got %q, want it to fetch again and get the token", v)
	}
}

func TestGetWaiterStopsWithOwnContext(t *testing.T) {
	c := New[string](time.Hour)
	gate := make(chan struct{})
	defer close(gate)
	fetch := func(context.Context) (string, time.Time, error) {
		<-gate
		return "token", time.Time{}, nil
	}
	go c.Get(t.Context(), "k", fetch)
	for {
		c.mu.Lock()
		_, ok := c.entries["k"]
		c.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if _, _, err := c.Get(ctx, "k", fetch); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiter error = %v, want its own context.DeadlineExceeded", err)
	}
}

func TestGetOutcome(t *testing.T) {
	c := New[string](time.Hour)
	gate := make(chan struct{})
	fetch := func(context.Context) (string, time.Time, error) {
		<-gate
		return "token", time.Time{}, nil
	}

	leader := make(chan Outcome, 1)
	go func() {
		_, _, o, _ := c.GetOutcome(t.Context(), "k", fetch)
		leader <- o
	}()
	for {
		c.mu.Lock()
		_, ok := c.entries["k"]
		c.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(time.Millisecond)
	}
	waiter := make(chan Outcome, 1)
	go func() {
		_, _, o, _ := c.GetOutcome(t.Context(), "k", fetch)
		waiter <- o
	}()
	time.Sleep(20 * time.Millisecond)
	close(gate)

	if o := <-leader; o != Fetched {
		t.Errorf("first lookup outcome = %v, want %v", o, Fetched)
	}
	if o := <-waiter; o != Shared {
		t.Errorf("concurrent lookup outcome = %v, want %v", o, Shared)
	}
	if _, _, o, _ := c.GetOutcome(t.Context(), "k", fetch); o != Hit {
		t.Errorf("later lookup outcome = %v, want %v", o, Hit)
	}
}
