"""Public API for loading container images into a daemon.

The `image_load` rule creates an executable target that loads container images into a local daemon (containerd or Docker).

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

# Create a load target with a single tag
image_load(
    name = "load",
    image = ":my_image",
    tag = "my-app:latest",
)

# Load with multiple tags
image_load(
    name = "load_multi",
    image = ":my_image",
    tag_list = ["my-app:latest", "my-app:v1.0.0"],
)
```

Then run:
```bash
# Load the image into your local daemon
bazel run //:load
```

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

image_load = _image_load
