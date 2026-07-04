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

> **WARNING:** `credential_helper` and `IMG_CREDENTIAL_HELPER` use Bazel's
> credential-helper HTTP header protocol. When the helper returns
> `Authorization: Bearer ...`, `rules_img` treats the value as a registry access
> token and sends it through directly; it is not exchanged as an OAuth refresh
> token.

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
Also note that the regsitry doesn't offer TLS nor authentication, so it should only listen on localhost, or be protected by a VPN or other gateway.

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

> **WARNING:** The credential helper warning from the lazy strategy also applies
> here: `Authorization: Bearer ...` must be an access token already, not a
> refresh token that `rules_img` should exchange.

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
