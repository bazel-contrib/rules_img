# Platforms in rules_img

This guide explains how to work with Bazel platforms when building container images with `rules_img`.

## Table of Contents

- [Platform Basics](#platform-basics)
- [Architecture Variants](#architecture-variants)
- [Common Use Cases](#common-use-cases)
  - [Multi-Platform Images](#multi-platform-images)
  - [macOS Docker Daemon](#macos-docker-daemon)
- [Further Reading](#further-reading)

## Platform Basics

In Bazel, a [platform](https://bazel.build/extending/platforms) defines the environment where code runs. It consists of constraint values that describe properties like operating system and CPU architecture.

`rules_img` uses platforms to:
- **Build multi-platform images** - Create image indexes containing variants for different architectures
- **Select base images** - Choose the appropriate base image variant for each target platform
- **Generate OCI metadata** - Populate the `os`, `architecture`, and `variant` fields in image manifests

### Platform Structure

In rules_img, the important constrains are:
- **OS constraint** - From `@platforms//os:*` (e.g., `linux`, `macos`, `windows`, although Linux is the only relevant target platform for container images in practice)
- **CPU constraint** - From `@platforms//cpu:*` (e.g., `x86_64`, `aarch64`, `arm`)
- **Optional variant constraint** - From `@rules_img//img/constraints:variant` for CPU microarchitecture levels

Example platform definition:
```starlark
platform(
    name = "linux_amd64",
    constraint_values = [
        "@platforms//os:linux",
        "@platforms//cpu:x86_64",
    ],
)
```

## Architecture Variants

OCI images support architecture variants to specify CPU microarchitecture levels or instruction set versions. This allows runtimes to select the most optimized image for their hardware.

### Available Variants

`rules_img` provides constraint values for common architecture variants:

#### AMD64 (x86-64 microarchitecture levels)
- `@rules_img//img/constraints/amd64:v1` - Baseline (all x86-64 CPUs)
- `@rules_img//img/constraints/amd64:v2` - SSE4.2, POPCNT (Nehalem+, 2009)
- `@rules_img//img/constraints/amd64:v3` - AVX, AVX2, FMA (Haswell+, 2013)
- `@rules_img//img/constraints/amd64:v4` - AVX-512 (Skylake-X+, 2017)

See [x86-64 microarchitecture levels](https://en.wikipedia.org/wiki/X86-64#Microarchitecture_levels) for details.

#### ARM64 (ARMv8/ARMv9)
- `@rules_img//img/constraints/arm64:v8` - ARMv8.0 baseline (alias: `v8.0`)
- `@rules_img//img/constraints/arm64:v8.1` through `v8.9` - ARMv8.x revisions
- `@rules_img//img/constraints/arm64:v9` - ARMv9.0 baseline (alias: `v9.0`)
- `@rules_img//img/constraints/arm64:v9.1` through `v9.7` - ARMv9.x revisions

#### ARM (32-bit)
- `@rules_img//img/constraints/arm:v5` - ARMv5
- `@rules_img//img/constraints/arm:v6` - ARMv6
- `@rules_img//img/constraints/arm:v7` - ARMv7
- `@rules_img//img/constraints/arm:v8` - ARMv8 (32-bit mode)

#### Other Architectures
- PPC64LE: `power8`, `power9`, `power10`
- RISCV64: `rva20u64`, etc.

See `img/constraints/{arch}/BUILD.bazel` for the complete list (or print it with `bazel query @rules_img//img/constraints/...`)

### Using Variants with Language Toolchains

When building binaries with language-specific toolchains (Go, Rust, etc.), you need to specify **both** the language toolchain's variant constraint **and** the `rules_img` variant constraint.

**Why both constraints?**
- The **language toolchain constraint** controls code generation (e.g., which CPU features the compiler can use)
- The **rules_img constraint** populates the `variant` field in the OCI manifest metadata (and selects the best available base image)

#### Example: Go with AMD64-v3

```starlark
platform(
    name = "linux_amd64_v3",
    constraint_values = [
        "@rules_go//go/constraints/amd64:v3",  # Tell Go compiler to use AVX2
        "@rules_img//img/constraints/amd64:v3",  # Set OCI variant field
    ],
    parents = ["@rules_go//go/toolchain:linux_amd64"],
)
```

#### Example: Go with ARM-v7

```starlark
platform(
    name = "linux_arm_v7",
    constraint_values = [
        "@rules_go//go/constraints/arm:7",     # Tell Go compiler to use ARMv7
        "@rules_img//img/constraints/arm:v7",  # Set OCI variant field
    ],
    parents = ["@rules_go//go/toolchain:linux_arm"],
)
```

### When to Use Variants

Use architecture variants when:
- **Optimizing for specific hardware** - Build separate images for newer CPUs with advanced features
- **Ensuring compatibility** - Specify baseline variants to guarantee images run on older hardware
- **Multi-variant indexes** - Create image indexes with multiple variants of the same architecture

Example multi-variant index:
```starlark
image_index(
    name = "optimized_variants",
    manifests = [":app"],
    platforms = [
        "//platform:linux_amd64",     # Baseline (works for any amd64 hardware)
        "//platform:linux_amd64_v2",  # For most modern servers
        "//platform:linux_amd64_v3",  # For Haswell+ with AVX2
        "//platform:linux_amd64_v4",  # For latest servers with AVX-512
    ],
)
```

Container runtimes will automatically select the most suitable variant for their CPU.

## Common Use Cases

### Multi-Platform Images

Create an image index with variants for multiple platforms:

```starlark
image_manifest(
    name = "app_image",
    layers = [":app_layer"],
    entrypoint = ["/app"],
)

image_index(
    name = "multiarch",
    manifests = [":app_image"],
    platforms = [
        "@platforms//linux_amd64",
        "@platforms//linux_arm64",
    ],
)
```

See the [e2e/go/multiarch](../e2e/go/multiarch/BUILD.bazel) example for a complete demonstration.

### macOS Docker Daemon

Docker Desktop on macOS runs a Linux VM to execute containers. To build and load Linux images on macOS, create a platform that uses the host CPU but targets Linux:

```starlark
platform(
    name = "host_docker_platform",
    parents = ["@platforms//host"],  # Use host CPU (x86_64 or arm64)
    constraint_values = [
        "@platforms//os:linux",  # But target Linux OS
    ],
)
```

Use this platform when building images for your local Docker daemon:

```starlark
image_manifest(
    name = "app",
    base = "@ubuntu",
    layers = [
        ":app_layer",
        ":config_layer",
    ],
)

image_manifest(
    name = "image_for_host",
    base = ":image",
    platform = ":host_docker_platform",
)

my_integration_test(
    name = "my_integration_test",
    image = ":image_for_host",
)
```

Then use it:
```bash
bazel test :my_integration_test
```

**Why is this necessary?** Without the custom platform, Bazel would try to select a macOS base image (which doesn't exist for most base images) or fail the build due to platform constraints.

### Platform in Rules

#### `image_manifest`

Build a single-platform image for a specific platform:

```starlark
image_manifest(
    name = "app_arm64",
    platform = "//platform:linux_arm64",  # Force specific platform
    base = "@ubuntu",
    layers = [":app_layer"],
)
```

#### `image_index`

Build multi-platform image indexes using platform transitions:

```starlark
image_index(
    name = "multiarch",
    manifests = [":app"],  # Single manifest target
    platforms = [         # Built for each platform
        "//platform:linux_amd64",
        "//platform:linux_arm64",
    ],
)
```

Or with explicit manifests (no transitions):

```starlark
image_index(
    name = "multiarch",
    manifests = [
        ":app_amd64",  # Pre-built for amd64
        ":app_arm64",  # Pre-built for arm64
    ],
)
```

### Further Reading

- [Bazel Platforms Documentation](https://bazel.build/extending/platforms)
- [OCI Image Spec - Platform](https://github.com/opencontainers/image-spec/blob/main/image-index.md)
- [x86-64 Microarchitecture Levels](https://en.wikipedia.org/wiki/X86-64#Microarchitecture_levels)
- [ARM Architecture Versions](https://en.wikipedia.org/wiki/ARM_architecture_family#Cores)
