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
    return [
        {
            _platforms_setting: str(platform),
            _original_platforms_setting: _encode_platforms_if_different(settings, platform),
        }
        for platform in attr.platforms
    ]

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
