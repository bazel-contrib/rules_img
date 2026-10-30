"""Helper functions for working with tar files."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private/common:build.bzl", "TOOLCHAIN")
load("//img/private/common:layout_blob.bzl", "generated_blob_or_none")
load("//img/private/common:media_types.bzl", "media_type_is_erofs")
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

# Hidden build-setting attributes a rule must expose to render a per-layer mtree
# via build_layer_mtree / layer_mtree_or_none. Spread this into a rule's (or
# aspect's) `attrs`.
LAYER_MTREE_ATTRS = {
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
}

# Hidden build-setting attributes an image rule must expose to compute an
# image-level mtree via image_mtree_or_none (path prefix and field set are shared
# with the per-layer mtree; layer/image layouts are separate). Spread this into a
# rule's `attrs`.
IMAGE_MTREE_ATTRS = dict(
    LAYER_MTREE_ATTRS,
    _mtree_image_layout = attr.label(
        default = Label("//img/settings:mtree_image_layout"),
        providers = [BuildSettingInfo],
    ),
)

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

# Sentinel default for the `history` attribute: distinguishes "unset" from an
# explicit `history = []`, mirroring INHERIT_FROM_BASE in //img/private/common:inherit.bzl.
_HISTORY_ATTR_DEFAULT = ["<default bazel build history>"]

def history_attr():
    """Returns an overridable `history` attribute for a layer-producing rule."""
    return attr.string_list(
        default = _HISTORY_ATTR_DEFAULT,
        doc = """\
Overrides the `created_by` history entry recorded for this layer.

