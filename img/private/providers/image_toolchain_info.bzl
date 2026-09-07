"""Defines providers for a container image builder toolchain."""

DOC = """\
Information about how to invoke the container image builder tool.
"""

FIELDS = dict(
    supports_treeartifact_uplevel_symlinks = "Whether directory outputs may use relative symlinks to inputs outside the tree artifact (Bazel >= 7.1 and a non-Windows execution OS). Tar outputs must always embed their content.",
    tool_exe = "The builder executable (File).",
)

ImageToolchainInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
