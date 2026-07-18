"""Shared deploy metadata computation functions.

Extracted from push.bzl and load.bzl so that image_manifest and image_index
can also compute deploy metadata when they have push_specs/load_specs attached.
"""

load("//img/private:layer_path_hints.bzl", "layer_hints_for_deploy_metadata")
load("//img/private:stamp.bzl", "expand_or_write")
load("//img/private/common:build.bzl", "TOOLCHAIN")
load("//img/private/common:deploy_helpers.bzl", "content_tracking_json_vars")
load("//img/private/providers:deploy_info.bzl", "DeployInfo")
load("//img/private/providers:load_config_info.bzl", "LoadConfigInfo")
load("//img/private/providers:push_config_info.bzl", "PushConfigInfo")

def _manifest_layer_sources(manifest_info):
    """Return the per-layer upstream sources of a manifest, aligned with its layers.

    Each element corresponds to a layer (in order) and is a list of
    {"registry": .., "repository": ..} dicts (empty for layers with no source).
    """
    return [
        [{"registry": source.registry, "repository": source.repository} for source in layer.sources]
        for layer in manifest_info.layers
    ]

def _write_layer_sources_file(ctx, *, manifest_info, index_info, output_prefix):
    """Write a JSON side file describing each layer's upstream sources.

    The file maps a manifest index (as a string) to a list aligned with that
    manifest's layers, each element being the list of sources for that layer. It is
    consumed by the deploy-metadata tool (`--layer-sources-file`) to populate the
    per-layer `sources` in the deploy manifest. Returns None (and writes nothing)
    when no layer records any source, so unaffected images stay byte-identical.
    """
    mapping = {}
    has_any = False
    if manifest_info != None:
        per_layer = _manifest_layer_sources(manifest_info)
        mapping["0"] = per_layer
        has_any = any([len(entry) > 0 for entry in per_layer])
    if index_info != None:
        for i, manifest in enumerate(index_info.manifests):
            per_layer = _manifest_layer_sources(manifest)
            mapping[str(i)] = per_layer
            if any([len(entry) > 0 for entry in per_layer]):
                has_any = True
    if not has_any:
        return None
    out = ctx.actions.declare_file(output_prefix + ".layer_sources.json")
    ctx.actions.write(out, json.encode(mapping))
    return out

def _add_manifest_compact_streams(manifest_index, manifest_info, args, inputs):
    """Record each compact-stream layer's .cstream for the deploy-metadata tool.

    For the "bes" strategy the layer's compressed blob is never materialized, so
    the syncer reconstructs it from the .cstream. We pass the .cstream so the tool
    can record its CAS digest, and add the .cstream plus the layer's
    content-addressed input files as action inputs so Bazel uploads them to the CAS
    the syncer reads from.
    """
    for layer_index, layer in enumerate(manifest_info.layers):
        if layer.compact_stream == None:
            continue
        args.add("--layer-compact-stream", "{},{}={}".format(manifest_index, layer_index, layer.compact_stream.path))
        inputs.append(layer.compact_stream)
        if layer.layer_input_files_cas != None:
            inputs.append(layer.layer_input_files_cas)

def _add_compact_stream_args(manifest_info, index_info, args, inputs):
    """Add --layer-compact-stream args for all compact-stream layers of the image."""
    if manifest_info != None:
        _add_manifest_compact_streams(0, manifest_info, args, inputs)
    if index_info != None:
        for i, manifest in enumerate(index_info.manifests):
            _add_manifest_compact_streams(i, manifest, args, inputs)

