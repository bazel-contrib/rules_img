# Blob existence cache

This is one feature of [`oci-distribution-gateway`](README.md), documented on its
own because there is a lot of it: what the cache holds, what fills and empties it,
how to size it, and how the instances of a serving deployment share what they learn.

A serving gateway memoizes one fact: **this blob is in this repository**. It is the
answer `200` to `HEAD /v2/<name>/blobs/<digest>`, the "is this layer already pushed?"
probe every push begins with. On a build farm that request dominates registry
traffic — a fleet re-pushing the same base image asks the same question thousands of
times, and each answer is a round trip a client waits on.

It is on by default, remembering a blob for six hours within 64 MiB of memory:

```bash
oci-distribution-gateway serve --policy-file /etc/img/policy.json \
  --blob-existence-cache-ttl 6h \
  --blob-existence-cache-max-memory 64MiB
```

Setting either flag to `0` turns the cache off, and every probe goes to the
registry as before.

**The key is the resolved upstream registry, the resolved repository, and the
digest** — all three. A blob is present *in a repository of a registry*, not in
general: the same digest in another repository of the same registry is a different
question, and gets asked. Resolution happens first, so a request naming `docker.io`
and one naming `index.docker.io` share entries (they are the same upstream) while
`registry.example.com` and `registry.example.com:5000` do not.

Of the answers a probe can get, only a plain `200` is remembered:

- **A `404` is not cached.** A blob that is absent now can be pushed a second
  later, so remembering "not there" would tell clients to re-upload content that
  exists — and, worse, would not expire fast enough to be corrected.
- **Errors are not cached**, so a `429` or a `503` cannot outlive the outage.
- **Manifests and tags are never cached**, whatever the reference. Both are
  mutable, and a `HEAD` on one is exactly how a client discovers that it changed.
- **Conditional requests** (`Range`, `If-Match`, `If-None-Match`,
  `If-Modified-Since`, `If-Unmodified-Since`, `If-Range`) go to the registry
  untouched and their answers are not stored, since those depend on more than
  whether the blob is there.

A cached answer carries `Content-Length` (which is how a client sizes a layer
without downloading it) and `Docker-Content-Digest` (the digest the client asked
about, by definition of a content-addressed blob). Nothing else the registry
happened to send is replayed to another client hours later, and every cached hit is
logged with `(cached)` so the decision log still accounts for every request.

**Authorization is not cached.** The policy is consulted on every request, before
the cache is; a reload that revokes access takes effect on the next probe even though
its answer is sitting in memory.

## What fills and empties it

A probe is not the only request that settles whether a blob is in a repository, and
every request that does keeps the cache current:

| Request | Effect |
| --- | --- |
| `HEAD .../blobs/<digest>` answered `200` | remembered |
| `GET .../blobs/<digest>` answered `200`, with a `Docker-Content-Digest` naming that blob | remembered, with the size it served |
| `PUT .../blobs/uploads/<ref>?digest=<digest>` answered `201` | remembered: the upload committed |
| `POST .../blobs/uploads/?digest=<digest>` answered `201` | remembered: the whole blob arrived in one request |
| `POST .../blobs/uploads/?mount=<digest>&from=<repo>` answered `201` | remembered: the mount was honoured |
| `DELETE .../blobs/<digest>` forwarded | forgotten, whatever the registry answers |

Only the response that *finishes* an upload counts — a `202` means a session was
opened or a chunk accepted, and a client told that a half-uploaded blob is already
there would skip an upload it still owes. A delete is read pessimistically the other
way: a `5xx` or a timeout leaves the gateway unable to tell whether the blob survived,
and dropping an entry only ever costs one probe. Those drops are counted under
`..._evictions_total{oci_gateway_cache_eviction_reason="deleted"}`.

An entry admitted by a commit or a mount carries no `Content-Length` — neither request
sees the blob's bytes — so a probe it answers omits the header rather than inventing a
size, exactly as it does for a registry that reports no length.

## Sizing the TTL

The TTL is what stands between a client and the wrong answer the gateway cannot see
coming. A blob cannot change, but a registry can **garbage-collect** one behind the
gateway's back — and a push client that believes a collected layer is still there will
skip re-uploading it and commit a manifest referring to a blob that is gone. (A blob
deleted *through* the gateway needs no such window; it is dropped when the delete goes
past.)

Set the TTL well inside the window in which your registry could collect a blob:
the grace period of its GC, the untagged-blob retention of your Artifactory or ECR
lifecycle rule, whichever applies. The six-hour default suits a registry that
collects daily at the earliest. If your registry collects aggressively, lower it;
if blobs are never collected, raising it costs nothing but memory.

