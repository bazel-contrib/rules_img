<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Public API for container oras rules.

<a id="oras_file_layer"></a>

## oras_file_layer

<pre>
load("@rules_img//img:oras.bzl", "oras_file_layer")

oras_file_layer(*, <a href="#oras_file_layer-name">name</a>, <a href="#oras_file_layer-src">src</a>, <a href="#oras_file_layer-annotations">annotations</a>, <a href="#oras_file_layer-aspect_hints">aspect_hints</a>, <a href="#oras_file_layer-compatible_with">compatible_with</a>, <a href="#oras_file_layer-deprecation">deprecation</a>,
                <a href="#oras_file_layer-exec_compatible_with">exec_compatible_with</a>, <a href="#oras_file_layer-exec_group_compatible_with">exec_group_compatible_with</a>, <a href="#oras_file_layer-exec_properties">exec_properties</a>, <a href="#oras_file_layer-features">features</a>, <a href="#oras_file_layer-kind">kind</a>,
                <a href="#oras_file_layer-media_type">media_type</a>, <a href="#oras_file_layer-package_metadata">package_metadata</a>, <a href="#oras_file_layer-restricted_to">restricted_to</a>, <a href="#oras_file_layer-tags">tags</a>, <a href="#oras_file_layer-target_compatible_with">target_compatible_with</a>, <a href="#oras_file_layer-testonly">testonly</a>,
                <a href="#oras_file_layer-title">title</a>, <a href="#oras_file_layer-toolchains">toolchains</a>, <a href="#oras_file_layer-visibility">visibility</a>)
</pre>

Creates an ORAS-compatible layer from a single file.

This macro wraps `layer_from_file` and adds standard ORAS annotations so the
resulting layer can be pushed to and pulled from OCI registries using ORAS
tooling. The `org.opencontainers.image.title` annotation is automatically
set to the base name of the source file (or overridden via the `title` attr).

Two modes are supported via the `kind` attribute:

- `"file"` (default): The source file is used as an opaque blob.
- `"directory"`: The source file is treated as a tar archive. ORAS clients
  will unpack it on pull. The `io.deis.oras.content.unpack` and
  `io.deis.oras.content.digest` annotations are set automatically.

Example:

