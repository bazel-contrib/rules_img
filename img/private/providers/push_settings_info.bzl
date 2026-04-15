"""Defines providers for settings of push rules."""

DOC = """\
Collection of active push settings.
"""

FIELDS = dict(
    strategy = "The strategy of the push rule. This can be one of the following: 'eager', 'lazy', 'cas_registry', or 'bes'.",
    remote_cache = "Bazel remote cache to use for the push rule as part of the lazy push strategy. Uses the same format as Bazel's --remote_cache flag. Uses $IMG_REAPI_ENDPOINT env var if not set.",
    credential_helper = "Credential helper to use for the push rule. This can be the same as Bazel's credential helper. Uses $IMG_CREDENTIAL_HELPER env var or tools/credential-helper if not set.",
    cross_mount = "Cross-mount configuration. Either 'same_registry', 'cross_registry' or 'disabled'.",
)

PushSettingsInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
