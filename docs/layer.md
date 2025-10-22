<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Public API for container image layer rules.

<a id="image_layer"></a>

## image_layer

<pre>
load("@rules_img//img:layer.bzl", "image_layer")

image_layer(<a href="#image_layer-name">name</a>, <a href="#image_layer-srcs">srcs</a>, <a href="#image_layer-annotations">annotations</a>, <a href="#image_layer-compress">compress</a>, <a href="#image_layer-default_metadata">default_metadata</a>, <a href="#image_layer-estargz">estargz</a>, <a href="#image_layer-file_metadata">file_metadata</a>, <a href="#image_layer-symlinks">symlinks</a>)
</pre>

Creates a container image layer from files, executables, and directories.

This rule packages files into a layer that can be used in container images. It supports:
- Adding files at specific paths in the image
- Setting file permissions and ownership
- Creating symlinks
- Including executables with their runfiles
- Compression (gzip, zstd) and eStargz optimization

Example:

```python
load("@rules_img//img:layer.bzl", "image_layer", "file_metadata")

# Simple layer with files
image_layer(
    name = "app_layer",
    srcs = {
        "/app/bin/server": "//cmd/server",
        "/app/config.json": ":config.json",
    },
)

# Layer with custom permissions
image_layer(
    name = "secure_layer",
    srcs = {
        "/etc/app/config": ":config",
        "/etc/app/secret": ":secret",
    },
    default_metadata = file_metadata(
        mode = "0644",
        uid = 1000,
        gid = 1000,
    ),
    file_metadata = {
        "/etc/app/secret": file_metadata(mode = "0600"),
    },
)

# Layer with symlinks
image_layer(
    name = "bin_layer",
    srcs = {
        "/usr/local/bin/app": "//cmd/app",
    },
    symlinks = {
        "/usr/bin/app": "/usr/local/bin/app",
    },
)
```

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="image_layer-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="image_layer-srcs"></a>srcs |  Files to include in the layer. Keys are paths in the image (e.g., "/app/bin/server"), values are labels to files or executables. Executables automatically include their runfiles.   | Dictionary: String -> Label | optional |  `{}`  |
| <a id="image_layer-annotations"></a>annotations |  Annotations to add to the layer metadata as key-value pairs.   | <a href="https://bazel.build/rules/lib/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="image_layer-compress"></a>compress |  Compression algorithm to use. If set to 'auto', uses the global default compression setting.   | String | optional |  `"auto"`  |
| <a id="image_layer-default_metadata"></a>default_metadata |  JSON-encoded default metadata to apply to all files in the layer. Can include fields like mode, uid, gid, uname, gname, mtime, and pax_records.   | String | optional |  `""`  |
| <a id="image_layer-estargz"></a>estargz |  Whether to use estargz format. If set to 'auto', uses the global default estargz setting. When enabled, the layer will be optimized for lazy pulling and will be compatible with the estargz format.   | String | optional |  `"auto"`  |
| <a id="image_layer-file_metadata"></a>file_metadata |  Per-file metadata overrides as a dict mapping file paths to JSON-encoded metadata. The path should match the path in the image (the key in srcs attribute). Metadata specified here overrides any defaults from default_metadata.   | <a href="https://bazel.build/rules/lib/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="image_layer-symlinks"></a>symlinks |  Symlinks to create in the layer. Keys are symlink paths in the image, values are the targets they point to.   | <a href="https://bazel.build/rules/lib/dict">Dictionary: String -> String</a> | optional |  `{}`  |


<a id="layer_from_tar"></a>

## layer_from_tar

<pre>
load("@rules_img//img:layer.bzl", "layer_from_tar")

layer_from_tar(<a href="#layer_from_tar-name">name</a>, <a href="#layer_from_tar-src">src</a>, <a href="#layer_from_tar-annotations">annotations</a>, <a href="#layer_from_tar-compress">compress</a>, <a href="#layer_from_tar-estargz">estargz</a>, <a href="#layer_from_tar-optimize">optimize</a>)
</pre>

