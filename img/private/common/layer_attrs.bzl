"""Common attributes shared by layer rules."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")

_common_attrs = dict(
    compress = attr.string(
        default = "auto",
        values = ["auto", "gzip", "zstd", "none"],
        doc = """\
Compression algorithm to use. If set to 'auto', uses the global default compression setting.

'none' leaves the layer uncompressed, which is the useful choice for an EROFS layer
(the blob is then mountable as it is). EROFS layers cannot be gzip-compressed.

A `cpio` layer (an initramfs) is also left uncompressed by default. It can be compressed with `gzip` or
`zstd`, which the kernel unpacks as it is. The other filesystem image formats (`ext4`, `fat32`) ignore this
attribute.
""",
    ),
    format = attr.string(
        default = "auto",
        values = ["auto", "tar", "erofs", "ext4", "fat32", "cpio"],
        doc = """\
Container format of the layer blob, orthogonal to `compress`.

'tar' writes an OCI tar changeset. 'erofs' writes a mountable EROFS filesystem image. 'ext4' writes an
ext4 filesystem image, 'fat32' a FAT32 filesystem image, 'cpio' a cpio archive in the newc format (an initramfs). If set to 'auto', uses the global default
--@rules_img//img/settings:layer_format setting.

An EROFS layer is a filesystem, not a changeset, which changes what an absent
directory entry means. See docs/erofs-layers.md before using it.

An `ext4` layer is written directly by `img layer`, for using the layer on its own: each regular file
is stored as one contiguous run of blocks, the image is only as large as its content needs, and
regular files with the same content and metadata are stored once, as hard links (like in tar layers).
The layer file has the extension `.ext4`.

A `fat32` layer is written the same way, with one contiguous run of clusters per file. FAT32 volumes
need at least 65525 clusters, so the image is at least about 33 MiB (a sparse file, if the file system
supports it). The layer file has the extension `.fat32`. FAT cannot store symbolic links, device nodes,
FIFOs, ownership or permissions: a layer with symbolic links or special files fails to build, hard links
become copies, and the owner write bit becomes the read-only attribute. Timestamps are limited to 1980
through 2107 with a resolution of two seconds. Names are case insensitive.

A `cpio` layer is an uncompressed cpio archive in the "newc" format, which is what the Linux kernel
unpacks as an initramfs (initrd). Entries are sorted by path,
and directories that nothing describes are added with mode 0755 owned by root, because the kernel does not
create parent directories. Symbolic links, device nodes, FIFOs, hard links and ownership are kept. Sizes,
ownership, timestamps (seconds since the epoch) and device numbers must fit in 32 bits. Compact layers are
not supported. The layer file has the extension `.cpio`, `.cpio.gz` or `.cpio.zst`, depending on `compress`;
the kernel unpacks the compressed archives as they are.

`ext4`, `fat32` and `cpio` layers are **not** valid OCI layers: the metadata recorded for them (media type,
digests) does not describe an OCI blob, so do not use such a layer in an `image_manifest`. Options that
only apply to tar layers are ignored: `compress` (except for `cpio`), `estargz`, `soci`, the global compression settings,
compact layers, and `mtree` output. Entries are limited to what the file system can store (for example,
file names of up to 255 bytes).
""",
    ),
    sector_size = attr.int(
        default = 0,
        values = [0, 512, 1024, 2048, 4096],
        doc = """\
Logical sector size of a `fat32` layer in bytes. 0 (the default) is 512. It has to match the
sector size of the disk image the layer ends up in (see `disk_image`): firmware reads the FAT of a
volume with the sector size of the disk. A FAT32 volume needs at least 65525 clusters, so with
4096 byte sectors the image is at least about 256 MiB (a sparse file, if the file system
supports it).

Only used by the `fat32` layer format.
""",
    ),
    estargz = attr.string(
        default = "auto",
        values = ["auto", "enabled", "disabled"],
        doc = """\
Whether to use estargz format. If set to 'auto', uses the global default estargz setting.
When enabled, the layer will be optimized for lazy pulling and will be compatible with the estargz format.
""",
    ),
    soci = attr.string(
        default = "auto",
        values = ["auto", "enabled", "disabled"],
        doc = """\
Whether to emit a SOCI ztoc (table of contents) for this layer. If set to 'auto',
uses the global default //img/settings:soci setting. When enabled and the layer is
gzip-compressed, a ztoc is produced in the layer action and recorded on the
SingleLayerInfo provider, so images that build a SOCI Index Manifest v2 can reuse
it instead of regenerating it. Non-gzip layers never emit a ztoc.
""",
    ),
    create_parent_directories = attr.string(
        default = "auto",
        values = ["auto", "enabled", "disabled"],
        doc = """\
Whether to automatically create parent directory entries in the tar file for all files.
If set to 'auto', uses the global default create_parent_directories setting.
When enabled, parent directories will be created automatically for all files in the layer.
""",
    ),
    tree_artifact_handling = attr.string(
        default = "auto",
        values = ["auto", "full", "deduplicate_symlink"],
        doc = """\
How to handle duplicate tree artifacts (directories) in the layer.
If set to 'full', each tree artifact is stored at its intended path (no deduplication).
If set to 'deduplicate_symlink', duplicate tree artifacts are replaced with symlinks to the first occurrence.
If set to 'auto', uses the global default from --@rules_img//img/settings:layer_tree_artifact_handling.
When file metadata is configured, trees are stored in full so path-specific metadata
is preserved. Regular files with identical contents and metadata still deduplicate.
""",
    ),
    include_runfiles = attr.bool(
        default = True,
        doc = """\
