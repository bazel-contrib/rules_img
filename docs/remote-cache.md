# Remote Cache Reliability

Every rules_img strategy that reads blobs out of Bazel's remote cache — [lazy
push](push-strategies.md#lazy-push), [CAS registry
push](push-strategies.md#cas-registry-push), [BES push](push-strategies.md#bes-push)
and compact-layer reconstruction — talks to the cache over the [Remote Execution
API][reapi] (REAPI) and its ByteStream service. This page describes what happens
when those requests fail or stall, and the environment variables that tune it.

## Retries

Transient failures are retried, the way Bazel's own remote cache client retries
them. Blobs are content-addressed and every read is idempotent, so retrying is
always safe: the digest validates whatever comes back. A read that the server
tears down mid-transfer is resumed at the offset it reached, so no bytes are
transferred twice, and an interrupted upload continues from the `committed_size`
the server reports.

`UNAVAILABLE`, `INTERNAL`, `ABORTED`, `UNKNOWN`, `DEADLINE_EXCEEDED`,
`RESOURCE_EXHAUSTED` and a `CANCELLED` that is not our own cancellation are
retried with exponential backoff and jitter. `NOT_FOUND` is a cache miss, not a
failure: it falls through to the next blob source (a registry, the disk cache)
instead. Everything else, and anything that is not a gRPC status at all, is
permanent.

When the server sends a [`RetryInfo`][retryinfo] error detail, the delay it asks
for is used instead of the computed backoff (bounded by
`IMG_REAPI_RETRY_MAX_DELAY`), as the REAPI specification asks clients to do.

The attempt budget counts *consecutive* failures: a transfer that is making
forward progress gets its full budget back, so a large blob on a lossy link is
not killed by an attempt counter.

| Variable | Effect |
|----------|--------|
| `IMG_REAPI_RETRY_MAX_ATTEMPTS` | Attempts per operation, including the first (default `6`). `1` disables retrying |
| `IMG_REAPI_RETRY_BASE_DELAY` | Backoff before the first retry, doubling from there (default `250ms`) |
| `IMG_REAPI_RETRY_MAX_DELAY` | Cap on a single wait, including one the server asked for (default `5s`) |

These are the remote-cache counterpart of the `IMG_REGISTRY_RETRY_*` variables,
which govern container registry traffic.

## Timeouts

| Variable | Effect |
|----------|--------|
| `IMG_REAPI_RPC_TIMEOUT` | Deadline for one attempt of a unary call — `FindMissingBlobs`, `BatchReadBlobs`, `BatchUpdateBlobs`, `GetCapabilities`. Like Bazel's `--remote_timeout` (default `60s`, `0` disables) |
| `IMG_REAPI_IDLE_TIMEOUT` | How long a blob download may receive no data before it is torn down and resumed. Like Bazel's `--remote_grpc_download_idle_timeout` (default `60s`, `0` disables) |

Bulk transfers get an inactivity limit rather than a deadline on the whole call,
because a deadline cannot tell a slow-but-progressing transfer from a hung one. A
download that stops producing data is resumed from the offset it reached instead
of blocking the deploy forever.

Uploads are deliberately left without an idle limit: unlike a read, a write
cannot be resumed for free.

## Connection pooling

A single gRPC connection multiplexes every request onto one TCP connection, which
caps bulk-download throughput on high-latency links. `img deploy` therefore opens
a pool of connections and spreads reads across them, mirroring Bazel's
`--remote_max_connections`.

| Variable | Effect |
|----------|--------|
| `IMG_REAPI_MAX_CONNECTIONS` | Connections to the remote cache (default: the deploy's `--jobs`, i.e. the host CPU count) |

Retries make use of the pool: an attempt that failed on one connection is retried
on the next, so a connection the server poisoned is not immediately reused.

### Keepalive

Connections ping the server after 5 minutes with a stream in flight and no data
arriving, and are closed if the ping goes unacked for 20 seconds — so a peer that
disappeared mid-transfer is noticed instead of leaving a read blocked forever. The
read is then resumed from the offset it reached.

5 minutes is not a tuning choice: it is the minimum gRPC servers enforce by
default (`keepalive.EnforcementPolicy.MinTime`). A client that pings more often
collects strikes and, after three, is sent
`GOAWAY(ENHANCE_YOUR_CALM, "too_many_pings")`, which drops the whole connection
and fails every stream on it. If you see that in a server log alongside a burst of
`UNAVAILABLE`, an intermediary — not rules_img — is pinging too eagerly.

The 20-second timeout matches Bazel's default. On Linux it is also applied as
`TCP_USER_TIMEOUT`, so it bounds how long any unacknowledged write may go before
the kernel drops the connection — not just an unacked keepalive ping. That makes a
short value costly on a congested link, where it would tear down connections that
are merely slow.

## Resumable uploads

`ByteStream.Write` is resumable by design: after a failure the client asks
`QueryWriteStatus` how much the server committed and continues from there. How far
that gets depends on whether the bytes can be reproduced.

- A source that can seek — a file, an in-memory buffer — is rewound to the
  server's committed offset. These uploads resume from anywhere.
- A source that cannot (an HTTP request body being relayed into the cache by the
  [CAS registry](push-strategies.md#cas-registry-push)) is resumable only as far
  as the chunk still held in memory, which covers a stream the server tore down
  while it was being sent. A failure that leaves the server behind older,
  already-discarded data is reported to the caller, who still has the source and
  can start over.

A server that does not implement `QueryWriteStatus` (it may answer
`UNIMPLEMENTED`) makes the upload start over from offset 0 under a fresh upload
id, so the server never sees two different prefixes written under one resource
name.

## What gets reported

Retries are printed on stderr as they happen, up to 20 per process:

```
WARNING: remote cache: reading blob 4f8a…9c1 (1258291 bytes) from the remote cache
failed (rpc error: code = Unavailable desc = unavailable); retrying in 268ms (attempt 2/6)
```

An operation that exhausts its budget says so, which distinguishes a give-up from
a first-attempt failure:

```
giving up reading blob 4f8a…9c1 (1258291 bytes) from the remote cache after 6 attempts over 7.9s: …
```

`img deploy` summarizes the retries alongside its blob statistics:

```
    blob transfers: 3 from disk, 0 from disk cache, 0 from container registry, 5 from remote cache, 0 from compact stream
    remote cache blobs: 2 from local cache (48.1 MiB), 3 fetched (210.4 MiB), 1 deduplicated, 0 evicted
    remote cache requests: 12 retried (Internal 1, Unavailable 11)
```

## See also

- [Push Strategies](push-strategies.md) — the strategies that use the remote
  cache, and the [local blob cache](push-strategies.md#local-blob-cache) that
  fronts it
- [Credential Helpers](credential-helpers.md) — authenticating remote cache
  traffic, and scoping a helper to the cache with
  `credential_helper_remote_cache`

[reapi]: https://github.com/bazelbuild/remote-apis
[retryinfo]: https://github.com/googleapis/googleapis/blob/master/google/rpc/error_details.proto
