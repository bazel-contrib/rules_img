"""Build settings for container image rules."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private/providers:push_settings_info.bzl", "PushSettingsInfo")

def _push_settings_impl(ctx):
    strategy = ctx.attr._push_strategy[BuildSettingInfo].value
    remote_cache = ctx.attr._remote_cache[BuildSettingInfo].value
    remote_instance_name = ctx.attr._remote_instance_name[BuildSettingInfo].value
    credential_helper = ctx.attr._credential_helper[BuildSettingInfo].value
    credential_helper_oci_registry = ctx.attr._credential_helper_oci_registry[BuildSettingInfo].value
    credential_helper_remote_cache = ctx.attr._credential_helper_remote_cache[BuildSettingInfo].value
    cross_mount = ctx.attr._cross_mount[BuildSettingInfo].value
    blob_repository = ctx.attr._blob_repository[BuildSettingInfo].value
    forbid_layer_push = ctx.attr._forbid_layer_push[BuildSettingInfo].value == "enabled"

    return [PushSettingsInfo(
        strategy = strategy,
        remote_cache = remote_cache,
        remote_instance_name = remote_instance_name,
        credential_helper = credential_helper,
        credential_helper_oci_registry = credential_helper_oci_registry,
        credential_helper_remote_cache = credential_helper_remote_cache,
        cross_mount = cross_mount,
        blob_repository = blob_repository,
        forbid_layer_push = forbid_layer_push,
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
        "_remote_instance_name": attr.label(
            default = Label("//img/settings:remote_instance_name"),
            providers = [BuildSettingInfo],
        ),
        "_credential_helper": attr.label(
            default = Label("//img/settings:credential_helper"),
            providers = [BuildSettingInfo],
        ),
        "_credential_helper_oci_registry": attr.label(
            default = Label("//img/settings:credential_helper_oci_registry"),
            providers = [BuildSettingInfo],
        ),
        "_credential_helper_remote_cache": attr.label(
            default = Label("//img/settings:credential_helper_remote_cache"),
            providers = [BuildSettingInfo],
        ),
        "_cross_mount": attr.label(
            default = Label("//img/settings:cross_mount"),
            providers = [BuildSettingInfo],
        ),
        "_blob_repository": attr.label(
            default = Label("//img/settings:push_at_build_time_repository"),
            providers = [BuildSettingInfo],
        ),
        "_forbid_layer_push": attr.label(
            default = Label("//img/settings:forbid_layer_push"),
            providers = [BuildSettingInfo],
        ),
    },
)