```python
load("@rules_img//img:oras.bzl", "oras_file_layer")

# Push a single file as an ORAS artifact
oras_file_layer(
    name = "readme_layer",
    src = "README.md",
    media_type = "text/plain",
)

# Push a tar archive that ORAS clients will unpack
oras_file_layer(
    name = "docs_layer",
    src = ":docs.tar.gz",
    kind = "directory",
)
```

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="oras_file_layer-name"></a>name |  A unique name for this macro instance. Normally, this is also the name for the macro's main or only target. The names of any other targets that this macro might create will be this name with a string suffix.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="oras_file_layer-src"></a>src |  The file to use as a layer blob.   | <a href="https://bazel.build/concepts/labels">Label</a> | required |  |
| <a id="oras_file_layer-annotations"></a>annotations |  Annotations to add to the layer metadata as key-value pairs.   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a>; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `{}`  |
| <a id="oras_file_layer-aspect_hints"></a>aspect_hints |  <a href="https://bazel.build/reference/be/common-definitions#common.aspect_hints">Inherited rule attribute</a>   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `None`  |
| <a id="oras_file_layer-compatible_with"></a>compatible_with |  <a href="https://bazel.build/reference/be/common-definitions#common.compatible_with">Inherited rule attribute</a>   | <a href="https://bazel.build/concepts/labels">List of labels</a>; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="oras_file_layer-deprecation"></a>deprecation |  <a href="https://bazel.build/reference/be/common-definitions#common.deprecation">Inherited rule attribute</a>   | String; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="oras_file_layer-exec_compatible_with"></a>exec_compatible_with |  <a href="https://bazel.build/reference/be/common-definitions#common.exec_compatible_with">Inherited rule attribute</a>   | <a href="https://bazel.build/concepts/labels">List of labels</a>; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="oras_file_layer-exec_group_compatible_with"></a>exec_group_compatible_with |  <a href="https://bazel.build/reference/be/common-definitions#common.exec_group_compatible_with">Inherited rule attribute</a>   | Dictionary: String -> List of labels; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="oras_file_layer-exec_properties"></a>exec_properties |  <a href="https://bazel.build/reference/be/common-definitions#common.exec_properties">Inherited rule attribute</a>   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `None`  |
| <a id="oras_file_layer-features"></a>features |  <a href="https://bazel.build/reference/be/common-definitions#common.features">Inherited rule attribute</a>   | List of strings | optional |  `None`  |
| <a id="oras_file_layer-kind"></a>kind |  The kind of layer. "file" interprets the layer as-is, "directory" interprets the layer blob as a tar file containing a tree.   | String; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `"file"`  |
| <a id="oras_file_layer-media_type"></a>media_type |  Layer media type. Defaults to "application/vnd.oci.image.layer.v1.tar" if not set.   | String | optional |  `None`  |
| <a id="oras_file_layer-package_metadata"></a>package_metadata |  <a href="https://bazel.build/reference/be/common-definitions#common.package_metadata">Inherited rule attribute</a>   | <a href="https://bazel.build/concepts/labels">List of labels</a>; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="oras_file_layer-restricted_to"></a>restricted_to |  <a href="https://bazel.build/reference/be/common-definitions#common.restricted_to">Inherited rule attribute</a>   | <a href="https://bazel.build/concepts/labels">List of labels</a>; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="oras_file_layer-tags"></a>tags |  <a href="https://bazel.build/reference/be/common-definitions#common.tags">Inherited rule attribute</a>   | List of strings; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="oras_file_layer-target_compatible_with"></a>target_compatible_with |  <a href="https://bazel.build/reference/be/common-definitions#common.target_compatible_with">Inherited rule attribute</a>   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `None`  |
| <a id="oras_file_layer-testonly"></a>testonly |  <a href="https://bazel.build/reference/be/common-definitions#common.testonly">Inherited rule attribute</a>   | Boolean; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="oras_file_layer-title"></a>title |  Optional override for org.opencontainers.image.title. If left empty, the title is derived from the base name of the blob.   | String; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `""`  |
| <a id="oras_file_layer-toolchains"></a>toolchains |  <a href="https://bazel.build/reference/be/common-definitions#common.toolchains">Inherited rule attribute</a>   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `None`  |
| <a id="oras_file_layer-visibility"></a>visibility |  The visibility to be passed to this macro's exported targets. It always implicitly includes the location where this macro is instantiated, so this attribute only needs to be explicitly set if you want the macro's targets to be additionally visible somewhere else.   | <a href="https://bazel.build/concepts/labels">List of labels</a>; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  |


<a id="oras_layer"></a>

## oras_layer

<pre>
load("@rules_img//img:oras.bzl", "oras_layer")

oras_layer(*, <a href="#oras_layer-name">name</a>, <a href="#oras_layer-srcs">srcs</a>, <a href="#oras_layer-annotations">annotations</a>, <a href="#oras_layer-annotations_file">annotations_file</a>, <a href="#oras_layer-aspect_hints">aspect_hints</a>, <a href="#oras_layer-compatible_with">compatible_with</a>, <a href="#oras_layer-compress">compress</a>,
           <a href="#oras_layer-create_parent_directories">create_parent_directories</a>, <a href="#oras_layer-default_metadata">default_metadata</a>, <a href="#oras_layer-deprecation">deprecation</a>, <a href="#oras_layer-estargz">estargz</a>, <a href="#oras_layer-exec_compatible_with">exec_compatible_with</a>,
           <a href="#oras_layer-exec_group_compatible_with">exec_group_compatible_with</a>, <a href="#oras_layer-exec_properties">exec_properties</a>, <a href="#oras_layer-features">features</a>, <a href="#oras_layer-file_metadata">file_metadata</a>, <a href="#oras_layer-include_runfiles">include_runfiles</a>,
           <a href="#oras_layer-media_type">media_type</a>, <a href="#oras_layer-package_metadata">package_metadata</a>, <a href="#oras_layer-restricted_to">restricted_to</a>, <a href="#oras_layer-symlinks">symlinks</a>, <a href="#oras_layer-tags">tags</a>, <a href="#oras_layer-target_compatible_with">target_compatible_with</a>,
           <a href="#oras_layer-testonly">testonly</a>, <a href="#oras_layer-title">title</a>, <a href="#oras_layer-toolchains">toolchains</a>, <a href="#oras_layer-tree_artifact_handling">tree_artifact_handling</a>, <a href="#oras_layer-visibility">visibility</a>)
