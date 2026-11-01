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

_compact_layers_setting = str(Label("//img/settings:experimental_compact_layers"))
_compact_layers_materialize_blob_setting = str(Label("//img/settings:experimental_compact_layers_materialize_blob"))
_disk_image_extent_stream_setting = str(Label("//img/settings:disk_image_extent_stream"))

_original_layer_settings_setting = str(Label("//img/private/settings:original_layer_settings"))

# The settings that image_dd(compact = ...) sets. They are put back (see
# reset_compact_image_transition) where the build leaves the image, so that the
# files it is made of, and the tools, are built once, whatever the image is.
_compact_image_settings = [
    _compact_layers_setting,
    _compact_layers_materialize_blob_setting,
    _disk_image_extent_stream_setting,
]
_compact_image_all_settings = _compact_image_settings + [_original_layer_settings_setting]

def _unchanged(settings):
    # An empty dict would be a split transition with no branches.
    return {k: v for k, v in settings.items()}

def _compact_image_transition_impl(settings, attr):
    """Builds the image of a rule with `compact` set as compact streams all the way."""
    compact = getattr(attr, "compact", "auto")
    if compact == "auto" or settings[_original_layer_settings_setting]:
        return _unchanged(settings)
    value = "enabled" if compact == "enabled" else "disabled"
    wanted = {
        _compact_layers_setting: value,
        _compact_layers_materialize_blob_setting: "disabled",
        _disk_image_extent_stream_setting: value,
    }

    # Only what changes is recorded. JSON, so that a setting that was not set
    # explicitly differs from one that was set to its default.
    changed = {s: settings[s] for s in _compact_image_settings if settings[s] != wanted[s]}
    if not changed:
        return _unchanged(settings)
    wanted[_original_layer_settings_setting] = json.encode(changed)
    return wanted

compact_image_transition = transition(
    implementation = _compact_image_transition_impl,
    inputs = _compact_image_all_settings,
    outputs = _compact_image_all_settings,
)

def _reset_compact_image_transition_impl(settings, _attr):
    origin = settings[_original_layer_settings_setting]
    if not origin:
        return _unchanged(settings)
    result = _unchanged(settings)
    result.update(json.decode(origin))
    result[_original_layer_settings_setting] = ""
    return result

# For the attributes of the rules in an image that are not part of the image: the
# files and binaries that go into a layer. They are built as they are without the
# image, with the settings that image_dd(compact = ...) recorded.
reset_compact_image_transition = transition(
    implementation = _reset_compact_image_transition_impl,
    inputs = _compact_image_all_settings,
    outputs = _compact_image_all_settings,
)
