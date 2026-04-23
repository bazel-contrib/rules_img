"""Reusable functions for creating tar-based container image layers."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private/common:build.bzl", "TOOLCHAIN")
load("//img/private/common:layer_helper.bzl", "compression_tuning_args")
load("//img/private/providers:layer_info.bzl", "LayerInfo")

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
        list of [DefaultInfo, OutputGroupInfo, LayerInfo].
    """
    out = ctx.actions.declare_file(ctx.attr.name + settings.out_ext)
    metadata_out = ctx.actions.declare_file(ctx.attr.name + "_metadata.json")

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
    return [
        DefaultInfo(files = depset([out])),
        OutputGroupInfo(
            layer = depset([out]),
            metadata = depset([metadata_out]),
        ),
        LayerInfo(
            blob = out,
            metadata = metadata_out,
            media_type = settings.media_type,
            estargz = settings.estargz,
        ),
    ]