def compute_push_metadata(
        ctx,
        *,
        configuration_json,
        manifest_info,
        index_info,
        strategy,
        cross_mount_strategy,
        cross_mount_from,
        referrers,
        manifest_tags_expanded,
        pull_info,
        destination_file,
        output_prefix):
    """Compute push metadata for a deploy operation.

    Args:
        ctx: Rule context (for ctx.actions, ctx.toolchains, ctx.label).
        configuration_json: File with expanded registry/repository/tags JSON.
        manifest_info: ImageManifestInfo or None.
        index_info: ImageIndexInfo or None.
        strategy: Resolved push strategy string (never 'auto').
        cross_mount_strategy: Resolved cross-mount strategy string.
        cross_mount_from: DeployInfo for cross-mounting, or None.
        referrers: List of struct(manifest_info, index_info) for referrer pushes.
        manifest_tags_expanded: List of (child_index, File) tuples with already-expanded tag files.
        pull_info: PullInfo or None (for original registry/repository/tag/digest).
        destination_file: File containing {registry}/{repository}, or None.
        output_prefix: String prefix for declared output files.

    Returns:
        Tuple of (metadata_file, layer_hints_file).
    """
    if manifest_info == None and index_info == None:
        fail("exactly one of manifest_info or index_info must be provided")
    if manifest_info != None and index_info != None:
        fail("exactly one of manifest_info or index_info must be provided, not both")

    inputs = [configuration_json]
    args = ctx.actions.args()
    push_metadata_args = [args]
    args.add("deploy-metadata")
    args.add("--command", "push")
    args.add("--strategy", strategy)
    args.add("--configuration-file", configuration_json.path)

    if destination_file != None:
        inputs.append(destination_file)
        args.add("--destination-file", destination_file.path)

    if pull_info != None:
        if pull_info.registries:
            args.add_all(pull_info.registries, before_each = "--original-registry")
        if pull_info.repository:
            args.add("--original-repository", pull_info.repository)
        if pull_info.tag != None:
            args.add("--original-tag", pull_info.tag)
        if pull_info.digest != None:
            args.add("--original-digest", pull_info.digest)

    args.add("--cross-mount-strategy={}".format(cross_mount_strategy))

    if cross_mount_from != None:
        inputs.append(cross_mount_from.deploy_manifest)
        args.add("--cross-mount-from-manifest-path", cross_mount_from.deploy_manifest.path)

    if manifest_info != None:
        args.add("--root-path", manifest_info.manifest.path)
        args.add("--root-kind", "manifest")
        args.add("--manifest-path", "0=" + manifest_info.manifest.path)
        inputs.append(manifest_info.manifest)

    if index_info != None:
        args.add("--root-path", index_info.index.path)
        args.add("--root-kind", "index")
        for i, manifest in enumerate(index_info.manifests):
            args.add("--manifest-path", "{}={}".format(i, manifest.manifest.path))
        for child_index, tag_file in manifest_tags_expanded:
            args.add("--manifest-tag-file", "{}={}".format(child_index, tag_file.path))
            inputs.append(tag_file)
        inputs.append(index_info.index)
        inputs.extend([manifest.manifest for manifest in index_info.manifests])

    layer_sources_file = _write_layer_sources_file(
        ctx,
        manifest_info = manifest_info,
        index_info = index_info,
        output_prefix = output_prefix,
    )
    if layer_sources_file != None:
        inputs.append(layer_sources_file)
        args.add("--layer-sources-file", layer_sources_file.path)

    # For the bes strategy, compact-stream layers are reconstructed by the syncer
    # from the CAS, so record each .cstream and pull its input files into the CAS.
    if strategy == "bes":
        _add_compact_stream_args(manifest_info, index_info, args, inputs)

    for ref_idx, referrer in enumerate(referrers):
        ref_manifest_info = referrer.manifest_info
        ref_index_info = referrer.index_info
        if ref_manifest_info != None:
            args.add("--referrer-root-path", "{}={}".format(ref_idx, ref_manifest_info.manifest.path))
            args.add("--referrer-root-kind", "{}=manifest".format(ref_idx))
            args.add("--referrer-manifest-path", "{},0={}".format(ref_idx, ref_manifest_info.manifest.path))
            inputs.append(ref_manifest_info.manifest)
        elif ref_index_info != None:
            args.add("--referrer-root-path", "{}={}".format(ref_idx, ref_index_info.index.path))
            args.add("--referrer-root-kind", "{}=index".format(ref_idx))
            for i, manifest in enumerate(ref_index_info.manifests):
                args.add("--referrer-manifest-path", "{},{}={}".format(ref_idx, i, manifest.manifest.path))
            inputs.append(ref_index_info.index)
            inputs.extend([manifest.manifest for manifest in ref_index_info.manifests])

    outputs = []
    layer_hints_file = layer_hints_for_deploy_metadata(
        ctx,
        index_info = index_info,
        manifest_info = manifest_info,
        strategy = strategy,
        args = push_metadata_args,
        inputs = inputs,
        outputs = outputs,
    )
    metadata_out = ctx.actions.declare_file(output_prefix + ".json")
    output_args = ctx.actions.args()
    output_args.add(metadata_out)
    push_metadata_args.append(output_args)
    outputs.append(metadata_out)
    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = inputs,
        outputs = outputs,
        executable = img_toolchain_info.tool_exe,
        arguments = push_metadata_args,
        mnemonic = "PushMetadata",
    )
    return metadata_out, layer_hints_file

