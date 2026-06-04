"""Layer rule that accepts rules_pkg PackageFilesInfo and PackageFilegroupInfo providers."""

load("@rules_pkg_providers//:providers.bzl", "PackageDirsInfo", "PackageFilegroupInfo", "PackageFilesInfo", "PackageSymlinkInfo")
load("//img/private/common:build.bzl", "TOOLCHAINS")
load("//img/private/common:layer_attrs.bzl", "layer_attrs")
load(
    "//img/private/common:tar_layer.bzl",
    "create_tar_layer",
    "files_arg",
    "resolve_layer_settings",
)
load("//img/private/providers:layers_info.bzl", "LayersInfo")

def _layer_from_pkg_impl(ctx):
    settings = resolve_layer_settings(ctx)

    extra_args = []
    extra_inputs = []

    if ctx.attr.default_metadata:
        extra_args.extend(["--default-metadata", ctx.attr.default_metadata])
    for path_key, metadata in ctx.attr.file_metadata.items():
        path_key = path_key.removeprefix("/")
        extra_args.extend(["--file-metadata", "{}={}".format(path_key, metadata)])

    files_args = ctx.actions.args()
    files_args.set_param_file_format("multiline")
    files_args.use_param_file("--add-from-file=%s", use_always = True)

    symlink_args = ctx.actions.args()
    symlink_args.set_param_file_format("multiline")
    symlink_args.use_param_file("--symlinks-from-file=%s", use_always = True)
    has_symlinks = False

    # Collect all pkg_dirs entries across all srcs so we can assign unique names.
    all_pkg_dirs = []

    for src in ctx.attr.srcs:
        if PackageFilegroupInfo in src:
            fg = src[PackageFilegroupInfo]

            for pfi, _ in fg.pkg_files:
                for dest, src_file in pfi.dest_src_map.items():
                    dest = dest.removeprefix("/")
                    files_args.add_all(
                        [src_file],
                        map_each = files_arg,
                        format_each = dest + "\0%s",
                        expand_directories = False,
                    )
                    extra_inputs.append(depset([src_file]))

            for psi, _ in fg.pkg_symlinks:
                dest = psi.destination.removeprefix("/")
                symlink_args.add("{}\0{}".format(dest, psi.target))
                has_symlinks = True

            for pdi, _ in fg.pkg_dirs:
                all_pkg_dirs.append(pdi)

        if PackageFilesInfo in src:
            pfi = src[PackageFilesInfo]
            for dest, src_file in pfi.dest_src_map.items():
                dest = dest.removeprefix("/")
                files_args.add_all(
                    [src_file],
                    map_each = files_arg,
                    format_each = dest + "\0%s",
                    expand_directories = False,
                )
                extra_inputs.append(depset([src_file]))

        if PackageSymlinkInfo in src:
            psi = src[PackageSymlinkInfo]
            dest = psi.destination.removeprefix("/")
            symlink_args.add("{}\0{}".format(dest, psi.target))
            has_symlinks = True

        if PackageDirsInfo in src:
            all_pkg_dirs.append(src[PackageDirsInfo])

    for i, pdi in enumerate(all_pkg_dirs):
        for j, dir_path in enumerate(pdi.dirs):
            dir_path = dir_path.removeprefix("/")
            empty_dir = ctx.actions.declare_directory(
                "{}_pkg_dir_{}_{}".format(ctx.attr.name, i, j),
            )
            ctx.actions.run_shell(
                outputs = [empty_dir],
                command = "",
            )
            files_args.add_all(
                [empty_dir],
                map_each = files_arg,
                format_each = dir_path + "\0%s",
                expand_directories = False,
            )
            extra_inputs.append(depset([empty_dir]))

    extra_args.append(files_args)
    if has_symlinks:
        extra_args.append(symlink_args)

    return create_tar_layer(ctx, settings, extra_args = extra_args, extra_inputs = extra_inputs)

layer_from_pkg = rule(
    implementation = _layer_from_pkg_impl,
    doc = """Creates a container image layer from rules_pkg providers.

Accepts `pkg_files`, `pkg_filegroup`, `pkg_symlink`, and `pkg_mkdirs` targets from
[rules_pkg](https://github.com/bazelbuild/rules_pkg) and packages their contents
into an OCI image layer.

This rule reads the `PackageFilesInfo`, `PackageFilegroupInfo`, `PackageSymlinkInfo`,
and `PackageDirsInfo` providers from `rules_pkg_providers` to determine the file
mapping without materialising an intermediate archive.

Example:

```python
load("@rules_img//img:layer.bzl", "layer_from_pkg")
load("@rules_pkg//:pkg.bzl", "pkg_files", "pkg_filegroup", "pkg_mkdirs", "pkg_symlink")

pkg_files(
    name = "bin_files",
    srcs = ["//cmd/server"],
    prefix = "/usr/local/bin",
)

pkg_files(
    name = "config_files",
    srcs = ["config.json"],
    prefix = "/etc/app",
)

pkg_symlink(
    name = "app_symlink",
    src = "/usr/local/bin/server",
    dest = "/usr/bin/server",
)

pkg_mkdirs(
    name = "app_dirs",
    dirs = ["/var/log/app"],
)

pkg_filegroup(
    name = "app_pkg",
    srcs = [
        ":bin_files",
        ":config_files",
        ":app_symlink",
        ":app_dirs",
    ],
)

layer_from_pkg(
    name = "app_layer",
    srcs = [":app_pkg"],
)
```
""",
    attrs = {
        "srcs": attr.label_list(
            doc = """List of rules_pkg targets whose packaging providers describe the layer contents.

Each label must provide one of: `PackageFilesInfo` (from `pkg_files`),
`PackageFilegroupInfo` (from `pkg_filegroup`), `PackageSymlinkInfo` (from
`pkg_symlink`), or `PackageDirsInfo` (from `pkg_mkdirs`).""",
            providers = [
                [PackageFilesInfo],
                [PackageFilegroupInfo],
                [PackageSymlinkInfo],
                [PackageDirsInfo],
            ],
        ),
        "default_metadata": attr.string(
            default = "",
            doc = """JSON-encoded default metadata to apply to all files in the layer.
Can include fields like mode, uid, gid, uname, gname, mtime, and pax_records.""",
        ),
        "file_metadata": attr.string_dict(
            default = {},
            doc = """Per-file metadata overrides as a dict mapping destination paths to JSON-encoded metadata.
The path must match the destination path as declared in the rules_pkg provider (the key in dest_src_map).
Metadata specified here overrides any defaults from default_metadata.""",
        ),
    } | layer_attrs.common,
    toolchains = TOOLCHAINS,
    provides = [LayersInfo],
)
