<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Public API for pulling base container images.

<a id="pull"></a>

## pull

<pre>
load("@rules_img//img:pull.bzl", "pull")

pull(<a href="#pull-name">name</a>, <a href="#pull-digest">digest</a>, <a href="#pull-downloader">downloader</a>, <a href="#pull-layer_handling">layer_handling</a>, <a href="#pull-registries">registries</a>, <a href="#pull-registry">registry</a>, <a href="#pull-repository">repository</a>, <a href="#pull-tag">tag</a>,
     <a href="#pull-unsafe_allow_tag_without_digest">unsafe_allow_tag_without_digest</a>)
</pre>

Pulls a container image from a registry using shallow pulling.

This repository rule implements shallow pulling - it only downloads the image manifest
and config, not the actual layer blobs. The layers are downloaded on-demand during
push operations or when explicitly needed. This significantly reduces bandwidth usage
and speeds up builds, especially for large base images.

Example usage in MODULE.bazel:
```starlark
pull = use_repo_rule("@rules_img//img:pull.bzl", "pull")

pull(
    name = "ubuntu",
    digest = "sha256:1e622c5f073b4f6bfad6632f2616c7f59ef256e96fe78bf6a595d1dc4376ac02",
    registry = "index.docker.io",
    repository = "library/ubuntu",
    tag = "24.04",
)
```

The `digest` parameter is recommended for reproducible builds. If omitted, the rule
will resolve the tag to a digest at fetch time and print a warning.

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="pull-name"></a>name |  A unique name for this repository.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="pull-digest"></a>digest |  The image digest for reproducible pulls (e.g., "sha256:abc123...").<br><br>When specified, the image is pulled by digest instead of tag, ensuring reproducible builds. The digest must be a full SHA256 digest starting with "sha256:".   | String | optional |  `""`  |
| <a id="pull-downloader"></a>downloader |  The tool to use for downloading manifests and blobs.<br><br>**Available options:**<br><br>* **`img_tool`** (default): Uses the `img` tool for all downloads.<br><br>* **`bazel`**: Uses Bazel's native HTTP capabilities for downloading manifests and blobs.   | String | optional |  `"img_tool"`  |
| <a id="pull-layer_handling"></a>layer_handling |  Strategy for handling image layers.<br><br>This attribute controls when and how layer data is fetched from the registry.<br><br>**Available strategies:**<br><br>* **`shallow`** (default): Layer data is fetched only if needed during push operations,   but is not available during the build. This is the most efficient option for images   that are only used as base images for pushing.<br><br>* **`eager`**: Layer data is fetched in the repository rule and is always available.   This ensures layers are accessible in build actions but is inefficient as all layers   are downloaded regardless of whether they're needed. Use this for base images that   need to be read or inspected during the build.<br><br>* **`lazy`**: Layer data is downloaded in a build action when requested. This provides   access to layers during builds while avoiding unnecessary downloads, but requires   network access during the build phase. **EXPERIMENTAL:** Use at your own risk.   | String | optional |  `"shallow"`  |
| <a id="pull-registries"></a>registries |  List of mirror registries to try in order.<br><br>These registries will be tried in order before the primary registry. Useful for corporate environments with registry mirrors or air-gapped setups.   | List of strings | optional |  `[]`  |
| <a id="pull-registry"></a>registry |  Primary registry to pull from (e.g., "index.docker.io", "gcr.io").<br><br>If not specified, defaults to Docker Hub. Can be overridden by entries in registries list.   | String | optional |  `""`  |
| <a id="pull-repository"></a>repository |  The image repository within the registry (e.g., "library/ubuntu", "my-project/my-image").<br><br>For Docker Hub, official images use "library/" prefix (e.g., "library/ubuntu").   | String | required |  |
| <a id="pull-tag"></a>tag |  The image tag to pull (e.g., "latest", "24.04", "v1.2.3").<br><br>While required, it's recommended to also specify a digest for reproducible builds.   | String | optional |  `""`  |
| <a id="pull-unsafe_allow_tag_without_digest"></a>unsafe_allow_tag_without_digest |  Allow pulling by tag without specifying a digest.<br><br>**WARNING:** This is not recommended for reproducible builds as tags can be moved to point to different image versions. Only use this when you're managing reproducibility through other means (e.g., content-based tags).<br><br>When enabled, the rule will resolve the tag to a digest at fetch time and use that digest, but will not fail if no digest is explicitly provided.   | Boolean | optional |  `False`  |


