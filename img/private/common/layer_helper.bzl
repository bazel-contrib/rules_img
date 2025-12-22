"""Helper functions for working with tar files."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private/common:build.bzl", "TOOLCHAIN")
load("//img/private/providers:layer_info.bzl", "LayerInfo")

allow_tar_files = [".tar", ".tar.gz", ".tgz", ".tar.zst", ".tzst"]

extension_to_compression = {
    "tar": "none",
    "gz": "gzip",
    "tar.gz": "gzip",
    "tgz": "gzip",
    "zst": "zstd",
    "tar.zst": "zstd",
    "tzst": "zstd",
}

def compression_tuning_args(ctx, compression, estargz):
    """Compression tuning arguments for img tools based on build mode.

    Returns additional CLI arguments to tune gzip compression defaults
    according to Bazel's compilation mode. This
    function prefers faster, parallel compression in fastbuild, and
    smaller, single-threaded high-compression in opt. Other compression
    algorithms are left unchanged.

    Args:
        ctx: Rule context used to read `COMPILATION_MODE`.
        compression: String name of the target compression algorithm
            (e.g., "gzip", "zstd", "none").
        estargz: Boolean indicating whether the layer is an estargz layer.

    Returns:
        list[string]: Flat list of flags and values, suitable for
        `ctx.actions.args().add_all(...)`.
    """

    valid_levels = {
        "gzip": ["-1", "0", "1", "2", "3", "4", "5", "6", "7", "8", "9"],
        "zstd": ["1", "2", "3", "4"],
    }

    # Start with mode-based defaults
    mode = ctx.var.get("COMPILATION_MODE", "fastbuild")
    jobs = "nproc" if mode != "opt" else "1"
    default_level = "-1"  # default compression (no flag set)
    max_level = "9" if compression == "gzip" else "4"
    level = default_level
    if mode == "opt":
        level = max_level  # best compression
    elif mode == "fastbuild":
        level = "1"  # faster, lower compression

    # Apply global overrides if present as hidden attrs
    if hasattr(ctx.attr, "_compression_jobs") and ctx.attr._compression_jobs != None:
        val = ctx.attr._compression_jobs[BuildSettingInfo].value
        if val and val != "auto":
            jobs = val
    if hasattr(ctx.attr, "_compression_level") and ctx.attr._compression_level != None:
        lvl = ctx.attr._compression_level[BuildSettingInfo].value
        if lvl and lvl != "auto":
            level = lvl

    if level == "fastest":
        level = "1"
    elif level == "best":
        level = max_level

    tuned_args = []
    if compression == "gzip" and not estargz:
        # For gzip, we can tune the number of compression threads (pgzip)
        tuned_args.extend(["--compressor-jobs", jobs])
    if level != "-1" and compression != "none":
        if level not in valid_levels[compression]:
            fail("Invalid compression level {} for {}".format(level, compression))
        tuned_args.extend(["--compression-level", level])
    return tuned_args

def calculate_layer_info(*, ctx, media_type, tar_file, metadata_file, estargz, annotations = {}):
    """Calculates the layer info for a tar file.

    Args:
        ctx: Rule context.
        media_type: Media type of the layer.
        tar_file: Input tar file.
        metadata_file: Output metadata file.
        estargz: Boolean indicating whether the layer is an estargz layer.
        annotations: Dict of string annotations to add to the layer metadata.

    Returns:
        LayerInfo provider with blob, metadata, and media type.
    """
    args = ctx.actions.args()
    args.add("--digest=sha256")
    args.add("--encoding=layer-metadata")
    args.add("--name", ctx.label)
    for key, value in annotations.items():
        args.add("--annotation", "{}={}".format(key, value))
    args.add(tar_file)
    args.add(metadata_file)
    args.set_param_file_format("multiline")
    args.use_param_file("@%s", use_always = True)

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = [tar_file],
        outputs = [metadata_file],
        executable = img_toolchain_info.tool_exe,
        arguments = ["hash", args],
        mnemonic = "LayerMetadata",
        execution_requirements = {
            "requires-worker-protocol": "json",
            "supports-workers": "1",
            "supports-multiplex-workers": "1",
            "supports-multiplex-sandboxing": "1",
            "supports-worker-cancellation": "1",
            "supports-path-mapping": "1",
        },
    )
    return LayerInfo(
        blob = tar_file,
        metadata = metadata_file,
        media_type = media_type,
        estargz = estargz,
    )

def recompress_layer(*, ctx, media_type, tar_file, metadata_file, output, target_compression, estargz, annotations):
    """Recompresses a tar file.

    Args:
        ctx: Rule context.
        media_type: Media type of the layer.
        tar_file: Input tar file.
        metadata_file: Input metadata file.
        output: Output recompressed file.
        target_compression: Target compression format.
        estargz: Boolean indicating whether the layer is an estargz layer.
        annotations: Dict of string annotations to add to the layer metadata.

    Returns:
        LayerInfo provider with recompressed blob and metadata.
    """
    args = ctx.actions.args()
    args.add("compress")
    args.add("--name", ctx.label)
    args.add("--format", target_compression)
    if estargz:
        args.add("--estargz")
    for key, value in annotations.items():
        args.add("--annotation", "{}={}".format(key, value))
    args.add("--metadata", metadata_file.path)
    args.add_all(compression_tuning_args(ctx, target_compression, estargz))
    args.add(tar_file.path)
    args.add(output)
    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = [tar_file],
        outputs = [output, metadata_file],
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        mnemonic = "LayerCompress",
    )
    return LayerInfo(
        blob = output,
        metadata = metadata_file,
        media_type = media_type,
        estargz = estargz,
    )

def optimize_layer(*, ctx, media_type, tar_file, metadata_file, output, target_compression, estargz, annotations):
    """Optimizes a tar file.

    Args:
        ctx: Rule context.
        media_type: Media type of the layer.
        tar_file: Input tar file.
        metadata_file: Input metadata file.
        output: Output optimized file.
        target_compression: Target compression format.
        estargz: Boolean indicating whether the layer is an estargz layer.
        annotations: Dict of string annotations to add to the layer metadata.

    Returns:
        LayerInfo provider with optimized blob and metadata.
    """
    inputs = [tar_file]
    args = ctx.actions.args()
    args.add("layer")
    args.add("--name", ctx.attr.name)
    args.add("--format", target_compression)
    if estargz:
        args.add("--estargz")
    for key, value in annotations.items():
        args.add("--annotation", "{}={}".format(key, value))
    args.add("--metadata", metadata_file.path)
    args.add("--import-tar", tar_file.path)
    args.add_all(compression_tuning_args(ctx, target_compression, estargz))
    args.add(output)
    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = depset(inputs),
        outputs = [output, metadata_file],
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        mnemonic = "LayerOptimize",
    )
    return LayerInfo(
        blob = output,
        metadata = metadata_file,
        media_type = media_type,
        estargz = estargz,
    )
