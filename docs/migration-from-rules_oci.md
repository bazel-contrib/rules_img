# Migration Guide: From rules_oci to rules_img

This guide provides step-by-step instructions for migrating from [rules_oci](https://github.com/bazel-contrib/rules_oci) to [rules_img](https://github.com/bazel-contrib/rules_img).

## Table of Contents

1. [Understanding the Differences](#understanding-the-differences)
2. [MODULE.bazel Setup](#modulebazel-setup)
3. [Building Layers](#building-layers)
4. [Building Images](#building-images)
5. [Multi-Platform Images](#multi-platform-images)
6. [Output Groups](#output-groups)
7. [Container Structure Tests](#container-structure-tests)
8. [Deployment](#deployment)

---

## Understanding the Differences

Both `rules_oci` and `rules_img` build OCI-compliant container images with Bazel, but they have fundamentally different architectural approaches:

### rules_oci
- Uses [OCI image layout](https://github.com/opencontainers/image-spec/blob/v1.1.1/image-layout.md) as on-disk representation at every step
- Uses off-the-shelf, pre-built tools for assembling images
- Downloads all base image layers during the pull phase

### rules_img
- Uses Bazel providers that contain just enough information for subsequent steps
- Uses custom-built tools optimized for specific workflows
- Performs **shallow pulling** - only downloads manifests/configs, not layer blobs
- Enables advanced optimizations like lazy push, CAS integration, and eStargz support

---

## MODULE.bazel Setup

### rules_oci (Before)

```starlark
bazel_dep(name = "rules_oci", version = "2.0.0")
# Most users either create images with rules_pkg or tar.bzl.
bazel_dep(name = "rules_pkg", version = "0.10.1")

oci = use_extension("@rules_oci//oci:extensions.bzl", "oci")
oci.pull(
    name = "distroless_cc",
    digest = "sha256:e1065a1d58800a7294f74e67c32ec4146d09d6cbe471c1fa7ed456b2d2bf06e0",
    image = "gcr.io/distroless/cc-debian12",
    platforms = ["linux/amd64", "linux/arm64"],
)
use_repo(oci, "distroless_cc")
```

### rules_img (After)

```starlark
bazel_dep(name = "rules_img", version = "0.2.8")
# rules_pkg and tar.bzl are no longer needed.

pull = use_repo_rule("@rules_img//img:pull.bzl", "pull")

pull(
    name = "distroless_cc",
    digest = "sha256:e1065a1d58800a7294f74e67c32ec4146d09d6cbe471c1fa7ed456b2d2bf06e0",
    registry = "gcr.io",
    repository = "distroless/cc-debian12",
)
```

**Key Changes**:
- `oci.pull()` becomes `pull()` (a repository rule, not an extension)
- `image` parameter splits into separate `registry` and `repository` parameters
- `registries` allows defining mirror registries that host the same image.
- `platforms` parameter is removed (single- and multi-platform images just work without extra configuration)
- No need for `use_repo()` - the `pull()` rule directly creates a repository

---

## Building Layers

There are three approaches to migrating layers from rules_oci:

### Option 1: Use existing layers (Tar files) Directly (Quickest Migration)

Both rules_oci and rules_img can consume `pkg_tar` and `tar.bzl` targets directly.

#### rules_oci

```starlark
load("@rules_pkg//pkg:tar.bzl", "pkg_tar")

pkg_tar(
    name = "app_layer",
    srcs = ["//cmd/server"],
    package_dir = "/app/bin",
)

oci_image(
    name = "image",
    tars = [":app_layer"],
)
```

#### rules_img

```starlark
load("@rules_pkg//pkg:tar.bzl", "pkg_tar")

pkg_tar(
    name = "app_layer",
    srcs = ["//cmd/server"],
    package_dir = "/app/bin",
)

image_manifest(
    name = "image",
    layers = [":app_layer"],  # Can use tar file directly!
)
```

**Key Change**: `tars` attribute becomes `layers` attribute.

### Option 2: Use layer_from_tar (Better Control)

For more control over compression and optimization:

#### rules_img

```starlark
load("@rules_pkg//pkg:tar.bzl", "pkg_tar")
load("@rules_img//img:layer.bzl", "layer_from_tar")

pkg_tar(
    name = "app_tar",
    srcs = ["//cmd/server"],
    package_dir = "/app/bin",
)

layer_from_tar(
    name = "app_layer",
    src = ":app_tar",
    compress = "zstd",     # Recompress with zstd
    optimize = True,       # Deduplicate files
)

image_manifest(
    name = "image",
    layers = [":app_layer"],
)
```

**Benefits**:
- Control compression algorithm (gzip, zstd, or none)
- Enable optimization to deduplicate identical files
- Add layer annotations
- Calculate layer digest in shared target

### Option 3: Use image_layer (Recommended)

The native `image_layer` rule provides the best integration with rules_img:

#### rules_oci

```starlark
load("@rules_pkg//pkg:tar.bzl", "pkg_tar")

pkg_tar(
    name = "app_layer",
    srcs = [
        "//cmd/server",
        "//configs:prod_config",
    ],
    package_dir = "/app",
)
```

#### rules_img

```starlark
load("@rules_img//img:layer.bzl", "image_layer")

image_layer(
    name = "app_layer",
    srcs = {
        "/app/bin/server": "//cmd/server",
        "/app/config/prod.json": "//configs:prod_config",
    },
    compress = "zstd",  # Optional: defaults to global setting
)
```

**Benefits**:
- More intuitive path mapping (destination → source)
- Deduplicates files using hardlinks
- Built-in support for file metadata, symlinks, and permissions
- Better performance (single Bazel action writes layer blob and layer metadata)

### Advanced: Layer with Custom Metadata

```starlark
load("@rules_img//img:layer.bzl", "image_layer", "file_metadata")

image_layer(
    name = "secure_layer",
    srcs = {
        "/app/bin/server": "//cmd/server",
        "/etc/app/config": "//configs:prod",
        "/etc/app/secret": "//secrets:api_key",
    },
    default_metadata = file_metadata(
        mode = "0644",
        uid = 1000,
        gid = 1000,
    ),
    file_metadata = {
        "/app/bin/server": file_metadata(mode = "0755", uid = 0, gid = 0),
        "/etc/app/secret": file_metadata(mode = "0600"),
    },
    symlinks = {
        "/usr/bin/server": "/app/bin/server",
    },
)
```

---

## Building Images

### Basic Image

#### rules_oci

```starlark
load("@rules_oci//oci:defs.bzl", "oci_image")

oci_image(
    name = "app_image",
    base = "@distroless_cc",
    tars = [":app_layer"],
    entrypoint = ["/app/bin/server"],
    env = {
        "PORT": "8080",
        "ENV": "production",
    },
    labels = {
        "org.opencontainers.image.version": "1.0.0",
        "org.opencontainers.image.source": "https://github.com/myorg/myapp",
    },
)
```

#### rules_img

```starlark
load("@rules_img//img:image.bzl", "image_manifest")

image_manifest(
    name = "app_image",
    base = "@distroless_cc",
    layers = [":app_layer"],
    entrypoint = ["/app/bin/server"],
    env = {
        "PORT": "8080",
        "ENV": "production",
    },
    labels = {
        "org.opencontainers.image.version": "1.0.0",
        "org.opencontainers.image.source": "https://github.com/myorg/myapp",
    },
)
```

**Key Changes**:
- `oci_image` → `image_manifest`
- `tars` → `layers`

### Attribute Mapping

| rules_oci | rules_img | Notes |
|-----------|-----------|-------|
| `base` | `base` | ✅ Same |
| `tars` | `layers` | ⚠️ Renamed |
| `entrypoint` | `entrypoint` | ✅ Same |
| `cmd` | `cmd` | ✅ Same |
| `created` | `created` | ✅ Same |
| `env` | `env` | ✅ Same |
| `labels` | `labels` | ✅ Same |
| `annotations` | `annotations` | ✅ Same |
| `user` | `user` | ✅ Same |
| `workdir` | `working_dir` | ⚠️ Renamed |
| `exposed_ports` | N/A | ⚠️ Use `config_fragment` |
| `volumes` | N/A | ⚠️ Use `config_fragment` |
| `os` | Uses Bazel target platform | ⚠️ Not manually configurable. See below |
| `architecture` | Uses Bazel target platform | ⚠️ Not manually configurable. See below |

Unlike rules_oci, users **cannot** set the images `os` and `architecture` directly.
Instead, rules_img uses the target platform of bazel (`--platforms=...`).
Most users should use the `image_index` rule to configure the target platform of the image.
In rare cases where you need to build a single-architecture image (`image_manifest`) for a specific platform, you can set the `platform` attribute.
This ensures that the layers of the image are actually built for the target platform (and there is no way to accidentally build images with layers for a different platform).

### Advanced Configuration

For attributes not directly supported (like `exposed_ports` or `volumes`), use `config_fragment`:

#### rules_img

```starlark
# config.json
{
  "ExposedPorts": {
    "8080/tcp": {},
    "9090/tcp": {}
  },
  "Volumes": {
    "/data": {}
  }
}

# BUILD.bazel
image_manifest(
    name = "app_image",
    base = "@distroless_cc",
    layers = [":app_layer"],
    entrypoint = ["/app/bin/server"],
    config_fragment = "config.json",
)
```

Refer to the [OCI specification](https://github.com/opencontainers/image-spec/blob/v1.1.1/config.md) to learn about all possible fields and their meaning.

---

## Multi-Platform Images

### rules_oci

```starlark
load("@rules_oci//oci:defs.bzl", "oci_image", "oci_image_index")

oci_image(
    name = "app",
    tars = [":app_layer"],
)

oci_image_index(
    name = "app_multiarch",
    images = [
        ":app",
    ],
    platforms = [
        "//platforms:linux_amd64",
        "//platforms:linux_arm64",
    ],
)
```

### rules_img (Platform Transitions - Recommended)

rules_img also offers a multi-platform transition:

```starlark
load("@rules_img//img:image.bzl", "image_manifest", "image_index")
load("@rules_img//img:layer.bzl", "image_layer")

image_layer(
    name = "app_layer",
    srcs = {
        "/bin/app": "//cmd/app",
    },
)

# Single manifest definition
image_manifest(
    name = "app",
    base = "@distroless_cc",
    layers = [":app_layer"],
    entrypoint = ["/bin/app"],
)

# Automatically builds for multiple platforms!
image_index(
    name = "app_multiarch",
    manifests = [":app"],  # Single manifest
    platforms = [
        "//platforms:linux_amd64",
        "//platforms:linux_arm64",
    ],
)
```

### rules_img (Explicit Manifests)

In rare cases, you can set leave `platforms` empty and manually specify the manifests to include.
Most users should no need this.

```starlark
image_manifest(
    name = "app_amd64",
    layers = [":app_layer"],
    platform = "//platforms:linux_amd64",
    labels = {"architecture": "amd64"},
)

image_manifest(
    name = "app_arm64",
    layers = [":app_layer"],
    platform = "//platforms:linux_arm64",
    labels = {"architecture": "arm64"},
)

image_index(
    name = "app_multiarch",
    manifests = [
        ":app_amd64",
        ":app_arm64",
    ],
)
```

---

## Output Groups

rules_img supports output groups for accessing different image formats.

> [!IMPORTANT]
> In rules_img, base images are shallow by default.
> In practice, this means that Bazel doesn't have access to layer blobs of externally pulled base images in build actions.
> When building oci layouts, those layers are needed, so you can change the `layer_handling` attribute of the `pull` rule if you run into issues.

### OCI Layout Directory

#### rules_oci

```starlark
# Access via default info: the image label refers to the tree artifact directly
oci_image(
    name = "app_image",
    tars = [":app_layer"],
)
```

You can access the oci layout directory using the `:app_image` label:

```bash
bazel build //path/to:app_image
```

#### rules_img

```starlark
image_manifest(
    name = "app_image",
    layers = [":app_layer"],
)

# Create filegroup to surface the output group containing the oci layout directory
filegroup(
    name = "image_layout",
    srcs = [":app_image"],
    output_group = "oci_layout",
)

# Anoterh
filegroup(
    name = "image_layout_tar",
    srcs = [":app_image"],
    output_group = "oci_tarball",
)
```

Build with:

```bash
# To build the oci layout
bazel build //path/to:image_layout
# alternatively, you can also access output groups directly:
build build //path/to:app_image --output_groups=oci_layout
# The same works for the oci layout tar
bazel build //path/to:image_layout_tar
build build //path/to:app_image --output_groups=oci_tarball
```

### Docker Save Format Tarball

For compatibility with `docker load`:

#### rules_oci

```starlark
load("@rules_oci//oci:defs.bzl", "oci_load")

oci_load(
    name = "tarball",
    image = ":app_image",
    repo_tags = ["myapp:latest"],
)

filegroup(
    name = "docker_tarball",
    srcs = [":tarball"],
    output_group = "tarball",
)
```

#### rules_img

```starlark
load("@rules_img//img:load.bzl", "image_load")

image_load(
    name = "load",
    image = ":app_image",
    tag = "myapp:latest",
)

filegroup(
    name = "docker_tarball",
    srcs = [":load"],
    output_group = "tarball",
)
```

**Note**: The Docker tarball is only available for single-platform images in rules_img.

---

## Container Structure Tests

Container structure tests verify the contents and configuration of your images.

### rules_oci

```starlark
load("@container_structure_test//:defs.bzl", "container_structure_test")
load("@rules_oci//oci:defs.bzl", "oci_load")

oci_load(
    name = "image_tarball",
    image = ":app_image",
    repo_tags = ["test:latest"],
)

container_structure_test(
    name = "structure_test",
    driver = "tar",
    image = ":image_tarball",
    configs = ["test_config.yaml"],
)
```

### rules_img

```starlark
load("@container_structure_test//:defs.bzl", "container_structure_test")
load("@rules_img//img:load.bzl", "image_load")

image_load(
    name = "image_tarball",
    image = ":app_image",
    tag = "test:latest",
)

container_structure_test(
    name = "structure_test",
    driver = "tar",
    image = ":image_tarball",
    configs = ["test_config.yaml"],
)
```

---

## Deployment

### Pushing to Registries

#### rules_oci

```starlark
load("@rules_oci//oci:defs.bzl", "oci_push")

oci_push(
    name = "push",
    image = ":app_image",
    repository = "ghcr.io/myorg/myapp",
    remote_tags = ["latest", "v1.0.0"],
)
```

#### rules_img

```starlark
load("@rules_img//img:push.bzl", "image_push")

image_push(
    name = "push",
    image = ":app_image",
    registry = "ghcr.io",
    repository = "myorg/myapp",
    tag_list = ["latest", "v1.0.0"],
)
```

**Key Changes**:
- `oci_push` → `image_push`
- `repository` parameter splits into `registry` and `repository`
- `remote_tags` in rules_oci takes a list of strings or a label (file) → in rules_img, there are separate attributes:
    - `tag` (string) and `tag_list` (list of string) allow setting tags in the build file
    - `tag_file` takes a file with newline-separated tags.

**Run with**:
```bash
bazel run //path/to:push
```

### Pushing Multi-Platform Images

#### rules_oci

```starlark
oci_push(
    name = "push_multiarch",
    image = ":app_multiarch",  # image_index
    repository = "ghcr.io/myorg/myapp",
    remote_tags = ["latest"],
)
```

#### rules_img

```starlark
image_push(
    name = "push_multiarch",
    image = ":app_multiarch",  # image_index
    registry = "ghcr.io",
    repository = "myorg/myapp",
    tag = "latest",
)
```

### Push with Single Tag

#### rules_oci

```starlark
oci_push(
    name = "push",
    image = ":app_image",
    repository = "ghcr.io/myorg/myapp",
    remote_tags = ["v1.0.0"],
)
```

#### rules_img

```starlark
image_push(
    name = "push",
    image = ":app_image",
    registry = "ghcr.io",
    repository = "myorg/myapp",
    tag = "v1.0.0",  # Use 'tag' for single tag
)
```

### Push Strategies (rules_img Advantage)

rules_img offers advanced push strategies for better performance:

```starlark
# Default: eager (downloads all layers locally, then pushes)
image_push(
    name = "push_eager",
    image = ":app_image",
    registry = "ghcr.io",
    repository = "myorg/myapp",
    tag = "latest",
    strategy = "eager",  # Default
)

# Lazy: streams from remote cache, skips existing blobs
image_push(
    name = "push_lazy",
    image = ":app_image",
    registry = "ghcr.io",
    repository = "myorg/myapp",
    tag = "latest",
    strategy = "lazy",  # Requires remote cache
)
```

Configure globally in `.bazelrc`:
```bash
# Use lazy push by default (requires remote cache)
common --@rules_img//img/settings:push_strategy=lazy

# Point to your remote cache
common --@rules_img//img/settings:remote_cache=grpcs://remote.buildbuddy.io
```

See the [push strategies documentation](push-strategies.md) for more details.

### Loading into Docker/containerd

#### rules_oci

```starlark
load("@rules_oci//oci:defs.bzl", "oci_load")

oci_load(
    name = "load",
    image = ":app_image",
    repo_tags = ["myapp:latest", "myapp:v1.0.0"],
)
```

#### rules_img

```starlark
load("@rules_img//img:load.bzl", "image_load")

image_load(
    name = "load",
    image = ":app_image",
    tag_list = ["myapp:latest", "myapp:v1.0.0"],
)
```

**Key Changes**:
- `oci_load` → `image_load`
- `repo_tags` → `tag_list`

**Run with**:
```bash
bazel run //path/to:load
```

### Incremental Loading (rules_img Advantage)

rules_img provides **incremental loading** when using containerd:

- Only new/changed layers are transferred
- Existing blobs are skipped automatically
- Much faster for iterative development

Enable with:
```starlark
image_load(
    name = "load",
    image = ":app_image",
    tag = "myapp:dev",
    daemon = "containerd",  # Use containerd directly
)
```

Or configure globally in `.bazelrc`:
```bash
common --@rules_img//img/settings:load_daemon=containerd
```

---

## Complete Migration Example

Here's a complete before/after example showing a full application migration:

### rules_oci (Before)

```starlark
# MODULE.bazel
bazel_dep(name = "rules_oci", version = "2.0.0")
bazel_dep(name = "rules_pkg", version = "0.10.1")
bazel_dep(name = "rules_go", version = "0.46.0")

oci = use_extension("@rules_oci//oci:extensions.bzl", "oci")
oci.pull(
    name = "distroless_base",
    digest = "sha256:e1065a1d58800a7294f74e67c32ec4146d09d6cbe471c1fa7ed456b2d2bf06e0",
    image = "gcr.io/distroless/base-debian12",
    platforms = ["linux/amd64", "linux/arm64"],
)
use_repo(oci, "distroless_base")

# BUILD.bazel
load("@rules_go//go:def.bzl", "go_binary")
load("@rules_pkg//pkg:tar.bzl", "pkg_tar")
load("@rules_oci//oci:defs.bzl", "oci_image", "oci_image_index", "oci_push", "oci_load")

go_binary(
    name = "server",
    srcs = ["main.go"],
)

pkg_tar(
    name = "server_layer",
    srcs = [":server"],
    package_dir = "/app",
)

oci_image(
    name = "server_image_amd64",
    base = "@distroless_base",
    tars = [":server_layer"],
    entrypoint = ["/app/server"],
    env = {"PORT": "8080"},
    target_compatible_with = ["@platforms//cpu:x86_64"],
)

oci_image(
    name = "server_image_arm64",
    base = "@distroless_base",
    tars = [":server_layer"],
    entrypoint = ["/app/server"],
    env = {"PORT": "8080"},
    target_compatible_with = ["@platforms//cpu:aarch64"],
)

oci_image_index(
    name = "server_multiarch",
    images = [
        ":server_image_amd64",
        ":server_image_arm64",
    ],
)

oci_push(
    name = "push",
    image = ":server_multiarch",
    repository = "ghcr.io/myorg/server",
    remote_tags = ["latest"],
)

oci_load(
    name = "load",
    image = ":server_image_amd64",
    repo_tags = ["server:dev"],
)
```

### rules_img (After)

```starlark
# MODULE.bazel
bazel_dep(name = "rules_img", version = "0.2.8")
bazel_dep(name = "rules_go", version = "0.46.0")

pull = use_repo_rule("@rules_img//img:pull.bzl", "pull")

pull(
    name = "distroless_base",
    digest = "sha256:e1065a1d58800a7294f74e67c32ec4146d09d6cbe471c1fa7ed456b2d2bf06e0",
    registry = "gcr.io",
    repository = "distroless/base-debian12",
)

# BUILD.bazel
load("@rules_go//go:def.bzl", "go_binary")
load("@rules_img//img:image.bzl", "image_manifest", "image_index")
load("@rules_img//img:layer.bzl", "image_layer")
load("@rules_img//img:push.bzl", "image_push")
load("@rules_img//img:load.bzl", "image_load")

go_binary(
    name = "server",
    srcs = ["main.go"],
)

# Native image_layer (recommended)
image_layer(
    name = "server_layer",
    srcs = {
        "/app/server": ":server",
    },
)

# Single manifest definition
image_manifest(
    name = "server_image",
    base = "@distroless_base",
    layers = [":server_layer"],
    entrypoint = ["/app/server"],
    env = {"PORT": "8080"},
)

# Automatically builds for multiple platforms
image_index(
    name = "server_multiarch",
    manifests = [":server_image"],
    platforms = [
        "//platforms:linux_x86_64",
        "//platforms:linux_aarch64",
    ],
)

image_push(
    name = "push",
    image = ":server_multiarch",
    registry = "ghcr.io",
    repository = "myorg/server",
    tag = "latest",
)

image_load(
    name = "load",
    image = ":server_image",  # Can load multiarch too!
    tag = "server:dev",
)
```

---

## Summary Checklist

Use this checklist to track your migration:

- [ ] Update `MODULE.bazel` to use `rules_img`
- [ ] Convert `oci.pull()` to `pull()` repository rules
- [ ] Update base image references (split `image` into `registry`/`repository`)
- [ ] Migrate layers:
  - [ ] Option 1: Use `pkg_tar`/`tar` targets directly in `layers` attribute
  - [ ] Option 2: Wrap with `layer_from_tar` for better control
  - [ ] Option 3: Migrate to `image_layer` (recommended)
- [ ] Convert `oci_image` to `image_manifest`
  - [ ] Rename `tars` → `layers`
  - [ ] Rename `workdir` → `working_dir`
  - [ ] Move unsupported attributes to `config_fragment` if needed
- [ ] Convert `oci_image_index` to `image_index`
- [ ] Use output groups:
  - [ ] `oci_layout` (for OCI layout dir)
  - [ ] `oci_tarball` (for OCI layout tar)
  - [ ] `tarball` output group on `image_load` for Docker format
- [ ] Update container structure tests (minimal changes)
- [ ] Convert `oci_push` to `image_push`
  - [ ] Split `repository` into `registry` + `repository`
  - [ ] Rename `remote_tags` → `tag_list` or `tag`
  - [ ] Consider using advanced push strategies
- [ ] Convert `oci_load` to `image_load`
  - [ ] Rename `repo_tags` → `tag`, `tag_list` or `tag_file`
  - [ ] Consider enabling incremental loading
- [ ] Test your migration:
  - [ ] Build images: `bazel build //path/to:image`
  - [ ] Push images: `bazel run //path/to:push`
  - [ ] Load images: `bazel run //path/to:load`
  - [ ] Run structure tests: `bazel test //path/to:structure_test`

---

## Additional Resources

- [rules_img Documentation](/README.md)
- [Push Strategies Guide](push-strategies.md)
- [Template Expansion](templating.md)
- [Example Projects](/e2e/)
  - [Go](/e2e/go/)
  - [C++](/e2e/cc/)
  - [Python](/e2e/python/)
  - [JavaScript](/e2e/js/)

## Getting Help

If you encounter issues during migration:

1. Check the [examples directory](/e2e/) for working patterns
2. Review the [API documentation](/README.md)
3. Open an issue on [GitHub](https://github.com/bazel-contrib/rules_img/issues)

Happy migrating! 🚀
