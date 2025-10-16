<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Rules to build container images from layers.

Use `image_manifest` to create a single-platform container image,
and `image_index` to compose a multi-platform container image index.

<a id="image_index"></a>

## image_index

<pre>
load("@rules_img//img:image.bzl", "image_index")

image_index(<a href="#image_index-name">name</a>, <a href="#image_index-annotations">annotations</a>, <a href="#image_index-build_settings">build_settings</a>, <a href="#image_index-manifests">manifests</a>, <a href="#image_index-platforms">platforms</a>, <a href="#image_index-stamp">stamp</a>)
</pre>

Creates a multi-platform OCI image index from platform-specific manifests.

This rule combines multiple single-platform images (created by image_manifest) into
a multi-platform image index. The index allows container runtimes to automatically
select the appropriate image for their platform.

The rule supports two usage patterns:
1. Explicit manifests: Provide pre-built manifests for each platform
2. Platform transitions: Provide one manifest target and a list of platforms

The rule produces:
- OCI image index JSON file
- An optional OCI layout directory or tar (via output groups)
- ImageIndexInfo provider for use by image_push

Example (explicit manifests):

```python
image_index(
    name = "multiarch_app",
    manifests = [
        ":app_linux_amd64",
        ":app_linux_arm64",
        ":app_darwin_amd64",
    ],
)
```

Example (platform transitions):
```python
image_index(
    name = "multiarch_app",
    manifests = [":app"],
    platforms = [
        "//platform:linux-x86_64",
        "//platform:linux-aarch64",
    ],
)
```

Output groups:
- `digest`: Digest of the image (sha256:...)
- `oci_layout`: Complete OCI layout directory with all platform blobs
- `oci_tarball`: OCI layout packaged as a tar file for downstream use

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="image_index-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="image_index-annotations"></a>annotations |  Arbitrary metadata for the image index.<br><br>Subject to [template expansion](/docs/templating.md).   | <a href="https://bazel.build/rules/lib/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="image_index-build_settings"></a>build_settings |  Build settings for template expansion.<br><br>Maps template variable names to string_flag targets. These values can be used in the annotations attribute using `{{.VARIABLE_NAME}}` syntax (Go template).<br><br>Example: <pre><code class="language-python">build_settings = {&#10;    "REGISTRY": "//settings:docker_registry",&#10;    "VERSION": "//settings:app_version",&#10;}</code></pre><br><br>See [template expansion](/docs/templating.md) for more details.   | Dictionary: String -> Label | optional |  `{}`  |
| <a id="image_index-manifests"></a>manifests |  List of manifests for specific platforms.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="image_index-platforms"></a>platforms |  (Optional) list of target platforms to build the manifest for. Uses a split transition. If specified, the 'manifests' attribute should contain exactly one manifest.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="image_index-stamp"></a>stamp |  Enable build stamping for template expansion.<br><br>Controls whether to include volatile build information: - **`auto`** (default): Uses the global stamping configuration - **`enabled`**: Always include stamp information (BUILD_TIMESTAMP, BUILD_USER, etc.) if Bazel's "--stamp" flag is set - **`disabled`**: Never include stamp information<br><br>See [template expansion](/docs/templating.md) for available stamp variables.   | String | optional |  `"auto"`  |


<a id="image_manifest"></a>

## image_manifest

<pre>
load("@rules_img//img:image.bzl", "image_manifest")

image_manifest(<a href="#image_manifest-name">name</a>, <a href="#image_manifest-annotations">annotations</a>, <a href="#image_manifest-base">base</a>, <a href="#image_manifest-build_settings">build_settings</a>, <a href="#image_manifest-cmd">cmd</a>, <a href="#image_manifest-config_fragment">config_fragment</a>, <a href="#image_manifest-entrypoint">entrypoint</a>, <a href="#image_manifest-env">env</a>,
               <a href="#image_manifest-labels">labels</a>, <a href="#image_manifest-layers">layers</a>, <a href="#image_manifest-platform">platform</a>, <a href="#image_manifest-stamp">stamp</a>, <a href="#image_manifest-stop_signal">stop_signal</a>, <a href="#image_manifest-user">user</a>, <a href="#image_manifest-working_dir">working_dir</a>)
</pre>

Builds a single-platform OCI container image from a set of layers.

