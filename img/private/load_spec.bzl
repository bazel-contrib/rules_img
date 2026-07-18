"""Load spec rule for defining load configuration without an image reference."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private/common:deploy_attrs.bzl", "COMMON_LOAD_ATTRS")
load("//img/private/common:deploy_helpers.bzl", "get_tags", "resolve_daemon", "resolve_load_destination", "resolve_load_strategy")
load("//img/private/common:transitions.bzl", "reset_platform_transition")
load("//img/private/providers:load_config_info.bzl", "LoadConfigInfo")
load("//img/private/providers:stamp_setting_info.bzl", "StampSettingInfo")

def _image_load_spec_impl(ctx):
    """Implementation of the load spec rule."""
    build_settings = {}
    for name, target in ctx.attr.build_settings.items():
        build_settings[name] = target[BuildSettingInfo].value

    registry, repository = resolve_load_destination(ctx)

    return [LoadConfigInfo(
        registry = registry,
        repository = repository,
        daemon = resolve_daemon(ctx),
        tags = get_tags(ctx),
        tag_file = ctx.file.tag_file,
        strategy = resolve_load_strategy(ctx),
        build_settings = build_settings,
        stamp = ctx.attr.stamp,
        stamp_settings = ctx.attr._stamp_settings[StampSettingInfo],
        tracks_content = ctx.attr.tracks_content,
    )]

image_load_spec = rule(
    implementation = _image_load_spec_impl,
    doc = """Defines load configuration for container images without referencing a specific image.

This rule captures registry, repository, daemon, tag, and strategy settings that
can be attached to `image_manifest` or `image_index` targets via their
`load_specs` attribute.
Template strings using Go template syntax (`{{.VAR}}`) are accepted but not
expanded — expansion happens when the deployment is consumed by the image rule.
Note that the template strings `{{.image_target_package}}` and `{{.image_target_name}}` are especially useful here.

This enables an inverted dependency pattern: instead of `image_load` depending
on the image, the image itself carries its load configuration, making it
directly usable with `multi_deploy`.

Example:

```python
load("@rules_img//img:load.bzl", "image_load_spec")

# Full image reference in a single tag (rules_oci-compatible).
image_load_spec(
    name = "load_config",
    tag = "{{.image_target_package}}/{{.image_target_name}}:latest",
    daemon = "containerd",
)

# Or split into registry/repository/tag (same API as image_push_spec):
image_load_spec(
    name = "load_config_split",
    registry = "gcr.io",
    repository = "my-project/{{.image_target_package}}/{{.image_target_name}}",
    tag = "latest",
    daemon = "containerd",
)

# Attach to an image:
image_manifest(
    name = "my_app_a",
    base = "@distroless_cc",
    layers = [":app_layer"],
    load_specs = [":load_config"],
)

# Attach to another image:
image_manifest(
    name = "my_app_b",
    base = "@distroless_cc",
    layers = [":app_layer"],
    load_specs = [":load_config"],
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
    attrs = COMMON_LOAD_ATTRS,
    provides = [LoadConfigInfo],
    cfg = reset_platform_transition,
)