def expand_manifest_tags_for_child(
        ctx,
        *,
        child_index,
        child_info,
        manifest_tags,
        build_settings_override,
        stamp_override,
        stamp_settings_override,
        output_prefix,
        extra_build_settings = None):
    """Expand manifest_tags templates for a single child manifest in an index.

    Args:
        ctx: Rule context.
        child_index: Index of the child manifest.
        child_info: ImageManifestInfo of the child manifest.
        manifest_tags: List of tag template strings.
        build_settings_override: Dict(string, string) of build settings.
        stamp_override: Stamp preference string.
        stamp_settings_override: StampSettingInfo provider.
        output_prefix: String prefix for output file names.
        extra_build_settings: Optional dict of additional template variables (merged with platform vars).

    Returns:
        Expanded tag File, or None if no expansion needed.
    """
    merged_extra = {}
    if extra_build_settings:
        merged_extra.update(extra_build_settings)
    merged_extra.update({
        "os": child_info.os or "",
        "architecture": child_info.architecture or "",
        "arch": child_info.architecture or "",
        "cpu": child_info.architecture or "",
        "variant": child_info.variant or "",
    })
    templates = dict(manifest_tags = manifest_tags)
    return expand_or_write(
        ctx = ctx,
        templates = templates,
        output_name = "{}.manifest_tags.{}.json".format(output_prefix, child_index),
        extra_build_settings = merged_extra,
        build_settings_override = build_settings_override,
        stamp_override = stamp_override,
        stamp_settings_override = stamp_settings_override,
    )

def merge_deploy_manifests(ctx, *, deploy_infos, push_strategy = "auto", load_strategy = "auto"):
    """Merge multiple deploy manifests using the deploy-merge tool.

    Args:
        ctx: Rule context.
        deploy_infos: List of struct(metadata=File, layer_hints=File-or-None).
        push_strategy: Push strategy string for the merge.
        load_strategy: Load strategy string for the merge.

    Returns:
        Tuple of (merged_metadata_file, merged_layer_hints_file).
    """
    inputs = []
    args = ctx.actions.args()
    args.add("deploy-merge")
    args.add("--push-strategy", push_strategy)
    args.add("--load-strategy", load_strategy)

    layer_hints_files = []
    for info in deploy_infos:
        inputs.append(info.metadata)
        if info.layer_hints != None:
            layer_hints_files.append(info.layer_hints)
            inputs.append(info.layer_hints)

    layer_hints_out = None
    if layer_hints_files:
        for f in layer_hints_files:
            args.add("--layer-hints-input", f.path)
        layer_hints_out = ctx.actions.declare_file(ctx.label.name + ".deploy_merged.layer_hints")
        args.add("--layer-hints-output", layer_hints_out.path)

    for info in deploy_infos:
        args.add(info.metadata.path)

    metadata_out = ctx.actions.declare_file(ctx.label.name + ".deploy_merged.json")
    args.add(metadata_out.path)

    outputs = [metadata_out]
    if layer_hints_out != None:
        outputs.append(layer_hints_out)

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = inputs,
        outputs = outputs,
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        mnemonic = "DeployMerge",
    )
    return metadata_out, layer_hints_out

