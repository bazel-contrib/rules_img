"""Binary layer rule for packaging a *_binary target into a container image layer."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("@rules_runfiles_group//runfiles_group:lib.bzl", "runfiles_groups")
load("//img/private/common:build.bzl", "TOOLCHAINS")
load("//img/private/common:layer_attrs.bzl", "layer_attrs")
load(
    "//img/private/common:tar_layer.bzl",
    "create_tar_layer",
    "create_tar_single_layer",
    "empty_runfile_short_path",
    "file_type",
    "files_arg",
    "get_repo_mapping_manifest",
    "place_extra_executable_files",
    "resolve_layer_settings",
    "root_symlinks_arg",
    "symlinks_arg",
    "to_short_path_pair",
)
load("//img/private/providers:layer_config_info.bzl", "ImageLayerConfigInfo")
load("//img/private/providers:layers_info.bzl", "LayersInfo")

_BinaryRunInfo = provider(
    doc = """\
This provider is only used by a private aspect and shouldn't be visible outside the layer_from_binary rule.
Collects args and env of a *_binary target, plus its aspect_hints.
""",
    fields = dict(
        args = "Arguments of the *_binary target",
        env = "Environment variables of the *_binary target",
        aspect_hints = "list of Target: the binary's aspect_hints, forwarded so the rule can resolve runfiles groups",
    ),
)

def _binary_run_info_extraction_aspect_impl(target, ctx):
    # https://bazel.build/reference/be/common-definitions#common-attributes-binaries
    # Find "args" attribute (list of strings)
    # Find RunEnvironmentInfo or "env" attribute (string -> string dict)
    extracted_args = []
    extracted_env = {}

    targets_for_expansion = [target]
    if hasattr(ctx.rule.attr, "data") and type(ctx.rule.attr.data) == type([]):
        # Collect data for expansion
        targets_for_expansion.extend(ctx.rule.attr.data)
    if hasattr(ctx.rule.attr, "args"):
        if type(ctx.rule.attr.args) != type([]):
            fail("Expected args to be a list, got", type(ctx.rule.attr.args))
        for arg in ctx.rule.attr.args:
            arg = ctx.expand_location(arg, targets = targets_for_expansion)
            arg = ctx.expand_make_variables("args", arg, {})
            extracted_args.append(arg)
    if RunEnvironmentInfo in target:
        env_info = target[RunEnvironmentInfo]
        extracted_env.update(env_info.environment)
    elif hasattr(ctx.rule.attr, "env"):
        env_attr = ctx.rule.attr.env
        if type(ctx.rule.attr.env) != type({}):
            fail("Expected env to be a dict, got", type(env_attr))
        for k, v in env_attr.items():
            v = ctx.expand_location(v, targets = targets_for_expansion)
            v = ctx.expand_make_variables("env", v, {})
            extracted_env[k] = v

    return [
        _BinaryRunInfo(
            args = extracted_args,
            env = extracted_env,
            # Only the hint targets are forwarded -- O(number of hints) references,
            # which Skyframe retains anyway -- and the rule does the O(groups) work
            # of runfiles_groups.resolve() transiently. Resolving here instead would
            # retain a list plus one entry per group on every layer target for the
            # life of the build.
            aspect_hints = getattr(ctx.rule.attr, "aspect_hints", []),
        ),
    ]

_binary_run_info_extraction_aspect = aspect(
    implementation = _binary_run_info_extraction_aspect_impl,
    attr_aspects = [],  # The aspect only inspect the target itself (not the deps)
    provides = [_BinaryRunInfo],
)

def _normalize_path(path):
    """Strip leading slash from a path for use in tar entries."""
    if path.startswith("/"):
        return path[1:]
    return path

def _extract_runfiles_top_level_dir(f):
    """Extract the top-level directory name from a runfiles file for symlink dedup.

    Used as map_each callback with uniquify=True to produce one symlink entry
    per unique top-level directory under the runfiles root.
    """
    if f.short_path.startswith("../"):
        remainder = f.short_path[3:]
        slash_pos = remainder.find("/")
        entry = remainder[:slash_pos] if slash_pos > 0 else remainder
    else:
        entry = "_main"
    if entry == "_repo_mapping":
        return None
    return entry

def _resolve_runfiles_config(ctx, path_in_image, has_runfiles_groups):
    """Resolve runfiles placement configuration."""
    mode = ctx.attr.runfiles_sharing_mode
    if mode == "auto":
        mode = ctx.attr._default_runfiles_sharing_mode[BuildSettingInfo].value
    if mode == "auto":
        mode = "shared" if has_runfiles_groups else "private"

    conventional_runfiles_path = ctx.attr.runfiles_path if ctx.attr.runfiles_path else "{}.runfiles".format(path_in_image)

    if mode == "shared":
        if ctx.attr.runfiles_shared_path:
            content_path = ctx.attr.runfiles_shared_path
        else:
            content_path = ctx.attr._default_runfiles_shared_path[BuildSettingInfo].value
        return struct(
            shared = True,
            runfiles_content_path = content_path,
            runfiles_symlink_path = conventional_runfiles_path,
        )
    else:
        return struct(
            shared = False,
            runfiles_content_path = conventional_runfiles_path,
            runfiles_symlink_path = None,
        )

def _find_executable_group_index(ordered_groups, executable_group):
    """Find the index of the group named by executable_group, if any."""
    if executable_group == None:
        return -1
    for i, entry in enumerate(ordered_groups):
        if entry.name == executable_group:
            return i
    return -1

def _append_extra_default_files(ctx, default_files, exe, path_in_image, extra_args, extra_inputs):
    """Append additional default outputs (beyond the executable) as tar entries.

    Files are placed relative to the executable (anchored at path_in_image),
    using the shared place_extra_executable_files helper. The depset is streamed
    lazily and never flattened in Starlark.
    """
    place_extra_executable_files(ctx, default_files, exe, _normalize_path(path_in_image), extra_args, extra_inputs)

def _append_binary_args(ctx, exe, path_in_image, ordered_groups, runfiles, runfiles_config, content_prefix, extra_args, extra_inputs, default_files):
    """Append binary executable, symlinks, and repo mapping args to a layer."""
    binary_args = ctx.actions.args()
    binary_args.set_param_file_format("multiline")
    binary_args.use_param_file("--add-from-file=%s", use_always = True)
    binary_args.add_all([exe], map_each = files_arg, format_each = "{}\0%s".format(_normalize_path(path_in_image)), expand_directories = False)
    extra_args.append(binary_args)

    if runfiles:
        symlink_add_args = ctx.actions.args()
        symlink_add_args.set_param_file_format("multiline")
        symlink_add_args.use_param_file("--add-from-file=%s", use_always = True)
        symlink_add_args.add_all(runfiles.symlinks, map_each = symlinks_arg, format_each = "{}/%s".format(content_prefix))
        symlink_add_args.add_all(runfiles.root_symlinks, map_each = root_symlinks_arg, format_each = "{}/%s".format(content_prefix))
        extra_args.append(symlink_add_args)

    if runfiles_config.shared:
        all_runfiles = depset(transitive = [runfiles_groups.files(entry) for entry in ordered_groups])
        symlink_prefix = _normalize_path(runfiles_config.runfiles_symlink_path)
        rel_content = "/".join([".."] * (symlink_prefix.count("/") + 1)) + "/" + content_prefix
        symlink_args = ctx.actions.args()
        symlink_args.set_param_file_format("multiline")
        symlink_args.use_param_file("--symlink-pairs-from-file=%s", use_always = True)
        symlink_args.add_all(all_runfiles, map_each = _extract_runfiles_top_level_dir, format_each = "{}\0{}\0%s".format(symlink_prefix, rel_content), uniquify = True, expand_directories = False)
        extra_args.append(symlink_args)

    if runfiles:
        symlink_inputs = []
        symlink_inputs.extend([se.target_file for se in runfiles.symlinks.to_list()])
        symlink_inputs.extend([se.target_file for se in runfiles.root_symlinks.to_list()])
        if len(symlink_inputs) > 0:
            extra_inputs.append(depset(symlink_inputs))

    repo_mapping_manifest = get_repo_mapping_manifest(ctx.attr.binary)
    if repo_mapping_manifest != None:
        extra_inputs.append(depset([repo_mapping_manifest]))
        repo_mapping_args = ctx.actions.args()
        repo_mapping_args.set_param_file_format("multiline")
        repo_mapping_args.use_param_file("--add-from-file=%s", use_always = True)
        repo_mapping_args.add_all([repo_mapping_manifest], map_each = files_arg, format_each = "{}.repo_mapping\0%s".format(_normalize_path(path_in_image)), expand_directories = False)
        repo_mapping_args.add_all([repo_mapping_manifest], map_each = files_arg, format_each = "{}/_repo_mapping\0%s".format(
            _normalize_path(runfiles_config.runfiles_symlink_path) if runfiles_config.shared else content_prefix,
        ), expand_directories = False)
        extra_args.append(repo_mapping_args)

    _append_extra_default_files(ctx, default_files, exe, path_in_image, extra_args, extra_inputs)

def _create_grouped_layers(ctx, settings, exe, path_in_image, ordered_groups, runfiles_config, executable_group_index):
    """Create multiple layers from RunfilesGroupInfo groups.

    Each runfiles group becomes its own layer. The binary executable, runfiles
    symlinks, and repo-mapping manifest are either merged into the group marked
    as executable_group, or appended as a separate layer if no group carries
    that annotation.
    """
    all_layers = []
    all_outs = []
    all_metadata = []
    all_compact_streams = []
    all_mtrees = []
    default_info = ctx.attr.binary[DefaultInfo]
    content_prefix = _normalize_path(runfiles_config.runfiles_content_path)

    for i, entry in enumerate(ordered_groups):
        layer_name = "{}_{}".format(ctx.attr.name, i)
        extra_args = []

        # The group's contents as a runfiles object. A group whose content is a bare
        # depset of File (the files-only form) is lifted here, yielding empty
        # symlink/root_symlink/empty_filename depsets, so both content forms are
        # placed by the same code below. entry.content itself is opaque and must not
        # be read directly.
        group_runfiles = runfiles_groups.runfiles(ctx, entry)
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

        if i == executable_group_index:
            _append_binary_args(ctx, exe, path_in_image, ordered_groups, None, runfiles_config, content_prefix, extra_args, extra_inputs, default_info.files)

        layer_info, out, metadata, compact_stream, mtree = create_tar_single_layer(ctx, settings, layer_name, extra_args, extra_inputs)
        all_layers.append(layer_info)
        if out:
            all_outs.append(out)
        all_metadata.append(metadata)
        if compact_stream:
            all_compact_streams.append(compact_stream)
        all_mtrees.append(mtree)

    if executable_group_index < 0:
        bin_layer_name = "{}_{}".format(ctx.attr.name, len(ordered_groups))
        bin_extra_args = []
        bin_extra_inputs = []
        _append_binary_args(ctx, exe, path_in_image, ordered_groups, default_info.default_runfiles, runfiles_config, content_prefix, bin_extra_args, bin_extra_inputs, default_info.files)

        layer_info, out, metadata, compact_stream, mtree = create_tar_single_layer(ctx, settings, bin_layer_name, bin_extra_args, bin_extra_inputs)
        all_layers.append(layer_info)
        if out:
            all_outs.append(out)
        all_metadata.append(metadata)
        if compact_stream:
            all_compact_streams.append(compact_stream)
        all_mtrees.append(mtree)

    output_groups = dict(
        metadata = depset(all_metadata),
        mtree = depset(all_mtrees),
    )
    if all_outs:
        output_groups["layer"] = depset(all_outs)
    if all_compact_streams:
        output_groups["experimental_compact_stream"] = depset(all_compact_streams)
    default_files = all_outs if all_outs else all_compact_streams
    return [
        DefaultInfo(files = depset(default_files)),
        OutputGroupInfo(**output_groups),
        LayersInfo(layers = all_layers),
    ]

def _layer_from_binary_impl(ctx):
    run_info = ctx.attr.binary[_BinaryRunInfo]
    exe = ctx.executable.binary
    path_in_image = ctx.attr.path
    if len(path_in_image) == 0:
        if exe.short_path.startswith("../"):
            path_in_image = exe.short_path[3:]
        else:
            path_in_image = "_main/{}".format(exe.short_path)
    elif path_in_image.endswith("/"):
        path_in_image = "{prefix}{basename}".format(
            prefix = path_in_image,
            basename = exe.basename,
        )
    absolute_entrypoint = path_in_image if path_in_image.startswith("/") else "/" + path_in_image

    # The whole resolution protocol in one call: flatten the binary's entry depset
    # exactly once, fold duplicate group names, run the RunfilesGroupTransformInfo
    # hint transforms, and order by (rank, name). Returns None for a binary that does
    # not group its runfiles; the ungrouped path below then packages
    # DefaultInfo.default_runfiles as a single layer.
    resolved = runfiles_groups.resolve(ctx, ctx.attr.binary, aspect_hints = run_info.aspect_hints)
    ordered_groups = None
    if resolved != None:
        # With an executable group the binary and its supporting files are merged
        # into that group's layer, so the whole budget is available for groups.
        # Without one, a layer is reserved for the binary; a budget of exactly 1
        # leaves nothing for groups, so the ungrouped single-layer path is used.
        #
        # limit() reports group_count, which do_not_merge and rank constraints can
        # leave above max_groups. That is accepted here: layer_budget is a target, not
        # a hard cap (see the attribute docs). It also carries executable_group
        # through a merge, so that is read off `resolved` rather than kept in a local.
        if resolved.executable_group != None:
            if ctx.attr.layer_budget > 0:
                resolved = runfiles_groups.limit(ctx, resolved, max_groups = ctx.attr.layer_budget)
            ordered_groups = resolved.groups
        elif ctx.attr.layer_budget != 1:
            if ctx.attr.layer_budget > 1:
                resolved = runfiles_groups.limit(ctx, resolved, max_groups = ctx.attr.layer_budget - 1)
            ordered_groups = resolved.groups

    has_runfiles_groups = (
        ordered_groups != None and
        len(ordered_groups) > 0
    )
    runfiles_config = _resolve_runfiles_config(ctx, path_in_image, has_runfiles_groups)

    working_dir = None
    if ctx.attr.include_runfiles:
        effective_runfiles_path = runfiles_config.runfiles_symlink_path if runfiles_config.shared else runfiles_config.runfiles_content_path
        abs_rf = effective_runfiles_path if effective_runfiles_path.startswith("/") else "/" + effective_runfiles_path
        working_dir = "{}/_main".format(abs_rf)

    settings = resolve_layer_settings(ctx)

    # The Go tool's --executable/--runfiles hardcodes {target}.runfiles as prefix.
    # We can only use that fast path when the content path matches that convention.
    can_use_executable_flag = (
        runfiles_config.runfiles_content_path == "{}.runfiles".format(path_in_image) and
        not runfiles_config.shared
    )

    use_groups = (
        ctx.attr.include_runfiles and
        has_runfiles_groups
    )

    if use_groups:
        executable_group_index = _find_executable_group_index(ordered_groups, resolved.executable_group)
        result = _create_grouped_layers(ctx, settings, exe, path_in_image, ordered_groups, runfiles_config, executable_group_index)
    else:
        extra_args = []
        extra_inputs = []

        default_info = ctx.attr.binary[DefaultInfo]
        extra_inputs.append(default_info.files)

        if ctx.attr.include_runfiles:
            content_prefix = _normalize_path(runfiles_config.runfiles_content_path)

            if can_use_executable_flag:
                extra_args.append("--executable={}={}".format(path_in_image, exe.path))

                runfiles = default_info.default_runfiles
                if runfiles:
                    runfiles_args = ctx.actions.args()
                    runfiles_args.set_param_file_format("multiline")
                    runfiles_args.use_param_file("--runfiles={}=%s".format(exe.path), use_always = True)
                    runfiles_args.add_all(runfiles.files, map_each = to_short_path_pair, expand_directories = False, uniquify = True)
                    runfiles_args.add_all(runfiles.symlinks, map_each = symlinks_arg)
                    runfiles_args.add_all(runfiles.root_symlinks, map_each = root_symlinks_arg)
                    extra_args.append(runfiles_args)
                    extra_inputs.append(runfiles.files)

                    symlink_inputs = []
                    symlink_inputs.extend([symlink_entry.target_file for symlink_entry in runfiles.symlinks.to_list()])
                    symlink_inputs.extend([symlink_entry.target_file for symlink_entry in runfiles.root_symlinks.to_list()])
                    if len(symlink_inputs) > 0:
                        extra_inputs.append(depset(symlink_inputs))

                    empty_args = ctx.actions.args()
                    empty_args.set_param_file_format("multiline")
                    empty_args.use_param_file("--empty-files-from-file=%s", use_always = True)
                    empty_args.add_all(runfiles.empty_filenames, map_each = empty_runfile_short_path, format_each = "{}/%s".format(content_prefix))
                    extra_args.append(empty_args)

                repo_mapping_manifest = get_repo_mapping_manifest(ctx.attr.binary)
                if repo_mapping_manifest != None:
                    extra_inputs.append(depset([repo_mapping_manifest]))
                    repo_mapping_args = ctx.actions.args()
                    repo_mapping_args.set_param_file_format("multiline")
                    repo_mapping_args.use_param_file("--add-from-file=%s", use_always = True)
                    repo_mapping_args.add_all([repo_mapping_manifest], map_each = files_arg, format_each = "{}.repo_mapping\0%s".format(_normalize_path(path_in_image)), expand_directories = False)
                    repo_mapping_args.add_all([repo_mapping_manifest], map_each = files_arg, format_each = "{}/_repo_mapping\0%s".format(content_prefix), expand_directories = False)
                    extra_args.append(repo_mapping_args)
            else:
                binary_args = ctx.actions.args()
                binary_args.set_param_file_format("multiline")
                binary_args.use_param_file("--add-from-file=%s", use_always = True)
                binary_args.add_all([exe], map_each = files_arg, format_each = "{}\0%s".format(_normalize_path(path_in_image)), expand_directories = False)
                extra_args.append(binary_args)

                runfiles = default_info.default_runfiles
                if runfiles:
                    runfiles_add_args = ctx.actions.args()
                    runfiles_add_args.set_param_file_format("multiline")
                    runfiles_add_args.use_param_file("--add-from-file=%s", use_always = True)
                    runfiles_add_args.add_all(runfiles.files, map_each = to_short_path_pair, format_each = "{}/%s".format(content_prefix), expand_directories = False, uniquify = True)
                    runfiles_add_args.add_all(runfiles.symlinks, map_each = symlinks_arg, format_each = "{}/%s".format(content_prefix))
                    runfiles_add_args.add_all(runfiles.root_symlinks, map_each = root_symlinks_arg, format_each = "{}/%s".format(content_prefix))
                    extra_args.append(runfiles_add_args)
                    extra_inputs.append(runfiles.files)

                    symlink_inputs = []
                    symlink_inputs.extend([symlink_entry.target_file for symlink_entry in runfiles.symlinks.to_list()])
                    symlink_inputs.extend([symlink_entry.target_file for symlink_entry in runfiles.root_symlinks.to_list()])
                    if len(symlink_inputs) > 0:
                        extra_inputs.append(depset(symlink_inputs))

                    empty_args = ctx.actions.args()
                    empty_args.set_param_file_format("multiline")
                    empty_args.use_param_file("--empty-files-from-file=%s", use_always = True)
                    empty_args.add_all(runfiles.empty_filenames, map_each = empty_runfile_short_path, format_each = "{}/%s".format(content_prefix))
                    extra_args.append(empty_args)

                if runfiles_config.shared and runfiles:
                    symlink_prefix = _normalize_path(runfiles_config.runfiles_symlink_path)
                    rel_content = "/".join([".."] * (symlink_prefix.count("/") + 1)) + "/" + content_prefix
                    symlink_args = ctx.actions.args()
                    symlink_args.set_param_file_format("multiline")
                    symlink_args.use_param_file("--symlink-pairs-from-file=%s", use_always = True)
                    symlink_args.add_all(runfiles.files, map_each = _extract_runfiles_top_level_dir, format_each = "{}\0{}\0%s".format(symlink_prefix, rel_content), uniquify = True, expand_directories = False)
                    extra_args.append(symlink_args)

                repo_mapping_manifest = get_repo_mapping_manifest(ctx.attr.binary)
                if repo_mapping_manifest != None:
                    extra_inputs.append(depset([repo_mapping_manifest]))
                    repo_mapping_args = ctx.actions.args()
                    repo_mapping_args.set_param_file_format("multiline")
                    repo_mapping_args.use_param_file("--add-from-file=%s", use_always = True)
                    repo_mapping_args.add_all([repo_mapping_manifest], map_each = files_arg, format_each = "{}.repo_mapping\0%s".format(_normalize_path(path_in_image)), expand_directories = False)
                    repo_mapping_args.add_all([repo_mapping_manifest], map_each = files_arg, format_each = "{}/_repo_mapping\0%s".format(
                        _normalize_path(runfiles_config.runfiles_symlink_path) if runfiles_config.shared else content_prefix,
                    ), expand_directories = False)
                    extra_args.append(repo_mapping_args)

            _append_extra_default_files(ctx, default_info.files, exe, path_in_image, extra_args, extra_inputs)
        else:
            binary_file_args = ctx.actions.args()
            binary_file_args.set_param_file_format("multiline")
            binary_file_args.use_param_file("--add-from-file=%s", use_always = True)
            binary_file_args.add_all(["{}\0{}{}".format(_normalize_path(path_in_image), file_type(exe), exe.path)])
            extra_args.append(binary_file_args)
            _append_extra_default_files(ctx, default_info.files, exe, path_in_image, extra_args, extra_inputs)

        result = create_tar_layer(ctx, settings, extra_args = extra_args, extra_inputs = extra_inputs)

    return result + [
        ImageLayerConfigInfo(
            entrypoint = [absolute_entrypoint],
            cmd = run_info.args,
            env = run_info.env,
            working_dir = working_dir,
        ),
    ]

layer_from_binary = rule(
    implementation = _layer_from_binary_impl,
    doc = """Creates a container image layer from a *_binary target.

