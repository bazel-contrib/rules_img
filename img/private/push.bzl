"""Push rule for uploading images to a registry."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private:root_symlinks.bzl", "calculate_root_symlinks")
load("//img/private:stamp.bzl", "expand_or_write")
load("//img/private/common:build.bzl", "TOOLCHAIN", "TOOLCHAINS")
load("//img/private/common:transitions.bzl", "host_platform_transition", "reset_platform_transition")
load("//img/private/providers:image_toolchain_info.bzl", "ImageToolchainInfo")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")
load("//img/private/providers:pull_info.bzl", "PullInfo")
load("//img/private/providers:push_settings_info.bzl", "PushSettingsInfo")
load("//img/private/providers:stamp_setting_info.bzl", "StampSettingInfo")

def _push_strategy(ctx):
    """Determine the push strategy to use based on the settings."""
    push_settings = ctx.attr._push_settings[PushSettingsInfo]
    strategy = ctx.attr.strategy
    if strategy == "auto":
        strategy = push_settings.strategy
    return strategy

def _target_info(ctx):
    pull_info = ctx.attr.image[PullInfo] if PullInfo in ctx.attr.image else None
    if pull_info == None:
        return {}
    return dict(
        original_registries = pull_info.registries,
        original_repository = pull_info.repository,
        original_tag = pull_info.tag,
        original_digest = pull_info.digest,
    )

def _get_tags(ctx):
    """Get the list of tags from the context, validating mutual exclusivity."""
    if ctx.attr.tag and ctx.attr.tag_list:
        fail("Cannot specify both 'tag' and 'tag_list' attributes")

    tags = []
    if ctx.attr.tag:
        tags = [ctx.attr.tag]
    elif ctx.attr.tag_list:
        tags = ctx.attr.tag_list

    # Empty list is allowed for digest-only push
    return tags

def _compute_push_metadata(*, ctx, configuration_json):
    inputs = [configuration_json]
    args = ctx.actions.args()
    args.add("deploy-metadata")
    args.add("--command", "push")
    manifest_info = ctx.attr.image[ImageManifestInfo] if ImageManifestInfo in ctx.attr.image else None
    index_info = ctx.attr.image[ImageIndexInfo] if ImageIndexInfo in ctx.attr.image else None
    if manifest_info == None and index_info == None:
        fail("image must provide ImageManifestInfo or ImageIndexInfo")
    if manifest_info != None and index_info != None:
        fail("image must provide either ImageManifestInfo or ImageIndexInfo, not both")
    args.add("--strategy", _push_strategy(ctx))
    args.add("--configuration-file", configuration_json.path)
    target_info = _target_info(ctx)
    if "original_registries" in target_info:
        args.add_all("--original-registry", target_info["original_registries"])
    if "original_repository" in target_info:
        args.add("--original-repository", target_info["original_repository"])
    if "original_tag" in target_info and target_info["original_tag"] != None:
        args.add("--original-tag", target_info["original_tag"])
    if "original_digest" in target_info and target_info["original_digest"] != None:
        args.add("--original-digest", target_info["original_digest"])

    if manifest_info != None:
        args.add("--root-path", manifest_info.manifest.path)
        args.add("--root-kind", "manifest")
        args.add("--manifest-path", "0=" + manifest_info.manifest.path)
        args.add("--missing-blobs-for-manifest", "0=" + (",".join(manifest_info.missing_blobs)))
        inputs.append(manifest_info.manifest)
    if index_info != None:
        args.add("--root-path", index_info.index.path)
        args.add("--root-kind", "index")
        for i, manifest in enumerate(index_info.manifests):
            args.add("--manifest-path", "{}={}".format(i, manifest.manifest.path))
            args.add("--missing-blobs-for-manifest", "{}={}".format(i, ",".join(manifest.missing_blobs)))
        inputs.append(index_info.index)
        inputs.extend([manifest.manifest for manifest in index_info.manifests])

    metadata_out = ctx.actions.declare_file(ctx.label.name + ".json")
    args.add(metadata_out.path)
    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = inputs,
        outputs = [metadata_out],
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        mnemonic = "PushMetadata",
    )
    return metadata_out

def _image_push_impl(ctx):
    """Implementation of the push rule."""
    pusher = ctx.actions.declare_file(ctx.label.name + ".exe")
    img_toolchain_info = ctx.attr._tool[0][ImageToolchainInfo]
    ctx.actions.symlink(
        output = pusher,
        target_file = img_toolchain_info.tool_exe,
        is_executable = True,
    )
    manifest_info = ctx.attr.image[ImageManifestInfo] if ImageManifestInfo in ctx.attr.image else None
    index_info = ctx.attr.image[ImageIndexInfo] if ImageIndexInfo in ctx.attr.image else None
    if manifest_info == None and index_info == None:
        fail("image must provide ImageManifestInfo or ImageIndexInfo")
    if manifest_info != None and index_info != None:
        fail("image must provide either ImageManifestInfo or ImageIndexInfo, not both")

    root_symlinks = calculate_root_symlinks(index_info, manifest_info, include_layers = _push_strategy(ctx) == "eager")

    templates = dict(
        registry = ctx.attr.registry,
        repository = ctx.attr.repository,
        tags = _get_tags(ctx),
    )

    # Either expand templates or write directly
    configuration_json = expand_or_write(
        ctx = ctx,
        templates = templates,
        output_name = ctx.label.name + ".configuration.json",
    )

    dispatch_json = _compute_push_metadata(
        ctx = ctx,
        configuration_json = configuration_json,
    )

    root_symlinks["dispatch.json"] = dispatch_json
    return [
        DefaultInfo(
            files = depset([dispatch_json]),
            executable = pusher,
            runfiles = ctx.runfiles(root_symlinks = root_symlinks),
        ),
        RunEnvironmentInfo(
            environment = {
                "IMG_REAPI_ENDPOINT": ctx.attr._push_settings[PushSettingsInfo].remote_cache,
                "IMG_CREDENTIAL_HELPER": ctx.attr._push_settings[PushSettingsInfo].credential_helper,
            },
            inherited_environment = [
                "IMG_REAPI_ENDPOINT",
                "IMG_CREDENTIAL_HELPER",
            ],
        ),
    ]

image_push = rule(
    implementation = _image_push_impl,
    doc = """Pushes container images to a registry.

