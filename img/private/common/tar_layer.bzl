"""Reusable functions for creating tar-based container image layers."""

load("@bazel_skylib//lib:paths.bzl", "paths")
load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private/common:build.bzl", "TOOLCHAIN")
load("//img/private/common:layer_helper.bzl", "compression_tuning_args")
load("//img/private/providers:layers_info.bzl", "LayersInfo")
load("//img/private/providers:single_layer_info.bzl", "SingleLayerInfo")

def file_type(f):
    """Returns the tar entry type character for a file.

    Args:
        f: File object to determine the type of.

    Returns:
        A single character: 'f' for regular file, 'd' for directory, 'l' for symlink.
    """
    type = "f"  # regular file
    if f.is_directory:
        type = "d"
    if hasattr(f, "is_symlink") and f.is_symlink:
        type = "l"
    return type

def files_arg(f):
    type = file_type(f)
    return "{}{}".format(type, f.path)

def to_short_path_pair(f):
    type = file_type(f)
    if f.short_path.startswith("../"):
        return "{}\0{}{}".format(f.short_path[3:], type, f.path)
    return "_main/{}\0{}{}".format(f.short_path, type, f.path)

def root_symlinks_arg(x):
    type = file_type(x.target_file)
    return "{}\0{}{}".format(x.path, type, x.target_file.path)

def symlinks_arg(x):
    type = file_type(x.target_file)
    return "_main/{}\0{}{}".format(x.path, type, x.target_file.path)

def _rebase_short_path(f):
    """Rebase a File's short_path into the unified runfiles-tree namespace.

    Main-repo files are placed under "_main/", external-repo files (whose
    short_path starts with "../") keep their repo name. This mirrors the layout
    produced by to_short_path_pair for runfiles.
    """
    if f.short_path.startswith("../"):
        return f.short_path[3:]
    return "_main/{}".format(f.short_path)

def _place_files_header(mode, dest, anchor, skip):
    """Build the header line for a --place-files parameter file.

    The header carries the per-target placement context that the Go tool needs
    to resolve each file's final path. It cannot be baked into the per-file
    lines because those are produced lazily by a map_each callback, which may
    not capture rule context. Fields are null-separated: mode, dest, anchor,
    skip. See placement.go for the consuming side.
    """
    return "{}\0{}\0{}\0{}".format(mode, dest, anchor, skip)

def place_extra_executable_files(ctx, files, exe, path_in_image, extra_args, extra_inputs):
    """Lazily place a target's default outputs other than the executable.

    Each file is placed at the same offset it has relative to the executable's
    directory, so that with the executable anchored at path_in_image, sidecar
    files keep their relative layout. The executable itself is skipped. Files
    above the executable's directory are allowed as long as they stay under the
    image root (enforced by the Go tool at execution time).

    The depset is streamed via add_all(map_each=...) and never flattened in
    Starlark.

    Args:
        ctx: rule context.
        files: depset of File objects (the target's default outputs).
        exe: the executable File, anchored at path_in_image.
        path_in_image: normalized (no leading "/") tar path of the executable.
        extra_args: list to append the ctx.actions.args() object to.
        extra_inputs: list of depsets; `files` is appended so the outputs are
            available to the action.
    """
    args = ctx.actions.args()
    args.set_param_file_format("multiline")
    args.use_param_file("--place-files=%s", use_always = True)
    args.add(_place_files_header(
        "relative",
        paths.dirname(path_in_image),
        paths.dirname(_rebase_short_path(exe)),
        _rebase_short_path(exe),
    ))
    args.add_all(files, map_each = to_short_path_pair, expand_directories = False, uniquify = True)
    extra_args.append(args)
    extra_inputs.append(files)

def place_non_executable_files(ctx, files, label, path_in_image, layout, extra_args, extra_inputs):
    """Lazily place a non-executable target's default outputs.

    A single output is placed exactly at path_in_image. Multiple outputs treat
    path_in_image as a directory: "package_relative" preserves each file's path
    relative to the producing target's package, while "flatten" places each file
    directly in the directory by basename. The single-output case is resolved by
    the Go tool, so the depset is never flattened in Starlark.

    Args:
        ctx: rule context.
        files: depset of File objects (the target's default outputs).
        label: the Label of the producing target (used as the package anchor).
        path_in_image: normalized (no leading "/") tar path.
        layout: "package_relative" or "flatten".
        extra_args: list to append the ctx.actions.args() object to.
        extra_inputs: list of depsets; `files` is appended so the outputs are
            available to the action.
    """
    pkg_anchor = "_main" if label.workspace_name == "" else label.workspace_name
    if label.package:
        pkg_anchor = "{}/{}".format(pkg_anchor, label.package)
    mode = "flatten" if layout == "flatten" else "package_relative"
    args = ctx.actions.args()
    args.set_param_file_format("multiline")
    args.use_param_file("--place-files=%s", use_always = True)
    args.add(_place_files_header(mode, path_in_image, pkg_anchor, ""))
    args.add_all(files, map_each = to_short_path_pair, expand_directories = False, uniquify = True)
    extra_args.append(args)
    extra_inputs.append(files)

def get_files_to_run_provider(src):
    """Retrieve FilesToRunProvider from a target.

    Args:
        src: target to get FilesToRunProvider from

    Returns:
        FilesToRunProvider or None: FilesToRunProvider if found in target
            provider, otherwise None
    """
    if not DefaultInfo in src:
        return None
    default_info = src[DefaultInfo]
    if not hasattr(default_info, "files_to_run"):
        return None
    return default_info.files_to_run

