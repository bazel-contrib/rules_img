"""Set of supported execution platforms."""

ANDROID = "android"
DARWIN = "darwin"
FREEBSD = "freebsd"
IOS = "ios"
LINUX = "linux"
NETBSD = "netbsd"
OPENBSD = "openbsd"
WINDOWS = "windows"

AMD64 = "amd64"
I386 = "386"
ARM = "arm"
ARM64 = "arm64"
PPC64LE = "ppc64le"
MIPS64 = "mips64"
S390X = "s390x"
WASM = "wasm"

# Skipped due to lack of support in Bazel:
# OS:
#   - "dragonfly"
#   - "illumos"
#   - "js"
#   - "plan9"
#   - "wasip1"
# CPU:
#   - "ppc64"
#   - "mips64le"
#   - "mipsle"

_goos_list = [
    ANDROID,
    DARWIN,
    FREEBSD,
    IOS,
    LINUX,
    NETBSD,
    OPENBSD,
    WINDOWS,
]

_goarch_list = [
    AMD64,
    I386,
    ARM,
    ARM64,
    PPC64LE,
    MIPS64,
    S390X,
    WASM,
]

def _bazel_os(name):
    if name == DARWIN:
        return "macos"
    return name

def _bazel_cpu(name):
    if name == AMD64:
        return "x86_64"
    if name == I386:
        return "x86_32"
    if name == WASM:
        return "wasm64"
    return name

def _platform_os(name):
    if name == "macos":
        return DARWIN
    return name

def _platform_cpu(name):
    if name == "x86_64":
        return AMD64
    if name == "x86_32":
        return I386
    if name == "aarch32":
        return ARM
    if name == "aarch64":
        return ARM64
    if name == "wasm64":
        return WASM
    return name

def _constraints(tup):
    return [
        "@platforms//os:" + _bazel_os(tup[0]),
        "@platforms//cpu:" + _bazel_cpu(tup[1]),
    ]

def _platform_name(tup):
    return tup[0] + "_" + tup[1]

_tuples = [
    # linux
    (LINUX, AMD64),
    (LINUX, ARM64),

    # darwin
    (DARWIN, AMD64),
    (DARWIN, ARM64),

    # windows
    (WINDOWS, AMD64),
    (WINDOWS, ARM64),
]

_config_setting_groups = [
    (goos, goarch)
    for goos in _goos_list
    for goarch in _goarch_list
]

PLATFORMS = {
    _platform_name(tup): struct(
        platform_info = str(Label(":" + _platform_name(tup))),
        constraints = _constraints(tup),
    )
    for tup in _tuples
}

CONFIG_SETTINGS = [
    struct(
        name = _platform_name(tup),
        match_all = _constraints(tup),
    )
    for tup in _config_setting_groups
]

def platform_for_goos_and_goarch(mangled_name):
    [goos, goarch] = mangled_name.split("_")
    os = _platform_os(goos)
    cpu = _platform_cpu(goarch)
    return PLATFORMS[_platform_name((os, cpu))]

def has_constraint_setting(goos, goarch):
    for tup in _config_setting_groups:
        if tup[0] == goos and tup[1] == goarch:
            return True
    return False