This rule creates an executable target that uploads OCI images to container registries.
It supports multiple push strategies optimized for different use cases, from simple
uploads to advanced content-addressable storage integration.

Key features:
- **Multiple push strategies**: Choose between eager, lazy, CAS-based, or BES-integrated pushing
- **Template expansion**: Dynamic registry, repository, and tag values using build settings
- **Stamping support**: Include build information in image tags
- **Incremental uploads**: Skip blobs that already exist in the registry

The rule produces an executable that can be run with `bazel run`.

Example:

```python
load("@rules_img//img:push.bzl", "image_push")

# Simple push to Docker Hub
image_push(
    name = "push_app",
    image = ":my_app",
    registry = "index.docker.io",
    repository = "myorg/myapp",
    tag = "latest",
)

# Push multi-platform image with multiple tags
image_push(
    name = "push_multiarch",
    image = ":my_app_index",  # References an image_index
    registry = "gcr.io",
    repository = "my-project/my-app",
    tag_list = ["latest", "v1.0.0"],
)

# Dynamic push with build settings
image_push(
    name = "push_dynamic",
    image = ":my_app",
    registry = "{{.REGISTRY}}",
    repository = "{{.PROJECT}}/my-app",
    tag = "{{.VERSION}}",
    build_settings = {
        "REGISTRY": "//settings:registry",
        "PROJECT": "//settings:project",
        "VERSION": "//settings:version",
    },
)

# Push with stamping for unique tags
image_push(
    name = "push_stamped",
    image = ":my_app",
    registry = "index.docker.io",
    repository = "myorg/myapp",
    tag = "latest-{{.BUILD_TIMESTAMP}}",
    stamp = "enabled",
)

# Digest-only push (no tag)
image_push(
    name = "push_by_digest",
    image = ":my_app",
    registry = "gcr.io",
    repository = "my-project/my-app",
    # No tag specified - will push by digest only
)
```

Push strategies:
- **`eager`**: Materializes all layers next to push binary. Simple, correct, but may be inefficient.
- **`lazy`**: Layers are not stored locally. Missing layers are streamed from Bazel's remote cache.
- **`cas_registry`**: Uses content-addressable storage for extreme efficiency. Requires
  CAS-enabled infrastructure.
- **`bes`**: Image is pushed as side-effect of Build Event Stream upload. No "bazel run" command needed.
  Requires Build Event Service integration.

See [push strategies documentation](/docs/push-strategies.md) for detailed comparisons.

Runtime usage:
```bash
# Push to registry
bazel run //path/to:push_app

# The push command will output the image digest
```
""",
    attrs = {
        "registry": attr.string(
            doc = """Registry URL to push the image to.

Common registries:
- Docker Hub: `index.docker.io`
- Google Container Registry: `gcr.io` or `us.gcr.io`
- GitHub Container Registry: `ghcr.io`
- Amazon ECR: `123456789.dkr.ecr.us-east-1.amazonaws.com`

Subject to [template expansion](/docs/templating.md).
""",
        ),
        "repository": attr.string(
            doc = """Repository path within the registry.

Subject to [template expansion](/docs/templating.md).
""",
        ),
        "tag": attr.string(
            doc = """Tag to apply to the pushed image.

Optional - if omitted, the image is pushed by digest only.

Subject to [template expansion](/docs/templating.md).
""",
        ),
        "tag_list": attr.string_list(
            doc = """List of tags to apply to the pushed image.

Useful for applying multiple tags in a single push:

```python
tag_list = ["latest", "v1.0.0", "stable"]
```

Cannot be used together with `tag`. Each tag is subject to [template expansion](/docs/templating.md).
""",
        ),
        "image": attr.label(
            doc = "Image to push. Should provide ImageManifestInfo or ImageIndexInfo.",
            mandatory = True,
        ),
        "strategy": attr.string(
            doc = """Push strategy to use.

See [push strategies documentation](/docs/push-strategies.md) for detailed information.
""",
            default = "auto",
            values = ["auto", "eager", "lazy", "cas_registry", "bes"],
        ),
        "build_settings": attr.string_keyed_label_dict(
            doc = """Build settings for template expansion.

Maps template variable names to string_flag targets. These values can be used in
registry, repository, and tag attributes using `{{.VARIABLE_NAME}}` syntax (Go template).

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
        "_push_settings": attr.label(
            default = Label("//img/private/settings:push"),
            providers = [PushSettingsInfo],
        ),
        "_stamp_settings": attr.label(
            default = Label("//img/private/settings:stamp"),
            providers = [StampSettingInfo],
        ),
        "_tool": attr.label(
            cfg = host_platform_transition,
            default = Label("//img:resolved_toolchain"),
        ),
    },
    executable = True,
    cfg = reset_platform_transition,
    toolchains = TOOLCHAINS,
)
