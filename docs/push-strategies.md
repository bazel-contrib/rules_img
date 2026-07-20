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

### Setup Guide
1. Deploy the CAS-integrated registry:
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

### Setup Guide
1. Deploy the BES backend with image push support:
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
per layer, plus (optionally) one for the config and manifest(s). The actions are
wired as Bazel [validation actions], so they run whenever the target is built
(with `--run_validations`, on by default) without sitting on the critical path of
the target's normal outputs.

`multi_deploy` has no push at build time of its own — it deploys at `bazel run`
time. The `image_push` targets (or images with `push_specs`) it references still
push at build time on their own when the setting is enabled; `multi_deploy` does
not push them a second time.

Two content modes are available, selected with `push_at_build_time_content`:

- **`blobs`** — only the layer blobs are pushed at build time (one `PushImage`
  action per layer). The config and manifest(s)/tags are *not* pushed at build
  time; you push them afterwards with `image_push` / `multi_deploy`.
- **`blobs_and_manifests`** (default) — layers, config, and manifest(s)/tags are
  all pushed at build time. The image exists in the registry as soon as the build
  finishes; no separate push step is required.

[validation actions]: https://bazel.build/extending/rules#validation_actions

### Diagram
The two content modes are illustrated below (see [Modes in detail](#modes-in-detail)
for the reasoning behind each).

**`blobs`** — layer blobs are pushed from the build cluster at build time; the config
and manifest are pushed afterwards:

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
Pair this mode with the [lazy push strategy](#lazy-push). Because the layer blobs
are already uploaded at build time, the follow-up `image_push` / `multi_deploy`
only has to write the config and manifest — and with the lazy strategy the layer
tarballs are never materialized on, or downloaded to, the machine running Bazel.
The net effect: layers are uploaded once, in parallel, from the build cluster, and
Bazel never touches layer bytes.

Since the layers are already in the registry, tell the follow-up push to reference
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
`--@rules_img//img/settings:push_at_build_time_repository=<repo>`.

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
existing blobs or write manifests. Any user can then upload layers at build time
with that restricted machine account, while the config, manifest, and tags are
written afterwards with the individual (local) Bazel user's own credentials. A
leaked or misused build-action credential can then only add blobs — it cannot read
other tenants' layers or publish images under their tags.

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

# Choose what to push: "blobs" (layers only) or "blobs_and_manifests" (default).
common --@rules_img//img/settings:push_at_build_time_content=blobs_and_manifests
```

Then just build the image target — the push happens as a validation action:
```bash
bazel build //your:image_target
```

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

## Choosing the Right Strategy

| Use Case | Recommended Strategy | Why |
|----------|---------------------|-----|
| Local development | CAS Registry | Fast iteration, minimal bandwidth |
| Small team CI/CD | Lazy | Good performance, simple setup |
| Large organization | BES | Maximum scalability, centralized control |
| Simple deployments | Eager | No infrastructure requirements |
| Air-gapped environments | Eager | Works without external dependencies |


[reapi-spec-cas-lifetime]: https://github.com/bazelbuild/remote-apis/blob/e95641649b5b4d3c582c89daabfaabeb8189dd77/build/bazel/remote/execution/v2/remote_execution.proto#L305-L308
