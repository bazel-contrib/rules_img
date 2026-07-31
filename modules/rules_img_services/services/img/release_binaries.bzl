"""Helper for building one binary once per release platform.

`release_binaries` applies `release_platforms_transition` to a single
executable, producing one build of it per release platform. Each build is
exposed as its own output group, named after the platform, so a `filegroup` can
pull an individual binary back out and hand it to `oras_file_layer`.

The output group names match the release asset names, e.g. `img_linux_amd64`
and `img_windows_amd64.exe`, so the artifact layer titles line up with the
files published on the GitHub release.
"""

load(
    "@rules_img_private//release_platforms:defs.bzl",
    "PLATFORM_NAMES",
    "is_windows",
    "release_platforms_transition",
)

def release_asset_name(basename, platform):
    """The published release asset name for one platform's binary.

    Args:
        basename: The tool's base name, e.g. "img".
        platform: A release platform name, e.g. "linux_amd64".

    Returns:
        The asset name, e.g. "img_linux_amd64" or "img_windows_amd64.exe".
    """
    return "{}_{}{}".format(basename, platform, ".exe" if is_windows(platform) else "")

def _release_binaries_impl(ctx):
    output_groups = {}
    all_binaries = []
    for platform in PLATFORM_NAMES:
        executable = ctx.split_attr.binary[platform][DefaultInfo].files_to_run.executable

        # Rename into the release asset name. oras_file_layer derives the
        # layer title from the file's basename unless told otherwise, and the
        # unrenamed binary is just called "img" for every platform.
        asset = ctx.actions.declare_file(release_asset_name(ctx.attr.basename, platform))
        ctx.actions.symlink(output = asset, target_file = executable, is_executable = True)
        output_groups[release_asset_name(ctx.attr.basename, platform)] = depset([asset])
        all_binaries.append(asset)

    return [
        DefaultInfo(files = depset(all_binaries)),
        OutputGroupInfo(**output_groups),
    ]

release_binaries = rule(
    implementation = _release_binaries_impl,
    doc = """Builds `binary` once per release platform.

Each platform's binary is available from the output group named after its
release asset (`img_linux_amd64`, `img_windows_amd64.exe`, ...), and the
default outputs are all of them at once.
""",
    attrs = {
        "binary": attr.label(
            cfg = release_platforms_transition,
            mandatory = True,
            doc = "The executable to build for every release platform.",
        ),
        "basename": attr.string(
            default = "img",
            doc = "Base name of the published release asset.",
        ),
    },
)
