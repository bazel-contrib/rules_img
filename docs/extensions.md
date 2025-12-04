<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Public API for rules_img module extensions.

<a id="images"></a>

## images

<pre>
images = use_extension("@rules_img//img:extensions.bzl", "images")
images.pull(<a href="#images.pull-name">name</a>, <a href="#images.pull-digest">digest</a>, <a href="#images.pull-layer_handling">layer_handling</a>, <a href="#images.pull-registries">registries</a>, <a href="#images.pull-registry">registry</a>, <a href="#images.pull-repository">repository</a>, <a href="#images.pull-tag">tag</a>)
images.settings(<a href="#images.settings-downloader">downloader</a>)
</pre>

Module extension for pulling container images in Bzlmod projects.

This extension enables declarative pulling of container images using Bazel's module
system. Images are pulled once and shared across all modules, with automatic deduplication
of blobs for efficient storage.

Example usage in MODULE.bazel:

```starlark
images = use_extension("@rules_img//img:extensions.bzl", "images")

# Pull with friendly name
images.pull(
    name = "ubuntu",
    digest = "sha256:1e622c5f073b4f6bfad6632f2616c7f59ef256e96fe78bf6a595d1dc4376ac02",
    registry = "index.docker.io",
    repository = "library/ubuntu",
    tag = "24.04",
)

# Pull without name - use repository as identifier
images.pull(
    digest = "sha256:029d4461bd98f124e531380505ceea2072418fdf28752aa73b7b273ba3048903",
    registry = "gcr.io",
    repository = "distroless/base",
)

use_repo(images, "rules_img_images.bzl")
```

Access pulled images in BUILD files using the generated helper. The `name` attribute
is optional - if not specified, use the `repository` value to reference the image:

```starlark
load("@rules_img_images.bzl", "image")

image_manifest(
    name = "my_app",
    base = image("ubuntu"),  # References the friendly name
    ...
)

image_manifest(
    name = "my_other_app",
    base = image("distroless/base"),  # References the repository
    ...
)
```

The extension creates deduplicated blob repositories, so pulling multiple images
from the same base only downloads shared layers once. The `digest` parameter is
required for reproducibility.


**TAG CLASSES**

<a id="images.pull"></a>

### pull

**Attributes**

| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="images.pull-name"></a>name |  Friendly name for the image (e.g., 'ubuntu', 'distroless-base').<br><br>This name is used to reference the image in your code via the `image()` helper function. If not specified, defaults to the repository name.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | optional |  `""`  |
| <a id="images.pull-digest"></a>digest |  The image digest for reproducible pulls (e.g., "sha256:abc123...").<br><br>When specified, the image is pulled by digest instead of tag, ensuring reproducible builds. The digest must be a full SHA256 digest starting with "sha256:".   | String | optional |  `""`  |
| <a id="images.pull-layer_handling"></a>layer_handling |  Strategy for handling image layers.<br><br>This attribute controls when and how layer data is fetched from the registry.<br><br>**Available strategies:**<br><br>* **`shallow`** (default): Layer data is fetched only if needed during push operations,   but is not available during the build. This is the most efficient option for images   that are only used as base images for pushing.<br><br>* **`eager`**: Layer data is fetched in the repository rule and is always available.   This ensures layers are accessible in build actions but is inefficient as all layers   are downloaded regardless of whether they're needed. Use this for base images that   need to be read or inspected during the build.<br><br>* **`lazy`**: Layer data is downloaded in a build action when requested. This provides   access to layers during builds while avoiding unnecessary downloads, but requires   network access during the build phase. **EXPERIMENTAL:** Use at your own risk.   | String | optional |  `"shallow"`  |
| <a id="images.pull-registries"></a>registries |  List of mirror registries to try in order.<br><br>These registries will be tried in order before the primary registry. Useful for corporate environments with registry mirrors or air-gapped setups.   | List of strings | optional |  `[]`  |
| <a id="images.pull-registry"></a>registry |  Primary registry to pull from (e.g., "index.docker.io", "gcr.io").<br><br>If not specified, defaults to Docker Hub. Can be overridden by entries in registries list.   | String | optional |  `""`  |
| <a id="images.pull-repository"></a>repository |  The image repository within the registry (e.g., "library/ubuntu", "my-project/my-image").<br><br>For Docker Hub, official images use "library/" prefix (e.g., "library/ubuntu").   | String | required |  |
| <a id="images.pull-tag"></a>tag |  The image tag to pull (e.g., "latest", "24.04", "v1.2.3").<br><br>While optional, it's recommended to also specify a digest for reproducible builds.   | String | optional |  `""`  |

<a id="images.settings"></a>

### settings

**Attributes**

| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="images.settings-downloader"></a>downloader |  The tool to use for downloading manifests and blobs if the current module is the root module.<br><br>**Available options:**<br><br>* **`img_tool`** (default): Uses the `img` tool for all downloads.<br><br>* **`bazel`**: Uses Bazel's native HTTP capabilities for downloading manifests and blobs.   | String | optional |  `"img_tool"`  |


