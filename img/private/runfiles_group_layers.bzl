"""Builds container image layers for runfiles groups while walking a binary's graph.

layer_from_binary attaches runfiles_group_layers_aspect to its binary. Wherever a
target describes runfiles groups (see rules_runfiles_group), the aspect turns the
runfiles that target adds into a layer right there, on that target. Every binary
that reaches the target reuses the layer instead of building its own.

An aspect cannot see the layer_from_binary target it was applied from, and aspect
parameters split the aspect into one instance per value. So the aspect builds every
layer with the global build settings only -- compression, estargz, SOCI, parent
directories, tree artifact handling and the shared runfiles path -- and records
those settings in the layer's handle. layer_from_binary reuses a layer whose
recorded settings match its own, and rebuilds the group from its content
otherwise, as it would without the aspect. Per-target overrides therefore stay
correct; they only give up the reuse.

The aspect has a single parameter, use_runfiles_groups, taken from
layer_from_binary's attribute of the same name. It is off by default: the aspect
then does nothing and, on Bazel 9 and newer, does not propagate past the binary.
"""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("@rules_runfiles_group//runfiles_group:lib.bzl", "runfiles_groups")
load("//img/private/common:build.bzl", "TOOLCHAIN")
load("//img/private/common:layer_attrs.bzl", "layer_attrs")
load(
    "//img/private/common:tar_layer.bzl",
    "create_tar_single_layer",
    "empty_runfile_short_path",
    "resolve_layer_settings",
    "root_symlinks_arg",
    "symlinks_arg",
    "to_short_path_pair",
)
load("//img/private/providers:single_layer_info.bzl", "SingleLayerInfo")

RulesImgRunfilesGroupsInfo = provider(
    doc = """\
rules_img's view of a target's runfiles groups. Each partial's handle is a struct:

  content  the group's runfiles content (a depset of File or a runfiles object),
           from which layer_from_binary can always rebuild the layer
  layer    the layer the aspect built for the group, as a struct of the
           SingleLayerInfo fields plus the output files, or None
  key      the settings that layer was built with, or None
""",
    fields = runfiles_groups.PACKAGER_INFO_FIELDS,
)

def normalize_path(path):
    """Strip a leading slash from a path for use in tar entries."""
    if path.startswith("/"):
        return path[1:]
    return path

def group_layer_args(ctx, group_runfiles, content_prefix, default_metadata):
    """The img layer arguments and inputs that place one group's runfiles.

    Shared by the aspect and by layer_from_binary, so a layer the aspect built and a
    layer layer_from_binary rebuilds hold the same entries.

    Args:
        ctx: A rule or aspect context.
        group_runfiles: The group's contents as a runfiles object.
        content_prefix: The normalized path the runfiles content is placed under.
        default_metadata: JSON-encoded default metadata, or "" for none.

    Returns:
        (extra_args, extra_inputs): lists for create_tar_single_layer.
    """
    extra_args = ["--default-metadata", default_metadata] if default_metadata else []
    extra_inputs = [group_runfiles.files]

    add_args = ctx.actions.args()
    add_args.set_param_file_format("multiline")
    add_args.use_param_file("--add-from-file=%s", use_always = True)
    add_args.add_all(group_runfiles.files, map_each = to_short_path_pair, format_each = "{}/%s".format(content_prefix), expand_directories = False, uniquify = True)
    extra_args.append(add_args)

    symlink_add_args = ctx.actions.args()
    symlink_add_args.set_param_file_format("multiline")
    symlink_add_args.use_param_file("--add-from-file=%s", use_always = True)
    symlink_add_args.add_all(group_runfiles.symlinks, map_each = symlinks_arg, format_each = "{}/%s".format(content_prefix))
    symlink_add_args.add_all(group_runfiles.root_symlinks, map_each = root_symlinks_arg, format_each = "{}/%s".format(content_prefix))
    extra_args.append(symlink_add_args)

    symlink_inputs = []
    symlink_inputs.extend([se.target_file for se in group_runfiles.symlinks.to_list()])
    symlink_inputs.extend([se.target_file for se in group_runfiles.root_symlinks.to_list()])
    if len(symlink_inputs) > 0:
        extra_inputs.append(depset(symlink_inputs))

    empty_args = ctx.actions.args()
    empty_args.set_param_file_format("multiline")
    empty_args.use_param_file("--empty-files-from-file=%s", use_always = True)
    empty_args.add_all(group_runfiles.empty_filenames, map_each = empty_runfile_short_path, format_each = "{}/%s".format(content_prefix))
    extra_args.append(empty_args)

    return extra_args, extra_inputs

