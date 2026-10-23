"""Helpers to reference a layer blob from inside a tree artifact."""

load(":build.bzl", "TOOLCHAIN")

def copy_blobs_to_output_base(ctx, pairs, mnemonic):
    """Copy each (source, destination) pair with a single `img copy-files` action.

    The copies are hardlinked, reflinked or, if the filesystem allows neither,
    really copied.

    Args:
        ctx: Rule context.
        pairs: List of (src, dst) File tuples.
        mnemonic: Action mnemonic string.
    """
    args = ctx.actions.args()
    args.add("copy-files")

    paths = ctx.actions.args()
    paths.set_param_file_format("multiline")
    paths.use_param_file("--from-file=%s", use_always = True)
    for (src, dst) in pairs:
        paths.add(src)
        paths.add(dst)

    ctx.actions.run(
        inputs = [src for (src, _) in pairs],
        outputs = [dst for (_, dst) in pairs],
        executable = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo.tool_exe,
        arguments = [args, paths],
        mnemonic = mnemonic,
    )

def generated_blob_or_none(ctx, blob, *, name):
    """Returns a copy of `blob` in the output base, or None if it needs no copy.

    A source file cannot be referenced from a tree artifact (see
    `blob_for_layout`), so rules that hand out one as a layer blob record a copy
    of it as `SingleLayerInfo.generated_blob`. Declaring the copy is free: the
    action only runs when something that needs it -- a layout with uplevel
    symlinks -- is actually built.

    Args:
        ctx: Rule context.
        blob: The layer blob File, or None.
        name: Name of the directory to put the copy in, unique within the rule.

    Returns:
        A File holding the copy, or None if `blob` is absent or already generated.
    """
    if blob == None or not blob.is_source:
        return None
    copy = ctx.actions.declare_file("{}/{}".format(name, blob.basename))
    copy_blobs_to_output_base(ctx, [(blob, copy)], "CopyLayerBlob")
    return copy

def layout_uses_uplevel_symlinks(ctx, format):
    """Whether a layout of this format references blobs outside the tree by symlink.

    A tar layout always embeds its blobs, and so does a directory layout on a
    toolchain that does not permit uplevel symlinks in a tree artifact.

    Args:
        ctx: Rule context.
        format: Output format, either "directory" or "tar".

    Returns:
        True if the layout will be written with `--symlink`.
    """
    return format == "directory" and ctx.toolchains[TOOLCHAIN].imgtoolchaininfo.supports_treeartifact_uplevel_symlinks

def blob_for_layout(ctx, layer, format):
    """Returns the file to use as a layer's blob inside a tree artifact layout.

    Tree artifact layouts reference blobs that live outside the tree with a
    relative symlink (where the toolchain allows it). Bazel does not track the
    target of such a symlink, so it only resolves within the execroot of the
    build that created it -- and for a source file that path does not exist at
    all once the tree is materialized somewhere else (as it is with
    `--experimental_remote_output_service` or
    `--experimental_remote_repo_contents_cache`). Rules whose blob is a source
    file therefore copy it into the output base and record the copy as
    `SingleLayerInfo.generated_blob`; see `image_import`.

    A layout that embeds its blobs reads them wherever they are, so it keeps
    using `blob` -- and the copy, being requested by nothing, never runs.

    Args:
        ctx: Rule context.
        layer: SingleLayerInfo provider.
        format: Output format of the layout, either "directory" or "tar".

    Returns:
        The generated copy of the layer's blob, the blob itself when there is no
        copy or none is needed, or None for a layer whose blob is unavailable
        (shallow layers).
    """
    blob = layer.blob
    if not layout_uses_uplevel_symlinks(ctx, format):
        return blob

    generated_blob = getattr(layer, "generated_blob", None)
    if generated_blob != None:
        return generated_blob

    if blob != None and blob.is_source:
        fail("""layer blob {} is a source file, which cannot soundly be referenced from a tree artifact.

The layout would point at the file with a relative symlink, and Bazel neither tracks nor materializes that symlink's target, so the layout only resolves in the execroot of the build that created it.

A rule providing a source file as a layer blob has to copy it into the output base and record the copy as SingleLayerInfo.generated_blob, the way image_import does. Please report this at https://github.com/bazel-contrib/rules_img/issues.""".format(blob.path))

    return blob
