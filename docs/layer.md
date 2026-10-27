<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Public API for container image layer rules.

<a id="erofs_config"></a>

## erofs_config

<pre>
load("@rules_img//img:layer.bzl", "erofs_config")

erofs_config(<a href="#erofs_config-name">name</a>, <a href="#erofs_config-block_size">block_size</a>, <a href="#erofs_config-inline_threshold">inline_threshold</a>, <a href="#erofs_config-synthetic_directories">synthetic_directories</a>)
</pre>

Options for layers written as an EROFS filesystem image.

Pass the target to the `format_config` attribute of a layer rule that uses
`format = "erofs"`. Options that are left at their default are decided by the
`img` tool or by the matching global setting.

Example:

```python
load("@rules_img//img:layer.bzl", "erofs_config", "image_layer")

erofs_config(
    name = "layer_config",
    block_size = 4096,
)

image_layer(
    name = "layer",
    srcs = {"/etc/hostname": ":hostname"},
    format = "erofs",
    format_config = ":layer_config",
)
```

See [docs/erofs-layers.md](erofs-layers.md) for the format itself.

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="erofs_config-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="erofs_config-block_size"></a>block_size |  Filesystem block size in bytes: a power of two of at least 512. 0 uses 4096, which matches the common page size.   | Integer | optional |  `0`  |
| <a id="erofs_config-inline_threshold"></a>inline_threshold |  Maximum size (in bytes) of a file payload the layer may tail-pack into its inode.<br><br>A file at least this large gets a data extent of its own, and therefore becomes a content-addressed reference in the compact stream rather than inline bytes. 0 packs whatever fits in the inode's block. -1 uses the global default `--@rules_img//img/settings:erofs_inline_threshold` setting.   | Integer | optional |  `-1`  |
| <a id="erofs_config-synthetic_directories"></a>synthetic_directories |  Attributes for directories the layer has to invent because nothing describes them.<br><br>'inherit' copies the nearest described ancestor's ownership (what mkfs.erofs does), 'default' uses 0755 root:root, 'strict' fails the layer instead of guessing. If set to 'auto', uses the global default `--@rules_img//img/settings:erofs_synthetic_directories` setting.   | String | optional |  `"auto"`  |


<a id="ext4_config"></a>

## ext4_config

<pre>
load("@rules_img//img:layer.bzl", "ext4_config")

ext4_config(<a href="#ext4_config-name">name</a>, <a href="#ext4_config-block_size">block_size</a>, <a href="#ext4_config-bytes_per_inode">bytes_per_inode</a>, <a href="#ext4_config-deduplicate">deduplicate</a>, <a href="#ext4_config-extra_inodes">extra_inodes</a>, <a href="#ext4_config-label">label</a>, <a href="#ext4_config-min_size">min_size</a>,
            <a href="#ext4_config-omit_lost_and_found">omit_lost_and_found</a>, <a href="#ext4_config-uuid">uuid</a>)
</pre>

Options for layers written as an ext4 filesystem image.

Pass the target to the `format_config` attribute of a layer rule that uses
`format = "ext4"`. Options that are left at their default are decided by the
`img` tool (or, for `deduplicate`, by the global
`--@rules_img//img/settings:ext4_deduplicate` setting).

Example:

```python
load("@rules_img//img:layer.bzl", "ext4_config", "image_layer")

ext4_config(
    name = "rootfs_config",
    # Mountable on a machine with 4 KiB pages or more.
    block_size = 4096,
    label = "rootfs",
    # Room to write into the filesystem once it is mounted.
    min_size = "512MiB",
    bytes_per_inode = "16KiB",
)

image_layer(
    name = "rootfs",
    srcs = {"/etc/hostname": ":hostname"},
    format = "ext4",
    format_config = ":rootfs_config",
)
```

