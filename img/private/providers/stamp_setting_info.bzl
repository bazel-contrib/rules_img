"""Defines providers for about stamping."""

DOC = """\
Information on stamping configuration.
"""

FIELDS = dict(
    bazel_setting = "bool: Whether or not the `--stamp` flag was enabled",
    user_preference = "string: Global stamp preference ('auto', 'force', or 'disabled')",
)

StampSettingInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
