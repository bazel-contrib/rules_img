package registry

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/registry"
	registryv1 "github.com/google/go-containerregistry/pkg/v1"
)

// keepAliveBatchSize bounds how many digests go into a single FindMissingBlobs
// call. A digest costs on the order of a hundred bytes on the wire, so a
// thousand of them stays far inside the default 4 MiB gRPC message limit while
// still amortizing the round trip.
const keepAliveBatchSize = 1000

// BlobPresenceChecker asks a content-addressed store which of a set of blobs it
// is missing. This is the subset of *cas.CAS a KeepAlive needs.
type BlobPresenceChecker interface {
	FindMissingBlobs(ctx context.Context, digests []cas.Digest) ([]cas.Digest, error)
}

// KeepAliveConfig configures a KeepAlive.
type KeepAliveConfig struct {
	// RemoteCacheTTL is how long the remote cache is believed to keep a blob
	// nobody asks about. It is a belief, not a contract: the remote execution
	// API promises nothing about how long a blob stays in the CAS.
	RemoteCacheTTL time.Duration

	// ScanInterval is how often the goroutine wakes up to look for blobs due a
	// refresh.
	ScanInterval time.Duration

	// Clock reads the current time. Zero defaults to time.Now.
	Clock func() time.Time

	// Log records refreshes and blobs the cache turned out to have lost. Zero
	// defaults to a logger on stderr.
	Log *log.Logger
}

// KeepAlive keeps a registry's live blobs resident in a content-addressed store
// that evicts on its own schedule.
//
// A registry backed by Bazel's CAS serves blobs it does not own. The CAS may
// drop any of them at any time, and a manifest whose layers are gone is a
// manifest that cannot be pulled. Reading a blob is what marks it as recently
// used, so this asks about blobs nobody has read lately -- FindMissingBlobs is
// the cheapest such question, since it transfers no blob data -- and in doing so
// keeps them from aging out.
//
// A blob is refreshed once RemoteCacheTTL minus two scan intervals have passed
// since it was last used or last refreshed. The two intervals of slack mean a
// blob is asked about before the cache could have dropped it even if one scan is
// missed or runs late.
type KeepAlive struct {
	collector *registry.Collector
	checker   BlobPresenceChecker
	sizeCache *BlobSizeCache

	remoteCacheTTL time.Duration
	scanInterval   time.Duration
	refreshAge     time.Duration
	now            func() time.Time
	log            *log.Logger

	// lastKeptAlive is when we last asked the store about a blob. It is kept
	// here rather than fed back into the collector on purpose: refreshing a
	// blob in the CAS must not also extend the registry's own retention, or a
	// blob would keep itself alive forever.
	lastKeptAlive map[registryv1.Hash]time.Time
}

// KeepAliveStats counts what one scan did.
type KeepAliveStats struct {
	// Live is how many blobs the registry currently considers reachable.
	Live int
	// Refreshed is how many of those were asked about this scan.
	Refreshed int
	// Missing is how many the store turned out not to have.
	Missing int
	// Skipped is how many could not be asked about, because their size or
	// digest algorithm is not something the store can be queried with.
	Skipped int
}

// NewKeepAlive returns a KeepAlive over the blobs collector considers live. The
// sizeCache may be nil; when set, a blob the store reports missing loses its
// cached size, so later requests go back to asking the blob stores.
//
// A collector is required: it is what knows which blobs are still reachable. A
// registry that does not evict can still have one, configured with no TTL, which
// tracks the object graph without collecting from it.
func NewKeepAlive(collector *registry.Collector, checker BlobPresenceChecker, sizeCache *BlobSizeCache, cfg KeepAliveConfig) (*KeepAlive, error) {
	if collector == nil {
		return nil, errors.New("keeping blobs alive needs a registry collector to know which blobs are live")
	}
	if checker == nil {
		return nil, errors.New("keeping blobs alive needs a blob store to ask")
	}
	if cfg.RemoteCacheTTL <= 0 {
		return nil, errors.New("remote cache TTL must be positive")
	}
	if cfg.ScanInterval <= 0 {
		return nil, errors.New("scan interval must be positive")
	}

	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	logger := cfg.Log
	if logger == nil {
		logger = log.New(os.Stderr, "", log.LstdFlags)
	}

	refreshAge := cfg.RemoteCacheTTL - 2*cfg.ScanInterval
	if refreshAge <= 0 {
		refreshAge = 0
		logger.Printf("blob keepalive: scanning every %s leaves no slack inside a %s remote cache TTL, so every live blob is refreshed on every scan", cfg.ScanInterval, cfg.RemoteCacheTTL)
	}

	return &KeepAlive{
		collector:      collector,
		checker:        checker,
		sizeCache:      sizeCache,
		remoteCacheTTL: cfg.RemoteCacheTTL,
		scanInterval:   cfg.ScanInterval,
		refreshAge:     refreshAge,
		now:            now,
		log:            logger,
		lastKeptAlive:  make(map[registryv1.Hash]time.Time),
	}, nil
}