This rule packages a binary executable and its runfiles into a layer, and additionally
provides image configuration (entrypoint, cmd, env, working_dir) via ImageLayerConfigInfo.
When used as a layer in image_manifest, the configuration is automatically applied to the
image with Dockerfile-like semantics.

The binary's `args` attribute becomes the image `cmd`, its `env` attribute (or
RunEnvironmentInfo provider) becomes `env`, and the binary path becomes the `entrypoint`.
When include_runfiles is True (default), the working directory is set to the runfiles root.

In addition to the executable and its runfiles, any other default outputs of the binary
target (the rest of `DefaultInfo.files`) are copied into the layer, each placed at the same
location relative to the executable that it has in the source tree.

If the binary provides RunfilesGroupInfo (from rules_runfiles_group), the runfiles are split
into separate layers based on the groups. This allows for better caching: stable layers
(interpreter, stdlib) change infrequently and can be shared, while the application code layer
changes with each build. Layers are emitted in the groups' `rank` order (lowest first), so
foundational content ends up in the earliest, most cacheable layers. Any
RunfilesGroupTransformInfo in the binary's `aspect_hints` is applied first, which lets users
drop or re-shape groups per target.

Note that RunfilesGroupInfo emission is off by default in rules_runfiles_group. Build with
`--@rules_runfiles_group//runfiles_group:enabled=true` to opt in; without it, group-aware
binaries produce a single layer.

