"""Prebuilt img tool management for rules_img.

This module provides functionality for managing prebuilt img binaries across
different platforms, including downloading, selecting, and registering them
as Bazel toolchains.
"""

load("//img/private/platforms:platforms.bzl", "platform_for_goos_and_goarch")

def _prebuilt_toolchain_definition_for_platform(platform_name, tool_target):
    platform = platform_for_goos_and_goarch(platform_name)
    return """
image_toolchain(
    name = "img_{platform_name}",
    tool_exe = "{tool_target}",
)

toolchain(
    name = "img_{platform_name}_toolchain",
    exec_compatible_with = {constraints},
    toolchain = "img_{platform_name}",
    toolchain_type = "@rules_img//img:toolchain_type",
)""".format(
        platform_name = platform_name,
        tool_target = tool_target,
        constraints = json.encode_indent(platform.constraints, prefix = "    ", indent = "    "),
    )

def _prebuilt_collection_hub_repo_impl(rctx):
    toolchain_defs = "\n".join([_prebuilt_toolchain_definition_for_platform(
        platform_name = platform_name,
        tool_target = tool_target,
    ) for (platform_name, tool_target) in rctx.attr.tools.items()])
    rctx.file(
        "BUILD.bazel",
        """\
load("@rules_img//img/private:image_toolchain.bzl", "image_toolchain")
load("@bazel_skylib//:bzl_library.bzl", "bzl_library")

bzl_library(
    name = "defs",
    srcs = ["defs.bzl"],
    visibility = ["//visibility:public"],
    deps = ["@rules_img//img/private/platforms:platforms"],
)

{}
""".format(toolchain_defs),
    )
    rctx.file(
        "defs.bzl",
        """\
load("@rules_img//img/private/platforms:platforms.bzl", "platform_for_repository_os")

TOOLS = {}

def tool_for_repository_os(rctx):
    platform = platform_for_repository_os(rctx)
    key = platform.name
    if key not in TOOLS:
        fail("No prebuilt img tool for platform " + key)
    return Label(TOOLS[key])
""".format(json.encode_indent(rctx.attr.tools, indent = "    ")),
    )

prebuilt_collection_hub_repo = repository_rule(
    implementation = _prebuilt_collection_hub_repo_impl,
    attrs = {
        "tools": attr.string_dict(),
    },
)

def _prebuilt_img_tool_repo_impl(rctx):
    extension = "exe" if rctx.attr.os == "windows" else ""
    dot = "." if len(extension) > 0 else ""
    urls = [template.format(
        version = rctx.attr.version,
        os = rctx.attr.os,
        cpu = rctx.attr.cpu,
        dot = dot,
        extension = extension,
    ) for template in rctx.attr.url_templates]
    rctx.download(
        urls,
        output = "img.exe",
        executable = True,
        integrity = rctx.attr.integrity,
    )
    rctx.file(
        "BUILD.bazel",
        content = """exports_files(["img.exe"])""",
    )

_prebuilt_attrs = {
    "version": attr.string(mandatory = True),
    "integrity": attr.string(mandatory = True),
    "os": attr.string(values = ["darwin", "linux", "windows"]),
    "cpu": attr.string(values = ["amd64", "arm64"]),
    "url_templates": attr.string_list(
        default = ["https://github.com/tweag/rules_img/releases/download/{version}/img_{os}_{cpu}{dot}{extension}"],
    ),
}

prebuilt_img_tool_repo = repository_rule(
    implementation = _prebuilt_img_tool_repo_impl,
    attrs = _prebuilt_attrs,
)

_prebuilt_tool_collection = tag_class(attrs = {"name": attr.string(), "override": attr.bool(default = False)})
_prebuilt_tool_from_file = tag_class(attrs = {"collection": attr.string(), "file": attr.label()})
_prebuilt_tool_download = tag_class(attrs = {"collection": attr.string()} | _prebuilt_attrs)

def _lockfile_to_dict(lockfile, basename):
    requested_tools = {}
    for item in lockfile:
        requested_tools["%s_%s_%s" % (basename, item["os"], item["cpu"])] = item
    return requested_tools

# buildifier: disable=uninitialized
def _prebuilt_img_tool_collection_for_module(ctx, mod):
    requested_tools = {}
    collections = {}
    for collection_meta in mod.tags.collection:
        collections[collection_meta.name] = {"override": collection_meta.override, "tools": {}}
    for from_file in mod.tags.from_file:
        lockfile = json.decode(ctx.read(from_file.file))
        tools_from_lockfile = _lockfile_to_dict(lockfile, from_file.collection)
        requested_tools.update(tools_from_lockfile)
        for tool in tools_from_lockfile.values():
            collections[from_file.collection]["tools"][(tool["os"], tool["cpu"])] = "%s_%s_%s" % (from_file.collection, tool["os"], tool["cpu"])
    for download in mod.tags.download:
        name = "%s_%s_%s" % (download.collection, download.os, download.cpu)
        requested_tools[name] = {member: getattr(download, member) for member in dir(download)}
        collections[download.collection]["tools"][(download.os, download.cpu)] = "%s_%s_%s" % (download.collection, download.os, download.cpu)
    return (requested_tools, collections)

def _prebuilt_img_tool(ctx):
    requested_tools = {}
    collections = {}
    root_module = None
    for mod in ctx.modules:
        if mod.is_root:
            root_module = mod
            continue
        for_module = _prebuilt_img_tool_collection_for_module(ctx, mod)
        for collection_name in for_module[1].keys():
            if collection_name in collections:
                fail("Duplicate definitions for prebuilt_img_tool %s. Only root module is allowed to override." % collection_name)
        requested_tools.update(for_module[0])
        collections.update(for_module[1])
    root_module_direct_deps = []
    if root_module != None:
        for_root_module = _prebuilt_img_tool_collection_for_module(ctx, root_module)
        for collection_name in for_root_module[1].keys():
            if collection_name in collections and not for_root_module[1][collection_name]["override"]:
                fail("Root module is redefining definition for prebuilt_img_tool %s. Set override to True if this is intended." % collection_name)
        requested_tools.update(for_root_module[0])
        collections.update(for_root_module[1])
        root_module_direct_deps = for_root_module[1].keys()

    for item in requested_tools.items():
        prebuilt_img_tool_repo(
            name = item[0],
            **item[1]
        )
    for (collection_name, collection) in collections.items():
        tools = {}
        for ((os, arch), tool_repo_name) in collection["tools"].items():
            tools["%s_%s" % (os, arch)] = "@%s//:img.exe" % tool_repo_name
        prebuilt_collection_hub_repo(
            name = collection_name,
            tools = tools,
        )

    return ctx.extension_metadata(
        root_module_direct_deps = root_module_direct_deps if ctx.root_module_has_non_dev_dependency else [],
        root_module_direct_dev_deps = [] if ctx.root_module_has_non_dev_dependency else root_module_direct_deps,
        reproducible = True,
    )

prebuilt_img_tool = module_extension(
    implementation = _prebuilt_img_tool,
    tag_classes = {
        "collection": _prebuilt_tool_collection,
        "from_file": _prebuilt_tool_from_file,
        "download": _prebuilt_tool_download,
    },
)
