"""Test fixtures that emit rules_pkg_providers providers directly.

These rules mirror pkg_files, pkg_symlink, pkg_mkdirs and pkg_filegroup from
rules_pkg but emit PackageFilesInfo etc. from rules_pkg_providers, which is
what layer_from_pkg consumes.  Once rules_pkg itself migrates to
rules_pkg_providers, users will be able to pass pkg_files targets directly.
"""

load("@rules_pkg_providers//:providers.bzl", "PackageDirsInfo", "PackageFilegroupInfo", "PackageFilesInfo", "PackageSymlinkInfo")

def _fake_pkg_files_impl(ctx):
    prefix = ctx.attr.prefix.removesuffix("/")
    dest_src_map = {}
    for f in ctx.files.srcs:
        dest = (prefix + "/" + f.basename) if prefix else f.basename
        dest_src_map[dest] = f
    return [
        PackageFilesInfo(dest_src_map = dest_src_map, attributes = {}),
        DefaultInfo(files = depset(ctx.files.srcs)),
    ]

fake_pkg_files = rule(
    doc = "Emit PackageFilesInfo (from rules_pkg_providers) for a set of files.",
    implementation = _fake_pkg_files_impl,
    attrs = {
        "srcs": attr.label_list(allow_files = True),
        "prefix": attr.string(default = ""),
    },
    provides = [PackageFilesInfo],
)

def _fake_pkg_symlink_impl(ctx):
    return [
        PackageSymlinkInfo(
            destination = ctx.attr.link_name,
            target = ctx.attr.target,
            attributes = {},
        ),
    ]

fake_pkg_symlink = rule(
    doc = "Emit PackageSymlinkInfo (from rules_pkg_providers) for a single symlink.",
    implementation = _fake_pkg_symlink_impl,
    attrs = {
        "link_name": attr.string(mandatory = True),
        "target": attr.string(mandatory = True),
    },
    provides = [PackageSymlinkInfo],
)

def _fake_pkg_mkdirs_impl(ctx):
    return [
        PackageDirsInfo(dirs = ctx.attr.dirs, attributes = {}),
    ]

fake_pkg_mkdirs = rule(
    doc = "Emit PackageDirsInfo (from rules_pkg_providers) for a list of directories.",
    implementation = _fake_pkg_mkdirs_impl,
    attrs = {
        "dirs": attr.string_list(mandatory = True),
    },
    provides = [PackageDirsInfo],
)

def _fake_pkg_filegroup_impl(ctx):
    pkg_files = []
    pkg_dirs = []
    pkg_symlinks = []
    for src in ctx.attr.srcs:
        if PackageFilegroupInfo in src:
            fg = src[PackageFilegroupInfo]
            pkg_files.extend(fg.pkg_files)
            pkg_dirs.extend(fg.pkg_dirs)
            pkg_symlinks.extend(fg.pkg_symlinks)
        if PackageFilesInfo in src:
            pkg_files.append((src[PackageFilesInfo], src.label))
        if PackageDirsInfo in src:
            pkg_dirs.append((src[PackageDirsInfo], src.label))
        if PackageSymlinkInfo in src:
            pkg_symlinks.append((src[PackageSymlinkInfo], src.label))
    return [
        PackageFilegroupInfo(
            pkg_files = pkg_files,
            pkg_dirs = pkg_dirs,
            pkg_symlinks = pkg_symlinks,
        ),
        DefaultInfo(),
    ]

fake_pkg_filegroup = rule(
    doc = "Aggregate PackageFilesInfo/PackageSymlinkInfo/PackageDirsInfo into PackageFilegroupInfo.",
    implementation = _fake_pkg_filegroup_impl,
    attrs = {
        "srcs": attr.label_list(
            providers = [
                [PackageFilesInfo],
                [PackageFilegroupInfo],
                [PackageSymlinkInfo],
                [PackageDirsInfo],
            ],
        ),
    },
    provides = [PackageFilegroupInfo],
)
