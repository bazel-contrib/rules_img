"""Common attributes shared by layer rules."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")

_common_attrs = dict(
    compress = attr.string(
        default = "auto",
        values = ["auto", "gzip", "zstd"],
        doc = """\
Compression algorithm to use. If set to 'auto', uses the global default compression setting.
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
""",
    ),
    include_runfiles = attr.bool(
        default = True,
        doc = """\
Whether to include runfiles for executable targets.
When True (default), executables in srcs will include their runfiles tree.
When False, only the executable file itself is included, without runfiles.
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
File containing newline-delimited KEY=VALUE annotations for the layer.

The file should contain one annotation per line in KEY=VALUE format. Empty lines are ignored.
Annotations from this file are merged with annotations specified via the `annotations` attribute.

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
    _default_estargz = attr.label(
        default = Label("//img/settings:estargz"),
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
    _experimental_layer_emit_tar_index = attr.label(
        default = Label("//img/settings:experimental_layer_emit_tar_index"),
        providers = [BuildSettingInfo],
    ),
    _experimental_layer_index_local_paths = attr.label(
        default = Label("//img/settings:experimental_layer_index_local_paths"),
        providers = [BuildSettingInfo],
    ),
)

layer_attrs = struct(
    common = _common_attrs,
)
