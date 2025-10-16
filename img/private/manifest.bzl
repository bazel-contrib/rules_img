"""Image rule for assembling OCI images based on a set of layers."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private:stamp.bzl", "expand_or_write")
load("//img/private/common:build.bzl", "TOOLCHAIN", "TOOLCHAINS")
load("//img/private/common:layer_helper.bzl", "allow_tar_files", "calculate_layer_info", "extension_to_compression")
load("//img/private/common:transitions.bzl", "normalize_layer_transition")
load("//img/private/config:defs.bzl", "TargetPlatformInfo")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
load("//img/private/providers:layer_info.bzl", "LayerInfo")
load("//img/private/providers:layer_group_info.bzl", "LayerGroupInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")
load("//img/private/providers:oci_layout_settings_info.bzl", "OCILayoutSettingsInfo")
load("//img/private/providers:pull_info.bzl", "PullInfo")
load("//img/private/providers:stamp_setting_info.bzl", "StampSettingInfo")

def _to_layer_arg(layer):
    """Convert a layer to a command line argument."""
    return layer.metadata.path

def _platform_matches(wanted_platform, manifest):
    """Check if the wanted platform matches the manifest platform."""
    if wanted_platform["os"] != manifest.os:
        return False
    if wanted_platform["architecture"] != manifest.architecture:
        return False
    for key in wanted_platform["platform"].keys():
        if key not in manifest:
            return False
        if wanted_platform[key] != manifest[key]:
            return False
    return True

def select_base(ctx):
    """Select the base image to use for this image.

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
    constraints_wanted = dict(
        os = os_wanted,
        architecture = arch_wanted,
        platform = ctx.attr.platform,
    )

    for manifest in ctx.attr.base[ImageIndexInfo].manifests:
        if _platform_matches(constraints_wanted, manifest):
            return manifest
    fail("no matching base image found for architecture {} and os {}".format(constraints_wanted["architecture"], constraints_wanted["os"]))

