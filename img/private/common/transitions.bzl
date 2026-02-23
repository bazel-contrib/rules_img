"""Platform transition implementations for container image rules."""

_platforms_setting = "//command_line_option:platforms"
_original_platforms_setting = str(Label("//img/private/settings:original_platforms"))

def _encode_platforms(platforms):
    return ",".join([str(platform) for platform in platforms])

def _encode_platforms_if_different(settings, input_platforms):
    before = _encode_platforms(settings[_platforms_setting])
    after = _encode_platforms([input_platforms])
    if before == after:
        return ""
    return after

def _decode_original_patforms(settings):
    maybe_original_platforms = settings[_original_platforms_setting]
    if len(maybe_original_platforms) == 0:
        return settings[_platforms_setting]
    return maybe_original_platforms.split(",")

def _multi_platform_image_transition_impl(settings, attr):
    if len(attr.platforms) == 0:
        # No platforms specified, no transition needed
        return []
    return {
        str(i): {
            _platforms_setting: str(platform),
            _original_platforms_setting: _encode_platforms_if_different(settings, platform),
        }
        for (i, platform) in enumerate(attr.platforms)
    }

multi_platform_image_transition = transition(
    implementation = _multi_platform_image_transition_impl,
    inputs = [_platforms_setting],
    outputs = [
        _platforms_setting,
        _original_platforms_setting,
    ],
)

def _reset_platform_transition_impl(settings, _attr):
    return {
        _platforms_setting: _decode_original_patforms(settings),
        # remove the saved info about the
        # original platform since we don't
        # want to propagate it further
        _original_platforms_setting: "",
    }

reset_platform_transition = transition(
    implementation = _reset_platform_transition_impl,
    inputs = [
        _platforms_setting,
        _original_platforms_setting,
    ],
    outputs = [
        _platforms_setting,
        _original_platforms_setting,
    ],
)

def _normalize_layer_transition_impl(_settings, _attr):
    return {
        # We don't need to track the original
        # platform outside of targets that have
        # a base image.
        _original_platforms_setting: "",
    }

normalize_layer_transition = transition(
    implementation = _normalize_layer_transition_impl,
    inputs = [],
    outputs = [_original_platforms_setting],
)

def _single_platform_transition_impl(settings, attr):
    """Transition to a single target platform if specified."""
    if not hasattr(attr, "platform") or not attr.platform:
        # No platform specified, no transition needed
        return {}

    return {
        _platforms_setting: str(attr.platform),
        _original_platforms_setting: _encode_platforms_if_different(settings, attr.platform),
    }

single_platform_transition = transition(
    implementation = _single_platform_transition_impl,
    inputs = [_platforms_setting],
    outputs = [
        _platforms_setting,
        _original_platforms_setting,
    ],
)

_host_platform_setting = "//command_line_option:host_platform"

def _host_platform_transition_impl(settings, _attr):
    """Transition to the host platform.

    Reads the --host_platform value (which Bazel resolves to
    @local_config_platform//:host by default) and sets --platforms to it.
    This avoids a direct label reference to @local_config_platform, which
    is a built-in repo not visible to non-root modules in Bzlmod.

    This is used to resolve toolchains (img tool, launcher template) for the
    host platform via target_compatible_with matching. Unlike the "host"
    exec_group approach, this does not require the host platform to be a
    registered execution platform — only a known target platform — making it
    compatible with cross-platform RBE setups (e.g. macOS host with
    Linux-only remote executors).
    """
    return {_platforms_setting: [str(settings[_host_platform_setting])]}

host_platform_transition = transition(
    implementation = _host_platform_transition_impl,
    inputs = [_host_platform_setting],
    outputs = [_platforms_setting],
)