def get_repo_mapping_manifest(src):
    """Retrieve repo_mapping_manifest from a target if it exists.

    Args:
        src: target to get repo_mapping_manifest from

    Returns:
        File or None: repo_mapping_manifest
    """
    files_to_run_provider = get_files_to_run_provider(src)
    if files_to_run_provider:
        return getattr(files_to_run_provider, "repo_mapping_manifest")
    return None

def resolve_layer_settings(ctx):
    """Resolve layer settings from common attributes and build settings.

    Resolves 'auto' values for compression, estargz, create_parent_directories,
    and tree_artifact_handling from global build settings. Computes derived values
    (media type and output file extension) based on the resolved compression.

    Args:
        ctx: Rule context. Must have attrs from layer_attrs.common.

    Returns:
        struct with fields: compression, estargz, create_parent_directories,
        tree_artifact_handling, media_type, out_ext.
    """
    compression = ctx.attr.compress
    if compression == "auto":
        compression = ctx.attr._default_compression[BuildSettingInfo].value

    estargz = ctx.attr.estargz
    if estargz == "auto":
        estargz = ctx.attr._default_estargz[BuildSettingInfo].value
    estargz_enabled = estargz == "enabled"

    create_parent_directories = ctx.attr.create_parent_directories
    if create_parent_directories == "auto":
        create_parent_directories = ctx.attr._default_create_parent_directories[BuildSettingInfo].value
    create_parent_directories_enabled = create_parent_directories == "enabled"

    tree_artifact_handling = ctx.attr.tree_artifact_handling
    if tree_artifact_handling == "auto":
        tree_artifact_handling = ctx.attr._default_tree_artifact_handling[BuildSettingInfo].value

    if compression == "gzip":
        out_ext = ".tgz"
        media_type = "application/vnd.oci.image.layer.v1.tar+gzip"
    elif compression == "zstd":
        out_ext = ".tar.zst"
        media_type = "application/vnd.oci.image.layer.v1.tar+zstd"
    else:
        fail("Unsupported compression: {}".format(compression))

    if ctx.attr.media_type:
        media_type = ctx.attr.media_type

    return struct(
        compression = compression,
        estargz = estargz_enabled,
        create_parent_directories = create_parent_directories_enabled,
        tree_artifact_handling = tree_artifact_handling,
        media_type = media_type,
        out_ext = out_ext,
    )

def create_tar_single_layer(ctx, settings, name, extra_args = [], extra_inputs = []):
    """Create a single tar layer using 'img layer'.

    Lower-level variant of create_tar_layer that accepts an explicit output
    name and returns the layer components directly, allowing the caller to
    assemble providers from multiple layers.

    Args:
        ctx: Rule context. Must have attrs from layer_attrs.common.
        settings: struct returned by resolve_layer_settings().
        name: Base name for output files (e.g. "app_layer_stdlib").
        extra_args: list of strings and/or ctx.actions.args() objects to insert
            between the base arguments and the output path.
        extra_inputs: list of depset objects to merge with base inputs.

    Returns:
        tuple of (SingleLayerInfo, out_file, metadata_file).
    """
    out = ctx.actions.declare_file(name + settings.out_ext)
    metadata_out = ctx.actions.declare_file(name + "_metadata.json")

    args = ["layer", "--name", str(ctx.label), "--metadata", metadata_out.path, "--format", settings.compression]
    if ctx.attr.media_type:
        args.extend(["--media-type", ctx.attr.media_type])
    args.extend(compression_tuning_args(ctx, settings.compression, settings.estargz))
    if settings.estargz:
        args.append("--estargz")
    if settings.create_parent_directories:
        args.append("--create-parent-directories")
    args.extend(["--layer-tree-artifact-handling", settings.tree_artifact_handling])
    for key, value in ctx.attr.annotations.items():
        args.extend(["--annotation", "{}={}".format(key, value)])
    if ctx.attr.annotations_file != None:
        args.extend(["--annotations-file", ctx.file.annotations_file.path])

    args.extend(extra_args)
    args.append(out.path)

    inputs = []
    if ctx.attr.annotations_file != None:
        inputs.append(depset([ctx.file.annotations_file]))
    inputs.extend(extra_inputs)

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        outputs = [out, metadata_out],
        inputs = depset(transitive = inputs),
        executable = img_toolchain_info.tool_exe,
        arguments = args,
        mnemonic = "LayerTar",
    )
    return (
        SingleLayerInfo(
            blob = out,
            metadata = metadata_out,
            media_type = settings.media_type,
            estargz = settings.estargz,
        ),
        out,
        metadata_out,
    )

def create_tar_layer(ctx, settings, extra_args = [], extra_inputs = []):
    """Create a tar layer using 'img layer' and return providers.

    Declares output files, builds base arguments for the 'img layer' command,
    appends rule-specific extra_args, runs the action with mnemonic 'LayerTar',
    and returns a list of providers.

    Args:
        ctx: Rule context. Must have attrs from layer_attrs.common.
        settings: struct returned by resolve_layer_settings().
        extra_args: list of strings and/or ctx.actions.args() objects to insert
            between the base arguments and the output path.
        extra_inputs: list of depset objects to merge with base inputs.

    Returns:
        list of [DefaultInfo, OutputGroupInfo, LayersInfo].
    """
    layer_info, out, metadata_out = create_tar_single_layer(ctx, settings, ctx.attr.name, extra_args, extra_inputs)
    return [
        DefaultInfo(files = depset([out])),
        OutputGroupInfo(
            layer = depset([out]),
            metadata = depset([metadata_out]),
        ),
        LayersInfo(layers = [layer_info]),
    ]
