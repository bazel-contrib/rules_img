# Garbage collection

The registry in this package can be told to forget things, which is what makes it
usable as a long-running service rather than a process you restart. This is that
mechanism: what it keeps, what it collects, and how to configure it. Its main user is
the [CAS registry](../../cmd/registry), which is where the flags below live; see
[Push strategies](../../../docs/push-strategies.md#cas-registry-push) for how that
registry is deployed.

Nothing is collected unless a `Collector` is passed to `New`. Without one the
registry keeps everything until the process exits, and it does no bookkeeping for
eviction at all:

```go
store := registry.NewMemStore()
collector := registry.NewCollector(store, registry.CollectorConfig{
    TTL:    6 * time.Hour,
    TagTTL: 7 * 24 * time.Hour,
})
handler := registry.New(
    registry.WithStore(store),
    registry.WithCollector(collector),
)
```

Those are the two abstractions. A **`Store`** holds manifests and tags and knows
nothing about eviction; every registry has one. A **`Collector`** decides what the
store may forget; only a registry that evicts has one.

## Why tracing, and not a TTL per entry

A stored object here is not independent of the others. An image index is made of the
per-platform manifests it lists; a manifest is made of its config and layer blobs. An
index anyone can still pull needs every one of them.

Giving each stored reference an expiry of its own cannot express that. Nothing in a set
of independent timers relates an index to its children, so they expire on their own
schedules — and any request that refreshes one of them without the others pulls them
further apart. The registry can then hold a tag that resolves and an index that
resolves over a child that is gone: a pull that fails `MANIFEST_UNKNOWN` partway
through, and a push of another tag for that index that fails its own sub-manifest
validation ([issue #695](https://github.com/bazel-contrib/rules_img/issues/695)). A
longer TTL moves that window without closing it.

So reachability decides, not age. **Roots** are tags, and any manifest or blob used
within the TTL. Everything reachable from a root survives, however long ago it was
last touched itself.

## The object graph

| Node | Keyed by | References |
| --- | --- | --- |
| tag | repository and name | the manifest it resolves to |
| manifest | repository and digest | its config and layer blobs, and its subject |
| index | repository and digest | the manifests it lists, and its subject |
| blob | digest | nothing — blobs are opaque leaves |

Plus one edge that runs backwards. A manifest declaring a `subject` is a *referrer*
of it, and **referrers stay alive as long as their subject does**. That is what keeps
a signature or an attestation from being swept out from under the image it describes.
The edge is one-way on purpose: a referrer does not keep its subject alive, or
attaching a signature to an image would pin the image forever.

Blobs are keyed by digest with no repository, because that is how this registry
actually behaves — `memHandler`, `diskHandler` and the CAS registry's S3 key function
all ignore the repository they are handed, and a remote-execution CAS is global. A
blob node remembers a repository it was seen in only so the callbacks below have a
plausible one to report. Manifests stay per-repository, where manifest existence
genuinely is per-repository.

`Kind` — manifest, index, or neither — is decided once, when a manifest is stored, from
the manifest's own `mediaType`, then its shape, and only then the `Content-Type` header
a client sent. Content outranks transport metadata: a manifest's `mediaType` is covered
by its digest, and reading an index as a plain manifest would hide the children it needs
from the collector. That same classification is what decides whether a push has its
sub-manifests validated, so validation and tracing can never disagree about what an
object is.

**Edges are re-derived by parsing, not stored.** containerd has to record its edges
as labels (`containerd.io/gc.ref.content.*`) because its content store holds bytes
that are opaque to the daemon; ours holds self-describing JSON, so parsing during a
sweep keeps one source of truth and cannot drift from the manifests it describes.

## A sweep

1. Walk every repository, parse every manifest, and build the forward references and
   the inverse of the subject edge.
2. Collect the roots: tags — all of them unless a `TagTTL` says otherwise — and every
   manifest and blob node used within `TTL`. A blob used recently is a root in its own
   right, which is what keeps a layer uploaded just before the manifest that will name
   it from being swept in between.
3. Mark, breadth first, following child and referrer edges. A visited set makes cycles
   terminate and keeps a manifest shared by several indexes from being walked twice.
   Content addressing means a real cycle cannot be pushed over HTTP — a manifest
   cannot name a digest that depends on its own bytes — but the traversal does not
   rely on that.
4. Sweep what was not marked: unreachable manifests, expired or dangling tags, and
   unreachable blobs.

An object the collector has no node for — a push that raced the sweep — is *adopted*
rather than collected, so nothing is ever swept on the sweep that first sees it.

Sweeps are triggered by manifest requests, at most once per `Interval` (a tenth of
the `TTL` by default). Blob requests only refresh what they serve; they are the hot
path and do not sweep. So an object can outlive its TTL by up to one interval, and a
request arriving in that window refreshes it — which for a cache is the right answer.

## A tag keeps everything it references

| Flag | Default | What it bounds |
| --- | --- | --- |
| `--ttl <dur>` | `0` | How long a manifest or blob is kept after it was last pushed or pulled. Anything reachable from a tag or an unexpired index is kept regardless of its own age. `0` keeps everything until the process exits. |
| `--tag-ttl <dur>` | `--ttl` | How long a tag is kept after it was last pushed or read. `0` keeps tags — and their images — forever. |

A zero `TagTTL` means tags never expire, and therefore neither does anything a tag
reaches. A rules_img push pushes each per-platform manifest by digest, then the index
by digest, then a tag — so with permanent tags, `TTL` reclaims only the digest-only
manifests of pushes that were abandoned partway, and every completed push stays
forever. A registry taking a new tag per CI build grows by one tag, and its graph, per
build.

That is a real answer for a registry whose images must stay pullable, but it is not a
safe default, so **`--tag-ttl` follows `--ttl` unless it is given explicitly**.
Bounding manifests while leaving tags permanent bounds nothing at all for a client
that always pushes a tag, which is every ordinary push. `--tag-ttl 0` asks for
permanent tags on purpose.

Set the two apart when a registry's tags and its untagged manifests deserve different
answers — a week for anything someone has pulled, six hours for the digest-only
residue of pushes that stopped partway:

```bash
registry --blob-store reapi --reapi-endpoint grpc://your-cas-server:9092 \
  --ttl 6h \
  --tag-ttl 168h
```

`CollectorConfig` itself keeps the zero value meaning permanent, since that is what a
zero TTL means everywhere else here; deriving one default from another is the command
line's business, where there is a difference between a flag left out and a flag set to
zero.

Reading a tag refreshes it, so a `TagTTL` evicts the tags nobody pulls rather than the
tags that are merely old.

## Collecting a blob

Two things can happen to a collected blob, and they are configured in different
places because they answer different questions.

**Its metadata goes.** `Collector.OnBlobCollected` reports every collected blob. The
CAS registry uses it to drop the blob's entry from its blob-size cache: a size
learned from a manifest that no longer exists is just stale metadata. That cache has
no TTL of its own — its lifetime *is* the collector's, which is what keeps it from
disagreeing with the manifests it was filled from.

**Its contents may go.** If the blob handler implements `BlobDeleteHandler`, `New`
deletes the blob's contents when it is collected. `WithBlobPruning(false)` turns that
off. This is on by default because the handlers in this package — in memory and on
disk — are storage the registry owns.

The CAS registry's storage is not. Bazel's CAS and an upstream registry hold blobs
that other clients put there and still expect to find, so a manifest going out of
scope here says nothing about whether those bytes should go. Its combined blob store
therefore marks each member `Prunable` or not, and advertises `BlobDeleteHandler` at
all only if at least one member is prunable — a read-only deployment keeps answering
`UNSUPPORTED` to blob deletes rather than accepting them and doing nothing. No member
is prunable today.

## Keeping blobs alive elsewhere

A registry backed by Bazel's CAS serves blobs it does not own, and
[the remote execution API promises nothing](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)
about how long the CAS keeps them. A manifest whose layers have been evicted is a
manifest that cannot be pulled, and the registry finds out when a client does.

`KeepAlive`, in [`pkg/serve/registry`](../serve/registry/keepalive.go), asks about
them before that happens. Reading a blob is what marks it as recently used in a cache
that evicts by least recent use, and `FindMissingBlobs` is the cheapest way to read
one — it transfers no blob data at all. So a goroutine wakes up every
`ScanInterval`, takes the live blobs the collector knows about, and asks about the
ones nobody has used lately.

It takes three settings: whether it runs, `RemoteCacheTTL` — how long the cache is
believed to keep a blob nobody asks about — and `ScanInterval`. A blob is refreshed
once `RemoteCacheTTL - 2*ScanInterval` has passed since it was last used or last
refreshed; the two intervals of slack mean a blob is asked about before the cache
could have dropped it even if one scan is missed or runs late. Scan far more often
than the belief: at `ScanInterval >= RemoteCacheTTL/2` there is no slack left and
every live blob is refreshed on every scan, which is safe but pointless traffic.

Refreshes are recorded by the keepalive, not fed back into the collector. Feeding
them back would extend the registry's own retention, and a blob would keep itself
alive forever.

A blob the CAS reports **missing** is logged: something reachable from a live
manifest is gone, so that image is already unpullable. Its cached size is dropped
too, since a size we can no longer act on is worse than no size — later requests go
back to asking the blob stores.

The keepalive needs a collector, because that is what knows which blobs are
reachable. A registry that should keep everything can still have one: a `Collector`
with no `TTL` tracks the object graph without ever collecting from it.

## What is not here

- **Persistence.** `Store` is the seam for it
  ([issue #413](https://github.com/bazel-contrib/rules_img/issues/413)); the only
  implementation today is in memory.
- **Leases.** containerd lets a client hold a temporary root over a set of objects
  for the length of a multi-step operation. That would be the principled fix for a
  push slow enough that its digest-only children could expire before the index
  arrives, which today is handled by a `TTL` comfortably longer than a push.
- **Size or count caps.** Retention is time and reachability, nothing else.