See [docs/filesystems.md](filesystems.md) for the format itself.

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="ext4_config-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="ext4_config-block_size"></a>block_size |  Filesystem block size in bytes: a power of two between 1024 and 65536. 0 uses 4096.<br><br>Linux can only mount a filesystem whose block size is at most the page size of the machine, so 4096 is the portable choice and 65536 only mounts on a machine with 64 KiB pages.   | Integer | optional |  `0`  |
| <a id="ext4_config-bytes_per_inode"></a>bytes_per_inode |  Reserve one inode for every this many bytes of `min_size`, in addition to the inodes the content needs, with an optional binary unit (`"16KiB"`).<br><br>Only useful together with `min_size`: it decides how many files can be created in the free space. Empty leaves only the inodes the content needs, plus `extra_inodes`.   | String | optional |  `""`  |
| <a id="ext4_config-deduplicate"></a>deduplicate |  Whether regular files with the same content and metadata are stored once, as hard links of each other (what tar layers do). If set to 'auto', uses the global default `--@rules_img//img/settings:ext4_deduplicate` setting.<br><br>Turning it off spares reading file content only to find duplicates, at the cost of a larger image.   | String | optional |  `"auto"`  |
| <a id="ext4_config-extra_inodes"></a>extra_inodes |  Number of free inodes to leave beyond those the content uses.   | Integer | optional |  `0`  |
| <a id="ext4_config-label"></a>label |  Volume label of the filesystem, at most 16 bytes.   | String | optional |  `""`  |
| <a id="ext4_config-min_size"></a>min_size |  Minimum size of the image, with an optional binary unit (`"512MiB"`, `"2GiB"`).<br><br>The filesystem is otherwise exactly as large as its content needs. A larger size leaves the extra space free, which is what a filesystem that is going to be written to needs. Blocks that hold nothing are not written, so a sparse-aware filesystem stores such an image in about the size of its content.   | String | optional |  `""`  |
| <a id="ext4_config-omit_lost_and_found"></a>omit_lost_and_found |  Leave out the `lost+found` directory, which fsck uses to reattach orphaned files.<br><br>An image that is only read does not need it, and leaving it out makes the layer's `mtree` match the one a tar or EROFS layer of the same content produces. `fsck` reports the directory as missing.   | Boolean | optional |  `False`  |
| <a id="ext4_config-uuid"></a>uuid |  Filesystem UUID, in the usual textual form (`"01234567-89ab-cdef-0123-456789abcdef"`).<br><br>Empty derives a UUID from the metadata of the content, so that equal inputs produce equal images. Set it when something outside the image refers to the filesystem by UUID, for example an `/etc/fstab` entry or a bootloader configuration.   | String | optional |  `""`  |


<a id="image_layer"></a>

## image_layer

<pre>
load("@rules_img//img:layer.bzl", "image_layer")

image_layer(<a href="#image_layer-name">name</a>, <a href="#image_layer-srcs">srcs</a>, <a href="#image_layer-annotations">annotations</a>, <a href="#image_layer-annotations_file">annotations_file</a>, <a href="#image_layer-compress">compress</a>, <a href="#image_layer-create_parent_directories">create_parent_directories</a>,
            <a href="#image_layer-default_metadata">default_metadata</a>, <a href="#image_layer-estargz">estargz</a>, <a href="#image_layer-extra_default_outputs">extra_default_outputs</a>, <a href="#image_layer-file_metadata">file_metadata</a>, <a href="#image_layer-format">format</a>, <a href="#image_layer-format_config">format_config</a>,
            <a href="#image_layer-history">history</a>, <a href="#image_layer-include_runfiles">include_runfiles</a>, <a href="#image_layer-media_type">media_type</a>, <a href="#image_layer-multi_file_layout">multi_file_layout</a>, <a href="#image_layer-soci">soci</a>, <a href="#image_layer-symlinks">symlinks</a>,
            <a href="#image_layer-tree_artifact_handling">tree_artifact_handling</a>, <a href="#image_layer-zstd_chunked">zstd_chunked</a>)
</pre>

Creates a container image layer from files, executables, and directories.

This rule packages files into a layer that can be used in container images. It supports:
- Adding files at specific paths in the image
- Setting file permissions and ownership
- Creating symlinks
- Including executables with their runfiles and any additional default outputs
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

### Output groups

