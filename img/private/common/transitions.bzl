"""Platform transition implementations for container image rules."""

_platforms_setting = "//command_line_option:platforms"
_host_platform_setting = "//command_line_option:host_platform"
_multiarch_platform = str(Label("//img/private/platforms:multiarch"))
_index_platform_setting = str(Label("//img/settings:index_platform"))

def _multi_platform_image_transition_impl(_settings, attr):
    if len(attr.platforms) == 0:
        # No platforms specified, no transition needed
        return []
    return {
        str(i): {_platforms_setting: str(platform)}
        for (i, platform) in enumerate(attr.platforms)
    }

multi_platform_image_transition = transition(
    implementation = _multi_platform_image_transition_impl,
    inputs = [],
    outputs = [_platforms_setting],
)

def _image_index_transition_impl(settings, attr):
    # An index with platforms builds each manifest for those platforms, so
    # moving it to one fixed platform configures it once, wherever it is
    # reached from. When attr.platforms is a select() on the platform, Bazel
    # can hide it from one evaluation of this transition; skipping it there
    # lets Bazel report the select() that can't match instead of a traceback
    # here.
    index_platform = getattr(attr, "index_platform", "auto")
    if index_platform == "auto":
        index_platform = settings[_index_platform_setting]
    if index_platform == "multiarch" and hasattr(attr, "platforms") and len(attr.platforms) > 0:
        return {_platforms_setting: _multiarch_platform}
    return {}

image_index_transition = transition(
    implementation = _image_index_transition_impl,
    inputs = [_index_platform_setting],
    outputs = [_platforms_setting],
)

def _single_platform_transition_impl(_settings, attr):
    """Transition to a single target platform if specified."""
    if not hasattr(attr, "platform") or not attr.platform:
        # No platform specified, no transition needed
        return {}

    return {_platforms_setting: str(attr.platform)}

single_platform_transition = transition(
    implementation = _single_platform_transition_impl,
    inputs = [],
    outputs = [_platforms_setting],
)

def _host_platform_transition_impl(settings, _attr):
    return {_platforms_setting: str(settings[_host_platform_setting])}

host_platform_transition = transition(
    implementation = _host_platform_transition_impl,
    inputs = [_host_platform_setting],
    outputs = [_platforms_setting],
)