</pre>

Creates an ORAS-compatible layer from files and directories.

This macro wraps `image_layer` and adds standard ORAS annotations so the
resulting layer can be pushed to and pulled from OCI registries using ORAS
tooling:

- `org.opencontainers.image.title` is set to the `title` attribute (defaults
  to the target name).
- `io.deis.oras.content.unpack` is set to `"true"` so ORAS clients unpack the
  layer on pull.
- `io.deis.oras.content.digest` is automatically derived from the layer's diff
  ID after the tar is produced.

Example:

```python
load("@rules_img//img:oras.bzl", "oras_layer")

# Package application files as an ORAS artifact
oras_layer(
    name = "app_layer",
    srcs = {
        "/app/bin/server": "//cmd/server",
        "/app/config.json": ":config",
    },
)
```

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="oras_layer-name"></a>name |  A unique name for this macro instance. Normally, this is also the name for the macro's main or only target. The names of any other targets that this macro might create will be this name with a string suffix.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="oras_layer-srcs"></a>srcs |  Files to include in the layer. Keys are paths in the image (e.g., "/app/bin/server"), values are labels to files or executables. Executables automatically include their runfiles unless include_runfiles is set to False.   | Dictionary: String -> Label | optional |  `None`  |
| <a id="oras_layer-annotations"></a>annotations |  Annotations to add to the layer metadata as key-value pairs.   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a>; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `{}`  |
| <a id="oras_layer-annotations_file"></a>annotations_file |  File containing newline-delimited KEY=VALUE annotations for the layer.<br><br>The file should contain one annotation per line in KEY=VALUE format. Empty lines are ignored. Annotations from this file are merged with annotations specified via the `annotations` attribute.<br><br>Example file content: <pre><code>version=1.0.0&#10;build.date=2024-01-15&#10;source.url=https://github.com/...</code></pre>   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="oras_layer-aspect_hints"></a>aspect_hints |  <a href="https://bazel.build/reference/be/common-definitions#common.aspect_hints">Inherited rule attribute</a>   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `None`  |
| <a id="oras_layer-compatible_with"></a>compatible_with |  <a href="https://bazel.build/reference/be/common-definitions#common.compatible_with">Inherited rule attribute</a>   | <a href="https://bazel.build/concepts/labels">List of labels</a>; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="oras_layer-compress"></a>compress |  Compression algorithm to use. If set to 'auto', uses the global default compression setting.   | String | optional |  `None`  |
| <a id="oras_layer-create_parent_directories"></a>create_parent_directories |  Whether to automatically create parent directory entries in the tar file for all files. If set to 'auto', uses the global default create_parent_directories setting. When enabled, parent directories will be created automatically for all files in the layer.   | String | optional |  `None`  |
| <a id="oras_layer-default_metadata"></a>default_metadata |  JSON-encoded default metadata to apply to all files in the layer. Can include fields like mode, uid, gid, uname, gname, mtime, and pax_records.   | String | optional |  `None`  |
| <a id="oras_layer-deprecation"></a>deprecation |  <a href="https://bazel.build/reference/be/common-definitions#common.deprecation">Inherited rule attribute</a>   | String; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="oras_layer-estargz"></a>estargz |  Whether to use estargz format. If set to 'auto', uses the global default estargz setting. When enabled, the layer will be optimized for lazy pulling and will be compatible with the estargz format.   | String | optional |  `None`  |
| <a id="oras_layer-exec_compatible_with"></a>exec_compatible_with |  <a href="https://bazel.build/reference/be/common-definitions#common.exec_compatible_with">Inherited rule attribute</a>   | <a href="https://bazel.build/concepts/labels">List of labels</a>; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="oras_layer-exec_group_compatible_with"></a>exec_group_compatible_with |  <a href="https://bazel.build/reference/be/common-definitions#common.exec_group_compatible_with">Inherited rule attribute</a>   | Dictionary: String -> List of labels; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="oras_layer-exec_properties"></a>exec_properties |  <a href="https://bazel.build/reference/be/common-definitions#common.exec_properties">Inherited rule attribute</a>   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `None`  |
| <a id="oras_layer-features"></a>features |  <a href="https://bazel.build/reference/be/common-definitions#common.features">Inherited rule attribute</a>   | List of strings | optional |  `None`  |
| <a id="oras_layer-file_metadata"></a>file_metadata |  Per-file metadata overrides as a dict mapping file paths to JSON-encoded metadata. The path should match the path in the image (the key in srcs attribute). Metadata specified here overrides any defaults from default_metadata.   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `None`  |
| <a id="oras_layer-include_runfiles"></a>include_runfiles |  Whether to include runfiles for executable targets. When True (default), executables in srcs will include their runfiles tree. When False, only the executable file itself is included, without runfiles.   | Boolean | optional |  `None`  |
| <a id="oras_layer-media_type"></a>media_type |  Override the layer media type. By default, the media type is auto-detected from the compression algorithm.   | String | optional |  `None`  |
| <a id="oras_layer-package_metadata"></a>package_metadata |  <a href="https://bazel.build/reference/be/common-definitions#common.package_metadata">Inherited rule attribute</a>   | <a href="https://bazel.build/concepts/labels">List of labels</a>; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="oras_layer-restricted_to"></a>restricted_to |  <a href="https://bazel.build/reference/be/common-definitions#common.restricted_to">Inherited rule attribute</a>   | <a href="https://bazel.build/concepts/labels">List of labels</a>; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="oras_layer-symlinks"></a>symlinks |  Symlinks to create in the layer. Keys are symlink paths in the image, values are the targets they point to.   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `None`  |
| <a id="oras_layer-tags"></a>tags |  <a href="https://bazel.build/reference/be/common-definitions#common.tags">Inherited rule attribute</a>   | List of strings; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="oras_layer-target_compatible_with"></a>target_compatible_with |  <a href="https://bazel.build/reference/be/common-definitions#common.target_compatible_with">Inherited rule attribute</a>   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `None`  |
| <a id="oras_layer-testonly"></a>testonly |  <a href="https://bazel.build/reference/be/common-definitions#common.testonly">Inherited rule attribute</a>   | Boolean; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `None`  |
| <a id="oras_layer-title"></a>title |  Optional value for org.opencontainers.image.title. If left empty, the title is derived from the target name.   | String; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  `""`  |
| <a id="oras_layer-toolchains"></a>toolchains |  <a href="https://bazel.build/reference/be/common-definitions#common.toolchains">Inherited rule attribute</a>   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `None`  |
| <a id="oras_layer-tree_artifact_handling"></a>tree_artifact_handling |  How to handle duplicate tree artifacts (directories) in the layer. If set to 'full', each tree artifact is stored at its intended path (no deduplication). If set to 'deduplicate_symlink', duplicate tree artifacts are replaced with symlinks to the first occurrence. If set to 'auto', uses the global default from --@rules_img//img/settings:layer_tree_artifact_handling.   | String | optional |  `None`  |
| <a id="oras_layer-visibility"></a>visibility |  The visibility to be passed to this macro's exported targets. It always implicitly includes the location where this macro is instantiated, so this attribute only needs to be explicitly set if you want the macro's targets to be additionally visible somewhere else.   | <a href="https://bazel.build/concepts/labels">List of labels</a>; <a href="https://bazel.build/reference/be/common-definitions#configurable-attributes">nonconfigurable</a> | optional |  |