def compute_load_metadata(
        ctx,
        *,
        configuration_json,
        manifest_info,
        index_info,
        strategy,
        pull_info,
        output_prefix):
    """Compute load metadata for a deploy operation.

    Args:
        ctx: Rule context (for ctx.actions, ctx.toolchains, ctx.label).
        configuration_json: File with expanded tags/daemon JSON.
        manifest_info: ImageManifestInfo or None.
        index_info: ImageIndexInfo or None.
        strategy: Resolved load strategy string (never 'auto').
        pull_info: PullInfo or None (for original registry/repository/tag/digest).
        output_prefix: String prefix for declared output files.

    Returns:
        Tuple of (metadata_file, layer_hints_file).
    """
    if manifest_info == None and index_info == None:
        fail("exactly one of manifest_info or index_info must be provided")
    if manifest_info != None and index_info != None:
        fail("exactly one of manifest_info or index_info must be provided, not both")

    inputs = [configuration_json]
    args = ctx.actions.args()
    load_metadata_args = [args]
    args.add("deploy-metadata")
    args.add("--command", "load")
    args.add("--strategy", strategy)
    args.add("--configuration-file", configuration_json.path)

    if pull_info != None:
        if pull_info.registries:
            args.add_all(pull_info.registries, before_each = "--original-registry")
        if pull_info.repository:
            args.add("--original-repository", pull_info.repository)
        if pull_info.tag != None:
            args.add("--original-tag", pull_info.tag)
        if pull_info.digest != None:
            args.add("--original-digest", pull_info.digest)

    if manifest_info != None:
        args.add("--root-path", manifest_info.manifest.path)
        args.add("--root-kind", "manifest")
        args.add("--manifest-path", "0=" + manifest_info.manifest.path)
        inputs.append(manifest_info.manifest)

    if index_info != None:
        args.add("--root-path", index_info.index.path)
        args.add("--root-kind", "index")
        for i, manifest in enumerate(index_info.manifests):
            args.add("--manifest-path", "{}={}".format(i, manifest.manifest.path))
        inputs.append(index_info.index)
        inputs.extend([manifest.manifest for manifest in index_info.manifests])

    layer_sources_file = _write_layer_sources_file(
        ctx,
        manifest_info = manifest_info,
        index_info = index_info,
        output_prefix = output_prefix,
    )
    if layer_sources_file != None:
        inputs.append(layer_sources_file)
        args.add("--layer-sources-file", layer_sources_file.path)

    outputs = []
    layer_hints_file = layer_hints_for_deploy_metadata(
        ctx,
        index_info = index_info,
        manifest_info = manifest_info,
        strategy = strategy,
        args = load_metadata_args,
        inputs = inputs,
        outputs = outputs,
    )
    metadata_out = ctx.actions.declare_file(output_prefix + ".load.json")
    output_args = ctx.actions.args()
    output_args.add(metadata_out)
    load_metadata_args.append(output_args)
    outputs.append(metadata_out)
    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = inputs,
        outputs = outputs,
        executable = img_toolchain_info.tool_exe,
        arguments = load_metadata_args,
        mnemonic = "LoadMetadata",
    )
    return metadata_out, layer_hints_file