By default the layer records one entry, "bazel build <label>", so two layers
with identical contents but different labels have different config digests.
`history = []` records no history entry, so identical contents give identical
config digests regardless of label. A single-element list is recorded verbatim.
The img tool records at most one history entry per layer, so longer lists are
rejected.
""",
    )

def layer_history_args(ctx):
    """Returns the `["--history", value]` argv fragment for the img tool.

    Rules without a `history` attribute get the default "bazel build <label>"
    entry automatically.

    Args:
        ctx: Rule context.

    Returns:
        Two-element list: `["--history", value]`.
    """
    history = getattr(ctx.attr, "history", _HISTORY_ATTR_DEFAULT)
    if history == _HISTORY_ATTR_DEFAULT:
        return ["--history", layer_history(layer_name(ctx.label))]
    if len(history) > 1:
        fail("history supports at most one entry (the img tool records a single --history value); got {}".format(history))
    return ["--history", history[0] if history else ""]

def resolve_tar_format(ctx, compression, erofs = False):
    """Resolve the estargz / zstd_chunked attributes into a tar format.

    Both attributes default to "auto", which defers to the matching global
    build setting. The two formats are mutually exclusive and zstd:chunked
    requires zstd compression; violating either is a configuration error.
    Both are tar formats: an EROFS layer resolves "auto" to disabled and
    rejects an explicit "enabled".

    Args:
        ctx: Rule context with `estargz`, `zstd_chunked`, `_default_estargz`
            and `_default_zstd_chunked` attributes.
        compression: Resolved compression algorithm name.
        erofs: Whether the layer is an EROFS filesystem image.

    Returns:
        struct with boolean fields `estargz` and `zstd_chunked`.
    """
    estargz = ctx.attr.estargz
    if estargz == "auto":
        estargz = "disabled" if erofs else ctx.attr._default_estargz[BuildSettingInfo].value
    estargz_enabled = estargz == "enabled"

    zstd_chunked = ctx.attr.zstd_chunked
    if zstd_chunked == "auto":
        zstd_chunked = "disabled" if erofs else ctx.attr._default_zstd_chunked[BuildSettingInfo].value
    zstd_chunked_enabled = zstd_chunked == "enabled"

    if erofs and estargz_enabled:
        fail("estargz is a tar format and cannot be combined with format = \"erofs\".")
    if erofs and zstd_chunked_enabled:
        fail("zstd_chunked is a tar format and cannot be combined with format = \"erofs\".")
    if estargz_enabled and zstd_chunked_enabled:
        fail("estargz and zstd_chunked are mutually exclusive; set one of them to \"disabled\" (attribute or //img/settings flag)")
    if zstd_chunked_enabled and compression != "zstd":
        fail("zstd_chunked requires zstd compression, got {}".format(compression))

    return struct(
        estargz = estargz_enabled,
        zstd_chunked = zstd_chunked_enabled,
    )

def tar_format_args(tar_format):
    """Returns the img tool flags selecting the tar stream format.

    Args:
        tar_format: struct returned by resolve_tar_format().

    Returns:
        list[string]: `["--estargz"]`, `["--zstd-chunked"]` or `[]`.
    """
    if tar_format.estargz:
        return ["--estargz"]
    if tar_format.zstd_chunked:
        return ["--zstd-chunked"]
    return []

def compression_tuning_args(ctx, compression, tar_format):
    """Compression tuning arguments for img tools based on build mode.

    Returns additional CLI arguments to tune compression defaults
    according to Bazel's compilation mode. This
    function prefers faster, parallel compression in fastbuild, and
    smaller, single-threaded high-compression in opt. For gzip the job count
    also selects the compressor implementation; zstd parallelizes a single
    stream without changing its output, so it defaults to 2 jobs in every mode
    and takes a configured job count as-is. estargz and zstd:chunked layers
    are never parallelized, so they get no job count.

    A compression_level outside the target codec's range is ignored for that
    codec, which keeps its mode default.

    Args:
        ctx: Rule context used to read `COMPILATION_MODE`.
        compression: String name of the target compression algorithm
            (e.g., "gzip", "zstd", "none").
        tar_format: struct returned by resolve_tar_format().

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
    jobs_explicit = False
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
            jobs_explicit = True
    if hasattr(ctx.attr, "_compression_level") and ctx.attr._compression_level != None:
        lvl = ctx.attr._compression_level[BuildSettingInfo].value

        # compression_level is shared by gzip and zstd, which accept different
        # ranges. A level outside this codec's range (for example gzip's 6 on a
        # zstd layer) keeps the mode default, so one setting can be tuned for
        # gzip without breaking zstd layers.
        out_of_range = compression in valid_levels and lvl not in valid_levels[compression] + ["fastest", "best"]
        if lvl and lvl != "auto" and not out_of_range:
            level = lvl

    if level == "fastest":
        level = "1"
    elif level == "best":
        level = max_level

    tuned_args = []
    single_stream = tar_format.estargz or tar_format.zstd_chunked
    if compression == "gzip" and not single_stream:
        # For gzip, we can tune the number of compression threads (pgzip)
        tuned_args.extend(["--compressor-jobs", jobs])
    if compression == "zstd" and not single_stream:
        # zstd compresses a single stream in parallel, so the job count is a
        # pure speed/memory trade-off: it does not change the output bytes the
        # way it does for gzip (where it also picks the implementation), and 2
        # is the minimum the parallel encoder needs. Keep that conservative
        # default - roughly 64 MiB of peak memory per job - in every mode, and
        # let users spend more with the compression_jobs setting.
        tuned_args.extend(["--compressor-jobs", jobs if jobs_explicit else "2"])
    if level != "-1" and compression != "none":
        if level not in valid_levels[compression]:
            fail("Invalid compression level {} for {}".format(level, compression))
        tuned_args.extend(["--compression-level", level])
    return tuned_args

def build_layer_mtree(ctx, name, *, tar_blob = None, compact_stream = None, erofs_blob = None, erofs_compact_stream = None):
    """Produce an mtree spec describing the metadata of a layer.

    Runs `img mtree` over exactly one of: a materialized (possibly compressed)
    tar blob, a tar layer's compact stream, a materialized EROFS layer blob, or
    an EROFS layer's compact stream (read as a sparse image whose file payloads
    are absent). Writes a single mtree text file describing the layer's entries.
    The path layout, included fields, and layer layout are controlled by the
    //img/settings:mtree_path_prefix, :mtree_options, and :mtree_layer_layout
    build settings (read from ctx.attr, so the rule must expose the matching
    _mtree_* hidden attributes).

    Args:
        ctx: Rule context.
        name: Base name for the output file (the result is name + ".mtree").
        tar_blob: A layer tar File (optionally gzip/zstd compressed).
        compact_stream: A tar layer's compact stream (.cstream) File.
        erofs_blob: An EROFS layer blob File (optionally zstd compressed).
        erofs_compact_stream: An EROFS layer's compact stream (.cstream) File.

    Returns:
        The generated mtree File.
    """
    inputs = [i for i in [tar_blob, compact_stream, erofs_blob, erofs_compact_stream] if i != None]
    if len(inputs) != 1:
        fail("build_layer_mtree requires exactly one of tar_blob, compact_stream, erofs_blob or erofs_compact_stream")
    input = inputs[0]

    mtree_out = ctx.actions.declare_file(name + ".mtree")
    args = ctx.actions.args()
    args.add("mtree")
    if tar_blob != None:
        args.add("--tar", tar_blob)
    elif compact_stream != None:
        args.add("--cstream", compact_stream)
    elif erofs_blob != None:
        args.add("--erofs", erofs_blob)
    else:
        args.add("--erofs-cstream", erofs_compact_stream)
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

