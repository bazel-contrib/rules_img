<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Public API for container image push rules.

<a id="image_push"></a>

## image_push

<pre>
load("@rules_img//img:push.bzl", "image_push")

image_push(<a href="#image_push-name">name</a>, <a href="#image_push-build_settings">build_settings</a>, <a href="#image_push-image">image</a>, <a href="#image_push-registry">registry</a>, <a href="#image_push-repository">repository</a>, <a href="#image_push-stamp">stamp</a>, <a href="#image_push-strategy">strategy</a>, <a href="#image_push-tag">tag</a>, <a href="#image_push-tag_file">tag_file</a>,
           <a href="#image_push-tag_list">tag_list</a>)
</pre>

Pushes container images to a registry.

This rule creates an executable target that uploads OCI images to container registries.
It supports multiple push strategies optimized for different use cases, from simple
uploads to advanced content-addressable storage integration.

Key features:
- **Multiple push strategies**: Choose between eager, lazy, CAS-based, or BES-integrated pushing
- **Template expansion**: Dynamic registry, repository, and tag values using build settings
- **Stamping support**: Include build information in image tags
- **Incremental uploads**: Skip blobs that already exist in the registry

The rule produces an executable that can be run with `bazel run`.

Example:

```python
load("@rules_img//img:push.bzl", "image_push")

# Simple push to Docker Hub
image_push(
    name = "push_app",
    image = ":my_app",
    registry = "index.docker.io",
    repository = "myorg/myapp",
    tag = "latest",
)

# Push multi-platform image with multiple tags
image_push(
    name = "push_multiarch",
    image = ":my_app_index",  # References an image_index
    registry = "gcr.io",
    repository = "my-project/my-app",
    tag_list = ["latest", "v1.0.0"],
)

# Dynamic push with build settings
image_push(
    name = "push_dynamic",
    image = ":my_app",
    registry = "{{.REGISTRY}}",
    repository = "{{.PROJECT}}/my-app",
    tag = "{{.VERSION}}",
    build_settings = {
        "REGISTRY": "//settings:registry",
        "PROJECT": "//settings:project",
        "VERSION": "//settings:version",
    },
)

# Push with stamping for unique tags
image_push(
    name = "push_stamped",
    image = ":my_app",
    registry = "index.docker.io",
    repository = "myorg/myapp",
    tag = "latest-{{.BUILD_TIMESTAMP}}",
    stamp = "enabled",
)

# Digest-only push (no tag)
image_push(
    name = "push_by_digest",
    image = ":my_app",
    registry = "gcr.io",
    repository = "my-project/my-app",
    # No tag specified - will push by digest only
)
```

Push strategies:
- **`eager`**: Materializes all layers next to push binary. Simple, correct, but may be inefficient.
- **`lazy`**: Layers are not stored locally. Missing layers are streamed from Bazel's remote cache.
- **`cas_registry`**: Uses content-addressable storage for extreme efficiency. Requires
  CAS-enabled infrastructure.
- **`bes`**: Image is pushed as side-effect of Build Event Stream upload. No "bazel run" command needed.
  Requires Build Event Service integration.

See [push strategies documentation](/docs/push-strategies.md) for detailed comparisons.

Runtime usage:
```bash
# Push to registry
bazel run //path/to:push_app

# The push command will output the image digest
```

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="image_push-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="image_push-build_settings"></a>build_settings |  Build settings for template expansion.<br><br>Maps template variable names to string_flag targets. These values can be used in registry, repository, and tag attributes using `{{.VARIABLE_NAME}}` syntax (Go template).<br><br>Example: <pre><code class="language-python">build_settings = {&#10;    "REGISTRY": "//settings:docker_registry",&#10;    "VERSION": "//settings:app_version",&#10;}</code></pre><br><br>See [template expansion](/docs/templating.md) for more details.   | Dictionary: String -> Label | optional |  `{}`  |
| <a id="image_push-image"></a>image |  Image to push. Should provide ImageManifestInfo or ImageIndexInfo.   | <a href="https://bazel.build/concepts/labels">Label</a> | required |  |
| <a id="image_push-registry"></a>registry |  Registry URL to push the image to.<br><br>Common registries: - Docker Hub: `index.docker.io` - Google Container Registry: `gcr.io` or `us.gcr.io` - GitHub Container Registry: `ghcr.io` - Amazon ECR: `123456789.dkr.ecr.us-east-1.amazonaws.com`<br><br>Subject to [template expansion](/docs/templating.md).   | String | optional |  `""`  |
| <a id="image_push-repository"></a>repository |  Repository path within the registry.<br><br>Subject to [template expansion](/docs/templating.md).   | String | optional |  `""`  |
| <a id="image_push-stamp"></a>stamp |  Enable build stamping for template expansion.<br><br>Controls whether to include volatile build information: - **`auto`** (default): Uses the global stamping configuration - **`enabled`**: Always include stamp information (BUILD_TIMESTAMP, BUILD_USER, etc.) if Bazel's "--stamp" flag is set - **`disabled`**: Never include stamp information<br><br>See [template expansion](/docs/templating.md) for available stamp variables.   | String | optional |  `"auto"`  |
| <a id="image_push-strategy"></a>strategy |  Push strategy to use.<br><br>See [push strategies documentation](/docs/push-strategies.md) for detailed information.   | String | optional |  `"auto"`  |
| <a id="image_push-tag"></a>tag |  Tag to apply to the pushed image.<br><br>Optional - if omitted, the image is pushed by digest only.<br><br>Subject to [template expansion](/docs/templating.md).   | String | optional |  `""`  |
| <a id="image_push-tag_file"></a>tag_file |  File containing newline-delimited tags to apply to the pushed image.<br><br>The file should contain one tag per line. Empty lines are ignored. Tags from this file are merged with tags specified via `tag` or `tag_list` attributes.<br><br>Example file content: <pre><code>latest&#10;v1.0.0&#10;stable</code></pre><br><br>Can be combined with `tag` or `tag_list` to merge tags from multiple sources. Each tag is subject to [template expansion](/docs/templating.md).   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_push-tag_list"></a>tag_list |  List of tags to apply to the pushed image.<br><br>Useful for applying multiple tags in a single push:<br><br><pre><code class="language-python">tag_list = ["latest", "v1.0.0", "stable"]</code></pre><br><br>Cannot be used together with `tag`. Can be combined with `tag_file` to merge tags from both sources. Each tag is subject to [template expansion](/docs/templating.md).   | List of strings | optional |  `[]`  |