def layer_reuse_key(settings, content_prefix):
    """The settings a group layer is built with, compared to decide on reuse.

    Args:
        settings: The struct from resolve_layer_settings().
        content_prefix: The normalized path the runfiles content is placed under.

    Returns:
        A struct that compares equal for layers with identical contents.
    """
    return struct(settings = settings, content_prefix = content_prefix)

def thaw_layer(layer):
    """Turns a handle's frozen layer back into create_tar_single_layer's tuple.

    Args:
        layer: The `layer` struct of a handle.

    Returns:
        (SingleLayerInfo, out, metadata, compact_stream, mtree, ztoc).
    """
    return (
        SingleLayerInfo(
            blob = layer.blob,
            metadata = layer.metadata,
            media_type = layer.media_type,
            estargz = layer.estargz,
            compact_stream = layer.compact_stream,
            layer_input_files = layer.layer_input_files,
            layer_input_files_cas = layer.layer_input_files_cas,
            sources = [],
            mtree = layer.mtree,
            ztoc = layer.ztoc,
        ),
        layer.out,
        layer.metadata,
        layer.compact_stream,
        layer.mtree,
        layer.ztoc,
    )

def _freeze_layer(created):
    # A handle lives inside a depset element, so it must be immutable while the
    # aspect implementation still runs. SingleLayerInfo carries a list (`sources`),
    # so its fields are copied into a struct of Files, depsets, strings and bools.
    info, out, metadata, compact_stream, mtree, ztoc = created
    return struct(
        blob = info.blob,
        metadata = metadata,
        media_type = info.media_type,
        estargz = info.estargz,
        compact_stream = compact_stream,
        layer_input_files = info.layer_input_files,
        layer_input_files_cas = info.layer_input_files_cas,
        mtree = mtree,
        ztoc = ztoc,
        out = out,
    )

# The public attributes of layer_attrs.common, at the values that mean "follow the
# global build setting". The aspect cannot declare them: a public aspect attribute is
# a parameter, which layer_from_binary would have to pass and which would split the
# aspect into one instance per value.
_GLOBAL_DEFAULT_ATTRS = dict(
    compress = "auto",
    estargz = "auto",
    soci = "auto",
    create_parent_directories = "auto",
    tree_artifact_handling = "auto",
    media_type = "",
    annotations = {},
    annotations_file = None,
)

def _layer_ctx(ctx):
    """An adapter that lets the tar_layer helpers run on the aspect's ctx.

    The helpers read the layer rule's attributes from ctx.attr. This hands them the
    public ones at their global defaults and the aspect's own private build-setting
    attributes, and passes everything else through.
    """
    attrs = dict(_GLOBAL_DEFAULT_ATTRS)
    for name in _ASPECT_PRIVATE_ATTRS:
        attrs[name] = getattr(ctx.attr, name)
    return struct(
        actions = ctx.actions,
        attr = struct(**attrs),
        file = struct(annotations_file = None),
        label = ctx.label,
        toolchains = ctx.toolchains,
        var = ctx.var,
    )

def _layer_file_name(ctx, kind, name):
    """The base name of a group layer's outputs, in the visited target's package.

    A target's own per-target group -- the common case -- is named after the target
    alone. A target can also produce other layers: the groups it synthesizes for its
    file dependencies, and the groups it merges. Those get a suffix that tells them
    apart, from the group's name and from whether the layer adds or merges it.
    """
    base = ctx.label.name + ".runfiles_group"
    if kind == "add" and name == ctx.label:
        return base
    return "%s.%x" % (base, hash(kind + "\0" + runfiles_groups.name_str(name)) & 0xffffffff)

