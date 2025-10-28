<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Public API for pulling base container images.

<a id="pull"></a>

## pull

<pre>
load("@rules_img//img:pull.bzl", "pull")

pull(<a href="#pull-name">name</a>, <a href="#pull-airgapped">airgapped</a>, <a href="#pull-blob_files">blob_files</a>, <a href="#pull-digest">digest</a>, <a href="#pull-downloader">downloader</a>, <a href="#pull-layer_handling">layer_handling</a>, <a href="#pull-registries">registries</a>, <a href="#pull-registry">registry</a>,
     <a href="#pull-repo_mapping">repo_mapping</a>, <a href="#pull-repository">repository</a>, <a href="#pull-tag">tag</a>)
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
| <a id="pull-airgapped"></a>airgapped |  Enable airgapped mode.<br><br>When enabled, the pull tool will only use locally cached blobs and will not attempt any network requests. This is useful for completely offline/air-gapped environments where all required blobs must be provided via blob_files.<br><br>If a required blob is not available locally, the pull will fail rather than attempting to download it.   | Boolean | optional |  `False`  |
| <a id="pull-blob_files"></a>blob_files |  Pre-downloaded blob files to use.<br><br>A dictionary mapping blob digests (e.g., "sha256:abc123...") to file labels. These blobs will be verified and used instead of downloading from the registry when available. This is useful for air-gapped environments or to avoid redundant downloads of common base layers.   | Dictionary: String -> Label | optional |  `{}`  |
| <a id="pull-digest"></a>digest |  The image digest for reproducible pulls (e.g., "sha256:abc123...").<br><br>When specified, the image is pulled by digest instead of tag, ensuring reproducible builds. The digest must be a full SHA256 digest starting with "sha256:".   | String | optional |  `""`  |
| <a id="pull-downloader"></a>downloader |  The tool to use for downloading manifests and blobs.<br><br>**Available options:**<br><br>* **`img_tool`** (default): Uses the `img` tool for all downloads.<br><br>* **`bazel`**: Uses Bazel's native HTTP capabilities for downloading manifests and blobs.   | String | optional |  `"img_tool"`  |
| <a id="pull-layer_handling"></a>layer_handling |  Strategy for handling image layers.<br><br>This attribute controls when and how layer data is fetched from the registry.<br><br>**Available strategies:**<br><br>* **`shallow`** (default): Layer data is fetched only if needed during push operations,   but is not available during the build. This is the most efficient option for images   that are only used as base images for pushing.<br><br>* **`eager`**: Layer data is fetched in the repository rule and is always available.   This ensures layers are accessible in build actions but is inefficient as all layers   are downloaded regardless of whether they're needed. Use this for base images that   need to be read or inspected during the build.<br><br>* **`lazy`**: Layer data is downloaded in a build action when requested. This provides   access to layers during builds while avoiding unnecessary downloads, but requires   network access during the build phase. **EXPERIMENTAL:** Use at your own risk.   | String | optional |  `"shallow"`  |
| <a id="pull-registries"></a>registries |  List of mirror registries to try in order.<br><br>These registries will be tried in order before the primary registry. Useful for corporate environments with registry mirrors or air-gapped setups.   | List of strings | optional |  `[]`  |
| <a id="pull-registry"></a>registry |  Primary registry to pull from (e.g., "index.docker.io", "gcr.io").<br><br>If not specified, defaults to Docker Hub. Can be overridden by entries in registries list.   | String | optional |  `""`  |
| <a id="pull-repo_mapping"></a>repo_mapping |  In `WORKSPACE` context only: a dictionary from local repository name to global repository name. This allows controls over workspace dependency resolution for dependencies of this repository.<br><br>For example, an entry `"@foo": "@bar"` declares that, for any time this repository depends on `@foo` (such as a dependency on `@foo//some:target`, it should actually resolve that dependency within globally-declared `@bar` (`@bar//some:target`).<br><br>This attribute is _not_ supported in `MODULE.bazel` context (when invoking a repository rule inside a module extension's implementation function).   | <a href="https://bazel.build/rules/lib/dict">Dictionary: String -> String</a> | optional |  |
| <a id="pull-repository"></a>repository |  The image repository within the registry (e.g., "library/ubuntu", "my-project/my-image").<br><br>For Docker Hub, official images use "library/" prefix (e.g., "library/ubuntu").   | String | required |  |
| <a id="pull-tag"></a>tag |  The image tag to pull (e.g., "latest", "24.04", "v1.2.3").<br><br>While required, it's recommended to also specify a digest for reproducible builds.   | String | optional |  `""`  |


