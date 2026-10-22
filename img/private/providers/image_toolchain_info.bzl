"""Defines providers for a container image builder toolchain."""

DOC = """\
Information about how to invoke the container image builder tool.
"""

FIELDS = dict(
    use_treeartifact_uplevel_symlinks = "Whether directory outputs use relative symlinks to inputs outside the tree artifact (Bazel >= 7.1, a non-Windows execution OS, and --@rules_img//img/settings:treeartifact_symlinks=enabled). Tar outputs must always embed their content.",
    tool_exe = "The builder executable (File).",
)

ImageToolchainInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
