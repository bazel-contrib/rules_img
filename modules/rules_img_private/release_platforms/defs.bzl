"""Release platform definitions and the split transition over them.

This lives in `rules_img_private` rather than in `@rules_img` so that it can be
loaded without the root module's dev-only dependencies. `@rules_img`'s own
`//img/private/release:defs.bzl` loads `@rules_pkg`, and the transition below
needs `@rules_go`; both are `dev_dependency` edges of `@rules_img`, so neither
is visible when `@rules_img` is consumed as a dependency rather than as the root
module. Depending on this module instead lets any module build a target once per
release platform.
"""

GOOS_LINUX = "linux"
GOOS_DARWIN = "darwin"
GOOS_WINDOWS = "windows"

GOARCH_AMD64 = "amd64"
GOARCH_ARM64 = "arm64"
GOARCH_S390X = "s390x"
GOARCH_RISCV64 = "riscv64"

go_to_constraint_value = {
    GOOS_LINUX: "@platforms//os:linux",
    GOOS_DARWIN: "@platforms//os:macos",
    GOOS_WINDOWS: "@platforms//os:windows",
    GOARCH_AMD64: "@platforms//cpu:x86_64",
    GOARCH_ARM64: "@platforms//cpu:arm64",
    GOARCH_S390X: "@platforms//cpu:s390x",
    GOARCH_RISCV64: "@platforms//cpu:riscv64",
}

_goos_list = [
    GOOS_LINUX,
    GOOS_DARWIN,
    GOOS_WINDOWS,
]

# buildifier: disable=unused-variable
_goarch_list = [
    GOARCH_AMD64,
    GOARCH_ARM64,
    GOARCH_S390X,
    # TODO: fix rules_go upstream:
    # add riscv64 to BAZEL_GOARCH_CONSTRAINTS
    GOARCH_RISCV64,
]

_os_to_arches = {
    GOOS_LINUX: [GOARCH_AMD64, GOARCH_ARM64, GOARCH_S390X],
    GOOS_DARWIN: [GOARCH_AMD64, GOARCH_ARM64],
    GOOS_WINDOWS: [GOARCH_AMD64, GOARCH_ARM64],
}

def _generate_platforms():
    platforms = []
    for os in _goos_list:
        for arch in _os_to_arches[os]:
            platforms.append((os, arch))
    return platforms

def platform_name(tup):
    return tup[0] + "_" + tup[1]

def _parse_platform_name(name):
    return tuple(name.split("_"))

PLATFORMS = _generate_platforms()

PLATFORM_NAMES = [platform_name(platform) for platform in PLATFORMS]

def is_windows(platform):
    """Whether a release platform's binaries carry a .exe extension.

    Args:
        platform: A release platform name, e.g. "windows_amd64".

    Returns:
        True if the platform is Windows.
    """
    return platform.startswith(GOOS_WINDOWS + "_")

ReleasePlatformInfo = provider(doc = "Holds information about a platform configuration", fields = ["os", "arch", "platform"])

def _release_platform_flag_impl(ctx):
    tup = _parse_platform_name(ctx.build_setting_value)
    if tup not in PLATFORMS:
        fail("unknown release platform %s" % ctx.build_setting_value)

    return ReleasePlatformInfo(os = tup[0], arch = tup[1], platform = Label(ctx.build_setting_value))

release_platform_flag = rule(
    implementation = _release_platform_flag_impl,
    build_setting = config.string(flag = True),
)

def _release_platforms_transition_impl(_settings, _attr):
    return {
        platform: {
            "//command_line_option:platforms": str(Label("@rules_img_private//release_platforms:" + platform)),
            "//command_line_option:strip": "always",
            "//command_line_option:compilation_mode": "opt",
            "@rules_go//go/config:pure": True,
            "@rules_img_private//release_platforms:release_platform": platform,
        }
        for platform in PLATFORM_NAMES
    }

# Released binaries must also be built with `--experimental_output_paths=strip`,
# which this transition cannot set: Bazel rejects `--experimental_*` options as
# transition outputs. Modules building release binaries have to enable it in their
# .bazelrc instead, per host platform, since path mapping does not work on Windows.
# Without it, Bazel's output directory name embeds the legacy `--cpu` value, which
# stays at its host-derived default even though we transition `--platforms` above,
# so output paths that end up baked into an artifact -- the Go compiler records the
# source paths of generated files for stack traces -- differ between a macOS and a
# Linux build host.
release_platforms_transition = transition(
    implementation = _release_platforms_transition_impl,
    inputs = [],
    outputs = [
        "//command_line_option:platforms",
        "//command_line_option:strip",
        "//command_line_option:compilation_mode",
        "@rules_go//go/config:pure",
        "@rules_img_private//release_platforms:release_platform",
    ],
)