def _build_layer(ctx, kind, name, content):
    """Builds the layer for one group's content on the aspect's target."""
    if ctx.toolchains[TOOLCHAIN] == None:
        # No img toolchain for this target's execution platform: leave the layer to
        # layer_from_binary, which needs the toolchain anyway.
        return None, None
    lctx = _layer_ctx(ctx)
    settings = resolve_layer_settings(lctx)
    content_prefix = normalize_path(ctx.attr._default_runfiles_shared_path[BuildSettingInfo].value)
    extra_args, extra_inputs = group_layer_args(ctx, runfiles_groups.runfiles(ctx, content), content_prefix, "")

    created = create_tar_single_layer(lctx, settings, _layer_file_name(ctx, kind, name), extra_args, extra_inputs)
    return _freeze_layer(created), layer_reuse_key(settings, content_prefix)

def _is_aspect_ctx(ctx):
    # hasattr(ctx, "rule") is true for a rule's ctx as well -- the field exists and
    # only fails when read -- but the two contexts print differently.
    return repr(ctx).startswith("<aspect context")

def _materialize(ctx, entry):
    layer, key = _build_layer(ctx, "add", entry.name, entry.content)
    return struct(content = entry.content, layer = layer, key = key)

def _merge(ctx, name, partials):
    # The partials' pieces are disjoint, so the union of their contents is exactly
    # the merged group. Where this runs during the walk, the merged layer is built on
    # the target where the pieces met. Where it runs in layer_from_binary --
    # finalize() and limit() -- the layer is left to layer_from_binary, which builds
    # it with its own settings.
    content = runfiles_groups.union(ctx, [partial.handle.content for partial in partials])
    if not _is_aspect_ctx(ctx):
        return struct(content = content, layer = None, key = None)
    layer, key = _build_layer(ctx, "merge", name, content)
    return struct(content = content, layer = layer, key = key)

RUNFILES_GROUP_LAYERS_OPS = runfiles_groups.packager_ops(
    materialize = _materialize,
    merge = _merge,
)

_ASPECT_PRIVATE_ATTRS = [name for name in layer_attrs.common if name.startswith("_")]

_NO_GROUPS = depset()

def _runfiles_group_layers_aspect_impl(target, ctx):
    if not ctx.attr.use_runfiles_groups:
        return [RulesImgRunfilesGroupsInfo(
            owned = _NO_GROUPS,
            shared = _NO_GROUPS,
            fallback = None,
            executable_group = None,
        )]
    return [RulesImgRunfilesGroupsInfo(**runfiles_groups.aspect_step(
        target,
        ctx,
        RUNFILES_GROUP_LAYERS_OPS,
        info = RulesImgRunfilesGroupsInfo,
    ))]

_GROUPS_ATTR_ASPECTS = runfiles_groups.ATTR_ASPECTS

def _propagate(propagation_ctx):
    # Off by default: the aspect stays on the binary and goes no further.
    if not propagation_ctx.attr.use_runfiles_groups:
        return []
    return _GROUPS_ATTR_ASPECTS(propagation_ctx)

runfiles_group_layers_aspect = aspect(
    implementation = _runfiles_group_layers_aspect_impl,
    # Before Bazel 9 there is no propagation function: the aspect then visits the
    # whole graph, and returns at once where use_runfiles_groups is False.
    attr_aspects = _propagate if type(_GROUPS_ATTR_ASPECTS) != type([]) else _GROUPS_ATTR_ASPECTS,
    attrs = dict(
        {name: layer_attrs.common[name] for name in _ASPECT_PRIVATE_ATTRS},
        use_runfiles_groups = attr.bool(
            doc = "Whether to describe and build runfiles group layers. Taken from layer_from_binary.",
        ),
    ),
    # Optional: the aspect visits targets whose execution platform may have no img
    # toolchain. Without one it builds no layers and layer_from_binary builds them.
    toolchains = [config_common.toolchain_type(TOOLCHAIN, mandatory = False)],
    provides = [RulesImgRunfilesGroupsInfo],
)
