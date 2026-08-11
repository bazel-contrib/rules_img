# Push Strategies

rules_img supports multiple push strategies optimized for different scenarios. Each strategy offers unique trade-offs between performance, infrastructure requirements, and use cases.

## Eager Push

### Overview
The eager push strategy is the traditional approach where all image layers are downloaded to the machine running Bazel and then uploaded to the target registry. This is similar to how most container build tools work, including rules_oci.

### How it Works
1. Downloads all required blobs (layers, configs, manifests) to local machine
2. Uploads all blobs to the target registry
3. Writes the manifest to the registry

### Diagram
![Eager Push Strategy](visuals/eager-push-light.svg#gh-light-mode-only)
![Eager Push Strategy](visuals/eager-push-dark.svg#gh-dark-mode-only)

### Pros
- ✅ Simple and straightforward
- ✅ Works with any standard container registry
- ✅ No special infrastructure required (works without remote cache)
- ✅ Predictable behavior

### Cons
- ❌ Requires downloading all layers locally
- ❌ Uses significant bandwidth for large images
- ❌ Slower for images with many or large layers
- ❌ Not optimized for remote execution

### Setup Guide
```bash
# Enable eager push strategy (this is the default)
$ bazel run //your:push_target --@rules_img//img/settings:push_strategy=eager

# Or set in .bazelrc
common --@rules_img//img/settings:push_strategy=eager
```

No additional infrastructure setup required.

## Lazy Push

### Overview
The lazy push strategy optimizes uploads by checking the registry first and only uploading missing blobs. It streams blobs directly from Bazel's remote cache when needed, avoiding unnecessary downloads to the local machine.

### How it Works
1. Downloads only image metadata to machine running Bazel
2. Streams missing blobs from Bazel's remote cache to the registry
3. Writes the manifest to the registry

### Diagram
![Lazy Push Strategy](visuals/lazy-push-light.svg#gh-light-mode-only)
![Lazy Push Strategy](visuals/lazy-push-dark.svg#gh-dark-mode-only)

### Pros
- ✅ Work with huge container images without sacrificing local disk space
- ✅ Works with standard registries
- ✅ Supports Build without the Bytes

### Cons
- ❌ Requires a Bazel remote cache
- ❌ Slightly more complex than eager push
- ❌ Push fails if required blobs are evicted from the CAS before the push runs (see [Remote Cache Eviction](#remote-cache-eviction))
- ❌ Requires `--digest_function=sha256` (the default; see [Digest Function](#digest-function))

### Setup Guide
1. Ensure you have a Bazel remote cache configured:
```bash
# Example remote cache configuration.
# This also works with --remote_executor
build --remote_cache=grpc://your-cache-server:9092
```

2. Enable lazy push strategy:
```bash
# In .bazelrc
common --@rules_img//img/settings:push_strategy=lazy

# Optionally, configure remote cache and credential helper via rules_img settings
# instead of environment variables:
common --@rules_img//img/settings:remote_cache=grpc://your-cache-server:9092
common --@rules_img//img/settings:credential_helper=tweag-credential-helper

# If your remote cache requires a remote instance name, set it here:
common --@rules_img//img/settings:remote_instance_name=my-instance-name
```

> See [Credential Helpers](credential-helpers.md) for exactly how
> `credential_helper` / `IMG_CREDENTIAL_HELPER` is used to authenticate this
> remote-cache traffic. To scope a helper to the cache without affecting
> registry auth, use `credential_helper_remote_cache` /
> `IMG_CREDENTIAL_HELPER_REMOTE_CACHE` instead.

3. Run your push target:
```bash
# Configure the push utility via environment variables:
export IMG_REAPI_ENDPOINT=grpc://your-cache-server:9092
export IMG_CREDENTIAL_HELPER=tweag-credential-helper
# Set the remote instance name if required by your RBE backend:
export IMG_REAPI_INSTANCE_NAME=my-instance-name
bazel run //your:push_target

# Or use the settings flags (if configured above):
bazel run //your:push_target
```

4. (Optional) Use the local Bazel disk cache as an additional blob source:
```bash
export IMG_DISK_CACHE=$(bazel info disk_cache)
bazel run //your:push_target
```

When `IMG_DISK_CACHE` is set, rules_img reads blobs directly from the local Bazel
disk cache before falling back to the remote CAS, and blobs it fetches from the
remote CAS are written back into that directory (see
[Local blob cache](#local-blob-cache) below). This can speed up pushes on
developer machines that have a warm disk cache.

> **Note:** As an additional *source*, this has little effect in most setups:
> - With **Build without the Bytes** (BwoB) enabled, Bazel does not download action
>   outputs to the disk cache, so the cache will be mostly empty for recently-built
>   layers. Blobs that rules_img itself fetched are there, though.
> - If you are **not using lazy push** (for example, with the eager strategy), all
>   blobs are already materialized as runfiles and the disk cache is never consulted.

### Local blob cache

Every blob `img deploy` reads from the remote CAS goes through a local cache. It
does two things:

- **Deduplicates reads.** Concurrent readers of one blob — the same layer pushed to
  several registries, or a compact layer's input file referenced by several layers —
  share a single download. A reader that arrives while a download is in flight
  streams the bytes as they arrive instead of starting a second one.
- **Keeps blobs on disk**, in Bazel's disk cache layout (`cas/<xx>/<digest>`), so a
  later push — including a later `bazel run`, and Bazel itself when the directory is
  its disk cache — reads them locally instead of from the remote CAS.

By default the cache lives in a `rules_img` directory inside the user cache
directory (`$XDG_CACHE_HOME` or `~/.cache` on Linux, `~/Library/Caches` on macOS,
`%LocalAppData%` on Windows) and is capped at 10 GiB, evicting the least recently
used blobs beyond that. Setting `IMG_DISK_CACHE` moves it into Bazel's disk cache,
which shares the blobs with Bazel.

Local caching never changes the outcome of a deploy: if the directory cannot be
written, fills up, or is unavailable, the affected read falls back to streaming
straight from the remote CAS.

| Variable | Effect |
|----------|--------|
| `IMG_CAS_CACHE` | Set to `0`, `false`, `off` or `no` to disable local caching (and read deduplication) entirely |
| `IMG_CAS_CACHE_DIR` | Cache directory, overriding both the default and `IMG_DISK_CACHE` |
| `IMG_CAS_CACHE_MAX_SIZE` | Maximum cached bytes, e.g. `20GiB`. `0` means unlimited |
| `IMG_CAS_CACHE_BUFFER_SIZE` | How much of a blob is written to disk before readers can consume it (default `1MiB`) |

A size limit means rules_img manages that directory: it indexes what is already
there on startup and prunes it. A directory you name — your own, or Bazel's disk
cache — has no limit by default, so rules_img only ever adds to it and leaves
pruning to Bazel's own disk cache garbage collection. It does evict blobs it wrote
itself if the file system runs out of space.

`img deploy` reports what the cache did after each deploy:

```
    blob transfers: 3 from disk, 0 from disk cache, 0 from container registry, 5 from remote cache, 0 from compact stream
    remote cache blobs: 2 from local cache (48.1 MiB), 3 fetched (210.4 MiB), 1 deduplicated, 0 evicted
```

## CAS Registry Push

### Overview
The CAS (Content Addressable Storage) registry push strategy uses a special container registry that is directly integrated with Bazel's remote cache. This eliminates data duplication and provides the fastest possible push performance. Please note that the remote cache may evict cached data at any time, as per [the specification][reapi-spec-cas-lifetime]. For that reason, using a remote cache as the backend of your container registry is only recommended during development.
Also note that the registry doesn't offer TLS nor authentication, so it should only listen on localhost, or be protected by a VPN or other gateway.

### How it Works
1. The special registry reads blobs directly from Bazel's CAS
2. No blob transfer needed - registry and cache share storage
3. Only metadata (manifests) need to be written
4. Registry serves blobs on-demand from CAS

### Diagram
![CAS Registry Push Strategy](visuals/cas-registry-light.svg#gh-light-mode-only)
![CAS Registry Push Strategy](visuals/cas-registry-dark.svg#gh-dark-mode-only)

### Pros
- ✅ Fastest push performance possible
- ✅ Zero data duplication
- ✅ Minimal bandwidth usage
- ✅ Perfect for development workflows
- ✅ Ideal for CI pipelines where images are tested shortly after a build

### Cons
- ❌ Requires special registry implementation
- ❌ More complex infrastructure setup
- ❌ Registry must have access to CAS
- ❌ Requires `--digest_function=sha256` (the default; see [Digest Function](#digest-function))
- ❌ Images stop being pullable if the CAS evicts a layer underneath them (see [`--cas-keepalive`](#bounding-what-the-registry-keeps-and-keeping-blobs-alive))

### Setup Guide
1. Deploy the CAS-integrated registry using the pre-built image from GitHub Container Registry:

```bash
docker run --rm \
  ghcr.io/bazel-contrib/rules_img/cas-registry:latest \
  --reapi-endpoint grpc://your-cas-server:9092 \
  --credential-helper tweag-credential-helper \
  --address 0.0.0.0 \
  --port 80 \
  --grpc-port 4444 \
  --enable-blobcache \
  --blob-store reapi
```

Alternatively, build from source:
```bash
# Build the registry
bazel build @rules_img_tool//cmd/registry

# Start registry server
bazel-bin/external/rules_img_tool+/cmd/registry/registry_/registry \
  --reapi-endpoint grpc://your-cas-server:9092 \
  --credential-helper tweag-credential-helper \
  --address localhost \
  --port 80 \
  --grpc-port 4444 \
  --enable-blobcache \
  --blob-store reapi
```

2. Configure Bazel to use CAS registry push:
```bash
# In .bazelrc
common --@rules_img//img/settings:push_strategy=cas_registry
# This also works with --remote_executor
build --remote_cache=grpc://your-cache-server:9092

# Optionally, configure credential helper via rules_img settings:
common --@rules_img//img/settings:credential_helper=tweag-credential-helper
```

> See [Credential Helpers](credential-helpers.md) — the same
> credential helper behavior from the lazy strategy applies here.

3. Push to your CAS registry:
```bash
export IMG_BLOB_CACHE_ENDPOINT=grpc://localhost:4444
bazel run //your:push_target
```

The registry can use multiple blob backends, including a remote cache (`reapi`, default), another container registry (`upstream`), and an S3 bucket (`s3`). Those backends are experimental.

### Bounding what the registry keeps, and keeping blobs alive

The registry keeps every manifest it is pushed until the process exits, and serves
blobs the CAS may evict at any time. `--ttl` bounds the first by garbage collecting
manifests and blobs by tracing references, so nothing a tag or an unexpired index still
names is ever collected. `--cas-keepalive` handles the second by periodically asking
the cache about the blobs live manifests reference, which keeps a cache that evicts by
least recent use from dropping them.

```bash
registry --blob-store reapi --reapi-endpoint grpc://your-cas-server:9092 \
  --ttl 6h --cas-keepalive
```

See [Garbage collection](../img_tool/pkg/registry/garbage-collection.md) for every
flag, what the retention rules actually are, and how to size the keepalive.

## BES Push

### Overview
The BES (Build Event Service) push strategy performs image pushes as a side-effect of Bazel's build event uploads. This is the most sophisticated strategy, designed for large organizations with thousands of builds per day.
Note that the BES service doesn't offer TLS nor authentication, so it should only listen on localhost, or be protected by a VPN or other gateway.

### How it Works
1. Bazel uploads build events to BES as normal
2. BES backend detects image push events
3. Images are assembled and pushed asynchronously
4. No client-side push needed

### Diagram
![BES Push Strategy](visuals/bes-light.svg#gh-light-mode-only)
![BES Push Strategy](visuals/bes-dark.svg#gh-dark-mode-only)

### Pros
- ✅ Zero client-side overhead
- ✅ Pushes happen asynchronously
- ✅ Extremely scalable
- ✅ Perfect for large organizations
- ✅ Centralized push management

### Cons
- ❌ Requires custom BES backend
- ❌ Most complex setup
- ❌ Requires significant infrastructure
- ❌ Requires `--digest_function=sha256` (the default; see [Digest Function](#digest-function))

### Setup Guide
1. Deploy the BES backend using the pre-built image from GitHub Container Registry:

```bash
docker run --rm \
  ghcr.io/bazel-contrib/rules_img/bes-listener:latest \
  --address 0.0.0.0 \
  --port 8080 \
  --cas-endpoint grpc://your-cas-server:9092 \
  --credential-helper tweag-credential-helper
```

Alternatively, build from source:
```bash
# Build the BES server
bazel build @rules_img_tool//cmd/bes

# Run with CAS backend
bazel-bin/external/rules_img_tool+/cmd/bes/bes_/bes \
  --address localhost \
  --port 8080 \
  --cas-endpoint grpc://your-cas-server:9092 \
  --credential-helper tweag-credential-helper
```

2. Configure Bazel to use your BES:
```bash
# In .bazelrc
build --bes_backend=grpc://localhost:8080
common --@rules_img//img/settings:push_strategy=bes
```

3. Build your targets normally - pushes happen automatically:
```bash
# Just build - no need to run push targets!
bazel build //your:image_target
```

## Push at Build Time

Push at build time is not a push *strategy* — it is an orthogonal option that can
be combined with the strategies above. Instead of pushing when you run a push
target, it pushes image content to the registry *as part of the build itself*.

### Overview
When `push_at_build_time` is enabled, every `image_manifest` / `image_index` that
has `push_specs`, as well as every `image_push` target, gains extra build actions
(mnemonic `PushImage`) that upload content directly to the registry: one action
per image blob (each layer and each config), plus — in `blobs_and_manifests` mode
— one more action per push that writes the config and manifest(s)/tags. The actions
are wired as Bazel [validation actions], so they run whenever the target is built
(with `--run_validations`, on by default) without sitting on the critical path of
the target's normal outputs.

`multi_deploy` has no push at build time of its own — it deploys at `bazel run`
time. The `image_push` targets (or images with `push_specs`) it references still
push at build time on their own when the setting is enabled; `multi_deploy` does
not push them a second time.

Two content modes are available, selected with `push_at_build_time_content`:

- **`blobs`** — every image blob is pushed at build time: one `PushImage` action
  per layer and one per config blob. The manifest(s)/tags are *not* pushed at build
  time; you write them afterwards with `image_push` / `multi_deploy`.
- **`blobs_and_manifests`** (default) — layers, config, and manifest(s)/tags are
  all pushed at build time. The image exists in the registry as soon as the build
  finishes; no separate push step is required.

[validation actions]: https://bazel.build/extending/rules#validation_actions

### Diagram
The two content modes are illustrated below (see [Modes in detail](#modes-in-detail)
for the reasoning behind each).

**`blobs`** — all image blobs (layers and the config) are pushed from the build
cluster at build time; the manifest(s)/tags are pushed afterwards:

![Push at build time (blobs)](visuals/push-at-build-time-blobs-light.svg#gh-light-mode-only)
![Push at build time (blobs)](visuals/push-at-build-time-blobs-dark.svg#gh-dark-mode-only)

**`blobs_and_manifests`** — the whole image (layers, config, manifest and tags) is
pushed from the build cluster at build time:

![Push at build time (blobs and manifests)](visuals/push-at-build-time-all-light.svg#gh-light-mode-only)
![Push at build time (blobs and manifests)](visuals/push-at-build-time-all-dark.svg#gh-dark-mode-only)

### Why push at build time?
The clearest win: **all layers are uploaded in parallel, directly from the remote
execution cluster to the registry**. When the `PushImage` actions run on a remote
executor, each layer is uploaded by its own action from the worker that already
holds the blob — so the layer bytes never flow through the machine running Bazel.
This is especially valuable for large images and high-fan-out CI.

### Modes in detail

#### Blobs at build time, manifest afterwards (`blobs`)
Pair this mode with the [lazy push strategy](#lazy-push). Because every blob (all
layers and the config) is already uploaded at build time, the follow-up
`image_push` / `multi_deploy` only has to write the manifest — and with the lazy
strategy the layer tarballs are never materialized on, or downloaded to, the
machine running Bazel. The net effect: blobs are uploaded once, in parallel, from
the build cluster, and Bazel never touches layer bytes.

Since the blobs are already in the registry, tell the follow-up push to reference
them instead of re-uploading:

```bash
common --@rules_img//img/settings:push_at_build_time=enabled
common --@rules_img//img/settings:push_at_build_time_content=blobs
common --@rules_img//img/settings:push_strategy=lazy
# Make `bazel run` deploy refuse to re-upload layers; it only mounts / HEADs them.
common --@rules_img//img/settings:forbid_layer_push=enabled
```

Optionally push the blobs to a shared staging repository and have the manifest push
cross-mount them into each image's real repository with
`--@rules_img//img/settings:push_at_build_time_blob_repository=<repo>`. In
`blobs_and_manifests` mode you can additionally stage the manifests themselves in a
separate repository with
`--@rules_img//img/settings:push_at_build_time_manifest_repository=<repo>`; this
only redirects where the build-time manifest push writes the manifest(s) and config
— the layer blobs are still cross-mounted from the blob repository.

Registries expose a blob to any repository the caller can read, so the manifest push
does not have to re-upload the layers it finds in the staging repository — it
cross-mounts them (`POST /v2/<image>/blobs/uploads/?mount=<digest>&from=<staging>`),
which the registry resolves internally by linking the existing blob:

![Cross-mounting blobs (multi-tenant)](visuals/blob-mount-light.svg#gh-light-mode-only)
![Cross-mounting blobs (multi-tenant)](visuals/blob-mount-dark.svg#gh-dark-mode-only)

This split is also a good fit for **multi-tenant** setups, because blob uploads and
manifest/tag writes use *different* credentials. In most cases it is acceptable to
hand every user of the remote execution cluster a shared machine account that may
*upload* blobs (`HEAD` / `POST` / `PUT` to `/v2/.../blobs/`) but may not read
existing blobs or write manifests. Any user can then upload every image blob (all
layers and the config) at build time with that restricted machine account, while
the manifest and tags are written afterwards with the individual (local) Bazel
user's own credentials. A leaked or misused build-action credential can then only
add blobs — it cannot read other tenants' layers or publish images under their tags.

#### Everything at build time (`blobs_and_manifests`)
The image is fully pushed by the time the build action finishes — the simplest to
operate, but harder to reason about: there is no push step to watch, you don't see
what was pushed, and the image already exists once the build action completes.

If you still need the digest and tags afterwards (for example to feed a downstream
deployment), you can run `image_push` or `multi_deploy` in this mode. They detect
that everything is already present, do a lightweight `HEAD` request instead of
uploading, and print the resulting digest and tags.

> **Signing always happens client-side.** Push at build time never signs images —
> the `PushImage` build actions only upload content. Image signing (see
> [Image Signing](image-signing.md)) is performed by `img deploy` when you
> `bazel run` an `image_push` / `multi_deploy` target, using the configured signer
> plugin and your local credentials. So even when the whole image is already in the
> registry via `blobs_and_manifests`, producing a *signed* image still requires the
> `bazel run` deploy step: it detects the content is already present (a lightweight
> `HEAD` instead of an upload) and then attaches the signature as an OCI referrer.

### Requirements and trade-offs
Pushing from a build action has the same infrastructure needs as lazy base image
pulls (`layer_handling`), plus write access:

- ❌ Build actions need **network access** to the registry (like lazy
  `layer_handling`), which makes them non-hermetic.
- ❌ Build actions need **registry credentials with write access** (lazy
  `layer_handling` only needs read). See
  [Authenticating Build Actions](authenticating-build-actions.md) for how to give
  pull/push actions their credentials.
- ❌ Harder to reason about than an explicit push step, especially in
  `blobs_and_manifests` mode (see above).

### Setup Guide
```bash
# Enable push at build time. "best_effort" logs push failures but keeps the build
# green; "enabled" fails the build if a push fails; "disabled" (default) is off.
common --@rules_img//img/settings:push_at_build_time=enabled

# Choose what to push: "blobs" (all layers and the config) or "blobs_and_manifests" (default).
common --@rules_img//img/settings:push_at_build_time_content=blobs_and_manifests
```

Then just build the image target — the push happens as a validation action:
```bash
bazel build //your:image_target
```

### Per-target configuration
The global flags above set the baseline; each `image_push` target and each
`image_push_spec` (the push config attached to an `image_manifest` / `image_index`
via `push_specs`) can override push at build time on its own. The relevant
attributes default to deferring to the global flag, so unset targets behave
exactly as before:

| Attribute | Default | Overrides global flag |
| --- | --- | --- |
| `push_at_build_time` | `auto` | `push_at_build_time` |
| `push_at_build_time_content` | `auto` | `push_at_build_time_content` |
| `push_at_build_time_blob_repository` | *(sentinel)* | `push_at_build_time_blob_repository` |
| `push_at_build_time_manifest_repository` | *(sentinel)* | `push_at_build_time_manifest_repository` |
| `forbid_layer_push` | `auto` | `forbid_layer_push` |
| `push_at_build_time_exec_properties` | `{"requires-network": "1"}` | *(none — per-target only)* |

`auto` (and the repository sentinel) means "use the global setting"; any other
value is used verbatim — for the repository attributes, `""` forces "no staging
repository" even when the global flag is set. `push_at_build_time_exec_properties`
is forwarded as the `execution_requirements` of every `PushImage` action, so you
can, for example, route the network-bound push actions to a specific remote
execution pool per target.

```python
load("@rules_img//img:push.bzl", "image_push_spec")

# This image pushes at build time to a staging repository and refuses to
# re-upload layers on a later `bazel run`, regardless of the global default.
image_push_spec(
    name = "push_spec",
    registry = "gcr.io",
    repository = "my-project/my-app",
    tag = "latest",
    push_at_build_time = "enabled",
    push_at_build_time_content = "blobs",
    push_at_build_time_blob_repository = "my-project/_staging",
    forbid_layer_push = "enabled",
)
```

Because the staging repository is only used for cross-mounting when push at build
time actually runs, setting `push_at_build_time_blob_repository` while
`push_at_build_time` is `disabled` for a target records **no** cross-mount source
in that target's deploy manifest — a later `bazel run` deploy will not try to
mount blobs from a repository nothing was pushed to.


## Remote Cache Eviction

The lazy and CAS registry push strategies stream blobs directly from Bazel's
remote cache (CAS). If a blob is evicted from the CAS before the push runs, the
push will fail and the layer bytes cannot be recovered.

The eager push strategy is immune to this failure case.
It adds all required blobs to the runfiles of the push target, so the push works
fully offline even if the remote cache is unavailable.

The safest approach is to use `bazel run` on the push target directly — the push
happens immediately after the Bazel invocation, so all required blobs are probably
present. If the push happens later, make sure to consume the blobs soon after the build.

For the CAS registry the exposure outlasts the push: an image it has accepted stops
being pullable once the CAS drops a layer underneath it.
[`--cas-keepalive`](#bounding-what-the-registry-keeps-and-keeping-blobs-alive) keeps the
registry asking the cache about the blobs its live manifests name.

## Digest Function

Every strategy that reads blobs from Bazel's cache requires Bazel's digest
function to be **sha256** — which is the default. Do not set Bazel's
`--digest_function` startup flag to anything else (`blake3`, `sha1`, …) when using
one of those strategies.

The reason: Bazel's remote cache (CAS) and a container registry are both
content-addressed stores, and rules_img looks a layer up in the CAS under the very
same digest the registry knows it by. That only works while both sides hash with
the same function. Both ecosystems default to sha256 — but OCI registries are
effectively pinned to it, while Bazel's digest function is configurable, so
changing it on the Bazel side breaks the mapping.

| Strategy | Requires `--digest_function=sha256` |
|----------|-------------------------------------|
| Eager | No — all blobs travel in the push target's runfiles, the CAS is never consulted |
| Lazy | **Yes** — missing blobs are streamed from the CAS by digest |
| CAS Registry | **Yes** — the registry serves blobs straight out of the CAS |
| BES | **Yes** — the BES backend assembles images from CAS blobs |

The same requirement applies to the other blob sources that are addressed by
digest:
- the [local blob cache](#local-blob-cache) and the local Bazel disk cache
  (`IMG_DISK_CACHE`), which are looked up as `cas/<sha256>`, and
- the CAS references inside [compact layers](compact-stream.md): during a lazy
  push the layer's input files are fetched from the disk / remote cache by their
  content digest.

[Push at build time](#push-at-build-time) is *not* affected. Those actions receive
the blobs — and, for compact layers, the content-addressed input directory — as
regular action inputs, so they never look anything up in the CAS themselves.

### Symptom
With a non-sha256 digest function, the push fails while resolving a layer, and the
remote CAS line of the blob-source report says the blob is not there:

```
Error during deploy: building VFS: locating source for layer with digest sha256:eda6250a… …
layer with digest sha256:eda6250a… not found in any source:
  …
  - remote CAS: [blob missing] blob not found in remote CAS
```

The blob *is* in the CAS — just under a different (e.g. BLAKE3) digest, so
rules_img cannot find it. If you hit this, remove the `--digest_function`
override, or switch the affected targets to the eager strategy.

Supporting Bazel's other digest functions is tracked in
[issue #690](https://github.com/bazel-contrib/rules_img/issues/690).

## Choosing the Right Strategy

| Use Case | Recommended Strategy | Why |
|----------|---------------------|-----|
| Local development | CAS Registry | Fast iteration, minimal bandwidth |
| Small team CI/CD | Lazy | Good performance, simple setup |
| Large organization | BES | Maximum scalability, centralized control |
| Simple deployments | Eager | No infrastructure requirements |
| Air-gapped environments | Eager | Works without external dependencies |


[reapi-spec-cas-lifetime]: https://github.com/bazelbuild/remote-apis/blob/e95641649b5b4d3c582c89daabfaabeb8189dd77/build/bazel/remote/execution/v2/remote_execution.proto#L305-L308
