"""Image rule for assembling OCI images based on a set of layers."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private:annotations_util.bzl", "extract_annotations_from_pull_info")
load("//img/private:push_metadata.bzl", "process_deploy_specs")
load("//img/private:stamp.bzl", "expand_or_write")
load("//img/private/common:build.bzl", "TOOLCHAIN", "TOOLCHAINS")
load("//img/private/common:inherit.bzl", "INHERIT_FROM_BASE")
load("//img/private/common:layer_helper.bzl", "allow_tar_files", "build_image_mtree", "calculate_layer_info", "extension_to_compression", "image_layer_mtrees")
load("//img/private/common:transitions.bzl", "normalize_layer_transition", "single_platform_transition")
load("//img/private/config:defs.bzl", "TargetPlatformInfo")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
load("//img/private/providers:layer_config_info.bzl", "ImageLayerConfigInfo")
load("//img/private/providers:layers_info.bzl", "LayersInfo")
load("//img/private/providers:load_config_info.bzl", "LoadConfigInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")
load("//img/private/providers:oci_layout_settings_info.bzl", "OCILayoutSettingsInfo")
load("//img/private/providers:pull_info.bzl", "PullInfo")
load("//img/private/providers:push_at_build_time_settings_info.bzl", "PushAtBuildTimeSettingsInfo")
load("//img/private/providers:push_config_info.bzl", "PushConfigInfo")
load("//img/private/providers:single_layer_info.bzl", "SingleLayerInfo")
load("//img/private/providers:stamp_setting_info.bzl", "StampSettingInfo")

def _to_layer_arg(layer):
    """Convert a layer to a command line argument."""
    return layer.metadata.path

def _platform_vector(os, architecture, variant):
    """Generate an ordered vector of compatible platforms (best to worst).

    Based on containerd's platformVector logic:
    https://github.com/containerd/platforms/blob/2e51fd9435bd985e1753954b24f4b0453f4e4767/compare.go#L64

    Args:
        os: Operating system
        architecture: CPU architecture
        variant: Platform variant (may be empty)

    Returns:
        List of platform dicts in preference order (best match first)
    """
    base_platform = {
        "os": os,
        "architecture": architecture,
        "variant": variant,
    }
    vector = [base_platform]

    # AMD64: Parse variant as integer and create fallback chain
    if architecture == "amd64" and variant != "":
        # Try to parse variant like "v3" -> 3
        if variant.startswith("v"):
            variant_num_str = variant[1:]  # Remove "v" prefix
            if variant_num_str.isdigit():
                amd64_version = int(variant_num_str)
                if amd64_version > 1:
                    # Add fallback variants: v3 -> v2, v1
                    for v in range(amd64_version - 1, 0, -1):
                        vector.append({
                            "os": os,
                            "architecture": architecture,
                            "variant": "v" + str(v),
                        })

        # Add base amd64 (no variant) as final fallback
        vector.append({
            "os": os,
            "architecture": architecture,
            "variant": "",
        })

        # ARM 32-bit: Parse variant as integer and create fallback chain
    elif architecture == "arm" and variant != "":
        if variant.startswith("v"):
            variant_num_str = variant[1:]
            if variant_num_str.isdigit():
                arm_version = int(variant_num_str)
                if arm_version > 5:
                    # Add fallback variants: v7 -> v6, v5
                    for v in range(arm_version - 1, 4, -1):
                        vector.append({
                            "os": os,
                            "architecture": architecture,
                            "variant": "v" + str(v),
                        })

        # ARM64: Complex fallback with v8.x and v9.x support
    elif architecture == "arm64":
        # ARM64 variant defaults to v8 (already normalized by TargetPlatformInfo)
        effective_variant = variant if variant != "" else "v8"

        # Simplified arm64 variant support
        # Full implementation would need arm64variantToVersion map from containerd
        # For now, support basic v8 and v9 variants
        if effective_variant == "v8" or effective_variant.startswith("v8."):
            # v8.x can fall back to lower v8.y versions
            if effective_variant.startswith("v8."):
                # Parse v8.5 -> major=8, minor=5
                parts = effective_variant[1:].split(".")  # "8.5" -> ["8", "5"]
                if len(parts) == 2 and parts[0].isdigit() and parts[1].isdigit():
                    minor = int(parts[1])

                    # Add fallback from v8.5 -> v8.4 -> ... -> v8.0 -> v8
                    for m in range(minor - 1, -1, -1):
                        if m == 0:
                            vector.append({
                                "os": os,
                                "architecture": architecture,
                                "variant": "v8",
                            })
                        else:
                            vector.append({
                                "os": os,
                                "architecture": architecture,
                                "variant": "v8." + str(m),
                            })
        elif effective_variant == "v9" or effective_variant.startswith("v9."):
            # v9.x can fall back to lower v9.y, then to v8.x
            if effective_variant.startswith("v9."):
                parts = effective_variant[1:].split(".")
                if len(parts) == 2 and parts[0].isdigit() and parts[1].isdigit():
                    minor = int(parts[1])

                    # Add v9 fallbacks
                    for m in range(minor - 1, -1, -1):
                        if m == 0:
                            vector.append({
                                "os": os,
                                "architecture": architecture,
                                "variant": "v9",
                            })
                        else:
                            vector.append({
                                "os": os,
                                "architecture": architecture,
                                "variant": "v9." + str(m),
                            })

            # v9.x falls back to v8.5+ (per containerd mapping)
            # Simplified: just fall back to v8
            vector.append({
                "os": os,
                "architecture": architecture,
                "variant": "v8",
            })

    return vector

def _platform_matches_exact(wanted_platform, manifest):
    """Check if the wanted platform exactly matches the manifest platform.

    Args:
        wanted_platform: Dict with os, architecture, variant keys
        manifest: Manifest info with os, architecture, variant attributes

    Returns:
        True if all fields match exactly
    """
    if wanted_platform["os"] != manifest.os:
        return False
    if wanted_platform["architecture"] != manifest.architecture:
        return False

    # Check variant (both may be empty string)
    wanted_variant = wanted_platform.get("variant", "")
    manifest_variant = manifest.variant
    if wanted_variant != manifest_variant:
        return False

    return True

def select_base(ctx):
    """Select the base image to use for this image.

    Uses containerd's platform matching logic with variant fallback.

    Args:
        ctx: Rule context containing base image information.

    Returns:
        ImageManifestInfo for the selected base image, or None if no base.
    """
    if ctx.attr.base == None:
        return None
    if ImageManifestInfo in ctx.attr.base:
        return ctx.attr.base[ImageManifestInfo]
    if ImageIndexInfo not in ctx.attr.base:
        fail("base image must be an ImageManifestInfo or ImageIndexInfo")

    os_wanted = ctx.attr._os_cpu[TargetPlatformInfo].os
    arch_wanted = ctx.attr._os_cpu[TargetPlatformInfo].cpu
    variant_wanted = ctx.attr._os_cpu[TargetPlatformInfo].variant

    # Generate platform vector (ordered from best to worst match)
    platform_vector = _platform_vector(os_wanted, arch_wanted, variant_wanted)

    # Try each platform in the vector (best match first)
    for wanted_platform in platform_vector:
        for manifest in ctx.attr.base[ImageIndexInfo].manifests:
            if _platform_matches_exact(wanted_platform, manifest):
                return manifest

    # No match found - generate helpful error message
    variant_msg = ""
    if variant_wanted != "":
        variant_msg = " variant={}".format(variant_wanted)
    fail("no matching base image found for os={} architecture={}{}".format(
        os_wanted,
        arch_wanted,
        variant_msg,
    ))

def _build_oci_layout(ctx, format, manifest_out, config_out, layers):
    """Build the OCI layout for the image.

    Args:
        ctx: Rule context.
        format: The output format, either "directory" or "tar".
        manifest_out: The manifest file.
        config_out: The config file.
        layers: List of SingleLayerInfo providers.

    Returns:
        The OCI layout directory (tree artifact).
    """
    if format not in ["directory", "tar"]:
        fail('oci layout format must be either "directory" or "tar"')
    oci_layout_output = None
    if format == "directory":
        oci_layout_output = ctx.actions.declare_directory(ctx.label.name + "_oci_layout")
    else:
        oci_layout_output = ctx.actions.declare_file(ctx.label.name + "_oci_layout.tar")

    args = ctx.actions.args()
    args.add("oci-layout")
    args.add("--format", format)
    args.add("--manifest", manifest_out.path)
    args.add("--config", config_out.path)
    args.add("--output", oci_layout_output.path)
    if ctx.attr._oci_layout_settings[OCILayoutSettingsInfo].allow_shallow_oci_layout:
        args.add("--allow-missing-blobs")

    inputs = [manifest_out, config_out]

    # Add layers with metadata=blob mapping
    for layer in layers:
        if layer.blob != None:
            args.add("--layer", "{}={}".format(layer.metadata.path, layer.blob.path))
            inputs.append(layer.metadata)
            inputs.append(layer.blob)

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = inputs,
        outputs = [oci_layout_output],
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        env = {"RULES_IMG": "1"},
        mnemonic = "OCILayout",
    )

    return oci_layout_output

def _build_sparse_oci_layout(ctx, format, manifest_out, config_out, layers):
    """Build a sparse OCI layout for the image (without layer blobs).

    Args:
        ctx: Rule context.
        format: The output format, either "directory" or "tar".
        manifest_out: The manifest file.
        config_out: The config file.
        layers: List of SingleLayerInfo providers.

    Returns:
        The sparse OCI layout output (tree artifact or tar file).
    """
    if format not in ["directory", "tar"]:
        fail('sparse oci layout format must be either "directory" or "tar"')
    if format == "directory":
        output = ctx.actions.declare_directory(ctx.label.name + "_sparse_oci_layout")
    else:
        output = ctx.actions.declare_file(ctx.label.name + "_sparse_oci_layout.tar")

    args = ctx.actions.args()
    args.add("sparse-oci-layout")
    args.add("--format", format)
    args.add("--manifest", manifest_out.path)
    args.add("--config", config_out.path)
    args.add("--output", output.path)

    inputs = [manifest_out, config_out]

    for layer in layers:
        args.add("--layer", layer.metadata.path)
        inputs.append(layer.metadata)
        if layer.compact_stream != None:
            args.add("--layer-compact-stream", "{}={}".format(layer.metadata.path, layer.compact_stream.path))
            inputs.append(layer.compact_stream)

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = inputs,
        outputs = [output],
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        mnemonic = "SparseOCILayout",
    )

    return output

def _image_manifest_impl(ctx):
    inputs = []
    providers = []
    args = ctx.actions.args()
    args.add("manifest")
    base = select_base(ctx)
    os = ctx.attr._os_cpu[TargetPlatformInfo].os
    arch = ctx.attr._os_cpu[TargetPlatformInfo].cpu
    variant = ctx.attr._os_cpu[TargetPlatformInfo].variant
    history = []
    layers = []
    pull_info = None
    computed_annotations = {}
    if base != None:
        history = base.structured_config.get("history", [])
        layers.extend(base.layers)
        inputs.append(base.manifest)
        inputs.append(base.config)
        inputs.append(base.descriptor)
        args.add("--base-manifest", base.manifest.path)
        args.add("--base-config", base.config.path)
        args.add("--base-descriptor", base.descriptor.path)
    if ctx.attr.base != None and PullInfo in ctx.attr.base:
        pull_info = ctx.attr.base[PullInfo]
        computed_annotations.update(extract_annotations_from_pull_info(pull_info))
        providers.append(pull_info)
    for (layer_idx, layer) in enumerate(ctx.attr.layers):
        if LayersInfo in layer:
            layers.extend(layer[LayersInfo].layers)
            continue
        elif SingleLayerInfo in layer:
            fail("layer {} provides SingleLayerInfo directly, but LayersInfo is expected. Wrap layers using a layer rule that provides LayersInfo.".format(layer_idx))
        elif DefaultInfo not in layer:
            fail("layer {} needs to provide LayersInfo or DefaultInfo: {}".format(layer_idx, layer))

        # Calculate layer metadata on the fly
        default_info = layer[DefaultInfo]
        files = default_info.files.to_list()
        for (tar_idx, tar_file) in enumerate(files):
            found_extension = False
            for extension in allow_tar_files:
                if tar_file.path.endswith(extension):
                    found_extension = True
                    break
            if not found_extension:
                fail("layer with DefaultInfo must be a tar file with one of the following extensions: {}, but got: {}".format(allow_tar_files, tar_file.path))
            compression = extension_to_compression[tar_file.extension]
            media_type = "application/vnd.oci.image.layer.v1.tar"
            metadata_file = ctx.actions.declare_file("{}_metadata_layer_{}_{}.json".format(ctx.attr.name, layer_idx, tar_idx))
            if compression != "none":
                media_type += "+{}".format(compression)
            layer_info = calculate_layer_info(
                ctx = ctx,
                media_type = media_type,
                tar_file = tar_file,
                metadata_file = metadata_file,
                estargz = False,
                annotations = {},
            )
            layers.append(layer_info)

    # Merge ImageLayerConfigInfo from layers (Dockerfile-like semantics).
    # Later layers override earlier layers for entrypoint, cmd, and working_dir.
    # Env is merged across all layers. Rule attrs always win over layer config.
    layer_entrypoint = None
    layer_cmd = None
    layer_working_dir = None
    layer_env = {}
    for layer in ctx.attr.layers:
        if ImageLayerConfigInfo in layer:
            config_info = layer[ImageLayerConfigInfo]
            if config_info.entrypoint != None:
                layer_entrypoint = config_info.entrypoint
            if config_info.cmd != None:
                layer_cmd = config_info.cmd
            if config_info.working_dir != None:
                layer_working_dir = config_info.working_dir
            if config_info.env != None:
                layer_env.update(config_info.env)

    merged_env = dict(layer_env)
    merged_env.update(ctx.attr.env)

    # entrypoint, cmd, and working_dir support three states, distinguished by the
    # INHERIT_FROM_BASE sentinel (which is also their default value):
    #   * left at the sentinel default -> defer to a non-empty layer-provided config
    #     value if any, otherwise forward the sentinel so the tool inherits from the
    #     base. (An empty layer value carries no opinion and inherits, matching the
    #     historical behavior.)
    #   * explicitly set (including to an empty value) -> forward verbatim, so an
    #     empty value unsets the field and a value containing the sentinel expands
    #     the base value in place (see img/private/common/inherit.bzl).
    if ctx.attr.entrypoint == [INHERIT_FROM_BASE]:
        effective_entrypoint = layer_entrypoint if layer_entrypoint else [INHERIT_FROM_BASE]
    else:
        effective_entrypoint = ctx.attr.entrypoint
    if ctx.attr.cmd == [INHERIT_FROM_BASE]:
        effective_cmd = layer_cmd if layer_cmd else [INHERIT_FROM_BASE]
    else:
        effective_cmd = ctx.attr.cmd
    if ctx.attr.working_dir == INHERIT_FROM_BASE:
        effective_working_dir = layer_working_dir if layer_working_dir else INHERIT_FROM_BASE
    else:
        effective_working_dir = ctx.attr.working_dir

    args.add("--os", os)
    args.add("--architecture", arch)
    if variant != "":
        args.add("--variant", variant)
    for layer in layers:
        inputs.append(layer.metadata)
    args.add_all(layers, format_each = "--layer-from-metadata=%s", map_each = _to_layer_arg, expand_directories = False)
    if ctx.attr.config_fragment != None:
        inputs.append(ctx.file.config_fragment)
        args.add("--config-fragment", ctx.file.config_fragment.path)
    if ctx.attr.config_media_type != None and ctx.attr.config_media_type != "":
        if ctx.attr.config_fragment == None and ctx.attr.config_media_type != "application/vnd.oci.empty.v1+json":
            fail("config_media_type requires config_fragment (e.g. Helm config JSON), unless set to application/vnd.oci.empty.v1+json")
        args.add("--config-media-type", ctx.attr.config_media_type)
    if ctx.attr.artifact_type:
        args.add("--artifact-type", ctx.attr.artifact_type)
    if ctx.attr.created != None:
        inputs.append(ctx.file.created)
        args.add("--created", ctx.file.created.path)

    # Resolve subject descriptor if provided
    if ctx.attr.subject != None:
        if ImageManifestInfo in ctx.attr.subject:
            subject_descriptor_file = ctx.attr.subject[ImageManifestInfo].descriptor
        elif ImageIndexInfo in ctx.attr.subject:
            subject_info = ctx.attr.subject[ImageIndexInfo]
            subject_descriptor_file = subject_info.descriptor
        else:
            fail("subject must provide ImageManifestInfo or ImageIndexInfo")
        inputs.append(subject_descriptor_file)
        args.add("--subject-descriptor", subject_descriptor_file.path)

    # Prepare newline_delimited_lists_files if annotations_file or label_files is provided
    newline_delimited_lists_files = None
    if ctx.attr.annotations_file != None or ctx.attr.label_files:
        newline_delimited_lists_files = {}
        if ctx.attr.annotations_file != None:
            newline_delimited_lists_files["annotations"] = ctx.file.annotations_file
        if ctx.attr.label_files:
            newline_delimited_lists_files["labels"] = ctx.files.label_files

    # Prepare json_vars with base image data if available
    json_vars = None
    json_path_to_root = None
    expose_kvs = None
    if base != None:
        json_vars = {
            "base.config": base.config,
            "base.manifest": base.manifest,
            "base.digest": base.descriptor,
        }
        json_path_to_root = {"base.digest": "digest"}
        expose_kvs = ["base.config.config.env"]

    # Finalize annotation templates.
    # The values set by the user directly on the image_manifest
    # take precedence over everything else.
    computed_annotations.update(ctx.attr.annotations)

    # Handle template expansion for labels, env, and annotations
    templates = {
        "env": merged_env,
        "labels": ctx.attr.labels,
        "annotations": computed_annotations,
    }

    # Try to expand templates - this will return None if no templates need expansion
    config_json = expand_or_write(
        ctx = ctx,
        templates = templates,
        output_name = ctx.label.name + "_config_templates.json",
        only_if_stamping = True,
        newline_delimited_lists_files = newline_delimited_lists_files,
        json_vars = json_vars,
        json_path_to_root = json_path_to_root,
        expose_kvs = expose_kvs,
    )

    if config_json != None:
        # Templates were expanded, use the config-templates flag
        inputs.append(config_json)
        args.add("--config-templates", config_json.path)
    else:
        # No templates to expand, use direct values
        for key, value in merged_env.items():
            args.add("--env", "%s=%s" % (key, value))
        for key, value in ctx.attr.labels.items():
            args.add("--label", "%s=%s" % (key, value))
        for key, value in computed_annotations.items():
            args.add("--annotation", "%s=%s" % (key, value))

    # Environment variables from a file are merged in by the tool.
    # Values from `env` (or expanded templates) take precedence over file entries.
    if ctx.attr.env_file != None:
        inputs.append(ctx.file.env_file)
        args.add("--env-file", ctx.file.env_file.path)

    # Build-wide extra labels from the //img/settings:additional_image_labels_file
    # label_flag. These are merged into every config.json by the tool and are NOT
    # template-expanded; per-target `labels`/`label_files` take precedence. The flag
    # defaults to an empty file, so this contributes nothing unless overridden.
    inputs.append(ctx.file._additional_image_labels_file)
    args.add("--additional-image-labels-file", ctx.file._additional_image_labels_file.path)

    # Image config value overrides (user, working_dir, stop_signal, entrypoint,
    # cmd). The scalar flags are always passed -- even when empty -- so the tool
    # can distinguish the INHERIT_FROM_BASE sentinel (inherit) from an explicit
    # empty value (unset). For the list flags, the sentinel default emits a single
    # --entrypoint/--cmd carrying the sentinel (inherit), while an explicit empty
    # list emits no flags at all (unset); the tool applies the inherit/unset/expand
    # semantics against the base image config (see cmd/manifest).
    args.add("--user", ctx.attr.user)
    for entry in effective_entrypoint:
        args.add("--entrypoint", entry)
    for entry in effective_cmd:
        args.add("--cmd", entry)
    args.add("--working-dir", effective_working_dir)
    args.add("--stop-signal", ctx.attr.stop_signal)

    structured_config = dict(
        architecture = arch,
        os = os,
        history = history,
    )

    manifest_out = ctx.actions.declare_file(ctx.label.name + "_manifest.json")
    config_out = ctx.actions.declare_file(ctx.label.name + "_config.json")
    descriptor_out = ctx.actions.declare_file(ctx.label.name + "_descriptor.json")
    digest_out = ctx.actions.declare_file(ctx.label.name + "_digest")
    args.add("--manifest", manifest_out.path)
    args.add("--config", config_out.path)
    args.add("--descriptor", descriptor_out.path)
    args.add("--digest", digest_out.path)

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = inputs,
        outputs = [manifest_out, config_out, descriptor_out, digest_out],
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        mnemonic = "ImageManifest",
    )

    sparse_layout = _build_sparse_oci_layout(ctx, "directory", manifest_out, config_out, layers)

    # Merge the per-layer mtree specs (in layer order) into a single image-level
    # mtree, exposed both as the `mtree` field on ImageManifestInfo and as the
    # `mtree` output group. Layers built by rules_img layer rules carry their own
    # mtree; for other layers (pulled/imported base-image layers, raw tars added
    # via DefaultInfo) an mtree is rendered on the fly from the layer's tar blob.
    # This is best-effort: a layer with no blob (shallow/lazy) or a non-tar blob is
    # skipped, so the merged mtree is None only when no layer contributes one.
    layer_mtrees = image_layer_mtrees(ctx, layers)
    image_mtree = build_image_mtree(ctx, ctx.label.name, layer_mtrees) if len(layer_mtrees) > 0 else None

    manifest_info_provider = ImageManifestInfo(
        descriptor = descriptor_out,
        manifest = manifest_out,
        config = config_out,
        structured_config = structured_config,
        architecture = arch,
        os = os,
        variant = variant,
        layers = layers,
        mtree = image_mtree,
        sparse_oci_layout = sparse_layout,
    )
    providers.extend([
        DefaultInfo(
            files = depset([manifest_out, config_out]),
        ),
        manifest_info_provider,
    ])

    push_at_build_time = ctx.attr._push_at_build_time_settings[PushAtBuildTimeSettingsInfo]
    deploy_info, validation_outputs = process_deploy_specs(
        ctx,
        manifest_info = manifest_info_provider,
        index_info = None,
        manifest_infos = [],
        pull_info = pull_info,
        push_specs = ctx.attr.push_specs,
        load_specs = ctx.attr.load_specs,
        allow_manifest_tags = False,
        push_at_build_time_mode = push_at_build_time.mode,
        push_at_build_time_content = push_at_build_time.content,
        push_at_build_time_gateway = push_at_build_time.gateway,
        push_at_build_time_push_gateway = push_at_build_time.push_gateway,
        push_at_build_time_pull_gateway = push_at_build_time.pull_gateway,
        sparse_layout = sparse_layout,
    )

    output_groups = dict(
        descriptor = depset([descriptor_out]),
        digest = depset([digest_out]),
        root_blob = depset([manifest_out]),
        oci_layout = depset([_build_oci_layout(ctx, "directory", manifest_out, config_out, layers)]),
        oci_tarball = depset([_build_oci_layout(ctx, "tar", manifest_out, config_out, layers)]),
        sparse_oci_layout = depset([sparse_layout]),
    )

    # Expose the image-level mtree (computed above, alongside the provider field)
    # as the `mtree` output group, when at least one layer contributes one.
    if image_mtree != None:
        output_groups["mtree"] = depset([image_mtree])

    if deploy_info != None:
        providers.append(deploy_info)
        output_groups["deploy_manifest"] = depset([deploy_info.deploy_manifest])
    if validation_outputs:
        output_groups["_validation"] = depset(validation_outputs)
    providers.append(OutputGroupInfo(**output_groups))

    return providers

image_manifest = rule(
    implementation = _image_manifest_impl,
    doc = """Builds a single-platform OCI container image from a set of layers.