def _build_oci_layout(ctx, format, manifest_out, config_out, layers):
    """Build the OCI layout for the image.

    Args:
        ctx: Rule context.
        format: The output format, either "directory" or "tar".
        manifest_out: The manifest file.
        config_out: The config file.
        layers: List of LayerInfo providers.

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

def _image_manifest_impl(ctx):
    inputs = []
    providers = []
    args = ctx.actions.args()
    args.add("manifest")
    base = select_base(ctx)
    os = ctx.attr._os_cpu[TargetPlatformInfo].os
    arch = ctx.attr._os_cpu[TargetPlatformInfo].cpu
    history = []
    layers = []
    if base != None:
        history = base.structured_config.get("history", [])
        layers.extend(base.layers)
        inputs.append(base.manifest)
        inputs.append(base.config)
        args.add("--base-manifest", base.manifest.path)
        args.add("--base-config", base.config.path)
    if ctx.attr.base != None and PullInfo in ctx.attr.base:
        providers.append(ctx.attr.base[PullInfo])
    for (layer_idx, layer) in enumerate(ctx.attr.layers):
        if LayerInfo in layer:
            # Use pre-built layer metadata
            layers.append(layer[LayerInfo])
            continue
        elif LayerGroupInfo in layer:
            # Use pre-built layer group
            group_info = layer[LayerGroupInfo]
            layers.extend(group_info.layers)
            continue
        elif DefaultInfo not in layer:
            fail("layer {} needs to provide LayerInfo, LayerGroupInfo, or DefaultInfo: {}".format(layer_idx, layer))

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

    args.add("--os", os)
    args.add("--architecture", arch)

    # todo: encode platform metadata
    for layer in layers:
        inputs.append(layer.metadata)
    args.add_all(layers, format_each = "--layer-from-metadata=%s", map_each = _to_layer_arg, expand_directories = False)
    if ctx.attr.config_fragment != None:
        inputs.append(ctx.file.config_fragment)
        args.add("--config-fragment", ctx.file.config_fragment.path)

    # Handle template expansion for labels, env, and annotations
    templates = {
        "env": ctx.attr.env,
        "labels": ctx.attr.labels,
        "annotations": ctx.attr.annotations,
    }

    # Try to expand templates - this will return None if no templates need expansion
    config_json = expand_or_write(
        ctx = ctx,
        templates = templates,
        output_name = ctx.label.name + "_config_templates.json",
        only_if_stamping = True,
    )

    if config_json != None:
        # Templates were expanded, use the config-templates flag
        inputs.append(config_json)
        args.add("--config-templates", config_json.path)
    else:
        # No templates to expand, use direct values
        for key, value in ctx.attr.env.items():
            args.add("--env", "%s=%s" % (key, value))
        for key, value in ctx.attr.labels.items():
            args.add("--label", "%s=%s" % (key, value))
        for key, value in ctx.attr.annotations.items():
            args.add("--annotation", "%s=%s" % (key, value))

    # Add other image config attributes
    if ctx.attr.user:
        args.add("--user", ctx.attr.user)
    for entry in ctx.attr.entrypoint:
        args.add("--entrypoint", entry)
    for entry in ctx.attr.cmd:
        args.add("--cmd", entry)
    if ctx.attr.working_dir:
        args.add("--working-dir", ctx.attr.working_dir)
    if ctx.attr.stop_signal:
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

    providers.extend([
        DefaultInfo(
            files = depset([manifest_out, config_out]),
        ),
        OutputGroupInfo(
            descriptor = depset([descriptor_out]),
            digest = depset([digest_out]),
            oci_layout = depset([_build_oci_layout(ctx, "directory", manifest_out, config_out, layers)]),
            oci_tarball = depset([_build_oci_layout(ctx, "tar", manifest_out, config_out, layers)]),
        ),
        ImageManifestInfo(
            base_image = base,
            descriptor = descriptor_out,
            manifest = manifest_out,
            config = config_out,
            structured_config = structured_config,
            architecture = arch,
            os = os,
            platform = ctx.attr.platform,
            layers = layers,
            missing_blobs = base.missing_blobs if base != None else [],
        ),
    ])
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
- `oci_layout`: Complete OCI layout directory with blobs
- `oci_tarball`: OCI layout packaged as a tar file for downstream use
""",
    attrs = {
        "base": attr.label(
            doc = "Base image to inherit layers from. Should provide ImageManifestInfo or ImageIndexInfo.",
        ),
        "layers": attr.label_list(
            doc = "Layers to include in the image. Either a LayerInfo provider, a LayerGroupInfo provider, or a DefaultInfo with tar files.",
            cfg = normalize_layer_transition,
        ),
        "platform": attr.string_dict(
            default = {},
            doc = "Dict containing additional runtime requirements of the image.",
        ),
        "user": attr.string(
            doc = """The username or UID which is a platform-specific structure that allows specific control over which user the process run as.
This acts as a default value to use when the value is not specified when creating a container.""",
        ),
        "env": attr.string_dict(
            doc = """Default environment variables to set when starting a container based on this image.

Subject to [template expansion](/docs/templating.md).
""",
            default = {},
        ),
        "entrypoint": attr.string_list(
            doc = "A list of arguments to use as the command to execute when the container starts. These values act as defaults and may be replaced by an entrypoint specified when creating a container.",
            default = [],
        ),
        "cmd": attr.string_list(
            doc = "Default arguments to the entrypoint of the container. These values act as defaults and may be replaced by any specified when creating a container. If an Entrypoint value is not specified, then the first entry of the Cmd array SHOULD be interpreted as the executable to run.",
            default = [],
        ),
        "working_dir": attr.string(
            doc = "Sets the current working directory of the entrypoint process in the container. This value acts as a default and may be replaced by a working directory specified when creating a container.",
        ),
        "labels": attr.string_dict(
            doc = """This field contains arbitrary metadata for the container.

Subject to [template expansion](/docs/templating.md).
""",
            default = {},
        ),
        "annotations": attr.string_dict(
            doc = """This field contains arbitrary metadata for the manifest.

Subject to [template expansion](/docs/templating.md).
""",
            default = {},
        ),
        "stop_signal": attr.string(
            doc = "This field contains the system call signal that will be sent to the container to exit. The signal can be a signal name in the format SIGNAME, for instance SIGKILL or SIGRTMIN+3.",
        ),
        "config_fragment": attr.label(
            doc = "Optional JSON file containing a partial image config, which will be used as a base for the final image config.",
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
            doc = """Enable build stamping for template expansion.

Controls whether to include volatile build information:
- **`auto`** (default): Uses the global stamping configuration
- **`enabled`**: Always include stamp information (BUILD_TIMESTAMP, BUILD_USER, etc.) if Bazel's "--stamp" flag is set
- **`disabled`**: Never include stamp information

See [template expansion](/docs/templating.md) for available stamp variables.
""",
            default = "auto",
            values = ["auto", "enabled", "disabled"],
        ),
        "_os_cpu": attr.label(
            default = Label("//img/private/config:target_os_cpu"),
            providers = [TargetPlatformInfo],
        ),
        "_oci_layout_settings": attr.label(
            default = Label("//img/private/settings:oci_layout"),
            providers = [OCILayoutSettingsInfo],
        ),
        "_stamp_settings": attr.label(
            default = Label("//img/private/settings:stamp"),
            providers = [StampSettingInfo],
        ),
    },
    provides = [ImageManifestInfo],
    toolchains = TOOLCHAINS,
)
