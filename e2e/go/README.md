# Go Container Image Examples

This directory contains examples demonstrating different ways of building container images for Go applications using `rules_img`. The examples are ordered by increasing complexity, showcasing various features and use cases.

## Examples

### 1. [image_from_binary_example/](./image_from_binary_example/BUILD.bazel) - Quick Start with `image_from_binary`

The fastest way to package a Go binary: just point `image_from_binary` at your `go_binary` target. The entrypoint, args, env, and working directory are configured automatically.

### 2. [image/](./image/BUILD.bazel) - Basic Single-Platform Image

The simplest example showing how to build a container image for a Go application:
- Creates a single-platform container image with a Go binary
- Uses the native platform (whatever platform Bazel is running on)
- Demonstrates basic `image_layer` and `image_manifest` usage
- Shows how to push images to a registry

### 3. [multiarch/](./multiarch/BUILD.bazel) - Automated Multi-Architecture Images

The recommended approach for multi-platform container images:
- Uses Bazel's platform transitions for automatic multi-platform builds
- Creates an `image_index` containing both ARM64 and AMD64 variants
- Minimal configuration - Bazel handles the platform-specific builds automatically

### 5. [customization/](./customization/BUILD.bazel) - Image Metadata and Configuration

Demonstrates comprehensive image customization options:
- Sets custom entrypoint with command arguments
- Configures environment variables for the container
- Adds OCI annotations for image metadata
- Applies labels for compatibility with label-schema
- Customizes runtime behavior (user, stop signal)
- Shows all available configuration options in `image_manifest`

## Running the Examples

Each example can be built and pushed using standard Bazel commands:

```bash
# Build an image
bazel build //image:go_image

# Push to a registry
bazel run //image:go_push

# Build all examples
bazel build //...
```
