"""Toolchain platform definitions and constraint mapping utilities.

This module provides utilities for mapping between Go platform identifiers
(GOOS/GOARCH) and Bazel platform constraints.
"""

# List of supported (OS, architecture) platform tuples
platform_tuples = [
    ("linux", "amd64"),
    ("linux", "arm64"),
    ("darwin", "amd64"),
    ("darwin", "arm64"),
    ("windows", "amd64"),
    ("windows", "arm64"),
]

def goos_to_constraint(goos):
    """Converts a Go OS identifier to a Bazel platform constraint.

    Args:
        goos: A string representing the Go OS identifier (e.g., "linux", "darwin", "windows")

    Returns:
        A string representing the Bazel platform constraint label for the OS
    """
    if goos == "linux":
        return "@platforms//os:linux"
    elif goos == "darwin":
        return "@platforms//os:macos"
    elif goos == "windows":
        return "@platforms//os:windows"
    else:
        fail("Unknown goos: {}".format(goos))

def goarch_to_constraint(goarch):
    """Converts a Go architecture identifier to a Bazel platform constraint.

    Args:
        goarch: A string representing the Go architecture identifier (e.g., "amd64", "arm64")

    Returns:
        A string representing the Bazel platform constraint label for the CPU architecture
    """
    if goarch == "amd64":
        return "@platforms//cpu:x86_64"
    elif goarch == "arm64":
        return "@platforms//cpu:aarch64"
    else:
        fail("Unknown goarch: {}".format(goarch))
