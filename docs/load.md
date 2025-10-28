<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Public API for loading container images into a daemon.

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

<a id="image_load"></a>

## image_load

<pre>
load("@rules_img//img:load.bzl", "image_load")

image_load(<a href="#image_load-name">name</a>, <a href="#image_load-build_settings">build_settings</a>, <a href="#image_load-daemon">daemon</a>, <a href="#image_load-image">image</a>, <a href="#image_load-stamp">stamp</a>, <a href="#image_load-strategy">strategy</a>, <a href="#image_load-tag">tag</a>, <a href="#image_load-tag_file">tag_file</a>, <a href="#image_load-tag_list">tag_list</a>)
</pre>

Loads container images into a local daemon (Docker or containerd).

This rule creates an executable target that imports OCI images into your local
container runtime. It supports both Docker and containerd, with intelligent
detection of the best loading method for optimal performance.

Key features:
- **Incremental loading**: Skips blobs that already exist in the daemon
- **Multi-platform support**: Can load entire image indexes or specific platforms
- **Direct containerd integration**: Bypasses Docker for faster imports when possible
- **Platform filtering**: Use `--platform` flag at runtime to select specific platforms

The rule produces an executable that can be run with `bazel run`.

Output groups:
- `tarball`: Docker save compatible tarball (only available for single-platform images)

Example:

```python
load("@rules_img//img:load.bzl", "image_load")

# Load a single-platform image with a single tag
image_load(
    name = "load_app",
    image = ":my_app",  # References an image_manifest
    tag = "my-app:latest",
)

# Load with multiple tags
image_load(
    name = "load_multi",
    image = ":my_app",
    tag_list = ["my-app:latest", "my-app:v1.0.0", "my-app:stable"],
)

# Load a multi-platform image
image_load(
    name = "load_multiarch",
    image = ":my_app_index",  # References an image_index
    tag = "my-app:latest",
    daemon = "containerd",  # Explicitly use containerd
)

# Load with dynamic tagging
image_load(
    name = "load_dynamic",
    image = ":my_app",
    tag = "my-app:{{.BUILD_USER}}",  # Template expansion
    build_settings = {
        "BUILD_USER": "//settings:username",
    },
)
```

Runtime usage:
```bash
# Load all platforms
bazel run //path/to:load_app

# Load specific platform only
bazel run //path/to:load_multiarch -- --platform linux/arm64

# Build Docker save tarball
bazel build //path/to:load_app --output_groups=tarball
```

Performance notes:
- When Docker uses containerd storage (Docker 23.0+), images are loaded directly
  into containerd for better performance if the containerd socket is accessible.
- For older Docker versions, falls back to `docker load` which requires building
  a tar file (slower and limited to single-platform images)
- The `--platform` flag filters which platforms are loaded from multi-platform images

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="image_load-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="image_load-build_settings"></a>build_settings |  Build settings to use for [template expansion](/docs/templating.md). Keys are setting names, values are labels to string_flag targets.   | Dictionary: String -> Label | optional |  `{}`  |
| <a id="image_load-daemon"></a>daemon |  Container daemon to use for loading the image.<br><br>Available options: - **`auto`** (default): Uses the global default setting (usually `docker`) - **`containerd`**: Loads directly into containerd namespace. Supports multi-platform images   and incremental loading. - **`docker`**: Loads via Docker daemon. When Docker uses containerd storage (23.0+),   loads directly into containerd. Otherwise falls back to `docker load` command which   is slower and limited to single-platform images.<br><br>The best performance is achieved with: - Direct containerd access (daemon = "containerd") - Docker 23.0+ with containerd storage enabled and accessible containerd socket   | String | optional |  `"auto"`  |
| <a id="image_load-image"></a>image |  Image to load. Should provide ImageManifestInfo or ImageIndexInfo.   | <a href="https://bazel.build/concepts/labels">Label</a> | required |  |
| <a id="image_load-stamp"></a>stamp |  Whether to use stamping for [template expansion](/docs/templating.md). If 'enabled', uses volatile-status.txt and version.txt if present. 'auto' uses the global default setting.   | String | optional |  `"auto"`  |
| <a id="image_load-strategy"></a>strategy |  Strategy for handling image layers during load.<br><br>Available strategies: - **`auto`** (default): Uses the global default load strategy - **`eager`**: Downloads all layers during the build phase. Ensures all layers are   available locally before running the load command. - **`lazy`**: Downloads layers only when needed during the load operation. More   efficient for large images where some layers might already exist in the daemon.   | String | optional |  `"auto"`  |
| <a id="image_load-tag"></a>tag |  Tag to apply when loading the image.<br><br>Optional - if omitted, the image is loaded without a tag.<br><br>Subject to [template expansion](/docs/templating.md).   | String | optional |  `""`  |
| <a id="image_load-tag_file"></a>tag_file |  File containing newline-delimited tags to apply when loading the image.<br><br>The file should contain one tag per line. Empty lines are ignored. Tags from this file are merged with tags specified via `tag` or `tag_list` attributes.<br><br>Example file content: <pre><code>latest&#10;v1.0.0&#10;stable</code></pre><br><br>Can be combined with `tag` or `tag_list` to merge tags from multiple sources. Each tag is subject to [template expansion](/docs/templating.md).   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_load-tag_list"></a>tag_list |  List of tags to apply when loading the image.<br><br>Useful for applying multiple tags in a single load:<br><br><pre><code class="language-python">tag_list = ["latest", "v1.0.0", "stable"]</code></pre><br><br>Cannot be used together with `tag`. Can be combined with `tag_file` to merge tags from both sources. Each tag is subject to [template expansion](/docs/templating.md).   | List of strings | optional |  `[]`  |


