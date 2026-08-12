"""Push spec rule for defining push configuration without an image reference."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private/common:deploy_attrs.bzl", "COMMON_PUSH_ATTRS")
load("//img/private/common:deploy_helpers.bzl", "extract_cross_mount_from", "extract_referrers", "get_tags", "resolve_push_at_build_time", "resolve_push_registry", "resolve_push_strategy", "resolve_signing")
load("//img/private/common:transitions.bzl", "reset_platform_transition")
load("//img/private/providers:push_config_info.bzl", "PushConfigInfo")
load("//img/private/providers:push_settings_info.bzl", "PushSettingsInfo")
load("//img/private/providers:stamp_setting_info.bzl", "StampSettingInfo")

def _image_push_spec_impl(ctx):
    """Implementation of the push spec rule."""
    registry = resolve_push_registry(ctx)
    strategy = resolve_push_strategy(ctx)
    pbt = resolve_push_at_build_time(ctx)

    build_settings = {}
    for name, target in ctx.attr.build_settings.items():
        build_settings[name] = target[BuildSettingInfo].value

    return [PushConfigInfo(
        registry = registry,
        repository = ctx.attr.repository,
        tags = get_tags(ctx),
        manifest_tags = ctx.attr.manifest_tags,
        tag_file = ctx.file.tag_file,
        destination_file = ctx.file.destination_file,
        referrers = extract_referrers(ctx),
        cross_mount_from = extract_cross_mount_from(ctx),
        strategy = strategy,
        cross_mount_strategy = ctx.attr._push_settings[PushSettingsInfo].cross_mount,
        build_settings = build_settings,
        stamp = ctx.attr.stamp,
        stamp_settings = ctx.attr._stamp_settings[StampSettingInfo],
        tracks_content = ctx.attr.tracks_content,
        signing = resolve_signing(ctx),
        blob_repository = pbt.blob_repository,
        forbid_layer_push = pbt.forbid_layer_push,
        deduplicated_push = pbt.deduplicated_push,
        deduplicated_push_blob_repository = pbt.deduplicated_push_blob_repository,
        deduplicated_push_content = pbt.deduplicated_push_content,
        push_at_build_time_mode = pbt.mode,
        push_at_build_time_content = pbt.content,
        push_at_build_time_manifest_repository = pbt.manifest_repository,
        push_at_build_time_exec_properties = pbt.exec_properties,
        push_at_build_time_gateway = pbt.gateway,
        push_at_build_time_push_gateway = pbt.push_gateway,
        push_at_build_time_pull_gateway = pbt.pull_gateway,
        insecure = ctx.attr._push_settings[PushSettingsInfo].insecure,
    )]

image_push_spec = rule(
    implementation = _image_push_spec_impl,
    doc = """Defines push configuration for container images without referencing a specific image.

This rule captures registry, repository, tag, and strategy settings that can be
attached to `image_manifest` or `image_index` targets via their `push_specs`
attribute. Template strings using Go template syntax (`{{.VAR}}`) are accepted
but not expanded — expansion happens when the deployment is consumed by the
image rule.
Note that the template strings `{{.image_target_package}}` and `{{.image_target_name}}` are especially useful here.

This enables an inverted dependency pattern: instead of `image_push` depending
on the image, the image itself carries its deployment configuration, making it
directly usable with `multi_deploy`.

Example:

```python
load("@rules_img//img:push.bzl", "image_push_spec")

image_push_spec(
    name = "push_config",
    registry = "gcr.io",
    repository = "my-project/{{.image_target_package}}/{{.image_target_name}}",
    tag = "{{.VERSION}}",
    build_settings = {
        "VERSION": "//settings:version",
    },
    stamp = "force",
)

# Attach to an image:
image_manifest(
    name = "my_app_a",
    base = "@distroless_cc",
    layers = [":app_layer"],
    push_specs = [":push_config"],
)

# Attach to another image:
image_manifest(
    name = "my_app_b",
    base = "@distroless_cc",
    layers = [":app_layer"],
    push_specs = [":push_config"],
)

# Now usable directly in multi_deploy:
multi_deploy(
    name = "deploy",
    operations = [
        ":my_app_a",
        ":my_app_b",
    ],
)
```
""",
    attrs = COMMON_PUSH_ATTRS,
    provides = [PushConfigInfo],
    cfg = reset_platform_transition,
)
