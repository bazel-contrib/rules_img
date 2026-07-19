"""Helpers for shipping sign_setting config files into deploy binary runfiles.

sign_setting config files live in a single fixed runfiles area with unique
basenames, discovered at deploy time by the `img` tool (by content digest, so
basenames only need to be collision-free). Each producing file's Bazel output
path is globally unique, so sha256(path) is a safe, stable basename.
"""

load("@sha256.bzl", "sha256")

# Fixed runfiles area for sign_setting config files. Kept separate from the
# per-target image area so the deploy tool can always find it at one location.
SIGN_SETTINGS_PREFIX = "++rules_img_private++/sign_settings/"

def add_sign_setting_symlinks(root_symlinks, sign_config_infos):
    """Add fixed-area root symlinks for sign_setting config files.

    Args:
        root_symlinks: dict mapping symlink path -> File, mutated in place.
        sign_config_infos: list of SigningConfigInfo. Entries whose config_file
            is None (the unset sentinel) are ignored.

    Returns:
        A list of runfiles objects (the plugins' runfiles) to merge into the
        deploy binary's runfiles.
    """
    plugin_runfiles = []
    for info in sign_config_infos:
        if info == None or info.config_file == None:
            continue
        basename = sha256(info.config_file.path)
        root_symlinks[SIGN_SETTINGS_PREFIX + basename] = info.config_file
        plugin_runfiles.append(info.runfiles)
    return plugin_runfiles
