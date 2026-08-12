"""Shared helper functions for push and load rules."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private/providers:deploy_info.bzl", "DeployInfo")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
load("//img/private/providers:load_settings_info.bzl", "LoadSettingsInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")
load("//img/private/providers:push_at_build_time_settings_info.bzl", "PushAtBuildTimeSettingsInfo")
load("//img/private/providers:push_settings_info.bzl", "PushSettingsInfo")
load("//img/private/providers:signing_config_info.bzl", "SigningConfigInfo")

# Sentinel default for the per-target `push_at_build_time_blob_repository` and
# `push_at_build_time_manifest_repository` string attributes. Left at this
# sentinel, the attribute defers to the corresponding global setting; set to any
# other string (including "") the value is used verbatim. This mirrors the
# INHERIT_FROM_BASE sentinel used by image_manifest's entrypoint/cmd/etc.: it
# lets the rule tell "untouched, use the global default" apart from "explicitly
# set to empty (no staging repository)". The value is a human-readable
# placeholder chosen to be extremely unlikely to collide with a real repository.
USE_GLOBAL_SETTING = "<use global setting>"

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

def resolve_load_destination(ctx):
    """Resolve and validate the registry/repository for a load target.

    Like push, load supports the global `destination_registry` fallback, but
    only for the split `registry`/`repository` form: when `repository` is set
    and `registry` is empty, the `--@rules_img//img/settings:destination_registry`
    flag fills in the registry. The registry/repository must end up set together
    (to reconstruct `<registry>/<repository>:<tag>` image names) or both empty
    (backwards-compatible mode, where the tags are already full image
    references). In the backwards-compatible mode (empty `repository`) the
    fallback deliberately does not apply, so legacy `rules_oci`-style targets
    never get a registry injected.

    Args:
        ctx: Rule context with registry/repository attributes.

    Returns:
        Tuple (registry, repository) of the (possibly empty) resolved values.
    """
    registry = ctx.attr.registry
    repository = ctx.attr.repository

    # Fall back to the global destination_registry only for the split
    # registry/repository/tag form. Legacy usage (a single full-reference `tag`
    # with empty registry+repository) must NOT get a registry injected.
    if not registry and repository:
        registry = ctx.attr._destination_registry[BuildSettingInfo].value

    if bool(registry) != bool(repository):
        fail("image_load/image_load_spec: 'registry' and 'repository' must be set together (or neither); got registry = {}, repository = {}".format(
            repr(registry),
            repr(repository),
        ))
    return registry, repository

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

def content_tracking_json_vars(descriptor):
    """Build json_vars wiring that exposes the image digest for content tracking.

    When `tracks_content` is enabled, the image descriptor is passed through the
    `json_vars` path of `expand_or_write`. This both declares the descriptor as an
    input of the template-expansion action (so the tag re-stamps when the image
    content changes) and exposes the digest to templates as `{{.digest}}`.

    Args:
        descriptor: File containing the image descriptor (manifest/index), or None
            to disable content tracking.

    Returns:
        Tuple of (json_vars, json_path_to_root) suitable for `expand_or_write`.
        Both are None when descriptor is None.
    """
    if descriptor == None:
        return None, None
    return {"digest": descriptor}, {"digest": "digest"}

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

def resolve_push_at_build_time(ctx):
    """Resolve the effective push-at-build-time settings for a push target.

    Combines the per-target attributes (shared by `image_push` and
    `image_push_spec` via COMMON_PUSH_ATTRS) with the global settings, so each
    attribute either takes an explicit per-target value or defers to its global
    flag:

    - `push_at_build_time` / `push_at_build_time_content` / `forbid_layer_push` /
      `deduplicated_push` / `deduplicated_push_content`: "auto" defers to the global
      setting.
    - `push_at_build_time_blob_repository` / `push_at_build_time_manifest_repository` /
      `deduplicated_push_blob_repository`: the USE_GLOBAL_SETTING sentinel defers to
      the global setting; any other string (including "") is used verbatim.
    - `push_at_build_time_exec_properties`: used verbatim (no global fallback).
    - gateway endpoints: always global (there is no per-target attribute).

    Args:
        ctx: Rule context with the push-at-build-time attributes, `_push_settings`
            and `_push_at_build_time_settings`.

    Returns:
        A struct with fields: mode, content, blob_repository, manifest_repository,
        forbid_layer_push (bool), deduplicated_push (mode string),
        deduplicated_push_blob_repository, deduplicated_push_content,
        exec_properties (dict), gateway, push_gateway, pull_gateway.
    """
    global_settings = ctx.attr._push_at_build_time_settings[PushAtBuildTimeSettingsInfo]
    push_settings = ctx.attr._push_settings[PushSettingsInfo]

    mode = ctx.attr.push_at_build_time
    if mode == "auto":
        mode = global_settings.mode

    content = ctx.attr.push_at_build_time_content
    if content == "auto":
        content = global_settings.content

    blob_repository = ctx.attr.push_at_build_time_blob_repository
    if blob_repository == USE_GLOBAL_SETTING:
        blob_repository = push_settings.blob_repository

    manifest_repository = ctx.attr.push_at_build_time_manifest_repository
    if manifest_repository == USE_GLOBAL_SETTING:
        manifest_repository = global_settings.manifest_repository

    forbid_layer_push = ctx.attr.forbid_layer_push
    if forbid_layer_push == "auto":
        forbid_layer_push = "enabled" if push_settings.forbid_layer_push else "disabled"

    deduplicated_push = ctx.attr.deduplicated_push
    if deduplicated_push == "auto":
        deduplicated_push = push_settings.deduplicated_push

    deduplicated_push_blob_repository = ctx.attr.deduplicated_push_blob_repository
    if deduplicated_push_blob_repository == USE_GLOBAL_SETTING:
        deduplicated_push_blob_repository = push_settings.deduplicated_push_blob_repository

    deduplicated_push_content = ctx.attr.deduplicated_push_content
    if deduplicated_push_content == "auto":
        deduplicated_push_content = push_settings.deduplicated_push_content

    return struct(
        mode = mode,
        content = content,
        blob_repository = blob_repository,
        manifest_repository = manifest_repository,
        forbid_layer_push = forbid_layer_push == "enabled",
        deduplicated_push = deduplicated_push,
        deduplicated_push_blob_repository = deduplicated_push_blob_repository,
        deduplicated_push_content = deduplicated_push_content,
        exec_properties = ctx.attr.push_at_build_time_exec_properties,
        gateway = global_settings.gateway,
        push_gateway = global_settings.push_gateway,
        pull_gateway = global_settings.pull_gateway,
    )

def cross_mount_blob_repository(mode, blob_repository):
    """The blob staging repository to record in the deploy manifest, or "".

    The `blob_repository` recorded in a push operation's deploy manifest drives
    blob cross-mounting at `img deploy` (`bazel run`) time. Only record it when
    push at build time actually stages the blobs (mode "enabled"/"best_effort");
    otherwise a deploy would try to cross-mount layers from a staging repository
    nothing was ever pushed to. So a non-empty blob_repository combined with a
    "disabled" push_at_build_time yields "" (no cross-mount attempt).

    Args:
        mode: Resolved push-at-build-time mode string.
        blob_repository: Resolved staging repository (may be "").

    Returns:
        blob_repository when push at build time is active, else "".
    """
    return blob_repository if mode in ("best_effort", "enabled") else ""

def resolve_signing(ctx):
    """Resolve whether and how this push target is signed.

    Combines the per-target `sign` attribute (auto -> global //img/settings:sign)
    with the `sign_setting` attribute (or global //img/settings:sign_setting).

    Args:
        ctx: Rule context with sign/sign_setting attributes and _sign/_sign_setting.

    Returns:
        A struct(config_info, best_effort, targets) when signing is active, or
        None when signing is disabled or best-effort with no configured setting.
    """
    mode = ctx.attr.sign
    if mode == "auto":
        mode = ctx.attr._sign[BuildSettingInfo].value
    if mode == "disabled":
        return None

    config_info = ctx.attr.sign_setting[SigningConfigInfo] if ctx.attr.sign_setting != None else ctx.attr._sign_setting[SigningConfigInfo]
    if config_info.config_file == None:
        if mode == "enabled":
            fail("sign is 'enabled' but no sign_setting is configured; set the 'sign_setting' attribute or --@rules_img//img/settings:sign_setting")
        return None  # best_effort with no configured setting: nothing to sign

    return struct(
        config_info = config_info,
        best_effort = mode == "best_effort",
        targets = config_info.targets,
    )