def process_deploy_specs(
        ctx,
        *,
        manifest_info,
        index_info,
        manifest_infos,
        pull_info,
        push_specs,
        load_specs,
        allow_manifest_tags):
    """Process push_specs and load_specs to produce a DeployInfo provider.

    Args:
        ctx: Rule context.
        manifest_info: ImageManifestInfo or None.
        index_info: ImageIndexInfo or None.
        manifest_infos: List of child ImageManifestInfo (for index manifest_tags expansion). Pass [] for manifest.
        pull_info: PullInfo or None.
        push_specs: List of targets providing PushConfigInfo.
        load_specs: List of targets providing LoadConfigInfo.
        allow_manifest_tags: If False, fail when a push spec has manifest_tags set.

    Returns:
        DeployInfo provider, or None if no specs are provided.
    """
    if not push_specs and not load_specs:
        return None

    image_info = manifest_info if manifest_info != None else index_info
    image_target_vars = {
        "image_target_package": ctx.label.package,
        "image_target_name": ctx.label.name,
    }

    deploy_infos = []

    for push_idx, deployment in enumerate(push_specs):
        push_config = deployment[PushConfigInfo]

        if not allow_manifest_tags and push_config.manifest_tags:
            fail("'manifest_tags' in push spec '{}' cannot be used with image_manifest (single-platform). Use image_index instead.".format(deployment.label))

        templates = dict(
            registry = push_config.registry,
            repository = push_config.repository,
            tags = push_config.tags,
        )
        newline_delimited_lists_files = None
        if push_config.tag_file:
            newline_delimited_lists_files = {"tags": push_config.tag_file}

        # When tracks_content is set, expose the image descriptor as a json-var so
        # the tag re-stamps when the digest changes and {{.digest}} is available.
        json_vars, json_path_to_root = content_tracking_json_vars(
            image_info.descriptor if push_config.tracks_content else None,
        )

        configuration_json = expand_or_write(
            ctx = ctx,
            templates = templates,
            output_name = "{}.push_deploy.{}.configuration.json".format(ctx.label.name, push_idx),
            newline_delimited_lists_files = newline_delimited_lists_files,
            build_settings_override = push_config.build_settings,
            stamp_override = push_config.stamp,
            stamp_settings_override = push_config.stamp_settings,
            extra_build_settings = image_target_vars,
            json_vars = json_vars,
            json_path_to_root = json_path_to_root,
        )

        manifest_tags_expanded = []
        if push_config.manifest_tags:
            for i, manifest in enumerate(manifest_infos):
                tag_file = expand_manifest_tags_for_child(
                    ctx,
                    child_index = i,
                    child_info = manifest,
                    manifest_tags = push_config.manifest_tags,
                    build_settings_override = push_config.build_settings,
                    stamp_override = push_config.stamp,
                    stamp_settings_override = push_config.stamp_settings,
                    output_prefix = "{}.push_deploy.{}".format(ctx.label.name, push_idx),
                    extra_build_settings = image_target_vars,
                )
                if tag_file != None:
                    manifest_tags_expanded.append((i, tag_file))

        deploy_metadata, layer_hints = compute_push_metadata(
            ctx,
            configuration_json = configuration_json,
            manifest_info = manifest_info,
            index_info = index_info,
            strategy = push_config.strategy,
            cross_mount_strategy = push_config.cross_mount_strategy,
            cross_mount_from = push_config.cross_mount_from,
            referrers = push_config.referrers,
            manifest_tags_expanded = manifest_tags_expanded,
            pull_info = pull_info,
            destination_file = push_config.destination_file,
            output_prefix = "{}.push_deploy.{}".format(ctx.label.name, push_idx),
        )
        deploy_infos.append(struct(metadata = deploy_metadata, layer_hints = layer_hints))

    for load_idx, deployment in enumerate(load_specs):
        load_config = deployment[LoadConfigInfo]

        templates = dict(
            registry = load_config.registry,
            repository = load_config.repository,
            tags = load_config.tags,
            daemon = load_config.daemon,
        )
        newline_delimited_lists_files = None
        if load_config.tag_file:
            newline_delimited_lists_files = {"tags": load_config.tag_file}

        # When tracks_content is set, expose the image descriptor as a json-var so
        # the tag re-stamps when the digest changes and {{.digest}} is available.
        json_vars, json_path_to_root = content_tracking_json_vars(
            image_info.descriptor if load_config.tracks_content else None,
        )

        configuration_json = expand_or_write(
            ctx = ctx,
            templates = templates,
            output_name = "{}.load_deploy.{}.configuration.json".format(ctx.label.name, load_idx),
            newline_delimited_lists_files = newline_delimited_lists_files,
            build_settings_override = load_config.build_settings,
            stamp_override = load_config.stamp,
            stamp_settings_override = load_config.stamp_settings,
            extra_build_settings = image_target_vars,
            json_vars = json_vars,
            json_path_to_root = json_path_to_root,
        )

        deploy_metadata, layer_hints = compute_load_metadata(
            ctx,
            configuration_json = configuration_json,
            manifest_info = manifest_info,
            index_info = index_info,
            strategy = load_config.strategy,
            pull_info = pull_info,
            output_prefix = "{}.load_deploy.{}".format(ctx.label.name, load_idx),
        )
        deploy_infos.append(struct(metadata = deploy_metadata, layer_hints = layer_hints))

    include_layers = (
        any([d[PushConfigInfo].strategy == "eager" for d in push_specs]) or
        any([d[LoadConfigInfo].strategy == "eager" for d in load_specs])
    )

    if len(deploy_infos) == 1:
        return DeployInfo(
            image = image_info,
            deploy_manifest = deploy_infos[0].metadata,
            layer_hints = deploy_infos[0].layer_hints,
            include_layers = include_layers,
        )

    first_push_strategy = push_specs[0][PushConfigInfo].strategy if push_specs else "auto"
    first_load_strategy = load_specs[0][LoadConfigInfo].strategy if load_specs else "auto"

    merged_metadata, merged_layer_hints = merge_deploy_manifests(
        ctx,
        deploy_infos = deploy_infos,
        push_strategy = first_push_strategy,
        load_strategy = first_load_strategy,
    )
    return DeployInfo(
        image = image_info,
        deploy_manifest = merged_metadata,
        layer_hints = merged_layer_hints,
        include_layers = include_layers,
    )