This rule assembles container images by combining:
- Optional base image layers (from another image_manifest or image_index)
- Additional layers created by image_layer rules
- Image configuration (entrypoint, environment, labels, etc.)

The rule produces:
- OCI manifest and config JSON files
- An optional OCI layout directory or tar (via output groups)
- ImageManifestInfo provider for use by image_index or image_push

Example:

```python
image_manifest(
    name = "my_app",
    base = "@distroless_cc",
    layers = [
        ":app_layer",
        ":config_layer",
    ],
    entrypoint = ["/usr/bin/app"],
    env = {
        "APP_ENV": "production",
    },
)
```

Output groups:
- `descriptor`: OCI descriptor JSON file
- `digest`: Digest of the image (sha256:...)
- `oci_layout`: Complete OCI layout directory with blobs
- `oci_tarball`: OCI layout packaged as a tar file for downstream use

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="image_manifest-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="image_manifest-annotations"></a>annotations |  This field contains arbitrary metadata for the manifest.<br><br>Subject to [template expansion](/docs/templating.md).   | <a href="https://bazel.build/rules/lib/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="image_manifest-base"></a>base |  Base image to inherit layers from. Should provide ImageManifestInfo or ImageIndexInfo.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_manifest-build_settings"></a>build_settings |  Build settings for template expansion.<br><br>Maps template variable names to string_flag targets. These values can be used in env, labels, and annotations attributes using `{{.VARIABLE_NAME}}` syntax (Go template).<br><br>Example: <pre><code class="language-python">build_settings = {&#10;    "REGISTRY": "//settings:docker_registry",&#10;    "VERSION": "//settings:app_version",&#10;}</code></pre><br><br>See [template expansion](/docs/templating.md) for more details.   | Dictionary: String -> Label | optional |  `{}`  |
| <a id="image_manifest-cmd"></a>cmd |  Default arguments to the entrypoint of the container. These values act as defaults and may be replaced by any specified when creating a container. If an Entrypoint value is not specified, then the first entry of the Cmd array SHOULD be interpreted as the executable to run.   | List of strings | optional |  `[]`  |
| <a id="image_manifest-config_fragment"></a>config_fragment |  Optional JSON file containing a partial image config, which will be used as a base for the final image config.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_manifest-entrypoint"></a>entrypoint |  A list of arguments to use as the command to execute when the container starts. These values act as defaults and may be replaced by an entrypoint specified when creating a container.   | List of strings | optional |  `[]`  |
| <a id="image_manifest-env"></a>env |  Default environment variables to set when starting a container based on this image.<br><br>Subject to [template expansion](/docs/templating.md).   | <a href="https://bazel.build/rules/lib/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="image_manifest-labels"></a>labels |  This field contains arbitrary metadata for the container.<br><br>Subject to [template expansion](/docs/templating.md).   | <a href="https://bazel.build/rules/lib/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="image_manifest-layers"></a>layers |  Layers to include in the image. Either a LayerInfo provider, a LayerGroupInfo provider, or a DefaultInfo with tar files.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="image_manifest-platform"></a>platform |  Dict containing additional runtime requirements of the image.   | <a href="https://bazel.build/rules/lib/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="image_manifest-stamp"></a>stamp |  Enable build stamping for template expansion.<br><br>Controls whether to include volatile build information: - **`auto`** (default): Uses the global stamping configuration - **`enabled`**: Always include stamp information (BUILD_TIMESTAMP, BUILD_USER, etc.) if Bazel's "--stamp" flag is set - **`disabled`**: Never include stamp information<br><br>See [template expansion](/docs/templating.md) for available stamp variables.   | String | optional |  `"auto"`  |
| <a id="image_manifest-stop_signal"></a>stop_signal |  This field contains the system call signal that will be sent to the container to exit. The signal can be a signal name in the format SIGNAME, for instance SIGKILL or SIGRTMIN+3.   | String | optional |  `""`  |
| <a id="image_manifest-user"></a>user |  The username or UID which is a platform-specific structure that allows specific control over which user the process run as. This acts as a default value to use when the value is not specified when creating a container.   | String | optional |  `""`  |
| <a id="image_manifest-working_dir"></a>working_dir |  Sets the current working directory of the entrypoint process in the container. This value acts as a default and may be replaced by a working directory specified when creating a container.   | String | optional |  `""`  |


