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

def _get_manifests(ctx):
    if len(ctx.attr.platforms) == 0:
        return ctx.attr.manifests
    manifests = []
    for i in range(len(ctx.attr.platforms)):
        manifests.extend(ctx.split_attr.manifests[str(i)])
    return manifests

def _image_index_impl(ctx):
    manifests = _get_manifests(ctx)
    manifest_infos = [manifest[ImageManifestInfo] for manifest in manifests]

    # Find the first PullInfo where the ManifestInfo has non-empty missing_blobs
    pull_info = None
    known_missing_blobs = []
    for manifest in manifests:
        if not PullInfo in manifest:
            continue
        manifest_info = manifest[ImageManifestInfo]
        if len(manifest_info.missing_blobs) > 0:
            pull_info = manifest[PullInfo]
            known_missing_blobs.extend(manifest_info.missing_blobs)
            break

    # Check for conflicting PullInfos
    for manifest in manifests:
        if not PullInfo in manifest:
            continue
        other = manifest[PullInfo]
        other_manifest_info = manifest[ImageManifestInfo]
        if pull_info != None and other != pull_info:
            # Only fail if other has missing blobs not covered by known_missing_blobs
            unknown_blobs = ["sha256:" + b for b in other_manifest_info.missing_blobs if b not in known_missing_blobs]
            if len(unknown_blobs) > 0:
                fail("index rule called with images based on different external images: {} and {}.\nMissing blobs from {} not covered by first image:\n    {}\nHint: you can work around this by pulling one or both of the base images via the \"eager\" layer handling method.".format(
                    pull_info.repository,
                    other.repository,
                    other.repository,
                    ", ".join(unknown_blobs),
                ))

    # Prepare template data for annotations
    templates = {}
    if ctx.attr.annotations:
        templates["annotations"] = ctx.attr.annotations

    # Prepare newline_delimited_lists_files if annotations_file is provided
    newline_delimited_lists_files = None
    if ctx.attr.annotations_file != None:
        annotations_file = ctx.file.annotations_file
        newline_delimited_lists_files = {"annotations": annotations_file}

    # Expand templates if needed (either from templates dict or from file)
    config_json = None
    if templates or newline_delimited_lists_files:
        config_json = expand_or_write(
            ctx = ctx,
            templates = templates,
            output_name = ctx.label.name + "_config_templates.json",
            only_if_stamping = True,
            newline_delimited_lists_files = newline_delimited_lists_files,
        )

    index_out = ctx.actions.declare_file(ctx.attr.name + "_index.json")
    digest_out = ctx.actions.declare_file(ctx.label.name + "_digest")
    write_index_json(
        ctx,
        output = index_out,
        digest = digest_out,
        manifests = manifest_infos,
        config_json = config_json,
    )
    providers = [
        DefaultInfo(files = depset([index_out])),
        OutputGroupInfo(
            digest = depset([digest_out]),
            oci_layout = depset([_build_oci_layout(ctx, "directory", index_out, manifest_infos)]),
            oci_tarball = depset([_build_oci_layout(ctx, "tar", index_out, manifest_infos)]),
        ),
        ImageIndexInfo(
            index = index_out,
            manifests = manifest_infos,
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
        "annotations_file": attr.label(
            doc = """File containing newline-delimited KEY=VALUE annotations for the image index.

The file should contain one annotation per line in KEY=VALUE format. Empty lines are ignored.
Annotations from this file are merged with annotations specified via the `annotations` attribute.

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
