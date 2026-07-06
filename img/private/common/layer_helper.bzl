"""Helper functions for working with tar files."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private/common:build.bzl", "TOOLCHAIN")
load("//img/private/providers:single_layer_info.bzl", "SingleLayerInfo")

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

# Hidden build-setting attributes an image rule must expose to compute an
# image-level mtree via image_mtree_or_none (path prefix and field set are shared
# with the per-layer mtree; layer/image layouts are separate). Spread this into a
# rule's `attrs`.
IMAGE_MTREE_ATTRS = {
    "_mtree_path_prefix": attr.label(
        default = Label("//img/settings:mtree_path_prefix"),
        providers = [BuildSettingInfo],
    ),
    "_mtree_options": attr.label(
        default = Label("//img/settings:mtree_options"),
        providers = [BuildSettingInfo],
    ),
    "_mtree_layer_layout": attr.label(
        default = Label("//img/settings:mtree_layer_layout"),
        providers = [BuildSettingInfo],
    ),
    "_mtree_image_layout": attr.label(
        default = Label("//img/settings:mtree_image_layout"),
        providers = [BuildSettingInfo],
    ),
}

def layer_name(label):
    """Returns a label's string form for use as a layer's history created_by.

    For a target in the main repository, Bazel's canonical label string carries a
    leading canonical-repository marker before the "//"; this strips that marker so
    the name reads like "//pkg:target". Labels from external repositories (whose
    canonical repository name is non-empty) are returned unchanged.

    Args:
        label: The Label (or value convertible via str()) to format.

    Returns:
        The label string with any leading main-repo canonical marker removed.
    """

    # Built by concatenation to avoid a literal canonical-repository token, which
    # buildifier's canonical-repository check flags.
    canonical_marker = "@" + "@"
    name = str(label)
    if name.startswith(canonical_marker + "//"):
        name = name.removeprefix(canonical_marker)
    return name

def layer_history(name):
    """Returns the created_by string recorded in a Bazel-built layer's history.

    The img tool records the value of its `--history` flag verbatim as the layer's
    history entry; the "bazel build" prefix is assembled here rather than in the
    tool so the tool stays agnostic about how the layer was produced.

    Args:
        name: Human-readable layer name, typically `layer_name(ctx.label)`.

    Returns:
        The string "bazel build <name>".
    """
    return "bazel build " + name

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

def build_layer_mtree(ctx, name, *, tar_blob = None, compact_stream = None):
    """Produce an mtree spec describing the metadata of a tar layer.

    Runs `img mtree` over either a materialized (possibly compressed) tar blob or
    a compact stream, writing a single mtree text file describing the tar entries.
    The path layout, included fields, and layer layout are controlled by the
    //img/settings:mtree_path_prefix, :mtree_options, and :mtree_layer_layout
    build settings (read from ctx.attr, so the rule must expose the matching
    _mtree_* hidden attributes).

    Exactly one of tar_blob or compact_stream must be set.

    Args:
        ctx: Rule context.
        name: Base name for the output file (the result is name + ".mtree").
        tar_blob: A layer tar File (optionally gzip/zstd compressed).
        compact_stream: A compact stream (.cstream) File.

    Returns:
        The generated mtree File.
    """
    if (tar_blob == None) == (compact_stream == None):
        fail("build_layer_mtree requires exactly one of tar_blob or compact_stream")

    mtree_out = ctx.actions.declare_file(name + ".mtree")
    args = ctx.actions.args()
    args.add("mtree")
    if tar_blob != None:
        args.add("--tar", tar_blob)
        input = tar_blob
    else:
        args.add("--cstream", compact_stream)
        input = compact_stream
    args.add("--output", mtree_out)
    args.add("--path-prefix", ctx.attr._mtree_path_prefix[BuildSettingInfo].value)
    args.add("--options", ctx.attr._mtree_options[BuildSettingInfo].value)
    args.add("--layout", ctx.attr._mtree_layer_layout[BuildSettingInfo].value)

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        outputs = [mtree_out],
        inputs = [input],
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        mnemonic = "LayerMtree",
    )
    return mtree_out

def build_image_mtree(ctx, name, mtree_files):
    """Merge per-layer mtree specs into a single image-level mtree.

    Runs `img mtree` over the given per-layer mtree files (in layer order),
    producing one combined mtree describing the assembled image. The path layout
    and included fields reuse the //img/settings:mtree_path_prefix and
    :mtree_options build settings (shared with the layer mtree); the merged layout
    is controlled by //img/settings:mtree_image_layout. Settings are read from
    ctx.attr, so the rule must expose the matching _mtree_path_prefix,
    _mtree_options, and _mtree_image_layout hidden attributes.

    Notes on the merge:
    - The "oci_layer_filesystem_applied_changeset" layout applies whiteouts across
      layers, so it relies on the per-layer mtree files retaining their whiteout
      markers -- i.e. //img/settings:mtree_layer_layout should be "tar" (its
      default). If the per-layer files were themselves rendered as applied
      changesets, their whiteouts are already consumed and lower-layer files they
      were meant to remove will incorrectly survive in the merged output.
    - Per-layer nlink (hardlink) counts are carried through verbatim; they are not
      recomputed across the layer boundary.

    Args:
        ctx: Rule context.
        name: Base name for the output file (the result is name + ".mtree").
        mtree_files: Ordered (layer-order) list of per-layer mtree Files to merge.
            Must be non-empty; `img mtree` requires at least one input.

    Returns:
        The generated image mtree File.
    """
    if len(mtree_files) == 0:
        fail("build_image_mtree requires at least one layer mtree file")

    mtree_out = ctx.actions.declare_file(name + ".mtree")
    args = ctx.actions.args()
    args.add("mtree")
    args.add_all(mtree_files, before_each = "--mtree")
    args.add("--output", mtree_out)
    args.add("--path-prefix", ctx.attr._mtree_path_prefix[BuildSettingInfo].value)
    args.add("--options", ctx.attr._mtree_options[BuildSettingInfo].value)
    args.add("--layout", ctx.attr._mtree_image_layout[BuildSettingInfo].value)

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        outputs = [mtree_out],
        inputs = mtree_files,
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        mnemonic = "ImageMtree",
    )
    return mtree_out

def media_type_is_tar(media_type):
    """Whether a layer blob with this media type can be rendered to an mtree.

    Every standard OCI/Docker tar layer media type carries a "tar" token
    (`...tar`, `...tar+gzip`, `...tar+zstd`, `...rootfs.diff.tar.gzip`). Blobs
    without it -- empty layers (`application/vnd.oci.empty.v1+json`), non-tar
    artifact layers, or an "unknown"/absent media type -- are not tars and are
    skipped.
    """
    return media_type != None and "tar" in media_type

def image_layer_mtrees(ctx, layers, name_prefix = None):
    """Ordered per-layer mtree Files for an image, building missing ones on the fly.

    For each layer (in image/layer order):
    - if it already carries an `mtree` (produced by a rules_img layer rule, or by
      the pull/import rule for a blob-bearing layer), use it;
    - else if it has a materialized tar blob (`blob` set and a tar media type),
      render an mtree from that blob on the fly with build_layer_mtree -- this
      covers raw tars added via `DefaultInfo` and any other blob-bearing tar layer
      whose source did not precompute one;
    - else skip it (a shallow/lazy layer with no blob, or a non-tar blob such as an
      empty layer or a non-tar artifact).

    Skips are best-effort: if any layer is skipped the merged mtree represents only
    a subset of the image. The rule must expose the same hidden _mtree_* attributes
    build_layer_mtree reads (path prefix, options, and layer layout).

    Args:
        ctx: Rule context of an image rule.
        layers: Ordered list of SingleLayerInfo providers.
        name_prefix: Optional base name for on-the-fly per-layer mtree files
            (defaults to ctx.label.name). A rule that builds several manifests
            (e.g. an image index) must pass a per-manifest-unique prefix so the
            declared `<prefix>_layer_<i>.mtree` files do not collide.

    Returns:
        Ordered list of per-layer mtree Files (possibly shorter than `layers`).
    """
    prefix = name_prefix if name_prefix != None else ctx.label.name
    mtrees = []
    for i, layer in enumerate(layers):
        if layer.mtree != None:
            mtrees.append(layer.mtree)
        elif layer.blob != None and media_type_is_tar(layer.media_type):
            mtrees.append(build_layer_mtree(ctx, "{}_layer_{}".format(prefix, i), tar_blob = layer.blob))
    return mtrees

def image_mtree_or_none(ctx, name, layers):
    """Merge an image's per-layer mtrees into a single image mtree, or None.

    Reuses each layer's precomputed mtree and renders any missing one on the fly
    from the layer's tar blob (see image_layer_mtrees), then merges them in layer
    order with build_image_mtree. Returns None when no layer contributes an mtree
    (e.g. every layer is shallow or non-tar). The rule must expose IMAGE_MTREE_ATTRS
    and the img toolchain; `name` must be unique per manifest (it names the merged
    output and prefixes any on-the-fly per-layer files).

    Args:
        ctx: Rule context of an image rule.
        name: Base name for the merged mtree (and the per-layer mtree prefix).
        layers: Ordered list of SingleLayerInfo providers.

    Returns:
        The merged image mtree File, or None.
    """
    layer_mtrees = image_layer_mtrees(ctx, layers, name_prefix = name)
    if len(layer_mtrees) == 0:
        return None
    return build_image_mtree(ctx, name, layer_mtrees)

def calculate_layer_info(*, ctx, media_type, tar_file, metadata_file, estargz, annotations = {}, digest_modes = ["digest", "diff_id"], mtree = None):
    """Calculates the layer info for a file.

    Args:
        ctx: Rule context.
        media_type: Media type of the layer.
        tar_file: Input file (tar or arbitrary blob).
        metadata_file: Output metadata file.
        estargz: Boolean indicating whether the layer is an estargz layer.
        annotations: Dict of string annotations to add to the layer metadata.
        digest_modes: List of digest modes to compute. Supported modes:
            "digest" - sha256 of the file as-is (the blob digest).
            "diff_id" - sha256 of the uncompressed content (the OCI diff ID).
            "diff_id_annotation:<name>" - same as diff_id but stored as annotation <name>.
        mtree: Optional mtree File to record on the returned SingleLayerInfo. This
            function does not build it, because the input may be an arbitrary
            (non-tar) blob; tar-based callers build it (see build_layer_mtree) and
            pass it here.

    Returns:
        SingleLayerInfo provider with blob, metadata, and media type.
    """
    args = ctx.actions.args()
    args.add("--digest=sha256")
    args.add("--encoding=layer-metadata")
    args.add("--history", layer_history(layer_name(ctx.label)))
    args.add("--media-type", media_type)
    for mode in digest_modes:
        args.add("--digest-mode", mode)
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
    return SingleLayerInfo(
        blob = tar_file,
        metadata = metadata_file,
        media_type = media_type,
        estargz = estargz,
        compact_stream = None,
        layer_input_files = None,
        layer_input_files_cas = None,
        sources = [],
        mtree = mtree,
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
        SingleLayerInfo provider with recompressed blob and metadata.
    """
    args = ctx.actions.args()
    args.add("compress")
    args.add("--history", layer_history(layer_name(ctx.label)))
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
    return SingleLayerInfo(
        blob = output,
        metadata = metadata_file,
        media_type = media_type,
        estargz = estargz,
        compact_stream = None,
        layer_input_files = None,
        layer_input_files_cas = None,
        sources = [],
        mtree = build_layer_mtree(ctx, ctx.label.name, tar_blob = output),
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
        SingleLayerInfo provider with optimized blob and metadata.
    """
    inputs = [tar_file]
    args = ctx.actions.args()
    args.add("layer")
    args.add("--history", layer_history(ctx.attr.name))
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
    return SingleLayerInfo(
        blob = output,
        metadata = metadata_file,
        media_type = media_type,
        estargz = estargz,
        compact_stream = None,
        layer_input_files = None,
        layer_input_files_cas = None,
        sources = [],
        mtree = build_layer_mtree(ctx, ctx.label.name, tar_blob = output),
    )
