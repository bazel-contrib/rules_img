package registry

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

// countingHelper answers every lookup with credentials naming the server URL,
// counting calls per URL. While gate is non-nil, each call blocks on it.
type countingHelper struct {
	mu    sync.Mutex
	calls map[string]int
	gate  chan struct{}
}

func (h *countingHelper) Get(serverURL string) (string, string, error) {
	h.mu.Lock()
	if h.calls == nil {
		h.calls = make(map[string]int)
	}
	h.calls[serverURL]++
	gate := h.gate
	h.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return "user", "token-for-" + serverURL, nil
}

func (h *countingHelper) count(serverURL string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[serverURL]
}

func TestCachingHelperSingleFlightsConcurrentRepositories(t *testing.T) {
	inner := &countingHelper{gate: make(chan struct{})}
	kc := authn.NewKeychainFromHelper(newCachingHelper("test", inner, time.Hour, nil))

	const registry = "registry.example"
	const repositories = 64
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := range repositories {
		wg.Add(1)
		go func() {
			defer wg.Done()
			repo, err := name.NewRepository(fmt.Sprintf("%s/team/image-%d", registry, i))
			if err != nil {
				t.Error(err)
				return
			}
			auth, err := kc.Resolve(repo)
			if err != nil {
				t.Error(err)
				return
			}
			cfg, err := auth.Authorization()
			if err != nil || cfg.Password != "token-for-"+registry {
				failures.Add(1)
			}
		}()
	}
	// Let the lookups pile up behind the first one before it answers.
	for inner.count(registry) == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(inner.gate)
	wg.Wait()

	if got := inner.count(registry); got != 1 {
		t.Errorf("inner helper called %d times for %d repositories in one registry, want 1", got, repositories)
	}
	if n := failures.Load(); n != 0 {
		t.Errorf("%d lookups returned the wrong credentials", n)
	}

	other := "other.example"
	repo, _ := name.NewRepository(other + "/image")
	if _, err := kc.Resolve(repo); err != nil {
		t.Fatal(err)
	}
	if got := inner.count(other); got != 1 {
		t.Errorf("inner helper called %d times for a second registry, want 1", got)
	}
}

func TestCachingHelperDebugLogsHitsAndMisses(t *testing.T) {
	var debugLog bytes.Buffer
	inner := &countingHelper{}
	c := newCachingHelper("test", inner, time.Hour, &debugLog)
	for range 2 {
		if _, _, err := c.Get("registry.example"); err != nil {
			t.Fatal(err)
		}
	}
	lines := strings.Split(strings.TrimSuffix(debugLog.String(), "\n"), "\n")
	wantPrefixes := []string{
		"test cache: miss for registry.example, fetching",
		"test cache: fetched registry.example, cached until ",
		"test cache: hit for registry.example, cached until ",
	}
	if len(lines) != len(wantPrefixes) {
		t.Fatalf("debug log has %d lines, want %d:\n%s", len(lines), len(wantPrefixes), debugLog.String())
	}
	for i, want := range wantPrefixes {
		if !strings.HasPrefix(lines[i], want) {
			t.Errorf("line %d = %q, want prefix %q", i, lines[i], want)
		}
	}
}

func TestPrefixWriter(t *testing.T) {
	var out bytes.Buffer
	w := &prefixWriter{prefix: []byte("P: "), out: &out}
	for _, s := range []string{"one\ntwo", " halves\n", "\n", "three\nfour\n"} {
		n, err := w.Write([]byte(s))
		if err != nil || n != len(s) {
			t.Fatalf("Write(%q) = %d, %v; want %d, nil", s, n, err, len(s))
		}
	}
	if got, want := out.String(), "P: one\nP: two halves\nP: \nP: three\nP: four\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}
