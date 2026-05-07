"""Shared helper functions for push and load rules."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private/providers:deploy_info.bzl", "DeployInfo")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
load("//img/private/providers:load_settings_info.bzl", "LoadSettingsInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")
load("//img/private/providers:push_settings_info.bzl", "PushSettingsInfo")

def get_tags(ctx):
    """Get the list of tags from the context, validating mutual exclusivity.

    Args:
        ctx: Rule context with tag/tag_list attributes.

    Returns:
        List of tag strings (may be empty for digest-only push).
    """
    if ctx.attr.tag and ctx.attr.tag_list:
        fail("Cannot specify both 'tag' and 'tag_list' attributes")

    tags = []
    if ctx.attr.tag:
        tags = [ctx.attr.tag]
    elif ctx.attr.tag_list:
        tags = ctx.attr.tag_list

    return tags

def image_target_vars(label):
    """Extract package/name variables from an image label.

    Args:
        label: A Bazel label.

    Returns:
        Dict with image_target_package and image_target_name keys.
    """
    return {
        "image_target_package": label.package,
        "image_target_name": label.name,
    }

def get_image_providers(ctx):
    """Extract and validate image providers from ctx.attr.image.

    Args:
        ctx: Rule context with an image attribute.

    Returns:
        Tuple of (manifest_info, index_info) where exactly one is non-None.
    """
    manifest_info = ctx.attr.image[ImageManifestInfo] if ImageManifestInfo in ctx.attr.image else None
    index_info = ctx.attr.image[ImageIndexInfo] if ImageIndexInfo in ctx.attr.image else None
    if manifest_info == None and index_info == None:
        fail("image must provide ImageManifestInfo or ImageIndexInfo")
    if manifest_info != None and index_info != None:
        fail("image must provide either ImageManifestInfo or ImageIndexInfo, not both")
    return manifest_info, index_info

def resolve_push_registry(ctx):
    """Resolve and validate the push registry from context attributes.

    Args:
        ctx: Rule context with registry/repository/destination_file attributes.

    Returns:
        The resolved registry string (empty string when destination_file is used).
    """
    registry = ctx.attr.registry
    if not registry:
        registry = ctx.attr._destination_registry[BuildSettingInfo].value

    if ctx.attr.destination_file:
        if ctx.attr.registry:
            fail("Cannot specify both 'destination_file' and 'registry' attributes")
        if ctx.attr.repository:
            fail("Cannot specify both 'destination_file' and 'repository' attributes")
        registry = ""
    else:
        if not registry:
            fail("'registry' is required when 'destination_file' is not set")
        if not ctx.attr.repository:
            fail("'repository' is required when 'destination_file' is not set")

    return registry

def resolve_push_strategy(ctx):
    """Determine the push strategy, resolving 'auto' from settings.

    Args:
        ctx: Rule context with strategy attribute and _push_settings.

    Returns:
        Resolved strategy string.
    """
    push_settings = ctx.attr._push_settings[PushSettingsInfo]
    strategy = ctx.attr.strategy
    if strategy == "auto":
        strategy = push_settings.strategy
    return strategy

def extract_referrers(ctx):
    """Extract referrer provider structs from ctx.attr.referrers.

    Args:
        ctx: Rule context with referrers attribute.

    Returns:
        List of struct(manifest_info, index_info).
    """
    referrers = []
    for referrer in ctx.attr.referrers:
        ref_manifest_info = referrer[ImageManifestInfo] if ImageManifestInfo in referrer else None
        ref_index_info = referrer[ImageIndexInfo] if ImageIndexInfo in referrer else None
        referrers.append(struct(manifest_info = ref_manifest_info, index_info = ref_index_info))
    return referrers

def extract_cross_mount_from(ctx):
    """Extract cross_mount_from DeployInfo if set.

    Args:
        ctx: Rule context with cross_mount_from attribute.

    Returns:
        DeployInfo or None.
    """
    return ctx.attr.cross_mount_from[DeployInfo] if ctx.attr.cross_mount_from != None else None

def resolve_load_strategy(ctx):
    """Determine the load strategy, resolving 'auto' from settings.

    Args:
        ctx: Rule context with strategy attribute and _load_settings.

    Returns:
        Resolved strategy string.
    """
    load_settings = ctx.attr._load_settings[LoadSettingsInfo]
    strategy = ctx.attr.strategy
    if strategy == "auto":
        strategy = load_settings.strategy
    return strategy

def resolve_daemon(ctx):
    """Determine the daemon to target, resolving 'auto' from settings.

    Args:
        ctx: Rule context with daemon attribute and _load_settings.

    Returns:
        Resolved daemon string.
    """
    load_settings = ctx.attr._load_settings[LoadSettingsInfo]
    daemon = ctx.attr.daemon
    if daemon == "auto":
        daemon = load_settings.daemon
    return daemon