Whether to include runfiles for executable targets.
When True (default), executables in srcs will include their runfiles tree.
When False, only the executable file itself is included, without runfiles.

Either way, any additional default outputs of the target (the rest of `DefaultInfo.files`
beyond the executable) are copied into the layer, placed relative to the executable,
as configured by `extra_default_outputs`.
""",
    ),
    extra_default_outputs = attr.string(
        default = "best_effort",
        values = ["best_effort", "drop", "strict"],
        doc = """\
How to handle an executable's additional default outputs (the rest of `DefaultInfo.files`
beyond the executable itself).

Those files are placed at the same offset relative to the executable that they have in the
source tree, so a file that lives above the executable's directory needs a path in the image
that is deep enough to place it. This setting decides what happens when it isn't:

- `"best_effort"` (default): place every additional default output that fits, and silently
  drop the ones that would land above the image root. This is what wrapper rules such as
  bazel-lib's `transition_binary` need, where the executable is a symlink in a sibling
  directory of the real outputs.
- `"drop"`: never place additional default outputs. Only the executable itself (and its
  runfiles, if `include_runfiles` is True) is included.
- `"strict"`: place every additional default output, and fail the build if any of them
  would land above the image root.
""",
    ),
    annotations = attr.string_dict(
        default = {},
        doc = """\
Annotations to add to the layer metadata as key-value pairs.
""",
    ),
    annotations_file = attr.label(
        doc = """\
File containing annotations for the layer, as JSON or newline-delimited text.

The file is parsed in one of the following formats, auto-detected from its contents:

- JSON object with string values: `{"key": "value"}`
- JSON object with list values: `{"key": ["value1", "value2"]}` (the last value wins)
- JSON array of `KEY=VALUE` strings: `["key=value"]`
- newline-delimited `KEY=VALUE` text (one per line; blank lines and `#` comments are ignored)

Values in JSON objects are used verbatim, so they can encode arbitrary strings including
values that contain `=`, spaces, or newlines. The `KEY=VALUE` forms (JSON array and text)
split on the first `=` and trim surrounding whitespace from the key and value.

Annotations from this file are merged with annotations specified via the `annotations`
attribute, which take precedence for matching keys.

Example file content:
```
version=1.0.0
build.date=2024-01-15
source.url=https://github.com/...
```
""",
        allow_single_file = True,
    ),
    media_type = attr.string(
        default = "",
        doc = """\
Override the layer media type. By default, the media type is auto-detected from the compression algorithm.
""",
    ),
    _default_compression = attr.label(
        default = Label("//img/settings:compress"),
        providers = [BuildSettingInfo],
    ),
    _default_layer_format = attr.label(
        default = Label("//img/settings:layer_format"),
        providers = [BuildSettingInfo],
    ),
    _erofs_block_size = attr.label(
        default = Label("//img/settings:erofs_block_size"),
        providers = [BuildSettingInfo],
    ),
    _erofs_inline_threshold = attr.label(
        default = Label("//img/settings:erofs_inline_threshold"),
        providers = [BuildSettingInfo],
    ),
    _erofs_synthetic_directories = attr.label(
        default = Label("//img/settings:erofs_synthetic_directories"),
        providers = [BuildSettingInfo],
    ),
    _default_estargz = attr.label(
        default = Label("//img/settings:estargz"),
        providers = [BuildSettingInfo],
    ),
    _default_soci = attr.label(
        default = Label("//img/settings:soci"),
        providers = [BuildSettingInfo],
    ),
    _default_soci_span_size = attr.label(
        default = Label("//img/settings:soci_span_size"),
        providers = [BuildSettingInfo],
    ),
    _default_create_parent_directories = attr.label(
        default = Label("//img/settings:create_parent_directories"),
        providers = [BuildSettingInfo],
    ),
    _default_tree_artifact_handling = attr.label(
        default = Label("//img/settings:layer_tree_artifact_handling"),
        providers = [BuildSettingInfo],
    ),
    _default_runfiles_shared_path = attr.label(
        default = Label("//img/settings:runfiles_shared_path"),
        providers = [BuildSettingInfo],
    ),
    _default_runfiles_sharing_mode = attr.label(
        default = Label("//img/settings:runfiles_sharing_mode"),
        providers = [BuildSettingInfo],
    ),
    _compression_jobs = attr.label(
        default = Label("//img/settings:compression_jobs"),
        providers = [BuildSettingInfo],
    ),
    _compression_level = attr.label(
        default = Label("//img/settings:compression_level"),
        providers = [BuildSettingInfo],
    ),
    _experimental_compact_layers = attr.label(
        default = Label("//img/settings:experimental_compact_layers"),
        providers = [BuildSettingInfo],
    ),
    _experimental_compact_layers_inline_threshold = attr.label(
        default = Label("//img/settings:experimental_compact_layers_inline_threshold"),
        providers = [BuildSettingInfo],
    ),
    _experimental_compact_layers_materialize_blob = attr.label(
        default = Label("//img/settings:experimental_compact_layers_materialize_blob"),
        providers = [BuildSettingInfo],
    ),
    _mtree_path_prefix = attr.label(
        default = Label("//img/settings:mtree_path_prefix"),
        providers = [BuildSettingInfo],
    ),
    _mtree_options = attr.label(
        default = Label("//img/settings:mtree_options"),
        providers = [BuildSettingInfo],
    ),
    _mtree_layer_layout = attr.label(
        default = Label("//img/settings:mtree_layer_layout"),
        providers = [BuildSettingInfo],
    ),
)

layer_attrs = struct(
    common = _common_attrs,
)
