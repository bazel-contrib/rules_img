# C++ Container Image Examples

This directory contains examples demonstrating how to build container images for C++ applications using `rules_img`. The examples showcase different standard library configurations and multi-platform support.

## Examples

### 1. [image_from_binary_example/](./image_from_binary_example/BUILD.bazel) - Quick Start with `image_from_binary`

The fastest way to package a C++ binary: just point `image_from_binary` at your `cc_binary` target. The entrypoint, args, env, and working directory are configured automatically.

### 2. [BUILD.bazel](./BUILD.bazel) - Basic Multi-Platform C++ Image

The simplest example showing how to build a container image for a C++ application:
- Creates a multi-platform container image (AMD64 and ARM64) with a C++ binary
- Uses the distroless/cc base image which includes glibc
- Demonstrates basic `cc_binary`, `image_layer`, `image_manifest`, and `image_index` usage
- Shows how to build for multiple architectures using platform transitions

### 2. [custom_standard_library/](./custom_standard_library/BUILD.bazel) - Custom Standard Library Support

Demonstrates building C++ images with different standard library implementations:
- Supports both libc++ (LLVM) and libstdc++ (GNU) standard libraries
- Uses custom base images that include the appropriate runtime libraries
- Creates separate image indexes for each standard library variant
- Shows how to use `select()` to choose different base images based on the platform
- Useful when you need specific C++ runtime compatibility or want to minimize image size

## Supporting Files

### [base/](./base/BUILD.bazel) - Custom Base Image Construction

Provides base images with different C++ standard libraries:
- Uses `layer_from_tar` to import pre-built base images
- Supports both libc++ and libstdc++ variants for AMD64 and ARM64
- Demonstrates platform-specific layer selection

### [platform/](./platform/BUILD.bazel) - Platform Definitions

Defines custom platforms that combine CPU architecture with standard library choice:
- `linux_amd64_libc++`, `linux_arm64_libc++` - Linux platforms with LLVM's libc++
- `linux_amd64_libstdc++`, `linux_arm64_libstdc++` - Linux platforms with GNU's libstdc++

## Building and Running

```bash
# Build the basic example
bazel build //:cc_image

# Build with custom standard library (libc++)
bazel build //custom_standard_library:cc_index_libc++

# Build with custom standard library (libstdc++)
bazel build //custom_standard_library:cc_index_libstdc++

# Push images to registry
bazel run //:push
bazel run //custom_standard_library:push_libc++
bazel run //custom_standard_library:push_libstdc++
```
