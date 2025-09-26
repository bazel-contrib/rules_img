"""Image index rule for composing multi-layer OCI images."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private:stamp.bzl", "expand_or_write")
load("//img/private/common:build.bzl", "TOOLCHAIN", "TOOLCHAINS")
load("//img/private/common:transitions.bzl", "multi_platform_image_transition", "reset_platform_transition")
load("//img/private/common:write_index_json.bzl", "write_index_json")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")
load("//img/private/providers:oci_layout_settings_info.bzl", "OCILayoutSettingsInfo")
load("//img/private/providers:pull_info.bzl", "PullInfo")
load("//img/private/providers:stamp_setting_info.bzl", "StampSettingInfo")

def _build_oci_layout(ctx, format, index_out, manifests):
    """Build the OCI layout for a multi-platform image.

    Args:
        ctx: Rule context.
        format: The output format, either "directory" or "tar".
        index_out: The index file.
        manifests: List of ImageManifestInfo providers.

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
    args.add("--index", index_out.path)
    args.add("--output", oci_layout_output.path)
    if ctx.attr._oci_layout_settings[OCILayoutSettingsInfo].allow_shallow_oci_layout:
        args.add("--allow-missing-blobs")

    inputs = [index_out]

    # Add manifest and config files for each platform
    for manifest in manifests:
        args.add("--manifest-path", manifest.manifest.path)
        args.add("--config-path", manifest.config.path)
        inputs.append(manifest.manifest)
        inputs.append(manifest.config)

        # Add layers with metadata=blob mapping
        for layer in manifest.layers:
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
        mnemonic = "OCIIndexLayout",
    )

    return oci_layout_output

def _image_index_impl(ctx):
    pull_infos = [manifest[PullInfo] for manifest in ctx.attr.manifests if PullInfo in manifest]
    pull_info = pull_infos[0] if len(pull_infos) > 0 else None
    for other in pull_infos:
        if pull_info != other:
            fail("index rule called with images that are based on different external images. This is not yet supported.")

    # Prepare template data for annotations
    templates = {}
    if ctx.attr.annotations:
        templates["annotations"] = ctx.attr.annotations

    # Expand templates if needed
    config_json = None
    if templates:
        config_json = expand_or_write(
            ctx = ctx,
            templates = templates,
            output_name = ctx.label.name + "_config_templates.json",
            only_if_stamping = True,
        )

    index_out = ctx.actions.declare_file(ctx.attr.name + "_index.json")
    digest_out = ctx.actions.declare_file(ctx.label.name + "_digest")
    manifests = [manifest[ImageManifestInfo] for manifest in ctx.attr.manifests]
    write_index_json(
        ctx,
        output = index_out,
        digest = digest_out,
        manifests = manifests,
        config_json = config_json,
    )
    providers = [
        DefaultInfo(files = depset([index_out])),
        OutputGroupInfo(
            digest = depset([digest_out]),
            oci_layout = depset([_build_oci_layout(ctx, "directory", index_out, manifests)]),
            oci_tarball = depset([_build_oci_layout(ctx, "tar", index_out, manifests)]),
        ),
        ImageIndexInfo(
            index = index_out,
            manifests = manifests,
        ),
    ]
    if pull_info != None:
        providers.append(pull_info)
    return providers

image_index = rule(
    implementation = _image_index_impl,
    doc = """Creates a multi-platform OCI image index from platform-specific manifests.

This rule combines multiple single-platform images (created by image_manifest) into
a multi-platform image index. The index allows container runtimes to automatically
select the appropriate image for their platform.

The rule supports two usage patterns:
1. Explicit manifests: Provide pre-built manifests for each platform
2. Platform transitions: Provide one manifest target and a list of platforms

The rule produces:
- OCI image index JSON file
- An optional OCI layout directory or tar (via output groups)
- ImageIndexInfo provider for use by image_push

Example (explicit manifests):

```python
image_index(
    name = "multiarch_app",
    manifests = [
        ":app_linux_amd64",
        ":app_linux_arm64",
        ":app_darwin_amd64",
    ],
)
```

Example (platform transitions):
```python
image_index(
    name = "multiarch_app",
    manifests = [":app"],
    platforms = [
        "//platform:linux-x86_64",
        "//platform:linux-aarch64",
    ],
)
```

Output groups:
- `digest`: Digest of the image (sha256:...)
- `oci_layout`: Complete OCI layout directory with all platform blobs
- `oci_tarball`: OCI layout packaged as a tar file for downstream use
""",
    attrs = {
        "manifests": attr.label_list(
            providers = [ImageManifestInfo],
            doc = "List of manifests for specific platforms.",
            cfg = multi_platform_image_transition,
        ),
        "platforms": attr.label_list(
            providers = [platform_common.PlatformInfo],
            doc = "(Optional) list of target platforms to build the manifest for. Uses a split transition. If specified, the 'manifests' attribute should contain exactly one manifest.",
        ),
        "annotations": attr.string_dict(
            doc = """Arbitrary metadata for the image index.

Subject to [template expansion](/docs/templating.md).""",
        ),
        "build_settings": attr.string_keyed_label_dict(
            providers = [BuildSettingInfo],
            doc = """Build settings for template expansion.

Maps template variable names to string_flag targets. These values can be used in
the annotations attribute using `{{.VARIABLE_NAME}}` syntax (Go template).

Example:
```python
build_settings = {
    "REGISTRY": "//settings:docker_registry",
    "VERSION": "//settings:app_version",
}
```

See [template expansion](/docs/templating.md) for more details.
""",
        ),
        "stamp": attr.string(
            default = "auto",
            values = ["auto", "enabled", "disabled"],
            doc = """Enable build stamping for template expansion.

Controls whether to include volatile build information:
- **`auto`** (default): Uses the global stamping configuration
- **`enabled`**: Always include stamp information (BUILD_TIMESTAMP, BUILD_USER, etc.) if Bazel's "--stamp" flag is set
- **`disabled`**: Never include stamp information

See [template expansion](/docs/templating.md) for available stamp variables.
""",
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
    toolchains = TOOLCHAINS,
    cfg = reset_platform_transition,
)