This rule assembles container images by combining:
- Optional base image layers (from another image_manifest or image_index)
- Additional layers created by image_layer rules
- Image configuration (entrypoint, environment, labels, etc.)

The rule produces:
- OCI manifest and config JSON files
- An optional OCI layout directory or tar (via output groups)
- ImageManifestInfo provider for use by image_index or image_push

Example:

```python
image_manifest(
    name = "my_app",
    base = "@distroless_cc",
    layers = [
        ":app_layer",
        ":config_layer",
    ],
    entrypoint = ["/usr/bin/app"],
    env = {
        "APP_ENV": "production",
    },
)
```

Output groups:
- `descriptor`: OCI descriptor JSON file
- `digest`: Digest of the image (sha256:...)
- `root_blob`: The manifest JSON blob file
- `oci_layout`: Complete OCI layout directory with blobs
- `oci_tarball`: OCI layout packaged as a tar file for downstream use
- `sparse_oci_layout`: Sparse OCI layout directory (without layer blobs, only layer descriptors)
- `mtree`: a single [mtree](https://man.freebsd.org/cgi/man.cgi?mtree(5)) text file describing the
  image's filesystem, merged (in layer order) from per-layer mtrees. Layers built by rules_img layer
  rules reuse their own `mtree`; for any other layer -- pulled/imported base-image layers, or raw
  tars added directly via `DefaultInfo` -- an mtree is rendered on the fly from the layer's tar blob.
  A layer is skipped on a best-effort basis only when its blob is unavailable (shallow/lazy layers)
  or is not a tar (empty layers, non-tar artifact blobs), so a skipped layer means the merged mtree
  reflects only a subset of the image. Only produced when at least one layer contributes an `mtree`.
""",
    attrs = {
        "base": attr.label(
            doc = "Base image to inherit layers from. Should provide ImageManifestInfo or ImageIndexInfo.",
        ),
        "layers": attr.label_list(
            doc = "Layers to include in the image. Either a LayersInfo provider or a DefaultInfo with tar files.",
            cfg = normalize_layer_transition,
        ),
        "platform": attr.label(
            doc = """Optional target platform to build this manifest for.

When specified, the image will be built for the provided platform regardless
of the current build configuration. This enables building single-platform images
for specific architectures.

Example:
```python
image_manifest(
    name = "app_arm64",
    platform = "//platforms:linux_arm64",
    base = "@ubuntu",
    layers = [":app_layer"],
)
```
""",
            providers = [platform_common.PlatformInfo],
        ),
        "user": attr.string(
            doc = """The username or UID which is a platform-specific structure that allows specific control over which user the process run as.
This acts as a default value to use when the value is not specified when creating a container.

Defaults to `INHERIT_FROM_BASE`: the value is inherited from the base image. Set it to
an explicit value to override, or to `""` to unset it (do not inherit from the base).""",
            default = INHERIT_FROM_BASE,
        ),
        "env": attr.string_dict(
            doc = """Default environment variables to set when starting a container based on this image.

Subject to [template expansion](/docs/templating.md).
""",
            default = {},
        ),
        "env_file": attr.label(
            allow_single_file = True,
            doc = """File containing environment variables to set when starting a container based on this image.

The file may be JSON or newline-delimited text, auto-detected from its contents:

- JSON object with string values: `{"KEY": "value"}`
- JSON object with list values: `{"KEY": ["value1", "value2"]}` (the last value wins)
- JSON array of `KEY=VALUE` strings: `["KEY=value"]`
- newline-delimited `KEY=VALUE` text (one per line; blank lines and `#` comments are ignored)

Values in JSON objects are used verbatim and may contain `=`, spaces, or newlines.
The `KEY=VALUE` forms split on the first `=` and trim surrounding whitespace.

Values from the `env` attribute (or expanded templates) take precedence over the file.""",
        ),
        "entrypoint": attr.string_list(
            doc = """A list of arguments to use as the command to execute when the container starts. These values act as defaults and may be replaced by an entrypoint specified when creating a container.

Defaults to `[INHERIT_FROM_BASE]`: the entrypoint is inherited from the base image (or,
for `image_from_binary`, from the packaged binary). Set it to an explicit list to override,
or to `[]` to unset it. An `INHERIT_FROM_BASE` item inside the list is replaced in place by
the base image's entrypoint, so `[INHERIT_FROM_BASE, "--flag"]` appends `"--flag"` to it.""",
            default = [INHERIT_FROM_BASE],
        ),
        "cmd": attr.string_list(
            doc = """Default arguments to the entrypoint of the container. These values act as defaults and may be replaced by any specified when creating a container. If an Entrypoint value is not specified, then the first entry of the Cmd array SHOULD be interpreted as the executable to run.

Defaults to `[INHERIT_FROM_BASE]`: the value is inherited from the base image (or, for
`image_from_binary`, from the packaged binary's `args`). Set it to an explicit list to
override, or to `[]` to unset it. An `INHERIT_FROM_BASE` item inside the list is replaced in
place by the base image's cmd, so `[INHERIT_FROM_BASE, "--flag"]` appends `"--flag"` to it.""",
            default = [INHERIT_FROM_BASE],
        ),
        "working_dir": attr.string(
            doc = """Sets the current working directory of the entrypoint process in the container. This value acts as a default and may be replaced by a working directory specified when creating a container.

Defaults to `INHERIT_FROM_BASE`: the value is inherited from the base image (or, for
`image_from_binary`, from the packaged binary). Set it to an explicit value to override, or
to `""` to unset it (do not inherit from the base).""",
            default = INHERIT_FROM_BASE,
        ),
        "labels": attr.string_dict(
            doc = """This field contains arbitrary metadata for the container.

Subject to [template expansion](/docs/templating.md).
""",
            default = {},
        ),
        "label_files": attr.label_list(
            doc = """Files containing labels for the image config, as JSON or newline-delimited text.

Each file is parsed in one of the following formats, auto-detected from its contents:

- JSON object with string values: `{"key": "value"}`
- JSON object with list values: `{"key": ["value1", "value2"]}` (the last value wins)
- JSON array of `KEY=VALUE` strings: `["key=value"]`
- newline-delimited `KEY=VALUE` text (one per line; blank lines and `#` comments are ignored)

Values in JSON objects are used verbatim, so they can encode arbitrary strings including
values that contain `=`, spaces, or newlines. The `KEY=VALUE` forms (JSON array and text)
split on the first `=` and trim surrounding whitespace from the key and value.

Labels from these files are merged together, and then merged with labels specified via
the `labels` attribute. Values from files take precedence over the `labels` attribute
for matching keys.

Example file content:
```
org.opencontainers.image.version=1.0.0
org.opencontainers.image.authors=team@example.com
```

Each label value is subject to [template expansion](/docs/templating.md).
""",
            allow_files = True,
            default = [],
        ),
        "annotations": attr.string_dict(
            doc = """This field contains arbitrary metadata for the manifest.

Subject to [template expansion](/docs/templating.md).
""",
            default = {},
        ),
        "annotations_file": attr.label(
            doc = """File containing annotations for the manifest, as JSON or newline-delimited text.

The file is parsed in one of the following formats, auto-detected from its contents:

- JSON object with string values: `{"key": "value"}`
- JSON object with list values: `{"key": ["value1", "value2"]}` (the last value wins)
- JSON array of `KEY=VALUE` strings: `["key=value"]`
- newline-delimited `KEY=VALUE` text (one per line; blank lines and `#` comments are ignored)

Values in JSON objects are used verbatim, so they can encode arbitrary strings including
values that contain `=`, spaces, or newlines. The `KEY=VALUE` forms (JSON array and text)
split on the first `=` and trim surrounding whitespace from the key and value.

Annotations from this file are merged with annotations specified via the `annotations`
attribute. Values from the file take precedence over the `annotations` attribute for matching keys.

Example file content:
```
version=1.0.0
build.date=2024-01-15
source.url=https://github.com/...
```

Each annotation is subject to [template expansion](/docs/templating.md).
""",
            allow_single_file = True,
        ),
        "stop_signal": attr.string(
            doc = """This field contains the system call signal that will be sent to the container to exit. The signal can be a signal name in the format SIGNAME, for instance SIGKILL or SIGRTMIN+3.

Defaults to `INHERIT_FROM_BASE`: the value is inherited from the base image. Set it to an
explicit value to override, or to `""` to unset it (do not inherit from the base).""",
            default = INHERIT_FROM_BASE,
        ),
        "config_fragment": attr.label(
            doc = """Optional JSON file containing a partial OCI image config, which will be used as a base for the final image config.

For OCI image configuration fields such as exposed ports or volumes, the JSON should use the top-level `config` object:

```json
{
  "config": {
    "ExposedPorts": {
      "8080/tcp": {}
    }
  }
}
```

When config_media_type is set to a non-OCI type (e.g. Helm), this file is used as the entire config blob as-is.""",
            allow_single_file = True,
        ),
        "config_media_type": attr.string(
            doc = """Override the config blob media type.

When set to \"application/vnd.oci.empty.v1+json\", config_fragment is optional. If omitted, an empty
JSON config descriptor is produced automatically with the content inlined as data (`"data": "e30="`).

For other non-OCI types (e.g. \"application/vnd.cncf.helm.config.v1+json\" for Helm charts),
config_fragment is required and used verbatim as the config blob (no OCI image structure).""",
        ),
        "artifact_type": attr.string(
            doc = """Optional IANA media type of the artifact when the manifest is used for an artifact.

This sets the `artifactType` field in the OCI manifest, as defined in the
[OCI Image Spec](https://github.com/opencontainers/image-spec/blob/main/manifest.md#image-manifest-property-descriptions).

Common values include `application/vnd.cncf.helm.chart.v1` for Helm charts
or `application/spdx+json` for SPDX SBOMs.""",
        ),
        "subject": attr.label(
            doc = """Optional subject for the manifest.

Sets the `subject` field in the OCI manifest, which is a descriptor pointing to
another manifest or index. This is used for establishing referrer relationships,
such as attaching SBOMs, signatures, or attestations to an existing image.

The target must provide either ImageManifestInfo or ImageIndexInfo.
""",
            providers = [[ImageManifestInfo], [ImageIndexInfo]],
        ),
        "created": attr.label(
            doc = """Optional file containing a datetime string (RFC 3339 format) for when the image was created.

This is commonly used with Bazel's stamping mechanism to embed the build timestamp.
""",
            allow_single_file = True,
        ),
        "build_settings": attr.string_keyed_label_dict(
            doc = """Build settings for template expansion.

Maps template variable names to string_flag targets. These values can be used in
env, labels, and annotations attributes using `{{.VARIABLE_NAME}}` syntax (Go template).

Example:
```python
build_settings = {
    "REGISTRY": "//settings:docker_registry",
    "VERSION": "//settings:app_version",
}
```

See [template expansion](/docs/templating.md) for more details.
""",
            providers = [BuildSettingInfo],
        ),
        "stamp": attr.string(
            doc = """Controls build stamping for template expansion.

- **`auto`** (default): Defers to the global `--@rules_img//img/settings:stamp` setting.
- **`force`**: Always stamp if templates contain `{{}}` placeholders, ignoring Bazel's `--stamp` flag.
- **`disabled`**: Never include stamp information.

See [template expansion](/docs/templating.md) for available stamp variables.
""",
            default = "auto",
            values = ["auto", "force", "disabled"],
        ),
        "_os_cpu": attr.label(
            default = Label("//img/private/config:target_os_cpu"),
            providers = [TargetPlatformInfo],
        ),
        "_oci_layout_settings": attr.label(
            default = Label("//img/private/settings:oci_layout"),
            providers = [OCILayoutSettingsInfo],
        ),
        "_push_at_build_time_settings": attr.label(
            default = Label("//img/private/settings:push_at_build_time"),
            providers = [PushAtBuildTimeSettingsInfo],
        ),
        "_stamp_settings": attr.label(
            default = Label("//img/private/settings:stamp"),
            providers = [StampSettingInfo],
        ),
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
        "_additional_image_labels_file": attr.label(
            default = Label("//img/settings:additional_image_labels_file"),
            allow_single_file = True,
        ),
        "push_specs": attr.label_list(
            doc = """Push configurations to produce DeployInfo for this image.

Each entry should be an `image_push_spec` target (providing `PushConfigInfo`).
When set (together with or without `load_specs`), this rule additionally returns
`DeployInfo`, making it directly usable as an operation in `multi_deploy`.

Example:
```python
image_push_spec(
    name = "push_config",
    registry = "gcr.io",
    repository = "my-project/my-app",
    tag = "latest",
)

image_manifest(
    name = "my_app",
    base = "@distroless_cc",
    layers = [":app_layer"],
    push_specs = [":push_config"],
)

multi_deploy(
    name = "deploy",
    operations = [":my_app"],
)
```
""",
            providers = [PushConfigInfo],
        ),
        "load_specs": attr.label_list(
            doc = """Load configurations to produce DeployInfo for this image.

Each entry should be an `image_load_spec` target (providing `LoadConfigInfo`).
When set (together with or without `push_specs`), this rule additionally returns
`DeployInfo`, making it directly usable as an operation in `multi_deploy`.

Example:
```python
image_load_spec(
    name = "load_config",
    tag = "my-app:latest",
)

image_manifest(
    name = "my_app",
    base = "@distroless_cc",
    layers = [":app_layer"],
    load_specs = [":load_config"],
)

multi_deploy(
    name = "deploy",
    operations = [":my_app"],
)
```
""",
            providers = [LoadConfigInfo],
        ),
    },
    provides = [ImageManifestInfo],
    toolchains = TOOLCHAINS,
    cfg = single_platform_transition,
)