Creates a container image layer from an existing tar archive.

This rule converts tar files into container image layers, useful for incorporating
pre-built artifacts, third-party distributions, or legacy build outputs.

The rule can:
- Use tar files as-is or recompress them
- Optimize tar contents by deduplicating files
- Add annotations to the layer metadata

Example:

```python
load("@rules_img//img:layer.bzl", "layer_from_tar")

# Use an existing tar file as a layer
layer_from_tar(
    name = "third_party_layer",
    src = "@third_party_lib//:lib.tar.gz",
)

# Optimize and recompress
layer_from_tar(
    name = "optimized_layer",
    src = "//legacy:build_output.tar",
    optimize = True,  # Deduplicate contents
    compress = "zstd",  # Use zstd compression
)

# Add metadata annotations
layer_from_tar(
    name = "annotated_layer",
    src = "//vendor:dependencies.tar.gz",
    annotations = {
        "org.opencontainers.image.title": "Vendor Dependencies",
        "org.opencontainers.image.version": "1.2.3",
    },
)
```

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="layer_from_tar-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="layer_from_tar-src"></a>src |  The tar file to convert into a layer. Must be a valid tar file (optionally compressed).   | <a href="https://bazel.build/concepts/labels">Label</a> | required |  |
| <a id="layer_from_tar-annotations"></a>annotations |  Annotations to add to the layer metadata as key-value pairs.   | <a href="https://bazel.build/rules/lib/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="layer_from_tar-compress"></a>compress |  Compression algorithm to use. If set to 'auto', it keeps the existing compression, unless the layer is being optimized.   | String | optional |  `"auto"`  |
| <a id="layer_from_tar-estargz"></a>estargz |  Whether to use estargz format. If set to 'auto', uses the global default estargz setting. When enabled, the layer will be optimized for lazy pulling and will be compatible with the estargz format.   | String | optional |  `"auto"`  |
| <a id="layer_from_tar-optimize"></a>optimize |  If set, rewrites the tar file to deduplicate it's contents. This is useful for reducing the size of the image, but will take extra time and space to store the optimized layer.   | Boolean | optional |  `False`  |


<a id="file_metadata"></a>

## file_metadata

<pre>
load("@rules_img//img:layer.bzl", "file_metadata")

file_metadata(*, <a href="#file_metadata-mode">mode</a>, <a href="#file_metadata-uid">uid</a>, <a href="#file_metadata-gid">gid</a>, <a href="#file_metadata-uname">uname</a>, <a href="#file_metadata-gname">gname</a>, <a href="#file_metadata-mtime">mtime</a>, <a href="#file_metadata-pax_records">pax_records</a>)
</pre>

Creates a JSON-encoded file metadata string for use with image_layer rules.

This function generates JSON metadata that can be used to customize file attributes
in container image layers, such as permissions, ownership, and timestamps.


**PARAMETERS**


| Name  | Description | Default Value |
| :------------- | :------------- | :------------- |
| <a id="file_metadata-mode"></a>mode |  File permission mode (e.g., "0755", "0644"). String format.   |  `None` |
| <a id="file_metadata-uid"></a>uid |  User ID of the file owner. Integer.   |  `None` |
| <a id="file_metadata-gid"></a>gid |  Group ID of the file owner. Integer.   |  `None` |
| <a id="file_metadata-uname"></a>uname |  User name of the file owner. String.   |  `None` |
| <a id="file_metadata-gname"></a>gname |  Group name of the file owner. String.   |  `None` |
| <a id="file_metadata-mtime"></a>mtime |  Modification time in RFC3339 format (e.g., "2023-01-01T00:00:00Z"). String.   |  `None` |
| <a id="file_metadata-pax_records"></a>pax_records |  Dict of extended attributes to set via PAX records.   |  `None` |

**RETURNS**

JSON-encoded string containing the file metadata.