- `mtree`: a single [mtree](https://man.freebsd.org/cgi/man.cgi?mtree(5)) text file

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="image_layer-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="image_layer-srcs"></a>srcs |  Files to include in the layer. Keys are paths in the image (e.g., "/app/bin/server"), values are labels to files or executables.<br><br>When a value is an executable, the executable is placed at the path key and its runfiles tree is included (unless include_runfiles is set to False). Any additional default outputs of the target (the rest of `DefaultInfo.files` beyond the executable) are also copied, each placed at the same location relative to the executable that it has in the source tree. Outputs that live above the executable need a path key deep enough to hold them; `extra_default_outputs` decides what happens to the ones that don't fit (by default they are dropped).<br><br>When a value is a non-executable target that produces more than one default output, the path key is treated as a directory and the outputs are placed inside it according to `multi_file_layout`. A path key that ends with "/" is always treated as a directory, even when the target produces exactly one output.   | Dictionary: String -> Label | optional |  `{}`  |
| <a id="image_layer-annotations"></a>annotations |  Annotations to add to the layer metadata as key-value pairs.   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="image_layer-annotations_file"></a>annotations_file |  File containing annotations for the layer, as JSON or newline-delimited text.<br><br>The file is parsed in one of the following formats, auto-detected from its contents:<br><br>- JSON object with string values: `{"key": "value"}` - JSON object with list values: `{"key": ["value1", "value2"]}` (the last value wins) - JSON array of `KEY=VALUE` strings: `["key=value"]` - newline-delimited `KEY=VALUE` text (one per line; blank lines and `#` comments are ignored)<br><br>Values in JSON objects are used verbatim, so they can encode arbitrary strings including values that contain `=`, spaces, or newlines. The `KEY=VALUE` forms (JSON array and text) split on the first `=` and trim surrounding whitespace from the key and value.<br><br>Annotations from this file are merged with annotations specified via the `annotations` attribute, which take precedence for matching keys.<br><br>Example file content: <pre><code>version=1.0.0&#10;build.date=2024-01-15&#10;source.url=https://github.com/...</code></pre>   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_layer-compress"></a>compress |  Compression algorithm to use. If set to 'auto', uses the global default compression setting.<br><br>'none' leaves the layer uncompressed, which is the useful choice for an EROFS layer (the blob is then mountable as it is). EROFS layers cannot be gzip-compressed.   | String | optional |  `"auto"`  |
| <a id="image_layer-create_parent_directories"></a>create_parent_directories |  Whether to automatically create parent directory entries in the tar file for all files. If set to 'auto', uses the global default create_parent_directories setting. When enabled, parent directories will be created automatically for all files in the layer.   | String | optional |  `"auto"`  |
| <a id="image_layer-default_metadata"></a>default_metadata |  JSON-encoded default metadata to apply to all files in the layer. Can include fields like mode, uid, gid, uname, gname, mtime, and pax_records. Applies to regular files (including tree contents) and symlinks. Generated directories inherit only mtime; use file_metadata for directory modes or ownership.   | String | optional |  `""`  |
| <a id="image_layer-estargz"></a>estargz |  Whether to use estargz format. If set to 'auto', uses the global default estargz setting. When enabled, the layer will be optimized for lazy pulling and will be compatible with the estargz format.   | String | optional |  `"auto"`  |
| <a id="image_layer-extra_default_outputs"></a>extra_default_outputs |  How to handle an executable's additional default outputs (the rest of `DefaultInfo.files` beyond the executable itself).<br><br>Those files are placed at the same offset relative to the executable that they have in the source tree, so a file that lives above the executable's directory needs a path in the image that is deep enough to place it. This setting decides what happens when it isn't:<br><br>- `"best_effort"` (default): place every additional default output that fits, and silently   drop the ones that would land above the image root. This is what wrapper rules such as   bazel-lib's `transition_binary` need, where the executable is a symlink in a sibling   directory of the real outputs. - `"drop"`: never place additional default outputs. Only the executable itself (and its   runfiles, if `include_runfiles` is True) is included. - `"strict"`: place every additional default output, and fail the build if any of them   would land above the image root.   | String | optional |  `"best_effort"`  |
| <a id="image_layer-file_metadata"></a>file_metadata |  Per-file metadata overrides as a dict mapping file paths to JSON-encoded metadata. The path should match the path in the image, including paths inside directory inputs. Directory paths should omit the trailing slash. Metadata specified here overrides any defaults from default_metadata.   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="image_layer-format"></a>format |  Container format of the layer blob, orthogonal to `compress`.<br><br>'tar' writes an OCI tar changeset. 'erofs' writes a mountable EROFS filesystem image. 'ext4' writes an ext4 filesystem image. If set to 'auto', uses the global default --@rules_img//img/settings:layer_format setting.<br><br>An EROFS layer is a filesystem, not a changeset. See docs/erofs-layers.md before using it.<br><br>See docs/filesystems.md for how the formats compare.   | String | optional |  `"auto"`  |
| <a id="image_layer-format_config"></a>format_config |  Options of the filesystem image the layer is written as: an `ext4_config` target for `format = "ext4"`, or an `erofs_config` target for `format = "erofs"`.<br><br>Without it, every option of the format is left at its default. A config for a format other than the layer's own is a configuration error, since it would silently have no effect.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_layer-history"></a>history |  Overrides the `created_by` history entry recorded for this layer.<br><br>By default the layer records one entry, "bazel build <label>", so two layers with identical contents but different labels have different config digests. `history = []` records no history entry, so identical contents give identical config digests regardless of label. A single-element list is recorded verbatim. The img tool records at most one history entry per layer, so longer lists are rejected.   | List of strings | optional |  `["<default bazel build history>"]`  |
| <a id="image_layer-include_runfiles"></a>include_runfiles |  Whether to include runfiles for executable targets. When True (default), executables in srcs will include their runfiles tree. When False, only the executable file itself is included, without runfiles.<br><br>Either way, any additional default outputs of the target (the rest of `DefaultInfo.files` beyond the executable) are copied into the layer, placed relative to the executable, as configured by `extra_default_outputs`.   | Boolean | optional |  `True`  |
| <a id="image_layer-media_type"></a>media_type |  Override the layer media type. By default, the media type is auto-detected from the compression algorithm.   | String | optional |  `""`  |
| <a id="image_layer-multi_file_layout"></a>multi_file_layout |  How to place a non-executable src that produces MORE THAN ONE default output.<br><br>- `"package_relative"` (default): treat the path key as a directory and place each file inside it,   preserving its path relative to the producing target's package. - `"flatten"`: place each file directly in the directory by basename (restores the older behavior).<br><br>A src that produces a single output is placed exactly at its path key, regardless of this setting. To opt out of that shortcut, end the path key with "/": it then always denotes a directory, and a single output is laid out inside it just like multiple outputs would be.   | String | optional |  `"package_relative"`  |
| <a id="image_layer-soci"></a>soci |  Whether to emit a SOCI ztoc (table of contents) for this layer. If set to 'auto', uses the global default //img/settings:soci setting. When enabled and the layer is gzip-compressed, a ztoc is produced in the layer action and recorded on the SingleLayerInfo provider, so images that build a SOCI Index Manifest v2 can reuse it instead of regenerating it. Non-gzip layers never emit a ztoc.   | String | optional |  `"auto"`  |
| <a id="image_layer-symlinks"></a>symlinks |  Symlinks to create in the layer. Keys are symlink paths in the image, values are the targets they point to.   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="image_layer-tree_artifact_handling"></a>tree_artifact_handling |  How to handle duplicate tree artifacts (directories) in the layer. If set to 'full', each tree artifact is stored at its intended path (no deduplication). If set to 'deduplicate_symlink', duplicate tree artifacts are replaced with symlinks to the first occurrence. If set to 'auto', uses the global default from --@rules_img//img/settings:layer_tree_artifact_handling. When file metadata is configured, trees are stored in full so path-specific metadata is preserved. Regular files with identical contents and metadata still deduplicate.   | String | optional |  `"auto"`  |
| <a id="image_layer-zstd_chunked"></a>zstd_chunked |  Whether to use the zstd:chunked format. If set to 'auto', uses the global default zstd_chunked setting. When enabled, the layer is written with content-defined chunks and a table of contents, so compatible runtimes (podman, CRI-O) can fetch and deduplicate individual chunks. Requires zstd compression and cannot be combined with estargz.   | String | optional |  `"auto"`  |


<a id="layer_from_binary"></a>

## layer_from_binary

<pre>
load("@rules_img//img:layer.bzl", "layer_from_binary")

layer_from_binary(<a href="#layer_from_binary-name">name</a>, <a href="#layer_from_binary-annotations">annotations</a>, <a href="#layer_from_binary-annotations_file">annotations_file</a>, <a href="#layer_from_binary-binary">binary</a>, <a href="#layer_from_binary-compress">compress</a>, <a href="#layer_from_binary-create_parent_directories">create_parent_directories</a>,
                  <a href="#layer_from_binary-default_metadata">default_metadata</a>, <a href="#layer_from_binary-estargz">estargz</a>, <a href="#layer_from_binary-extra_default_outputs">extra_default_outputs</a>, <a href="#layer_from_binary-format">format</a>, <a href="#layer_from_binary-format_config">format_config</a>,
                  <a href="#layer_from_binary-include_runfiles">include_runfiles</a>, <a href="#layer_from_binary-infer_working_dir">infer_working_dir</a>, <a href="#layer_from_binary-layer_budget">layer_budget</a>, <a href="#layer_from_binary-media_type">media_type</a>, <a href="#layer_from_binary-path">path</a>, <a href="#layer_from_binary-runfiles_path">runfiles_path</a>,
                  <a href="#layer_from_binary-runfiles_shared_path">runfiles_shared_path</a>, <a href="#layer_from_binary-runfiles_sharing_mode">runfiles_sharing_mode</a>, <a href="#layer_from_binary-soci">soci</a>, <a href="#layer_from_binary-tree_artifact_handling">tree_artifact_handling</a>,
                  <a href="#layer_from_binary-zstd_chunked">zstd_chunked</a>)
</pre>

Creates a container image layer from a *_binary target.

This rule packages a binary executable and its runfiles into a layer, and additionally
provides image configuration (entrypoint, cmd, env, working_dir) via ImageLayerConfigInfo.
When used as a layer in image_manifest, the configuration is automatically applied to the
image with Dockerfile-like semantics.

The binary's `args` attribute becomes the image `cmd`, its `env` attribute (or
RunEnvironmentInfo provider) becomes `env`, and the binary path becomes the `entrypoint`.
When include_runfiles is True (default), the working directory is set to the runfiles root.
Set `infer_working_dir = False` to leave the working directory unset, so the base image's
working directory (or image_manifest's own `working_dir` attribute) applies instead.

In addition to the executable and its runfiles, any other default outputs of the binary
target (the rest of `DefaultInfo.files`) are copied into the layer, each placed at the same
location relative to the executable that it has in the source tree.

If the binary provides RunfilesGroupInfo (from rules_runfiles_group), the runfiles are split
into separate layers based on the groups. This allows for better caching: stable layers
(interpreter, stdlib) change infrequently and can be shared, while the application code layer
changes with each build. Layers are emitted in the groups' `rank` order (lowest first), so
foundational content ends up in the earliest, most cacheable layers. Any
RunfilesGroupTransformInfo in the binary's `aspect_hints` is applied first, which lets users
drop or re-shape groups per target.

Note that RunfilesGroupInfo emission is off by default in rules_runfiles_group. Build with
`--@rules_runfiles_group//runfiles_group:enabled=true` to opt in; without it, group-aware
binaries produce a single layer.

When the number of groups exceeds what is practical for a container image, use `layer_budget`
to merge groups down to a maximum count. The merge algorithm respects group rank (only merges
within the same rank), do_not_merge flags, merge affinity (groups sharing an affinity are
preferred merge partners), and weight hints (lighter groups merge first).

Example:

```python
load("@rules_img//img:layer.bzl", "layer_from_binary")
load("@rules_img//img:image.bzl", "image_manifest")

# Package a Go binary with its runfiles
layer_from_binary(
    name = "app_layer",
    binary = "//cmd/server",
)

# Use in an image - entrypoint, cmd, env, and working_dir are set automatically
image_manifest(
    name = "image",
    base = "@distroless_base",
    layers = [":app_layer"],
)

# Override the path inside the image
layer_from_binary(
    name = "custom_path_layer",
    binary = "//cmd/server",
    path = "/usr/local/bin/",
)

# Without runfiles (static binary)
layer_from_binary(
    name = "static_layer",
    binary = "//cmd/server",
    path = "/usr/local/bin/server",
    include_runfiles = False,
)
```

### Output groups

- `mtree`: one [mtree](https://man.freebsd.org/cgi/man.cgi?mtree(5)) text file per produced layer

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="layer_from_binary-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="layer_from_binary-annotations"></a>annotations |  Annotations to add to the layer metadata as key-value pairs.   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="layer_from_binary-annotations_file"></a>annotations_file |  File containing annotations for the layer, as JSON or newline-delimited text.<br><br>The file is parsed in one of the following formats, auto-detected from its contents:<br><br>- JSON object with string values: `{"key": "value"}` - JSON object with list values: `{"key": ["value1", "value2"]}` (the last value wins) - JSON array of `KEY=VALUE` strings: `["key=value"]` - newline-delimited `KEY=VALUE` text (one per line; blank lines and `#` comments are ignored)<br><br>Values in JSON objects are used verbatim, so they can encode arbitrary strings including values that contain `=`, spaces, or newlines. The `KEY=VALUE` forms (JSON array and text) split on the first `=` and trim surrounding whitespace from the key and value.<br><br>Annotations from this file are merged with annotations specified via the `annotations` attribute, which take precedence for matching keys.<br><br>Example file content: <pre><code>version=1.0.0&#10;build.date=2024-01-15&#10;source.url=https://github.com/...</code></pre>   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="layer_from_binary-binary"></a>binary |  The *_binary target to package into the layer.<br><br>The binary's `args` and `env` attributes are extracted and provided as image configuration (cmd and env) via ImageLayerConfigInfo. The `data` attribute is used for `$(location)` expansion in args and env values.<br><br>If the binary provides RunfilesGroupInfo, the runfiles are split into separate layers per group.   | <a href="https://bazel.build/concepts/labels">Label</a> | required |  |
| <a id="layer_from_binary-compress"></a>compress |  Compression algorithm to use. If set to 'auto', uses the global default compression setting.<br><br>'none' leaves the layer uncompressed, which is the useful choice for an EROFS layer (the blob is then mountable as it is). EROFS layers cannot be gzip-compressed.   | String | optional |  `"auto"`  |
| <a id="layer_from_binary-create_parent_directories"></a>create_parent_directories |  Whether to automatically create parent directory entries in the tar file for all files. If set to 'auto', uses the global default create_parent_directories setting. When enabled, parent directories will be created automatically for all files in the layer.   | String | optional |  `"auto"`  |
| <a id="layer_from_binary-default_metadata"></a>default_metadata |  JSON-encoded default metadata to apply to all files in the layers. Can include fields like mode, uid, gid, uname, gname, mtime, and pax_records. Applies to regular files (including tree contents) and symlinks. Generated directories inherit only mtime, retaining their default mode and ownership.<br><br>Accepts the same value as image_layer's attribute of the same name, so img/layer.bzl's file_metadata() builds it.   | String | optional |  `""`  |
| <a id="layer_from_binary-estargz"></a>estargz |  Whether to use estargz format. If set to 'auto', uses the global default estargz setting. When enabled, the layer will be optimized for lazy pulling and will be compatible with the estargz format.   | String | optional |  `"auto"`  |
| <a id="layer_from_binary-extra_default_outputs"></a>extra_default_outputs |  How to handle an executable's additional default outputs (the rest of `DefaultInfo.files` beyond the executable itself).<br><br>Those files are placed at the same offset relative to the executable that they have in the source tree, so a file that lives above the executable's directory needs a path in the image that is deep enough to place it. This setting decides what happens when it isn't:<br><br>- `"best_effort"` (default): place every additional default output that fits, and silently   drop the ones that would land above the image root. This is what wrapper rules such as   bazel-lib's `transition_binary` need, where the executable is a symlink in a sibling   directory of the real outputs. - `"drop"`: never place additional default outputs. Only the executable itself (and its   runfiles, if `include_runfiles` is True) is included. - `"strict"`: place every additional default output, and fail the build if any of them   would land above the image root.   | String | optional |  `"best_effort"`  |
| <a id="layer_from_binary-format"></a>format |  Container format of the layer blob, orthogonal to `compress`.<br><br>'tar' writes an OCI tar changeset. 'erofs' writes a mountable EROFS filesystem image. 'ext4' writes an ext4 filesystem image. If set to 'auto', uses the global default --@rules_img//img/settings:layer_format setting.<br><br>An EROFS layer is a filesystem, not a changeset. See docs/erofs-layers.md before using it.<br><br>See docs/filesystems.md for how the formats compare.   | String | optional |  `"auto"`  |
| <a id="layer_from_binary-format_config"></a>format_config |  Options of the filesystem image the layer is written as: an `ext4_config` target for `format = "ext4"`, or an `erofs_config` target for `format = "erofs"`.<br><br>Without it, every option of the format is left at its default. A config for a format other than the layer's own is a configuration error, since it would silently have no effect.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="layer_from_binary-include_runfiles"></a>include_runfiles |  Whether to include runfiles for executable targets. When True (default), executables in srcs will include their runfiles tree. When False, only the executable file itself is included, without runfiles.<br><br>Either way, any additional default outputs of the target (the rest of `DefaultInfo.files` beyond the executable) are copied into the layer, placed relative to the executable, as configured by `extra_default_outputs`.   | Boolean | optional |  `True`  |
| <a id="layer_from_binary-infer_working_dir"></a>infer_working_dir |  Whether to infer the image's working directory from the binary's runfiles tree.<br><br>When True (default) and `include_runfiles` is True, `ImageLayerConfigInfo.working_dir` is set to the main workspace directory inside the runfiles tree (e.g. "/_main/cmd/server/server_/server.runfiles/_main"), which is the directory a binary launched through the runfiles convention expects to run in.<br><br>When False, `ImageLayerConfigInfo.working_dir` is None, which carries no opinion: `image_manifest` then keeps the base image's working directory, unless its own `working_dir` attribute is set explicitly.   | Boolean | optional |  `True`  |
| <a id="layer_from_binary-layer_budget"></a>layer_budget |  Maximum total number of layers produced by this rule. If set to a value > 0 and the binary provides RunfilesGroupInfo, groups are merged using the merge algorithm from rules_runfiles_group. The algorithm respects group rank (only merges within the same rank), do_not_merge flags, merge affinity (groups sharing an affinity are preferred merge partners), and weight hints (lighter groups merge first).<br><br>When the binary names an executable_group, the binary executable and supporting files are merged into that group's layer, and the full budget is available for runfiles groups. When no executable_group exists, one layer is reserved for a separate binary layer, and the remaining budget (layer_budget - 1) is used for groups; layer_budget=1 without an executable_group skips the grouped path entirely.<br><br>This is a target, not a hard cap: groups marked do_not_merge are never merged, and groups at different ranks never merge with each other, so a binary whose groups cannot be reduced far enough still produces more layers than the budget.<br><br>0 means no limit (all groups become separate layers, plus a binary layer unless an executable_group absorbs it).   | Integer | optional |  `0`  |
| <a id="layer_from_binary-media_type"></a>media_type |  Override the layer media type. By default, the media type is auto-detected from the compression algorithm.   | String | optional |  `""`  |
| <a id="layer_from_binary-path"></a>path |  Optional path of the binary inside the image. If the path ends with a slash ("/"), the basename of the binary will be automatically appended. If unset, this defaults to the rlocationpath of the binary (e.g., "_main/cmd/server/server_/server").   | String | optional |  `""`  |
| <a id="layer_from_binary-runfiles_path"></a>runfiles_path |  Optional path of the runfiles directory of the binary inside the image. If unset, this defaults to the path of the binary with a .runfiles suffix (e.g., "_main/cmd/server/server_/server.runfiles"). Note: depending on the runfiles_sharing_mode, this may be a symlink to a shared runfiles directory.   | String | optional |  `""`  |
| <a id="layer_from_binary-runfiles_shared_path"></a>runfiles_shared_path |  Optional path of the shared runfiles directory inside the image. This is only used when runfiles sharing is enabled and has a global default.   | String | optional |  `""`  |
| <a id="layer_from_binary-runfiles_sharing_mode"></a>runfiles_sharing_mode |  How to process runfiles. Runfiles can either be placed next to the executable (in a directory with a .runfiles suffix, the runfiles_path attribute), or placed in a shared runfiles path. When sharing runfiles, there will be symlink added: {runfiles_path} -> {runfiles_shared_path}.<br><br>Possible settings:<br><br>* `"auto"`: Share runfiles based on the global default and based on the presence of `RunfilesGroupInfo`.     Globally, runfiles sharing can be set to `"shared"`, `"private"`, or `"auto"`, where auto shares runfiles if `RunfilesGroupInfo` is provided. * `"shared"`: Always share runfiles. * `"private"`: Never share runfiles   | String | optional |  `"auto"`  |
| <a id="layer_from_binary-soci"></a>soci |  Whether to emit a SOCI ztoc (table of contents) for this layer. If set to 'auto', uses the global default //img/settings:soci setting. When enabled and the layer is gzip-compressed, a ztoc is produced in the layer action and recorded on the SingleLayerInfo provider, so images that build a SOCI Index Manifest v2 can reuse it instead of regenerating it. Non-gzip layers never emit a ztoc.   | String | optional |  `"auto"`  |
| <a id="layer_from_binary-tree_artifact_handling"></a>tree_artifact_handling |  How to handle duplicate tree artifacts (directories) in the layer. If set to 'full', each tree artifact is stored at its intended path (no deduplication). If set to 'deduplicate_symlink', duplicate tree artifacts are replaced with symlinks to the first occurrence. If set to 'auto', uses the global default from --@rules_img//img/settings:layer_tree_artifact_handling. When file metadata is configured, trees are stored in full so path-specific metadata is preserved. Regular files with identical contents and metadata still deduplicate.   | String | optional |  `"auto"`  |
| <a id="layer_from_binary-zstd_chunked"></a>zstd_chunked |  Whether to use the zstd:chunked format. If set to 'auto', uses the global default zstd_chunked setting. When enabled, the layer is written with content-defined chunks and a table of contents, so compatible runtimes (podman, CRI-O) can fetch and deduplicate individual chunks. Requires zstd compression and cannot be combined with estargz.   | String | optional |  `"auto"`  |


<a id="layer_from_file"></a>

## layer_from_file

<pre>
load("@rules_img//img:layer.bzl", "layer_from_file")

layer_from_file(<a href="#layer_from_file-name">name</a>, <a href="#layer_from_file-src">src</a>, <a href="#layer_from_file-annotations">annotations</a>, <a href="#layer_from_file-base_name_annotations">base_name_annotations</a>, <a href="#layer_from_file-diff_id">diff_id</a>, <a href="#layer_from_file-diff_id_annotations">diff_id_annotations</a>,
                <a href="#layer_from_file-media_type">media_type</a>)
</pre>

Creates a container image layer from an arbitrary file.

This rule uses any file as a container image layer blob, computing the
necessary metadata (digest, size) without any tar-specific processing
like compression or optimization.

This is useful for non-tar layer content such as Helm charts, WASM
modules, or other OCI artifacts.

If you want to use an existing tar file as a layer, use layer_from_tar instead.

Example:

```python
load("@rules_img//img:layer.bzl", "layer_from_file")

# Use an arbitrary artifact as a layer
layer_from_file(
    name = "artifact_layer",
    src = ":artifact.bin",
    media_type = "application/octet-stream",
    annotations = {
        "org.opencontainers.image.title": "artifact.bin",
    },
)
```

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="layer_from_file-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="layer_from_file-src"></a>src |  The file to use as a layer blob.   | <a href="https://bazel.build/concepts/labels">Label</a> | required |  |
| <a id="layer_from_file-annotations"></a>annotations |  Annotations to add to the layer metadata as key-value pairs.   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="layer_from_file-base_name_annotations"></a>base_name_annotations |  List of annotations that are set to the basename of the file.   | List of strings | optional |  `[]`  |
| <a id="layer_from_file-diff_id"></a>diff_id |  If set, interprets the file as a (potentially compressed) tar file and calculates the diff_id. Warning: if you do this, you probably want to use layer_from_tar instead.   | Boolean | optional |  `False`  |
| <a id="layer_from_file-diff_id_annotations"></a>diff_id_annotations |  List of annotations that are set to the diff_id of the file. Only works with tar files.   | List of strings | optional |  `[]`  |
| <a id="layer_from_file-media_type"></a>media_type |  Layer media type. Defaults to "application/vnd.oci.image.layer.v1.tar" if not set.   | String | optional |  `"application/vnd.oci.image.layer.v1.tar"`  |


<a id="layer_from_tar"></a>

## layer_from_tar

<pre>
load("@rules_img//img:layer.bzl", "layer_from_tar")

layer_from_tar(<a href="#layer_from_tar-name">name</a>, <a href="#layer_from_tar-src">src</a>, <a href="#layer_from_tar-annotations">annotations</a>, <a href="#layer_from_tar-compress">compress</a>, <a href="#layer_from_tar-estargz">estargz</a>, <a href="#layer_from_tar-format">format</a>, <a href="#layer_from_tar-format_config">format_config</a>, <a href="#layer_from_tar-media_type">media_type</a>,
               <a href="#layer_from_tar-optimize">optimize</a>, <a href="#layer_from_tar-zstd_chunked">zstd_chunked</a>)
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

### Output groups

- `mtree`: a single [mtree](https://man.freebsd.org/cgi/man.cgi?mtree(5)) text file

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="layer_from_tar-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="layer_from_tar-src"></a>src |  The tar file to convert into a layer. Must be a valid tar file (optionally compressed).   | <a href="https://bazel.build/concepts/labels">Label</a> | required |  |
| <a id="layer_from_tar-annotations"></a>annotations |  Annotations to add to the layer metadata as key-value pairs.   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="layer_from_tar-compress"></a>compress |  Compression algorithm to use. If set to 'auto', it keeps the existing compression, unless the layer is being optimized.   | String | optional |  `"auto"`  |
| <a id="layer_from_tar-estargz"></a>estargz |  Whether to use estargz format. If set to 'auto', uses the global default estargz setting. When enabled, the layer will be optimized for lazy pulling and will be compatible with the estargz format.   | String | optional |  `"auto"`  |
| <a id="layer_from_tar-format"></a>format |  Container format of the layer blob. If set to 'auto', uses the global default --@rules_img//img/settings:layer_format setting.<br><br>'erofs' converts the tar into a mountable EROFS filesystem image. That always rewrites the layer (a format change cannot preserve the source blob) and defaults to `compress = "none"`, since the resulting blob is mountable as it is. See docs/erofs-layers.md for what a filesystem image means for the layers below it.<br><br>'ext4' converts the tar into an ext4 filesystem image the same way (always rewritten, never compressed). Like every ext4 layer, it is not a valid OCI layer: use it on its own. The options that only apply to tar layers (`compress`, `estargz`, `zstd_chunked`) are ignored. See docs/filesystems.md.   | String | optional |  `"auto"`  |
| <a id="layer_from_tar-format_config"></a>format_config |  Options of the filesystem image the layer is written as: an `ext4_config` target for `format = "ext4"`, or an `erofs_config` target for `format = "erofs"`.<br><br>Without it, every option of the format is left at its default. A config for a format other than the layer's own is a configuration error, since it would silently have no effect.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="layer_from_tar-media_type"></a>media_type |  Override layer media type. Use e.g. "application/vnd.cncf.helm.chart.content.v1.tar" for Helm charts.   | String | optional |  `""`  |
| <a id="layer_from_tar-optimize"></a>optimize |  If set, rewrites the tar file to deduplicate it's contents. This is useful for reducing the size of the image, but will take extra time and space to store the optimized layer.   | Boolean | optional |  `False`  |
| <a id="layer_from_tar-zstd_chunked"></a>zstd_chunked |  Whether to use the zstd:chunked format. If set to 'auto', uses the global default zstd_chunked setting. When enabled, the layer is written with content-defined chunks and a table of contents, so compatible runtimes (podman, CRI-O) can fetch and deduplicate individual chunks. Requires zstd compression and cannot be combined with estargz.   | String | optional |  `"auto"`  |


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


