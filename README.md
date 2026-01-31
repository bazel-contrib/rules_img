<div align="center">

![rules_img logo](/.github/logo/light_hero.jpg#gh-light-mode-only)
![rules_img logo](/.github/logo/dark_hero.jpg#gh-dark-mode-only)

**Modern Bazel rules for building OCI container images with advanced performance optimizations**

Supports both **Bzlmod** and **WORKSPACE** setups. For WORKSPACE setup instructions, see the [releases page](https://github.com/bazel-contrib/rules_img/releases).

`rules_img` was originally written by (and receives ongoing support from) <br>
<a target="_blank" rel="noopener noreferrer" href="https://tweag.io/#gh-light-mode-only"><img src="./docs/visuals/tweag_light_mode.svg" alt="Tweag" style="width: 10rem;"></a><a target="_blank" rel="noopener noreferrer" href="https://tweag.io/#gh-dark-mode-only"><img src="./docs/visuals/tweag_dark_mode.svg" alt="Tweag" style="width: 10rem;"></a>.
</div>

## Features

- 🚀 **High Performance** - Minimizes data transfer and embraces *Build without the Bytes* from source code to container runtime
- 📦 **OCI Compliant** - Builds standard OCI images compatible with any container runtime
- 🔧 **Bazel Native** - No Docker daemon required, fully hermetic builds
- 🌍 **Multi-Platform** - Native cross-platform support through Bazel transitions
- ⚡ **eStargz Support** - Lazy pulling optimization for faster container starts
- 🪶 **Smaller layers** - Deduplicates files using hardlinks
- 🎯 **Shallow Base Images** - Avoid downloading layers from huge base images like CUDA
- 🏢 **Enterprise Ready** - Remote Build Execution and Content Addressable Storage integration

## Installation

Add to your `MODULE.bazel`:

```starlark
bazel_dep(name = "rules_img", version = "0.3.4")
```

<details>
<summary>Configure default settings (optional) in <code>.bazelrc</code></summary>

```
# The compression algorithm to use ("gzip" or "zstd")
common --@rules_img//img/settings:compress=zstd

# Number of parallel compression workers (gzip only)
# "1" uses single-threaded stdlib gzip, "auto" uses compilation mode defaults,
# "nproc" uses all available CPUs, or specify a number (e.g., "4").
# Any number above 1 uses pgzip, which results in slightly larger files,
# but is otherwise fully compatible with the gzip format.
common --@rules_img//img/settings:compression_jobs=auto

# Compression level
# gzip: 0-9, where 0=no compression, 1=fast compression, 9=best compression
# zstd: 1-4, where 1=fast compression, 4=best compressions
# "auto" uses compilation mode defaults (-1 for default, 1 for fastbuild, 9 for opt)
common --@rules_img//img/settings:compression_level=auto

# Support for seekable eStargz layers
# with the containerd stargz-snapshotter
common --@rules_img//img/settings:estargz=enabled

# Create parent directory entries in tar files for all files
# When enabled, parent directories are automatically created in the tar for all file entries.
# This is disabled by default to avoid overwriting existing directory permissions in lower layers.
common --@rules_img//img/settings:create_parent_directories=disabled

# Opt-in to stamping of image_push rules
common --@rules_img//img/settings:stamp=disabled

# The push strategy to use (see below for more info).
# "eager", "lazy", "cas_registry", or "bes"
common --@rules_img//img/settings:push_strategy=eager

# The load strategy to use.
# "eager" or "lazy"
common --@rules_img//img/settings:load_strategy=eager

# The daemon to target with image_load
# "docker", "containerd", "podman", or "generic"
# For "generic", set LOADER_BINARY environment variable at runtime
common --@rules_img//img/settings:load_daemon=docker

# Bazel remote cache to use for lazy pushing of container images.
# Uses the same format as Bazel's --remote_cache flag.
# Falls back to $IMG_REAPI_ENDPOINT env var.
common --@rules_img//img/settings:remote_cache=grpcs://remote.buildbuddy.io

# Credential helper to use for authenticating gRPC connections during push operations
# in some push strategies.
# This can be the same as Bazel's credential helper.
# Falls back to $IMG_CREDENTIAL_HELPER env var.
common --@rules_img//img/settings:credential_helper=tweag-credential-helper

# Path to Docker configuration file for registry authentication.
# If set, this will be used as REGISTRY_AUTH_FILE for authenticating to registries
# when downloading image layers during build time (e.g., for lazy base image pulling).
# Typically set to ~/.docker/config.json or similar.
common --@rules_img//img/settings:docker_config_path=/home/user/.docker/config.json
```

</details>
<br/>

## Quick Start

### Building a Simple Image

Add base image to `MODULE.bazel`:

```starlark
pull = use_repo_rule("@rules_img//img:pull.bzl", "pull")

pull(
    name = "ubuntu",
    digest = "sha256:1e622c5f073b4f6bfad6632f2616c7f59ef256e96fe78bf6a595d1dc4376ac02",
    registry = "index.docker.io",
    repository = "library/ubuntu",
    tag = "24.04",
)

pull(
    name = "cuda",
    digest = "sha256:f353ffca86e0cd93ab2470fe274ecf766519c24c37ed58cc2f91d915f7ebe53c",
    registry = "index.docker.io",
    repository = "nvidia/cuda",
    tag = "12.8.1-cudnn-devel-ubuntu20.04",
)
```

Compose images in `BUILD.bazel`:

```starlark
load("@rules_img//img:layer.bzl", "image_layer")
load("@rules_img//img:image.bzl", "image_manifest")

# Create a layer from files...
image_layer(
    name = "app_layer",
    srcs = {
        "/app/bin/server": "//cmd/server",
        "/app/config": "//configs:prod",
    },
    compress = "zstd",  # Use zstd compression (optional, uses global default otherwise)
)

# ... and a second layer (add as many as you need)
image_layer(
    name = "data_layer",
    srcs = {"/data/logo.png": "@static_assets//:logo.png"},
)

# Build a container image:
# This will contain all layers from base (if set) and the layers given in "layers" (in the specified order).
# Try to put frequently changing layers last for better performance.
image_manifest(
    name = "app_image",
    base = "@ubuntu", # Optional: build "from scratch" without base.
    layers = [
        ":data_layer",
        ":app_layer",
    ],
    config_fragment = "config.json",  # Optional image configuration, uses sane defaults.
)
```

### Multi-Platform Images

In most cases, you can just use the builtin transitions feature:

```starlark
load("@rules_img//img:image.bzl", "image_manifest", "image_index")

# Create platform-specific images
image_manifest(
    name = "app",
    layers = [":app_layer"],
)

# Combine into multi-platform index
image_index(
    name = "multiarch_app",
    manifests = [":app"],
    platforms = [
        "//:linux_amd64",
        "//:linux_arm64",
    ],
)
```

For more details on working with platforms, architecture variants, and building images for macOS Docker daemons, see the [Platforms Guide](docs/platforms.md).

### Pushing to a Registry

```starlark
load("@rules_img//img:push.bzl", "image_push")

image_push(
    name = "push",
    image = ":app",
    registry = "ghcr.io",
    repository = "my-project/app",
    tag = "latest",
)
```

Run with:
```bash
bazel run //:push
```

### Registry Authentication

`rules_img` uses [go-containerregistry](https://github.com/google/go-containerregistry) to interact with container registries, which provides automatic credential discovery from standard locations. This means authentication works the same way as with Docker CLI, Podman, and other container tools.

#### Credential Discovery

When pushing or pulling images, `rules_img` automatically searches for credentials in the following locations (in order):

1. **`~/.docker/config.json`** - Standard Docker credential file
2. **`$DOCKER_CONFIG/config.json`** - If the `DOCKER_CONFIG` environment variable is set
3. **`${XDG_RUNTIME_DIR}/containers/auth.json`** - Podman credential file (typically `/run/user/1000/containers/auth.json`)

Additionally, for Google Container Registry (gcr.io, pkg.dev, etc.), `rules_img` registers the [Google keychain](https://pkg.go.dev/github.com/google/go-containerregistry/pkg/v1/google#Keychain) alongside the default keychain. This provides automatic authentication using Application Default Credentials (ADC), making it seamless to push/pull from GCR when running on Google Cloud or with `gcloud` configured locally.

For more details on how credential discovery works, see the [go-containerregistry keychain documentation](https://github.com/google/go-containerregistry/tree/main/pkg/authn#tldr-for-consumers-of-this-package).

#### Setting up Credentials

The easiest way to configure credentials is using `docker login`:

```bash
# Login to Docker Hub
docker login

# Login to a private registry
docker login ghcr.io
docker login registry.example.com

# Login with username and password
docker login -u myusername registry.example.com
```

These commands will store credentials in `~/.docker/config.json`, which `rules_img` will automatically use.

#### Alternative: Podman

If you're using Podman, credentials are stored in a different location:

```bash
podman login registry.example.com
```

This stores credentials in `${XDG_RUNTIME_DIR}/containers/auth.json`, which `rules_img` also automatically discovers.

#### Bazel Sandbox and Authentication

When Bazel runs actions in a sandbox (which is the default behavior), it may hide certain environment information like the current username and home directory. This can prevent `rules_img` from automatically finding your Docker credential files.

If you encounter authentication failures, you can explicitly configure the path to your Docker configuration file:

```bash
# In your .bazelrc or on the command line
common --@rules_img//img/settings:docker_config_path=/home/username/.docker/config.json
```

Replace `/home/username/` with your actual home directory path. This setting affects:

1. **Build-time blob downloads**: When downloading image layers during builds (e.g., lazy base image pulling or the `download_blobs` rule), the `REGISTRY_AUTH_FILE` environment variable is set to this path.
2. **Push operations**: When running `image_push` targets with `bazel run`, this ensures authentication works correctly.
3. **Load operations**: When running `image_load` targets with `bazel run`, credentials are available for any required registry access.
4. **Multi-deploy operations**: When running `multi_deploy` targets, all combined operations can authenticate properly.

Additionally, the `DOCKER_CONFIG` environment variable is inherited from your shell environment for all push, load, and multi_deploy operations. This means you can also use the standard Docker environment variable as an alternative:

```bash
# Alternative: use DOCKER_CONFIG environment variable
export DOCKER_CONFIG=/path/to/docker/config/dir
bazel run //:push_image
```

#### Troubleshooting

If you're experiencing authentication issues:

1. **Verify credentials exist**: Check that `~/.docker/config.json` or `${XDG_RUNTIME_DIR}/containers/auth.json` contains the registry
2. **Check permissions**: Ensure the credential file is readable by the user running Bazel
3. **Environment variables**: If using `$DOCKER_CONFIG`, ensure it points to a directory containing `config.json`
4. **Test with Docker/Podman**: If `docker pull` or `podman pull` works, `rules_img` should work too
5. **Bazel sandbox issues**: If authentication works outside Bazel but fails during builds, try setting `--@rules_img//img/settings:docker_config_path` to your Docker config file path

For advanced authentication scenarios (credential helpers, custom authentication), refer to the [go-containerregistry authentication documentation](https://github.com/google/go-containerregistry/blob/main/cmd/crane/doc/crane.md#authenticating).

### Language-specific examples

* [C++](/e2e/cc/)
* [Go](/e2e/go/)
* [JS / TS](/e2e/js/)
* [Python](/e2e/python/)
* [Custom Distroless base image](/e2e/generic/custom_distroless_base_image/)

## Comparison with rules_oci

Both `rules_img` and `rules_oci` are modern Bazel rulesets for building OCI container images. While they share the goal of hermetic, reproducible container builds, they take fundamentally different architectural approaches.
`rules_oci` uses the [oci image layout][oci-image-layout] as an on-disk representation of container images at every step (base image pull, `oci_image` rule, `oci_image_index` rule).
Additionally, `rules_oci` chooses to use only off-the-shelf, pre-built tools for assembling images.
`rules_img` chooses to use providers that contain just enough information as needed for subsequent steps. We also use customized tools, instead of prebuilt ones.
This results in a more complex implementation, but also allows for interesting optimizations.

- ✅ [Shallow base image pulling](#shallow-base-image-pulling)
- ✅ [Layers are produced in a single action](#single-action-layers)
- ✅ [Deduplication of layer contents](#layer-optimization)
- ✅ [Advanced push strategies](#advanced-push-strategies)
- ✅ [eStargz support for lazy pulling](#estargz-lazy-pulling)
- ✅ [Incremental loading into daemons](#incremental-loading)

## Documentation

- [API Reference](docs/)
  - **Layer Rules**
    - [`image_layer`](docs/layer.md#image_layer) - Create layers from files
    - [`layer_from_tar`](docs/layer.md#layer_from_tar) - Create layers from tar archives
    - [`file_metadata`](docs/layer.md#file_metadata) - Helper for specifying file attributes of `image_layer` rule.
  - **Image Rules**
    - [`image_manifest`](docs/image.md#image_manifest) - Build single-platform images
    - [`image_index`](docs/image.md#image_index) - Build multi-platform image indexes
    - [`image_manifest_from_oci_layout`](docs/convert.md#image_manifest_from_oci_layout) - Convert oci_image to image_manifest
    - [`image_index_from_oci_layout`](docs/convert.md#image_index_from_oci_layout) - Convert oci_image_index to image_index
  - **Push, Pull and Load Rules**
    - [`pull`](docs/pull.md#pull) - Repository rule for pulling base images
    - [`images.pull`](docs/extensions.md#images) - Module extension for pulling base images (EXPERIMENTAL)
    - [`image_push`](docs/push.md#image_push) - Push images to registries
    - [`image_load`](docs/load.md#image_load) - Load images into container daemons
    - [`multi_deploy`](docs/multi_deploy.md#multi_deploy) - Deploy multiple operations as unified command
- [Platforms Guide](docs/platforms.md) - Working with Bazel platforms, architecture variants, and multi-platform builds
- [Migration Guide from rules_oci](docs/migration-from-rules_oci.md)

## Key Differences Explained

### Shallow Base Image Pulling

Unlike rules_oci which downloads all layers of a base image, rules_img uses a "shallow pull" approach. When you reference a base image like CUDA (which can be 10+ GB), rules_img only downloads the manifest and config - not the actual layer blobs. The layers are only downloaded when and if they're needed during push operations.

This results in:
- **Faster builds** - No waiting for large base image downloads
- **Reduced bandwidth** - Only download what you actually use
- **True Build-without-the-bytes** - Other rulesets download base layers to your local machine in a repository rule. This step cannot be remotely executed and is repeated on every machine running Bazel.

Example with a large CUDA base image:
```starlark
# This won't download the 10GB of CUDA layers!
pull(
    name = "cuda",
    digest = "sha256:...",
    registry = "index.docker.io",
    repository = "nvidia/cuda",
)
```

### Single Action Layers

rules_img produces both the layer blob and its metadata in a single Bazel action. This design has several advantages:

- **Remote execution friendly** - Single action works better with RBE
- **Image Manifest only depends on metadata** - In rules_oci, image actions depend on the actual blobs of their base image and layers, which must be available during the manifest writing action.

The metadata includes the layer's digest, size, and diff ID, all computed during layer creation.

### Layer Optimization

When writing a tar layer, rules_img uses hardlinks to deduplicate identical files.
This allows for smaller container images.

### Advanced Push Strategies

rules_img offers four sophisticated push strategies compared to rules_oci's traditional approach. These strategies enable:
- **Faster CI/CD** - Avoid unnecesary file transfer
- **Build without the bytes** - Never materialize container layers on your local machine
- **Scalability** - Designed for organizations with thousands of builds per day

| Strategy | Description | Use Case | Requirements |
|----------|-------------|----------|--------------|
| [`eager`](docs/push-strategies.md#eager-push) | Traditional push, download all blobs to the machine running Bazel, then uploads all blobs. | Simple deployments | Normal container registry |
| [`lazy`](docs/push-strategies.md#lazy-push) | Checks registry first, skips existing blobs and streams missing blobs from Bazel's remote cache | Faster CI/CD and Build without the Bytes | Bazel remote cache |
| [`cas_registry`](docs/push-strategies.md#cas-registry-push) | Uses special container registry that is directly connected to Bazel's remote cache | Fast development cycles. | Special container registry (`cmd/registry`), Bazel remote cache |
| [`bes`](docs/push-strategies.md#bes-push) | Image push happens as side-effect of BES upload. Requires self-hosted BES server. | Extremely fast and efficient for large organizations. | Special BES backend (`cmd/bes`), Bazel remote cache |

See the [Push Strategies Guide](docs/push-strategies.md) for detailed information about each strategy.

### eStargz Lazy Pulling

rules_img has first-class support for eStargz (enhanced stargz), enabling "lazy pulling" at container runtime. This means:

- **Instant container starts** - Containers can start before all layers download
- **Bandwidth savings** - Only accessed files are downloaded
- **Seekable layers** - Random access to files within compressed layers

Combined with containerd's stargz-snapshotter, this can reduce container startup time from minutes to seconds for large images.

```starlark
image_layer(
    name = "optimized_layer",
    srcs = {...},
    estargz = "enabled",  # Enable seekable compression
)
```

The same setting can be globally enabled using `--@rules_img//img/settings:estargz=enabled`.
Read the [stargz-snapshotter documentation][stargz-snapshotter] for more information.

### Incremental Loading

rules_img loads images incrementally and efficiently by directly interfacing with the containerd API. This provides significant performance advantages over traditional approaches:

- **Direct containerd integration** - When Docker is configured with containerd storage, rules_img bypasses `docker load` entirely
- **Incremental blob loading** - Only new or changed layers are loaded, existing blobs are skipped
- **Streaming architecture** - No temporary tar files or buffering entire images in memory
- **Platform selection** - Load only the platforms you need from multi-platform images

The performance difference is dramatic, especially for large images:

```bash
# Load only the platform you need
bazel run //my:image_load -- --platform linux/amd64

# Incremental loading: only new layers are transferred
# Second load of a slightly modified image is near-instant
bazel run //my:image_load  # Only changed layers loaded!
```

When Docker doesn't support containerd storage, rules_img automatically falls back to `docker load` with a clear warning about the performance impact.

This is particularly powerful in development workflows where you're iterating on application layers while keeping large base images (like CUDA) unchanged - subsequent loads only transfer your small application layers.

**Future Docker Support**: Docker is planning to expose its contentstore API in version 29.0.0, which will enable native incremental loading ([moby/moby#44369](https://github.com/moby/moby/issues/44369)). Once this ships, rules_img will adopt it to provide incremental loading performance even when the containerd socket isn't directly accesible by users. This will bring the same efficiency benefits to all Docker users, regardless of their platform or configuration.

## Hacking & Contributing

We invite external contributions and are eager to work together with the build systems community. Please refer to the [CONTRIBUTING](/CONTRIBUTING.md) guide to learn more. If you want to check out the code and run a development version, follow the [HACKING](/HACKING.md) guide to get started.

## Acknowledgments

Special thanks to **Sushain Cherivirala** from Stripe for the inspiring BazelCon talk ["Building 1300 Container Images in 4 Minutes"](https://www.youtube.com/watch?v=c-yvIQooOSA). This talk introduced the groundbreaking idea of using the Build Event Service (BES) to sync container images between the remote cache and registry as a side effect. While their implementation was based on the now-archived rules_docker and was never published, it laid the conceptual foundation for our BES push strategy. Their work demonstrated how to achieve dramatic performance improvements in container image builds at scale, inspiring many of the optimizations in rules_img.

[stargz-snapshotter]: https://github.com/containerd/stargz-snapshotter
[oci-image-layout]: https://github.com/opencontainers/image-spec/blob/v1.1.1/image-layout.md