def build_layer_ztoc(ctx, name, *, tar_blob, span_size):
    """Produce a ztoc (SOCI table of contents) for a gzip-compressed tar layer.

    Runs `img ztoc` over a materialized gzip layer blob, writing a single ztoc
    file. Used to generate a ztoc on the fly for layers whose SingleLayerInfo did
    not already carry one (e.g. recompressed/optimized/imported layers).

    Args:
        ctx: Rule context.
        name: Base name for the output file (the result is name + ".ztoc").
        tar_blob: A gzip-compressed layer tar File.
        span_size: Span size (uncompressed bytes between checkpoints) for the ztoc.

    Returns:
        The generated ztoc File.
    """
    ztoc_out = ctx.actions.declare_file(name + ".ztoc")
    args = ctx.actions.args()
    args.add("ztoc")
    args.add("--blob", tar_blob)
    args.add("--output", ztoc_out)
    args.add("--span-size", str(span_size))

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        outputs = [ztoc_out],
        inputs = [tar_blob],
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        mnemonic = "LayerZtoc",
    )
    return ztoc_out

def media_type_is_gzip(media_type):
    """Whether a layer blob with this media type is gzip-compressed (ztoc-able).

    ztoc/SOCI only supports gzip layers. Every gzip OCI/Docker layer media type
    carries a "gzip" token (`...tar+gzip`, `...rootfs.diff.tar.gzip`); estargz
    layers keep the same tar+gzip media type and are equally seekable. zstd,
    uncompressed, empty, and non-tar artifact layers are not gzip.
    """
    return media_type != None and "gzip" in media_type

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

def media_type_ships_no_filesystem(media_type):
    """Whether this media type positively states the layer carries no files.

    True only for a media type that is known and is neither a tar changeset nor
    an EROFS filesystem image -- an empty layer, say, or a non-tar artifact
    blob. Unlike `not media_type_is_tar(...)`, an absent or "unknown" media type
    is NOT reported as such: a layer whose descriptor simply omits the field may
    well be a filesystem layer, and a consumer that must account for every byte
    of the image (image_size_test) has to treat it as unknown rather than skip
    it silently.
    """
    if media_type == None or media_type == "" or media_type == "unknown":
        return False
    return not media_type_is_tar(media_type) and not media_type_is_erofs(media_type)

def layer_mtree_or_none(ctx, layer, name):
    """The mtree File describing one layer's tar entries, or None.

    Uses the layer's own `mtree` when it carries one (produced by a rules_img
    layer rule, or by the pull/import rule for a blob-bearing layer); otherwise
    renders one on the fly from the layer's materialized blob (a tar changeset
    or an EROFS filesystem image) or compact stream -- this covers raw tars
    added via `DefaultInfo` and any other blob-bearing layer whose source did
    not precompute one. Returns None for a layer whose filesystem content is not
    available: a shallow/lazy layer with no blob, or a blob that is neither,
    such as an empty layer or a non-tar artifact.

    Args:
        ctx: Rule or aspect context exposing the LAYER_MTREE_ATTRS settings.
        layer: A SingleLayerInfo provider.
        name: Base name for an on-the-fly mtree (the result is name + ".mtree").
            Must be unique within the rule.

    Returns:
        The per-layer mtree File, or None.
    """
    if layer.mtree != None:
        return layer.mtree
    if layer.blob != None and media_type_is_erofs(layer.media_type):
        return build_layer_mtree(ctx, name, erofs_blob = layer.blob)
    if layer.blob != None and media_type_is_tar(layer.media_type):
        return build_layer_mtree(ctx, name, tar_blob = layer.blob)
    if layer.compact_stream != None:
        return build_layer_mtree(ctx, name, compact_stream = layer.compact_stream)
    return None