// Run scans on every tick until ctx is done.
func (k *KeepAlive) Run(ctx context.Context) {
	ticker := time.NewTicker(k.scanInterval)
	defer ticker.Stop()

	k.log.Printf("blob keepalive: refreshing live blobs every %s to stay inside a %s remote cache TTL", k.scanInterval, k.remoteCacheTTL)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stats, err := k.scanOnce(ctx)
			if err != nil {
				k.log.Printf("blob keepalive: %v", err)
			}
			if stats.Refreshed > 0 || stats.Missing > 0 || stats.Skipped > 0 {
				k.log.Printf("blob keepalive: %d live, %d refreshed, %d missing, %d not queryable", stats.Live, stats.Refreshed, stats.Missing, stats.Skipped)
			}
		}
	}
}

// scanOnce refreshes every live blob that is due one.
func (k *KeepAlive) scanOnce(ctx context.Context) (KeepAliveStats, error) {
	var stats KeepAliveStats
	now := k.now()
	dueBefore := now.Add(-k.refreshAge)

	// Group by digest algorithm: a single FindMissingBlobs call cannot mix them.
	due := make(map[string][]dueBlob)
	live := make(map[registryv1.Hash]struct{})
	k.collector.RangeLiveBlobs(func(blob registry.LiveBlob) bool {
		stats.Live++
		live[blob.Digest] = struct{}{}
		if blob.Size <= 0 {
			// Either we never learned the size, or the blob is empty. Neither
			// can be looked up: a CAS is keyed by digest *and* size, and the
			// empty blob is never fetched from it anyway.
			stats.Skipped++
			return true
		}
		lastRefresh := blob.LastUsed
		if keptAlive, ok := k.lastKeptAlive[blob.Digest]; ok && keptAlive.After(lastRefresh) {
			lastRefresh = keptAlive
		}
		if lastRefresh.After(dueBefore) {
			return true
		}
		due[blob.Digest.Algorithm] = append(due[blob.Digest.Algorithm], dueBlob{digest: blob.Digest, size: blob.Size})
		return true
	})

	// Blobs that are no longer live stop being our concern, and their
	// bookkeeping goes with them.
	for digest := range k.lastKeptAlive {
		if _, ok := live[digest]; !ok {
			delete(k.lastKeptAlive, digest)
		}
	}

	var errs []error
	for algorithm, blobs := range due {
		for start := 0; start < len(blobs); start += keepAliveBatchSize {
			end := min(start+keepAliveBatchSize, len(blobs))
			batch := blobs[start:end]

			casDigests := make([]cas.Digest, 0, len(batch))
			asked := make([]registryv1.Hash, 0, len(batch))
			for _, blob := range batch {
				casDigest, err := casDigestOf(blob.digest, blob.size)
				if err != nil {
					stats.Skipped++
					continue
				}
				casDigests = append(casDigests, casDigest)
				asked = append(asked, blob.digest)
			}
			if len(casDigests) == 0 {
				continue
			}

			missing, err := k.checker.FindMissingBlobs(ctx, casDigests)
			if err != nil {
				// Leave the timestamps alone so the next scan tries again.
				errs = append(errs, fmt.Errorf("refreshing %d %s blobs: %w", len(casDigests), algorithm, err))
				continue
			}
			for _, digest := range asked {
				k.lastKeptAlive[digest] = now
			}
			stats.Refreshed += len(asked)

			for _, casDigest := range missing {
				digest := registryv1.Hash{Algorithm: algorithm, Hex: hex.EncodeToString(casDigest.Hash)}
				stats.Missing++
				// The store lost a blob a live manifest still names, so pulling
				// that image is already broken. Say so, and stop claiming to
				// know the blob's size so requests fall back to the other
				// stores.
				k.log.Printf("blob keepalive: %s is missing from the remote cache but is still referenced", digest)
				if k.sizeCache != nil {
					k.sizeCache.Delete(digest)
				}
				delete(k.lastKeptAlive, digest)
			}
		}
	}
	return stats, errors.Join(errs...)
}

// dueBlob is a live blob that needs refreshing, with the size a CAS lookup
// needs.
type dueBlob struct {
	digest registryv1.Hash
	size   int64
}

// casDigestOf converts a registry digest and its size into a CAS digest.
func casDigestOf(hash registryv1.Hash, size int64) (cas.Digest, error) {
	rawHash, err := hex.DecodeString(hash.Hex)
	if err != nil {
		return cas.Digest{}, fmt.Errorf("decoding digest %s: %w", hash, err)
	}
	switch hash.Algorithm {
	case "sha256":
		return cas.SHA256(rawHash, size), nil
	case "sha512":
		return cas.SHA512(rawHash, size), nil
	}
	return cas.Digest{}, fmt.Errorf("unsupported digest algorithm: %s", hash.Algorithm)
}
