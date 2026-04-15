"""Build settings for container image rules."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private/providers:push_settings_info.bzl", "PushSettingsInfo")

def _push_settings_impl(ctx):
    strategy = ctx.attr._push_strategy[BuildSettingInfo].value
    remote_cache = ctx.attr._remote_cache[BuildSettingInfo].value
    credential_helper = ctx.attr._credential_helper[BuildSettingInfo].value
    cross_mount = ctx.attr._cross_mount[BuildSettingInfo].value

    return [PushSettingsInfo(
        strategy = strategy,
        remote_cache = remote_cache,
        credential_helper = credential_helper,
        cross_mount = cross_mount,
    )]

push_settings = rule(
    implementation = _push_settings_impl,
    attrs = {
        "_push_strategy": attr.label(
            default = Label("//img/settings:push_strategy"),
            providers = [BuildSettingInfo],
        ),
        "_remote_cache": attr.label(
            default = Label("//img/settings:remote_cache"),
            providers = [BuildSettingInfo],
        ),
        "_credential_helper": attr.label(
            default = Label("//img/settings:credential_helper"),
            providers = [BuildSettingInfo],
        ),
        "_cross_mount": attr.label(
            default = Label("//img/settings:cross_mount"),
            providers = [BuildSettingInfo],
        ),
    },
)
