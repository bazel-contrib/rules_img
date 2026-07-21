"""Push-at-build-time settings rule."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private/providers:push_at_build_time_settings_info.bzl", "PushAtBuildTimeSettingsInfo")

def _push_at_build_time_settings_impl(ctx):
    return [PushAtBuildTimeSettingsInfo(
        mode = ctx.attr._mode[BuildSettingInfo].value,
        content = ctx.attr._content[BuildSettingInfo].value,
        manifest_repository = ctx.attr._manifest_repository[BuildSettingInfo].value,
        gateway = ctx.attr._gateway[BuildSettingInfo].value,
        push_gateway = ctx.attr._push_gateway[BuildSettingInfo].value,
        pull_gateway = ctx.attr._pull_gateway[BuildSettingInfo].value,
    )]

push_at_build_time_settings = rule(
    implementation = _push_at_build_time_settings_impl,
    attrs = {
        "_mode": attr.label(
            default = Label("//img/settings:push_at_build_time"),
            providers = [BuildSettingInfo],
        ),
        "_content": attr.label(
            default = Label("//img/settings:push_at_build_time_content"),
            providers = [BuildSettingInfo],
        ),
        "_manifest_repository": attr.label(
            default = Label("//img/settings:push_at_build_time_manifest_repository"),
            providers = [BuildSettingInfo],
        ),
        "_gateway": attr.label(
            default = Label("//img/settings:registry_gateway"),
            providers = [BuildSettingInfo],
        ),
        "_push_gateway": attr.label(
            default = Label("//img/settings:registry_push_gateway"),
            providers = [BuildSettingInfo],
        ),
        "_pull_gateway": attr.label(
            default = Label("//img/settings:registry_pull_gateway"),
            providers = [BuildSettingInfo],
        ),
    },
)