When the number of groups exceeds what is practical for a container image, use `layer_budget`
to merge groups down to a maximum count. The merge algorithm respects group rank (only merges
within the same rank), do_not_merge flags, merge affinity (groups sharing an affinity are
preferred merge partners), and weight hints (lighter groups merge first).

Example:

```python
load("@rules_img//img:layer.bzl", "layer_from_binary")
load("@rules_img//img:image.bzl", "image_manifest")

# Package a Go binary with its runfiles
layer_from_binary(
    name = "app_layer",
    binary = "//cmd/server",
)

# Use in an image - entrypoint, cmd, env, and working_dir are set automatically
image_manifest(
    name = "image",
    base = "@distroless_base",
    layers = [":app_layer"],
)

# Override the path inside the image
layer_from_binary(
    name = "custom_path_layer",
    binary = "//cmd/server",
    path = "/usr/local/bin/",
)

# Without runfiles (static binary)
layer_from_binary(
    name = "static_layer",
    binary = "//cmd/server",
    path = "/usr/local/bin/server",
    include_runfiles = False,
)
```

### Output groups

- `mtree`: one [mtree](https://man.freebsd.org/cgi/man.cgi?mtree(5)) text file per produced layer
""",
    attrs = {
        "binary": attr.label(
            doc = """The *_binary target to package into the layer.

The binary's `args` and `env` attributes are extracted and provided as image configuration
(cmd and env) via ImageLayerConfigInfo. The `data` attribute is used for `$(location)` expansion
in args and env values.

If the binary provides RunfilesGroupInfo, the runfiles are split into separate layers per group.""",
            executable = True,
            mandatory = True,
            cfg = "target",
            aspects = [_binary_run_info_extraction_aspect],
        ),
        "path": attr.string(
            mandatory = False,
            doc = """\
Optional path of the binary inside the image.
If the path ends with a slash ("/"), the basename of the binary will be automatically appended.
If unset, this defaults to the rlocationpath of the binary (e.g., "_main/cmd/server/server_/server").
""",
        ),
        "runfiles_path": attr.string(
            mandatory = False,
            doc = """\
Optional path of the runfiles directory of the binary inside the image.
If unset, this defaults to the path of the binary with a .runfiles suffix (e.g., "_main/cmd/server/server_/server.runfiles").
Note: depending on the runfiles_sharing_mode, this may be a symlink to a shared runfiles directory.
""",
        ),
        "runfiles_shared_path": attr.string(
            mandatory = False,
            doc = """\
Optional path of the shared runfiles directory inside the image.
This is only used when runfiles sharing is enabled and has a global default.
""",
        ),
        "runfiles_sharing_mode": attr.string(
            mandatory = False,
            doc = """\
How to process runfiles.
Runfiles can either be placed next to the executable (in a directory with a .runfiles suffix, the runfiles_path attribute),
or placed in a shared runfiles path. When sharing runfiles, there will be symlink added: {runfiles_path} -> {runfiles_shared_path}.

Possible settings:

* `"auto"`: Share runfiles based on the global default and based on the presence of `RunfilesGroupInfo`.
    Globally, runfiles sharing can be set to `"shared"`, `"private"`, or `"auto"`, where auto shares runfiles if `RunfilesGroupInfo` is provided.
* `"shared"`: Always share runfiles.
* `"private"`: Never share runfiles
""",
            default = "auto",
            values = ["auto", "shared", "private"],
        ),
        "layer_budget": attr.int(
            default = 0,
            doc = """\
Maximum total number of layers produced by this rule.
If set to a value > 0 and the binary provides RunfilesGroupInfo, groups are merged
using the merge algorithm from rules_runfiles_group. The algorithm respects
group rank (only merges within the same rank), do_not_merge flags, merge affinity
(groups sharing an affinity are preferred merge partners), and weight hints
(lighter groups merge first).

When the binary names an executable_group, the binary executable and supporting files
are merged into that group's layer, and the full budget is available for runfiles
groups. When no executable_group exists, one layer is reserved for a separate binary
layer, and the remaining budget (layer_budget - 1) is used for groups;
layer_budget=1 without an executable_group skips the grouped path entirely.

This is a target, not a hard cap: groups marked do_not_merge are never merged, and
groups at different ranks never merge with each other, so a binary whose groups cannot
be reduced far enough still produces more layers than the budget.

0 means no limit (all groups become separate layers, plus a binary layer unless
an executable_group absorbs it).
""",
        ),
    } | layer_attrs.common,
    toolchains = TOOLCHAINS,
    provides = [LayersInfo],
)