def image_layer_mtrees(ctx, layers, name_prefix = None):
    """Ordered per-layer mtree Files for an image, building missing ones on the fly.

    Resolves every layer with layer_mtree_or_none and drops the layers it cannot
    describe. Skips are best-effort: if any layer is skipped the merged mtree
    represents only a subset of the image. The rule must expose the same hidden
    _mtree_* attributes build_layer_mtree reads (LAYER_MTREE_ATTRS).

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
        mtree_file = layer_mtree_or_none(ctx, layer, "{}_layer_{}".format(prefix, i))
        if mtree_file != None:
            mtrees.append(mtree_file)
    return mtrees

def image_layer_ztocs(ctx, layers, *, span_size, name_prefix = None):
    """Ordered (layer-metadata, ztoc) pairs for an image's gzip layers.

    For each layer (in image/layer order):
    - if it already carries a `ztoc` (produced by a rules_img layer rule with SOCI
      enabled), pair its metadata with that ztoc;
    - else if it has a materialized gzip tar blob, generate a ztoc on the fly with
      build_layer_ztoc;
    - else skip it (a shallow/lazy layer with no blob, or a non-gzip layer).

    Layers below the SOCI minimum size are NOT filtered here (a layer's size is
    only known at action time); `img soci-index --min-layer-size` drops those from
    the assembled SOCI index. Skips are best-effort.

    Args:
        ctx: Rule context of an image rule.
        layers: Ordered list of SingleLayerInfo providers.
        span_size: Span size for on-the-fly ztoc generation.
        name_prefix: Optional base name for on-the-fly per-layer ztoc files
            (defaults to ctx.label.name). A rule that builds several manifests
            (e.g. an image index) must pass a per-manifest-unique prefix so the
            declared `<prefix>_layer_<i>.ztoc` files do not collide.

    Returns:
        Ordered list of struct(metadata = File, ztoc = File).
    """
    prefix = name_prefix if name_prefix != None else ctx.label.name
    pairs = []
    for i, layer in enumerate(layers):
        if layer.ztoc != None:
            pairs.append(struct(metadata = layer.metadata, ztoc = layer.ztoc))
        elif layer.blob != None and media_type_is_gzip(layer.media_type):
            ztoc = build_layer_ztoc(ctx, "{}_layer_{}".format(prefix, i), tar_blob = layer.blob, span_size = span_size)
            pairs.append(struct(metadata = layer.metadata, ztoc = ztoc))
    return pairs

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

def calculate_layer_info(*, ctx, media_type, tar_file, metadata_file, estargz, zstd_chunked = False, annotations = {}, digest_modes = ["digest", "diff_id"], mtree = None):
    """Calculates the layer info for a file.

    Args:
        ctx: Rule context.
        media_type: Media type of the layer.
        tar_file: Input file (tar or arbitrary blob).
        metadata_file: Output metadata file.
        estargz: Boolean indicating whether the layer is an estargz layer.
        zstd_chunked: Boolean indicating whether the layer is a zstd:chunked layer.
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
        # The input may be a source file (a checked-in or external tar), which a
        # tree artifact layout cannot reference. The copy is named after the
        # metadata file, which is already unique within the rule.
        generated_blob = generated_blob_or_none(
            ctx,
            tar_file,
            name = metadata_file.basename.removesuffix(".json") + "_blob_copy",
        ),
        metadata = metadata_file,
        media_type = media_type,
        estargz = estargz,
        zstd_chunked = zstd_chunked,
        compact_stream = None,
        layer_input_files = None,
        layer_input_files_cas = None,
        sources = [],
        mtree = mtree,
        ztoc = None,
    )