## Sizing the memory

The bound is allocated **in full at startup and never grows**, so the cache is a
fixed addition to the pod's memory request rather than a number that moves under
load. Nothing in a lookup or a store allocates, so it adds no garbage collection
pressure either.

An entry costs 376 bytes, so the default 64 MiB holds about 178,000 blob digests —
several times a large farm's working set. When the cache is full the least recently
used entry makes room for the new one, so a burst of one-off digests cannot push
out blobs that are still being probed.

Watch `oci_gateway_blob_existence_cache_entries / ..._capacity` for how full it is
and `..._evictions_total{oci_gateway_cache_eviction_reason="capacity"}` for whether the
memory bound, rather than the TTL, is deciding how long blobs are remembered — a
capacity eviction rate above zero is the signal to raise
`--blob-existence-cache-max-memory`. See [Metrics](README.md#metrics).

## Replicating the cache between instances

Each replica of a serving deployment learns for itself, so a first-seen blob costs
one upstream probe **per replica**. Replication removes that multiplier: whichever
instance pays for an answer tells the others, and a fleet pays once.

```bash
# Peers discovered from the Service this gateway is already behind.
oci-distribution-gateway serve --policy-file /etc/img/policy.yaml \
  --address 0.0.0.0 --port 8443 \
  --tls-cert-file /tls/tls.crt --tls-key-file /tls/tls.key --client-ca-file /tls/ca.crt \
  --blob-existence-cache-peer-service oci-distribution-gateway \
  --blob-existence-cache-peer-server-name oci-distribution-gateway.img-gateway.svc \
  --allowed-cache-peer-id spiffe://cluster.local/ns/img-gateway/sa/oci-distribution-gateway
```

or, without Kubernetes, by naming them:

```bash
oci-distribution-gateway serve ... \
  --blob-existence-cache-peer https://gateway-1.internal:8443 \
  --blob-existence-cache-peer https://gateway-2.internal:8443
```

**What travels is one tuple: the registry, the repository, and the digest.** Not
access times, not LRU order, not sizes. The receiving instance inserts the entry
exactly as if it had learned the fact itself — its own clock, its own TTL, the head
of its own LRU list — so the caches are free to diverge, and deliberately do: one
instance evicts a blob the other keeps alive with local traffic, and neither is
wrong. The cache is a hint.

Three things are sent, and nothing else:

| Event | When | Effect on a peer |
| --- | --- | --- |
| An insertion | this instance admits an entry (any row of the table above) | the same entry, with a fresh local TTL |
| A deletion | a client deleted a blob through this instance **and the registry confirmed it** | the entry is dropped |
| A donation | a starting instance asks a running one for its hottest entries | see [Warming up a new replica](#warming-up-a-new-replica) |

**Misses are never replicated.** A blob that is absent now can be pushed a second
later, which is the same reason a `404` is not cached in the first place.

**A fact received from a peer is never passed on.** Two instances would otherwise
feed each other the same fact forever. It is structural rather than a rule: the
receiving path writes to the cache and has no access to the send queue.

**A deletion is held to a higher standard than a local drop.** This instance drops
its own entry whenever a blob delete passes through, answer or no answer, because
that only ever costs it one probe. Asking the whole fleet to forget is not free, so
peers are told only when the registry actually confirmed the delete — which also
means a client cannot flush the fleet's caches by sending deletes a registry
refuses.

### It cannot slow a request down

Replication is best effort, in the strong sense that no client request waits for
any part of it:

- A fact is put on a bounded in-memory queue. If the queue is full it is
  **dropped** and counted (`..._replication_dropped_total`) — a dropped fact costs
  another instance one upstream probe, which is exactly what it would have paid
  without replication.
- Facts are batched for a few milliseconds (`5ms`) or until the batch is full,
  whichever comes first, and then sent to every peer at once. The timer is *not*
  restarted by later facts, so a steady stream is delivered continuously rather
  than held back. Within a batch, repeated facts about the same blob are condensed
  to the last one — a fleet reading the same layer hundreds of times in one window
  sends one fact, and a blob inserted then deleted arrives as a deletion.
- A send is fire-and-forget with a 5-second timeout, no retries, and a bound on how
  many are in flight. A peer that is down produces log lines and
  `..._replication_batches_total{oci_result="failure"}`, and nothing else.

### Who may write to the cache

The replication endpoints live under `/_rules_img/cache/` on the same listener as
the registry protocol, behind the same
[client authentication](README.md#client-authentication). But authenticating a
client only says it may *use* the gateway, and writing to the existence cache is a
different power: a client that claims a blob exists when it does not makes push
clients **skip an upload they still owe**, and then commit a manifest referencing a
blob that was never pushed.

So set **`--allowed-cache-peer-id`** to the identity your gateway pods present.
Without it, every client the listener authenticates may insert facts, which is only
appropriate when every client of that listener is a peer — the gateway logs a
warning at startup saying so. A forwarding sidecar deployment is exactly the case
where it is *not* true, and a forwarder refuses to relay `/_rules_img/` paths for
the same reason.

The credential this gateway presents to its peers defaults to its **serving
identity**: with one Deployment, one keypair and one CA, every instance already
holds a certificate its peers accept (`--client-ca-file`) and a bundle that
verifies theirs, so replication needs no material of its own. Pass
`--blob-existence-cache-peer-token-file` instead if your peers authenticate by
token.

### Discovering peers in Kubernetes

`--blob-existence-cache-peer-service <name>` watches the EndpointSlices of that
Service — the very Service the gateway is already behind, whose endpoints are by
definition the pods a client could have reached instead. The set follows a
scale-up, a scale-down, a rolling update and a lost node with no restart and no
configuration to keep in sync. It needs:

- an explicit `--port`, since a discovered peer is reached at its endpoint address
  on the port this instance itself serves,
- `--blob-existence-cache-peer-server-name`, because a certificate issued for the
  Service name does not cover the pod IPs that are dialled,
- and RBAC, which no built-in role grants:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: oci-gateway-peers, namespace: img-gateway}
rules:
  - apiGroups: ["discovery.k8s.io"]
    resources: ["endpointslices"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: oci-gateway-peers, namespace: img-gateway}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: oci-gateway-peers}
subjects:
  - {kind: ServiceAccount, name: oci-distribution-gateway, namespace: img-gateway}
```

Endpoints that are **not ready** are kept as replication targets and excluded as
donors: a replica that is still warming up should be told what the fleet learns, so
that it has it by the time it serves, but it has nothing to give away. A
**terminating** endpoint is neither.

Watch `oci_gateway_blob_existence_cache_replication_peers`. A deployment of several
replicas reporting zero peers is a discovery or connectivity problem, and the only
symptom otherwise is a hit rate that is quietly lower than it should be.

### Warming up a new replica

A replica that joins with an empty cache sends the fleet's whole working set
upstream again — every probe it serves is a miss until it has seen the blob itself.
So before reporting itself healthy, a starting instance asks a peer for the hottest
`--blob-existence-cache-warmup-entries` (20,000) of its cache:

1. It answers `/healthz` with **`503 warming up`**, which keeps a readiness probe
   from putting it in the Service.
2. It is already listening for its peers' broadcasts, so nothing the fleet learns
   in the meantime is lost.
3. It asks the candidate peers how long they have been up (a headers-only request)
   and takes the entries from the **oldest** one, which has had the longest to fill
   its cache. Ties are broken by who holds more, and the candidates are shuffled so
   that a deployment which started together spreads the donating.
4. Each entry arrives with what is **left** of the donor's deadline, not a fresh
   TTL: copying a fact between instances must not let it outlive the window the TTL
   exists to bound. Sizes travel with a donation too, so a seeded entry answers a
   probe with its `Content-Length`.
5. At `--blob-existence-cache-warmup-timeout` (10s) it reports healthy whatever
   happened. A replica that never becomes ready is a far worse outcome than one
   with a cold cache, so every failure — no peers, no answer, a donor that is busy
   — ends the same way.

A donor stays fully operational while donating: it snapshots the entries under its
shard locks for the length of a memcpy and streams them afterwards, and it serves
at most two donations at a time, refusing further ones with `503` so that seeding a
fleet of starting replicas cannot crowd out registry traffic. The asking instance
moves on to another peer.

Set the pod's `readinessProbe` (not the liveness probe) on `/healthz`, and keep
`--blob-existence-cache-warmup-timeout` well inside
`failureThreshold × periodSeconds` of any liveness probe pointed at the same path.

## In a two-hop deployment

In a [two-hop deployment](README.md#two-hop-deployment) the cache lives in the
**serving** tier, where the registry knowledge is: a forwarder has no policy, no
credentials and no view of which upstream a request resolves to, and caching in
every worker pod would multiply the memory by the fleet while sharing nothing. One
shared serving deployment means one shared cache — and the more workers behind it,
the better it works. Each replica still keeps its own cache, so turn on
[replication](#replicating-the-cache-between-instances) if you run more than one;
without it, a first-seen blob costs one upstream probe per replica.

A forwarder does **not** relay the `/_rules_img/` control paths: a build action's
request to a replication endpoint would otherwise reach the shared serving gateway
carrying the *forwarder's* identity, which is a peer identity there.
