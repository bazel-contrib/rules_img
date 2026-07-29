"""Public API for loading container images into a daemon.

The `image_load` rule creates an executable target that loads container images into a local daemon (containerd, Docker, or Podman).

## Example

```python
load("@rules_img//img:image.bzl", "image_manifest")
load("@rules_img//img:load.bzl", "image_load")
load("@rules_img//img:layer.bzl", "image_layer")

# Create a simple layer
image_layer(
    name = "app_layer",
    srcs = {
        "/app/hello.txt": "hello.txt",
    },
)

# Build an image
image_manifest(
    name = "my_image",
    base = "@alpine",
    layers = [":app_layer"],
)

# Preferred: the same registry/repository/tag split as image_push. The loaded
# image name is reconstructed as "my-registry.example.com/my-app:latest".
image_load(
    name = "load",
    image = ":my_image",
    registry = "my-registry.example.com",
    repository = "my-app",
    tag = "latest",
)

# The rules_oci-compatible form still works: when loading into a daemon the image
# name is just a string, so a single fully-qualified tag (with no
# registry/repository) is used verbatim.
image_load(
    name = "load_legacy",
    image = ":my_image",
    tag = "my-app:latest",
)

# Load with multiple full-reference tags
image_load(
    name = "load_multi",
    image = ":my_image",
    tag_list = ["my-app:latest", "my-app:v1.0.0"],
)

# A registry that includes a port works like any other registry.
image_load(
    name = "load_with_port",
    image = ":my_image",
    registry = "docker.mycompany.tld:1234",
    repository = "my-app",
    tag = "latest",
)
```

Splitting the name into `registry` / `repository` / `tag` is optional and does
not change what a local daemon sees; it just keeps the load target aligned with
a matching `image_push`, making it easy to push the same image to a registry
later.

Then run:
```bash
# Load the image into your local daemon
bazel run //:load
```

## Image names

The loaded image name is exactly the name you configure. `rules_img` does not
apply Docker's reference normalization: nothing is prepended to the name, and
the `library/` namespace of Docker Hub official images is never added, so
`tag = "my-app:latest"` loads an image literally called `my-app:latest`. The one
thing filled in is the tag: a reference written without one is loaded as
`:latest`, because an untagged name is not something `docker load` accepts. A
name that cannot be a valid image reference fails the build rather than being
silently rewritten.

Note that a short name like `my-app:latest` is not a fully-qualified reference.
Tools that insist on one — most notably `docker` with the containerd image
store, which hides images whose name is not canonical — need a name that
includes a registry, e.g. `docker.io/library/my-app:latest` or the
`registry`/`repository` split above.

## Platform Selection

When running the load target, you can use the `--platform` flag to filter which platforms to load from multi-platform images:

```bash
# Load all platforms (default)
bazel run //path/to:load_target

# Load only linux/amd64
bazel run //path/to:load_target -- --platform linux/amd64
```

**Note**: Docker daemon only supports loading a single platform at a time. If multiple platforms are specified with Docker, an error will be returned.
"""

load("//img/private:load.bzl", _image_load = "image_load")
load("//img/private:load_spec.bzl", _image_load_spec = "image_load_spec")

image_load = _image_load
image_load_spec = _image_load_spec