def recompress_layer(*, ctx, media_type, tar_file, metadata_file, output, target_compression, tar_format, annotations):
    """Recompresses a tar file.

    Args:
        ctx: Rule context.
        media_type: Media type of the layer.
        tar_file: Input tar file.
        metadata_file: Input metadata file.
        output: Output recompressed file.
        target_compression: Target compression format.
        tar_format: struct returned by resolve_tar_format().
        annotations: Dict of string annotations to add to the layer metadata.

    Returns:
        SingleLayerInfo provider with recompressed blob and metadata.
    """
    args = ctx.actions.args()
    args.add("compress")
    args.add("--history", layer_history(layer_name(ctx.label)))
    args.add("--format", target_compression)
    args.add_all(tar_format_args(tar_format))
    for key, value in annotations.items():
        args.add("--annotation", "{}={}".format(key, value))
    args.add("--metadata", metadata_file.path)
    args.add_all(compression_tuning_args(ctx, target_compression, tar_format))
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
        generated_blob = None,
        metadata = metadata_file,
        media_type = media_type,
        estargz = tar_format.estargz,
        zstd_chunked = tar_format.zstd_chunked,
        compact_stream = None,
        layer_input_files = None,
        layer_input_files_cas = None,
        sources = [],
        mtree = build_layer_mtree(ctx, ctx.label.name, tar_blob = output),
        ztoc = None,
    )

def optimize_layer(*, ctx, media_type, tar_file, metadata_file, output, target_compression, tar_format, annotations, layer_format = "tar", erofs_block_size = 0, erofs_inline_threshold = 0, erofs_synthetic_directories = ""):
    """Rewrites a tar file through the layer writer, deduplicating its contents.

    With layer_format = "erofs" the same rewrite converts the tar into an EROFS
    filesystem image: the tar is fed to the EROFS sink entry by entry, so every
    entry keeps its metadata. A format change always takes this path, because the
    source blob cannot be reused.

    Args:
        ctx: Rule context.
        media_type: Media type of the layer.
        tar_file: Input tar file.
        metadata_file: Output metadata file.
        output: Output rewritten layer file.
        target_compression: Target compression format.
        tar_format: struct returned by resolve_tar_format().
        annotations: Dict of string annotations to add to the layer metadata.
        layer_format: "tar" or "erofs".
        erofs_block_size: EROFS filesystem block size (only for layer_format = "erofs").
        erofs_inline_threshold: Cap on tail-packing file payloads into inodes
            (only for layer_format = "erofs").
        erofs_synthetic_directories: EROFS synthesized-directory policy (only for
            layer_format = "erofs").

    Returns:
        SingleLayerInfo provider with the rewritten blob and metadata.
    """
    is_erofs = layer_format == "erofs"
    inputs = [tar_file]
    args = ctx.actions.args()
    args.add("layer")
    args.add("--history", layer_history(ctx.attr.name))
    args.add("--format", target_compression)
    if is_erofs:
        args.add("--layer-format", "erofs")
        args.add("--erofs-block-size", str(erofs_block_size))
        args.add("--erofs-inline-threshold", str(erofs_inline_threshold))
        args.add("--erofs-synthetic-directories", erofs_synthetic_directories)
    args.add_all(tar_format_args(tar_format))
    for key, value in annotations.items():
        args.add("--annotation", "{}={}".format(key, value))
    args.add("--metadata", metadata_file.path)
    args.add("--import-tar", tar_file.path)
    args.add_all(compression_tuning_args(ctx, target_compression, tar_format))
    args.add(output)
    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = depset(inputs),
        outputs = [output, metadata_file],
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        mnemonic = "LayerErofs" if is_erofs else "LayerOptimize",
    )
    if is_erofs:
        mtree = build_layer_mtree(ctx, ctx.label.name, erofs_blob = output)
    else:
        mtree = build_layer_mtree(ctx, ctx.label.name, tar_blob = output)
    return SingleLayerInfo(
        blob = output,
        generated_blob = None,
        metadata = metadata_file,
        media_type = media_type,
        estargz = tar_format.estargz,
        zstd_chunked = tar_format.zstd_chunked,
        compact_stream = None,
        layer_input_files = None,
        layer_input_files_cas = None,
        sources = [],
        mtree = mtree,
        ztoc = None,
    )
