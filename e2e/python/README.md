# Python e2e example

This directory contains examples of creating OCI container images for Python applications using `rules_img`.

## Quick Start with `image_from_binary`

The fastest way to package a Python binary: just point `image_from_binary` at your `py_binary` target. The entrypoint, args, env, and working directory are configured automatically. See [image_from_binary_example/](./image_from_binary_example/BUILD.bazel).

## Building

```bash
bazel build //:image
```

## Pushing to registry

```bash
bazel run //:push
```

## Running locally

```bash
bazel run //:load
docker run --rm ghcr.io/malt3/rules_img/python:sideloaded
```

See the BUILD files for more details.
