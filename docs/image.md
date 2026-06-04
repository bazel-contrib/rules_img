<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Rules to build container images from layers.

Use `image_from_binary` to package any `*_binary` target into a container image,
`image_manifest` to create a single-platform container image from layers,
and `image_index` to compose a multi-platform container image index.

<a id="image_index"></a>

## image_index

<pre>
load("@rules_img//img:image.bzl", "image_index")

image_index(<a href="#image_index-name">name</a>, <a href="#image_index-annotations">annotations</a>, <a href="#image_index-annotations_file">annotations_file</a>, <a href="#image_index-build_settings">build_settings</a>, <a href="#image_index-load_specs">load_specs</a>, <a href="#image_index-manifests">manifests</a>, <a href="#image_index-platforms">platforms</a>,
            <a href="#image_index-push_specs">push_specs</a>, <a href="#image_index-stamp">stamp</a>, <a href="#image_index-subject">subject</a>)
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
- `root_blob`: The index JSON blob file
- `oci_layout`: Complete OCI layout directory with all platform blobs
- `oci_tarball`: OCI layout packaged as a tar file for downstream use
- `sparse_oci_layout`: Sparse OCI layout directory (without layer blobs, only layer descriptors)

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="image_index-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="image_index-annotations"></a>annotations |  Arbitrary metadata for the image index.<br><br>Subject to [template expansion](/docs/templating.md).   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="image_index-annotations_file"></a>annotations_file |  File containing newline-delimited KEY=VALUE annotations for the image index.<br><br>The file should contain one annotation per line in KEY=VALUE format. Empty lines are ignored. Annotations from this file are merged with annotations specified via the `annotations` attribute.<br><br>Example file content: <pre><code>version=1.0.0&#10;build.date=2024-01-15&#10;source.url=https://github.com/...</code></pre><br><br>Each annotation is subject to [template expansion](/docs/templating.md).   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_index-build_settings"></a>build_settings |  Build settings for template expansion.<br><br>Maps template variable names to string_flag targets. These values can be used in the annotations attribute using `{{.VARIABLE_NAME}}` syntax (Go template).<br><br>Example: <pre><code class="language-python">build_settings = {&#10;    "REGISTRY": "//settings:docker_registry",&#10;    "VERSION": "//settings:app_version",&#10;}</code></pre><br><br>See [template expansion](/docs/templating.md) for more details.   | Dictionary: String -> Label | optional |  `{}`  |
| <a id="image_index-load_specs"></a>load_specs |  Load configurations to produce DeployInfo for this image index.<br><br>Each entry should be an `image_load_spec` target (providing `LoadConfigInfo`). When set (together with or without `push_specs`), this rule additionally returns `DeployInfo`, making it directly usable as an operation in `multi_deploy`.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="image_index-manifests"></a>manifests |  List of manifests for specific platforms.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="image_index-platforms"></a>platforms |  (Optional) list of target platforms to build the manifest for. Uses a split transition. If specified, the 'manifests' attribute should contain exactly one manifest.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="image_index-push_specs"></a>push_specs |  Push configurations to produce DeployInfo for this image index.<br><br>Each entry should be an `image_push_spec` target (providing `PushConfigInfo`). When set (together with or without `load_specs`), this rule additionally returns `DeployInfo`, making it directly usable as an operation in `multi_deploy`.<br><br>For multi-platform pushes, `manifest_tags` on the push spec are expanded per child manifest with platform variables (`{{.os}}`, `{{.architecture}}`, etc.).   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="image_index-stamp"></a>stamp |  Controls build stamping for template expansion.<br><br>- **`auto`** (default): Defers to the global `--@rules_img//img/settings:stamp` setting. - **`force`**: Always stamp if templates contain `{{}}` placeholders, ignoring Bazel's `--stamp` flag. - **`disabled`**: Never include stamp information.<br><br>See [template expansion](/docs/templating.md) for available stamp variables.   | String | optional |  `"auto"`  |
| <a id="image_index-subject"></a>subject |  Optional subject for the index.<br><br>Sets the `subject` field in the OCI index, which is a descriptor pointing to another manifest or index. This is used for establishing referrer relationships, such as attaching SBOMs, signatures, or attestations to an existing image.<br><br>The target must provide either ImageManifestInfo or ImageIndexInfo.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |


<a id="image_manifest"></a>

## image_manifest

<pre>
load("@rules_img//img:image.bzl", "image_manifest")

image_manifest(<a href="#image_manifest-name">name</a>, <a href="#image_manifest-annotations">annotations</a>, <a href="#image_manifest-annotations_file">annotations_file</a>, <a href="#image_manifest-artifact_type">artifact_type</a>, <a href="#image_manifest-base">base</a>, <a href="#image_manifest-build_settings">build_settings</a>, <a href="#image_manifest-cmd">cmd</a>,
               <a href="#image_manifest-config_fragment">config_fragment</a>, <a href="#image_manifest-config_media_type">config_media_type</a>, <a href="#image_manifest-created">created</a>, <a href="#image_manifest-entrypoint">entrypoint</a>, <a href="#image_manifest-env">env</a>, <a href="#image_manifest-env_file">env_file</a>, <a href="#image_manifest-label_files">label_files</a>,
               <a href="#image_manifest-labels">labels</a>, <a href="#image_manifest-layers">layers</a>, <a href="#image_manifest-load_specs">load_specs</a>, <a href="#image_manifest-platform">platform</a>, <a href="#image_manifest-push_specs">push_specs</a>, <a href="#image_manifest-stamp">stamp</a>, <a href="#image_manifest-stop_signal">stop_signal</a>, <a href="#image_manifest-subject">subject</a>, <a href="#image_manifest-user">user</a>,
               <a href="#image_manifest-working_dir">working_dir</a>)
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
- `root_blob`: The manifest JSON blob file
- `oci_layout`: Complete OCI layout directory with blobs
- `oci_tarball`: OCI layout packaged as a tar file for downstream use
- `sparse_oci_layout`: Sparse OCI layout directory (without layer blobs, only layer descriptors)

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="image_manifest-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="image_manifest-annotations"></a>annotations |  This field contains arbitrary metadata for the manifest.<br><br>Subject to [template expansion](/docs/templating.md).   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="image_manifest-annotations_file"></a>annotations_file |  File containing newline-delimited KEY=VALUE annotations for the manifest.<br><br>The file should contain one annotation per line in KEY=VALUE format. Empty lines are ignored. Annotations from this file are merged with annotations specified via the `annotations` attribute.<br><br>Example file content: <pre><code>version=1.0.0&#10;build.date=2024-01-15&#10;source.url=https://github.com/...</code></pre><br><br>Each annotation is subject to [template expansion](/docs/templating.md).   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_manifest-artifact_type"></a>artifact_type |  Optional IANA media type of the artifact when the manifest is used for an artifact.<br><br>This sets the `artifactType` field in the OCI manifest, as defined in the [OCI Image Spec](https://github.com/opencontainers/image-spec/blob/main/manifest.md#image-manifest-property-descriptions).<br><br>Common values include `application/vnd.cncf.helm.chart.v1` for Helm charts or `application/spdx+json` for SPDX SBOMs.   | String | optional |  `""`  |
| <a id="image_manifest-base"></a>base |  Base image to inherit layers from. Should provide ImageManifestInfo or ImageIndexInfo.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_manifest-build_settings"></a>build_settings |  Build settings for template expansion.<br><br>Maps template variable names to string_flag targets. These values can be used in env, labels, and annotations attributes using `{{.VARIABLE_NAME}}` syntax (Go template).<br><br>Example: <pre><code class="language-python">build_settings = {&#10;    "REGISTRY": "//settings:docker_registry",&#10;    "VERSION": "//settings:app_version",&#10;}</code></pre><br><br>See [template expansion](/docs/templating.md) for more details.   | Dictionary: String -> Label | optional |  `{}`  |
| <a id="image_manifest-cmd"></a>cmd |  Default arguments to the entrypoint of the container. These values act as defaults and may be replaced by any specified when creating a container. If an Entrypoint value is not specified, then the first entry of the Cmd array SHOULD be interpreted as the executable to run.   | List of strings | optional |  `[]`  |
| <a id="image_manifest-config_fragment"></a>config_fragment |  Optional JSON file containing a partial OCI image config, which will be used as a base for the final image config.<br><br>For OCI image configuration fields such as exposed ports or volumes, the JSON should use the top-level `config` object:<br><br><pre><code class="language-json">{&#10;  "config": {&#10;    "ExposedPorts": {&#10;      "8080/tcp": {}&#10;    }&#10;  }&#10;}</code></pre><br><br>When config_media_type is set to a non-OCI type (e.g. Helm), this file is used as the entire config blob as-is.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_manifest-config_media_type"></a>config_media_type |  Override the config blob media type.<br><br>When set to "application/vnd.oci.empty.v1+json", config_fragment is optional. If omitted, an empty JSON config descriptor is produced automatically with the content inlined as data (`"data": "e30="`).<br><br>For other non-OCI types (e.g. "application/vnd.cncf.helm.config.v1+json" for Helm charts), config_fragment is required and used verbatim as the config blob (no OCI image structure).   | String | optional |  `""`  |
| <a id="image_manifest-created"></a>created |  Optional file containing a datetime string (RFC 3339 format) for when the image was created.<br><br>This is commonly used with Bazel's stamping mechanism to embed the build timestamp.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_manifest-entrypoint"></a>entrypoint |  A list of arguments to use as the command to execute when the container starts. These values act as defaults and may be replaced by an entrypoint specified when creating a container.   | List of strings | optional |  `[]`  |
| <a id="image_manifest-env"></a>env |  Default environment variables to set when starting a container based on this image.<br><br>Subject to [template expansion](/docs/templating.md).   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="image_manifest-env_file"></a>env_file |  File containing newline-delimited KEY=VALUE enviroment variables to set when starting a container based on this image.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_manifest-label_files"></a>label_files |  Files containing newline-delimited KEY=VALUE labels for the image config.<br><br>Each file should contain one label per line in KEY=VALUE format. Empty lines are ignored. Labels from these files are merged together, and then merged with labels specified via the `labels` attribute. Values from files take precedence over the `labels` attribute for matching keys.<br><br>Example file content: <pre><code>org.opencontainers.image.version=1.0.0&#10;org.opencontainers.image.authors=team@example.com</code></pre><br><br>Each label value is subject to [template expansion](/docs/templating.md).   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="image_manifest-labels"></a>labels |  This field contains arbitrary metadata for the container.<br><br>Subject to [template expansion](/docs/templating.md).   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="image_manifest-layers"></a>layers |  Layers to include in the image. Either a LayersInfo provider or a DefaultInfo with tar files.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="image_manifest-load_specs"></a>load_specs |  Load configurations to produce DeployInfo for this image.<br><br>Each entry should be an `image_load_spec` target (providing `LoadConfigInfo`). When set (together with or without `push_specs`), this rule additionally returns `DeployInfo`, making it directly usable as an operation in `multi_deploy`.<br><br>Example: <pre><code class="language-python">image_load_spec(&#10;    name = "load_config",&#10;    tag = "my-app:latest",&#10;)&#10;&#10;image_manifest(&#10;    name = "my_app",&#10;    base = "@distroless_cc",&#10;    layers = [":app_layer"],&#10;    load_specs = [":load_config"],&#10;)&#10;&#10;multi_deploy(&#10;    name = "deploy",&#10;    operations = [":my_app"],&#10;)</code></pre>   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="image_manifest-platform"></a>platform |  Optional target platform to build this manifest for.<br><br>When specified, the image will be built for the provided platform regardless of the current build configuration. This enables building single-platform images for specific architectures.<br><br>Example: <pre><code class="language-python">image_manifest(&#10;    name = "app_arm64",&#10;    platform = "//platforms:linux_arm64",&#10;    base = "@ubuntu",&#10;    layers = [":app_layer"],&#10;)</code></pre>   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_manifest-push_specs"></a>push_specs |  Push configurations to produce DeployInfo for this image.<br><br>Each entry should be an `image_push_spec` target (providing `PushConfigInfo`). When set (together with or without `load_specs`), this rule additionally returns `DeployInfo`, making it directly usable as an operation in `multi_deploy`.<br><br>Example: <pre><code class="language-python">image_push_spec(&#10;    name = "push_config",&#10;    registry = "gcr.io",&#10;    repository = "my-project/my-app",&#10;    tag = "latest",&#10;)&#10;&#10;image_manifest(&#10;    name = "my_app",&#10;    base = "@distroless_cc",&#10;    layers = [":app_layer"],&#10;    push_specs = [":push_config"],&#10;)&#10;&#10;multi_deploy(&#10;    name = "deploy",&#10;    operations = [":my_app"],&#10;)</code></pre>   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="image_manifest-stamp"></a>stamp |  Controls build stamping for template expansion.<br><br>- **`auto`** (default): Defers to the global `--@rules_img//img/settings:stamp` setting. - **`force`**: Always stamp if templates contain `{{}}` placeholders, ignoring Bazel's `--stamp` flag. - **`disabled`**: Never include stamp information.<br><br>See [template expansion](/docs/templating.md) for available stamp variables.   | String | optional |  `"auto"`  |
| <a id="image_manifest-stop_signal"></a>stop_signal |  This field contains the system call signal that will be sent to the container to exit. The signal can be a signal name in the format SIGNAME, for instance SIGKILL or SIGRTMIN+3.   | String | optional |  `""`  |
| <a id="image_manifest-subject"></a>subject |  Optional subject for the manifest.<br><br>Sets the `subject` field in the OCI manifest, which is a descriptor pointing to another manifest or index. This is used for establishing referrer relationships, such as attaching SBOMs, signatures, or attestations to an existing image.<br><br>The target must provide either ImageManifestInfo or ImageIndexInfo.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_manifest-user"></a>user |  The username or UID which is a platform-specific structure that allows specific control over which user the process run as. This acts as a default value to use when the value is not specified when creating a container.   | String | optional |  `""`  |
| <a id="image_manifest-working_dir"></a>working_dir |  Sets the current working directory of the entrypoint process in the container. This value acts as a default and may be replaced by a working directory specified when creating a container.   | String | optional |  `""`  |


<a id="image_optimize"></a>

## image_optimize

<pre>
load("@rules_img//img:image.bzl", "image_optimize")

image_optimize(<a href="#image_optimize-name">name</a>, <a href="#image_optimize-compress">compress</a>, <a href="#image_optimize-estargz">estargz</a>, <a href="#image_optimize-image">image</a>)
</pre>

Rewrites every available layer in an image manifest or image index.

This rule applies image-wide layer transformations, such as recompressing every
layer as eStargz. It is intentionally explicit because it requires every input
layer blob to be available to Bazel. Images that were pulled shallowly will fail
analysis instead of downloading missing base-image layers.

Example:

```python
load("@rules_img//img:image.bzl", "image_optimize")

image_optimize(
    name = "base_estargz",
    image = "@ubuntu//:image",
    estargz = "enabled",
)
```

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="image_optimize-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="image_optimize-compress"></a>compress |  Compression algorithm to use for rewritten layers. If set to 'auto', uses the global default compression setting.   | String | optional |  `"auto"`  |
| <a id="image_optimize-estargz"></a>estargz |  Whether to rewrite layers using eStargz. If set to 'auto', uses the global default eStargz setting.   | String | optional |  `"auto"`  |
| <a id="image_optimize-image"></a>image |  Image manifest or image index to optimize. All layer blobs must be available.   | <a href="https://bazel.build/concepts/labels">Label</a> | required |  |


<a id="image_from_binary"></a>

## image_from_binary

<pre>
load("@rules_img//img:image.bzl", "image_from_binary")

image_from_binary(*, <a href="#image_from_binary-name">name</a>, <a href="#image_from_binary-annotations">annotations</a>, <a href="#image_from_binary-annotations_file">annotations_file</a>, <a href="#image_from_binary-artifact_type">artifact_type</a>, <a href="#image_from_binary-aspect_hints">aspect_hints</a>, <a href="#image_from_binary-base">base</a>, <a href="#image_from_binary-binary">binary</a>,
                  <a href="#image_from_binary-build_settings">build_settings</a>, <a href="#image_from_binary-cmd">cmd</a>, <a href="#image_from_binary-compatible_with">compatible_with</a>, <a href="#image_from_binary-config_fragment">config_fragment</a>, <a href="#image_from_binary-config_media_type">config_media_type</a>, <a href="#image_from_binary-created">created</a>,
                  <a href="#image_from_binary-deprecation">deprecation</a>, <a href="#image_from_binary-entrypoint">entrypoint</a>, <a href="#image_from_binary-env">env</a>, <a href="#image_from_binary-env_file">env_file</a>, <a href="#image_from_binary-exec_compatible_with">exec_compatible_with</a>,
                  <a href="#image_from_binary-exec_group_compatible_with">exec_group_compatible_with</a>, <a href="#image_from_binary-exec_properties">exec_properties</a>, <a href="#image_from_binary-features">features</a>, <a href="#image_from_binary-include_runfiles">include_runfiles</a>, <a href="#image_from_binary-kind">kind</a>,
                  <a href="#image_from_binary-label_files">label_files</a>, <a href="#image_from_binary-labels">labels</a>, <a href="#image_from_binary-layer_budget">layer_budget</a>, <a href="#image_from_binary-layers">layers</a>, <a href="#image_from_binary-load_specs">load_specs</a>, <a href="#image_from_binary-package_metadata">package_metadata</a>, <a href="#image_from_binary-path">path</a>,
                  <a href="#image_from_binary-platforms">platforms</a>, <a href="#image_from_binary-push_specs">push_specs</a>, <a href="#image_from_binary-restricted_to">restricted_to</a>, <a href="#image_from_binary-stamp">stamp</a>, <a href="#image_from_binary-stop_signal">stop_signal</a>, <a href="#image_from_binary-subject">subject</a>, <a href="#image_from_binary-tags">tags</a>,
                  <a href="#image_from_binary-target_compatible_with">target_compatible_with</a>, <a href="#image_from_binary-testonly">testonly</a>, <a href="#image_from_binary-toolchains">toolchains</a>, <a href="#image_from_binary-user">user</a>, <a href="#image_from_binary-visibility">visibility</a>, <a href="#image_from_binary-working_dir">working_dir</a>)
</pre>

Packages a *_binary target into a container image.

This is a convenience macro that combines layer_from_binary and image_manifest (or image_index)
into a single target. It is the simplest way to containerize any Bazel `*_binary` target
(Go, C++, Python, Java, Rust, etc.).

The binary's `args`, `env`, and runfiles are automatically extracted and applied to the
image configuration:
- **entrypoint** is set to the binary's path inside the image
- **cmd** is populated from the binary's `args` attribute
- **env** is populated from the binary's `env` attribute (or RunEnvironmentInfo provider)
- **working_dir** is set to the binary's runfiles root

If the binary provides RunfilesGroupInfo (from rules_runfiles_group), the runfiles are split
into separate layers based on the groups. This allows for better caching: stable layers
(interpreter, stdlib) change infrequently and can be shared, while the application code layer
changes with each build. The resolution protocol respects RunfilesGroupTransformInfo and
RunfilesGroupMetadataInfo from the binary's aspect_hints.

All image_manifest attributes (base, env, labels, annotations, etc.) are inherited and
forwarded to the underlying image_manifest. The binary layer is always appended as the
last layer, after any layers specified in the `layers` attribute.

Example:

```python
load("@rules_go//go:def.bzl", "go_binary")
load("@rules_img//img:image.bzl", "image_from_binary")

go_binary(
    name = "server",
    srcs = ["main.go"],
    env = {"GIN_MODE": "release"},
)

# Package the Go binary with a distroless base
image_from_binary(
    name = "app_image",
    binary = ":server",
    base = "@distroless_base",
)

# Custom path and additional layers
image_from_binary(
    name = "full_image",
    binary = "//cmd/server",
    base = "@ubuntu",
    path = "/usr/local/bin/",
    layers = [":config_layer"],
    env = {"LOG_LEVEL": "info"},
)

# Multi-platform image
image_from_binary(
    name = "multiarch_image",
    binary = "//cmd/server",
    base = "@distroless_base",
    platforms = [
        "//:linux_amd64",
        "//:linux_arm64",
    ],
)
```

Targets created:
- `<name>.layer`: The layer_from_binary containing the executable and its runfiles
- `<name>` (or `<name>.manifest` + `<name>` for multi-platform): The image manifest/index

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="image_from_binary-name"></a>name |  A unique name for this macro instance. Normally, this is also the name for the macro's main or only target. The names of any other targets that this macro might create will be this name with a string suffix.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="image_from_binary-annotations"></a>annotations |  This field contains arbitrary metadata for the manifest.<br><br>Subject to [template expansion](/docs/templating.md).   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `None`  |
| <a id="image_from_binary-annotations_file"></a>annotations_file |  File containing newline-delimited KEY=VALUE annotations for the manifest.<br><br>The file should contain one annotation per line in KEY=VALUE format. Empty lines are ignored. Annotations from this file are merged with annotations specified via the `annotations` attribute.<br><br>Example file content: <pre><code>version=1.0.0&#10;build.date=2024-01-15&#10;source.url=https://github.com/...</code></pre><br><br>Each annotation is subject to [template expansion](/docs/templating.md).   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_from_binary-artifact_type"></a>artifact_type |  Optional IANA media type of the artifact when the manifest is used for an artifact.<br><br>This sets the `artifactType` field in the OCI manifest, as defined in the [OCI Image Spec](https://github.com/opencontainers/image-spec/blob/main/manifest.md#image-manifest-property-descriptions).<br><br>Common values include `application/vnd.cncf.helm.chart.v1` for Helm charts or `application/spdx+json` for SPDX SBOMs.   | String | optional |  `None`  |
| <a id="image_from_binary-aspect_hints"></a>aspect_hints |  <a href="https://bazel.build/reference/be/common-definitions#common.aspect_hints">Inherited rule attribute</a>   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `None`  |
| <a id="image_from_binary-base"></a>base |  Base image to inherit layers from. Should provide ImageManifestInfo or ImageIndexInfo.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_from_binary-binary"></a>binary |  The *_binary target to package into the image.<br><br>The binary's `args` and `env` attributes are extracted and applied as image configuration (cmd and env). The `data` attribute is used for `$(location)` expansion in args and env values.<br><br>If the binary provides RunfilesGroupInfo, the runfiles are split into separate layers per group.   | <a href="https://bazel.build/concepts/labels">Label</a> | required |  |
| <a id="image_from_binary-build_settings"></a>build_settings |  Build settings for template expansion.<br><br>Maps template variable names to string_flag targets. These values can be used in env, labels, and annotations attributes using `{{.VARIABLE_NAME}}` syntax (Go template).<br><br>Example: <pre><code class="language-python">build_settings = {&#10;    "REGISTRY": "//settings:docker_registry",&#10;    "VERSION": "//settings:app_version",&#10;}</code></pre><br><br>See [template expansion](/docs/templating.md) for more details.   | Dictionary: String -> Label | optional |  `None`  |
| <a id="image_from_binary-cmd"></a>cmd |  Default arguments to the entrypoint of the container. These values act as defaults and may be replaced by any specified when creating a container. If an Entrypoint value is not specified, then the first entry of the Cmd array SHOULD be interpreted as the executable to run.   | List of strings | optional |  `None`  |
| <a id="image_from_binary-compatible_with"></a>compatible_with |  <a href="https://bazel.build/reference/be/common-definitions#common.compatible_with">Inherited rule attribute</a>   | <a href="https://bazel.build/concepts/labels">List of labels</a>; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="image_from_binary-config_fragment"></a>config_fragment |  Optional JSON file containing a partial OCI image config, which will be used as a base for the final image config.<br><br>For OCI image configuration fields such as exposed ports or volumes, the JSON should use the top-level `config` object:<br><br><pre><code class="language-json">{&#10;  "config": {&#10;    "ExposedPorts": {&#10;      "8080/tcp": {}&#10;    }&#10;  }&#10;}</code></pre><br><br>When config_media_type is set to a non-OCI type (e.g. Helm), this file is used as the entire config blob as-is.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_from_binary-config_media_type"></a>config_media_type |  Override the config blob media type.<br><br>When set to "application/vnd.oci.empty.v1+json", config_fragment is optional. If omitted, an empty JSON config descriptor is produced automatically with the content inlined as data (`"data": "e30="`).<br><br>For other non-OCI types (e.g. "application/vnd.cncf.helm.config.v1+json" for Helm charts), config_fragment is required and used verbatim as the config blob (no OCI image structure).   | String | optional |  `None`  |
| <a id="image_from_binary-created"></a>created |  Optional file containing a datetime string (RFC 3339 format) for when the image was created.<br><br>This is commonly used with Bazel's stamping mechanism to embed the build timestamp.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_from_binary-deprecation"></a>deprecation |  <a href="https://bazel.build/reference/be/common-definitions#common.deprecation">Inherited rule attribute</a>   | String; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="image_from_binary-entrypoint"></a>entrypoint |  A list of arguments to use as the command to execute when the container starts. These values act as defaults and may be replaced by an entrypoint specified when creating a container.   | List of strings | optional |  `None`  |
| <a id="image_from_binary-env"></a>env |  Default environment variables to set when starting a container based on this image.<br><br>Subject to [template expansion](/docs/templating.md).   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `None`  |
| <a id="image_from_binary-env_file"></a>env_file |  File containing newline-delimited KEY=VALUE enviroment variables to set when starting a container based on this image.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_from_binary-exec_compatible_with"></a>exec_compatible_with |  <a href="https://bazel.build/reference/be/common-definitions#common.exec_compatible_with">Inherited rule attribute</a>   | <a href="https://bazel.build/concepts/labels">List of labels</a>; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="image_from_binary-exec_group_compatible_with"></a>exec_group_compatible_with |  <a href="https://bazel.build/reference/be/common-definitions#common.exec_group_compatible_with">Inherited rule attribute</a>   | Dictionary: String -> List of labels; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="image_from_binary-exec_properties"></a>exec_properties |  <a href="https://bazel.build/reference/be/common-definitions#common.exec_properties">Inherited rule attribute</a>   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `None`  |
| <a id="image_from_binary-features"></a>features |  <a href="https://bazel.build/reference/be/common-definitions#common.features">Inherited rule attribute</a>   | List of strings | optional |  `None`  |
| <a id="image_from_binary-include_runfiles"></a>include_runfiles |  Whether to include runfiles for the binary target. When True (default), the binary's runfiles tree is included and the working directory is set to the runfiles root. Set to False for statically linked binaries that don't need runfiles.   | Boolean | optional |  `True`  |
| <a id="image_from_binary-kind"></a>kind |  The kind of image to produce.<br><br>* "auto": Creates a single-platform manifest if zero or one platforms are provided, otherwise creates an index. * "manifest": Always creates a single-platform manifest. Fails if multiple platforms are provided. * "index": Always creates a multi-platform index.   | String; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `"auto"`  |
| <a id="image_from_binary-label_files"></a>label_files |  Files containing newline-delimited KEY=VALUE labels for the image config.<br><br>Each file should contain one label per line in KEY=VALUE format. Empty lines are ignored. Labels from these files are merged together, and then merged with labels specified via the `labels` attribute. Values from files take precedence over the `labels` attribute for matching keys.<br><br>Example file content: <pre><code>org.opencontainers.image.version=1.0.0&#10;org.opencontainers.image.authors=team@example.com</code></pre><br><br>Each label value is subject to [template expansion](/docs/templating.md).   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `None`  |
| <a id="image_from_binary-labels"></a>labels |  This field contains arbitrary metadata for the container.<br><br>Subject to [template expansion](/docs/templating.md).   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `None`  |
| <a id="image_from_binary-layer_budget"></a>layer_budget |  Maximum number of runfiles group layers. If set to a value > 0 and the binary provides RunfilesGroupInfo, groups are merged down to this limit using the merge algorithm from rules_runfiles_group. The algorithm respects group rank (only merges within the same rank), do_not_merge flags, and weight hints (lighter groups merge first). 0 means no limit (all groups become separate layers).   | Integer | optional |  `0`  |
| <a id="image_from_binary-layers"></a>layers |  Additional layers to include in the image. The binary layer is automatically appended to the end of this list.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="image_from_binary-load_specs"></a>load_specs |  Load configurations to produce DeployInfo for this image.<br><br>Each entry should be an `image_load_spec` target (providing `LoadConfigInfo`). When set (together with or without `push_specs`), this rule additionally returns `DeployInfo`, making it directly usable as an operation in `multi_deploy`.<br><br>Example: <pre><code class="language-python">image_load_spec(&#10;    name = "load_config",&#10;    tag = "my-app:latest",&#10;)&#10;&#10;image_manifest(&#10;    name = "my_app",&#10;    base = "@distroless_cc",&#10;    layers = [":app_layer"],&#10;    load_specs = [":load_config"],&#10;)&#10;&#10;multi_deploy(&#10;    name = "deploy",&#10;    operations = [":my_app"],&#10;)</code></pre>   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `None`  |
| <a id="image_from_binary-package_metadata"></a>package_metadata |  <a href="https://bazel.build/reference/be/common-definitions#common.package_metadata">Inherited rule attribute</a>   | <a href="https://bazel.build/concepts/labels">List of labels</a>; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="image_from_binary-path"></a>path |  Optional path of the binary inside the image. If the path ends with a slash ("/"), the basename of the binary will be automatically appended. If unset, this defaults to the rlocationpath of the binary (e.g., "_main/cmd/server/server_/server").   | String | optional |  `""`  |
| <a id="image_from_binary-platforms"></a>platforms |  Target platforms to build the image for. If empty, the image is built for the current target platform. If more than one platform is provided, an image_index is automatically created.   | <a href="https://bazel.build/concepts/labels">List of labels</a>; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `[]`  |
| <a id="image_from_binary-push_specs"></a>push_specs |  Push configurations to produce DeployInfo for this image.<br><br>Each entry should be an `image_push_spec` target (providing `PushConfigInfo`). When set (together with or without `load_specs`), this rule additionally returns `DeployInfo`, making it directly usable as an operation in `multi_deploy`.<br><br>Example: <pre><code class="language-python">image_push_spec(&#10;    name = "push_config",&#10;    registry = "gcr.io",&#10;    repository = "my-project/my-app",&#10;    tag = "latest",&#10;)&#10;&#10;image_manifest(&#10;    name = "my_app",&#10;    base = "@distroless_cc",&#10;    layers = [":app_layer"],&#10;    push_specs = [":push_config"],&#10;)&#10;&#10;multi_deploy(&#10;    name = "deploy",&#10;    operations = [":my_app"],&#10;)</code></pre>   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `None`  |
| <a id="image_from_binary-restricted_to"></a>restricted_to |  <a href="https://bazel.build/reference/be/common-definitions#common.restricted_to">Inherited rule attribute</a>   | <a href="https://bazel.build/concepts/labels">List of labels</a>; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="image_from_binary-stamp"></a>stamp |  Controls build stamping for template expansion.<br><br>- **`auto`** (default): Defers to the global `--@rules_img//img/settings:stamp` setting. - **`force`**: Always stamp if templates contain `{{}}` placeholders, ignoring Bazel's `--stamp` flag. - **`disabled`**: Never include stamp information.<br><br>See [template expansion](/docs/templating.md) for available stamp variables.   | String | optional |  `None`  |
| <a id="image_from_binary-stop_signal"></a>stop_signal |  This field contains the system call signal that will be sent to the container to exit. The signal can be a signal name in the format SIGNAME, for instance SIGKILL or SIGRTMIN+3.   | String | optional |  `None`  |
| <a id="image_from_binary-subject"></a>subject |  Optional subject for the manifest.<br><br>Sets the `subject` field in the OCI manifest, which is a descriptor pointing to another manifest or index. This is used for establishing referrer relationships, such as attaching SBOMs, signatures, or attestations to an existing image.<br><br>The target must provide either ImageManifestInfo or ImageIndexInfo.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_from_binary-tags"></a>tags |  <a href="https://bazel.build/reference/be/common-definitions#common.tags">Inherited rule attribute</a>   | List of strings; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="image_from_binary-target_compatible_with"></a>target_compatible_with |  <a href="https://bazel.build/reference/be/common-definitions#common.target_compatible_with">Inherited rule attribute</a>   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `None`  |
| <a id="image_from_binary-testonly"></a>testonly |  <a href="https://bazel.build/reference/be/common-definitions#common.testonly">Inherited rule attribute</a>   | Boolean; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="image_from_binary-toolchains"></a>toolchains |  <a href="https://bazel.build/reference/be/common-definitions#common.toolchains">Inherited rule attribute</a>   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `None`  |
| <a id="image_from_binary-user"></a>user |  The username or UID which is a platform-specific structure that allows specific control over which user the process run as. This acts as a default value to use when the value is not specified when creating a container.   | String | optional |  `None`  |
| <a id="image_from_binary-visibility"></a>visibility |  The visibility to be passed to this macro's exported targets. It always implicitly includes the location where this macro is instantiated, so this attribute only needs to be explicitly set if you want the macro's targets to be additionally visible somewhere else.   | <a href="https://bazel.build/concepts/labels">List of labels</a>; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  |
| <a id="image_from_binary-working_dir"></a>working_dir |  Sets the current working directory of the entrypoint process in the container. This value acts as a default and may be replaced by a working directory specified when creating a container.   | String | optional |  `None`  |


