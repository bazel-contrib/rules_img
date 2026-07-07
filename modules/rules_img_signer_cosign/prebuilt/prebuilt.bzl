"""Resolves the signer plugin binary to a prebuilt download (released versions)
or the source-built go_binary (any other commit, local, or git_override).

This is self-contained: it does not depend on rules_img, so the module can be
built and released independently. The decision is driven by
`prebuilt_lockfile.json`, which is an empty list `[]` in source and is overwritten
with per-platform digests when the release archive is produced.
"""

_URL_DEFAULT = "https://github.com/bazel-contrib/rules_img/releases/download/{tag}/{basename}_{os}_{cpu}{dot}{ext}"

# goos/goarch -> Bazel platform constraints.
_OS_CONSTRAINT = {
    "linux": "@platforms//os:linux",
    "darwin": "@platforms//os:macos",
    "windows": "@platforms//os:windows",
}
_CPU_CONSTRAINT = {
    "amd64": "@platforms//cpu:x86_64",
    "arm64": "@platforms//cpu:aarch64",
}

def _prebuilt_download_impl(rctx):
    ext = "exe" if rctx.attr.os == "windows" else ""
    dot = "." if ext else ""
    urls = [t.format(
        tag = rctx.attr.version,
        basename = rctx.attr.basename,
        os = rctx.attr.os,
        cpu = rctx.attr.cpu,
        dot = dot,
        ext = ext,
    ) for t in rctx.attr.url_templates]
    rctx.download(urls, output = "downloaded", executable = True, integrity = rctx.attr.integrity)

    # Wrap the downloaded file in native_binary so it is a runnable executable
    # target (usable as an attr.label(executable = True) / for bazel run).
    rctx.file("BUILD.bazel", """\
load("@bazel_skylib//rules:native_binary.bzl", "native_binary")

native_binary(
    name = "binary",
    src = "downloaded",
    out = "{basename}{dot}{ext}",
    visibility = ["//visibility:public"],
)
""".format(basename = rctx.attr.basename, dot = dot, ext = ext))

_prebuilt_download = repository_rule(
    implementation = _prebuilt_download_impl,
    attrs = {
        "version": attr.string(mandatory = True),
        "integrity": attr.string(mandatory = True),
        "basename": attr.string(mandatory = True),
        "os": attr.string(values = ["darwin", "linux", "windows"]),
        "cpu": attr.string(values = ["amd64", "arm64"]),
        "url_templates": attr.string_list(default = [_URL_DEFAULT]),
    },
)

def _resolved_hub_impl(rctx):
    # select_map maps "os_arch" -> download repo name (JSON; empty when no prebuilt).
    select_map = json.decode(rctx.attr.select_map)
    if not select_map:
        # No prebuilt for this version: alias straight to the source go_binary.
        rctx.file("BUILD.bazel", """\
alias(
    name = "binary",
    actual = "{source}",
    visibility = ["//visibility:public"],
)
""".format(source = rctx.attr.source))
        return

    config_settings = []
    arms = {}
    for platform_key, repo in select_map.items():
        os, _, cpu = platform_key.partition("_")
        config_settings.append("""\
config_setting(
    name = "is_{platform}",
    constraint_values = [
        "{os_constraint}",
        "{cpu_constraint}",
    ],
)
""".format(
            platform = platform_key,
            os_constraint = _OS_CONSTRAINT[os],
            cpu_constraint = _CPU_CONSTRAINT[cpu],
        ))
        arms[":is_{}".format(platform_key)] = "@{}//:binary".format(repo)
    arms["//conditions:default"] = rctx.attr.source

    rctx.file("BUILD.bazel", """\
{config_settings}
alias(
    name = "binary",
    actual = select({arms}),
    visibility = ["//visibility:public"],
)
""".format(
        config_settings = "\n".join(config_settings),
        arms = json.encode_indent(arms, prefix = "        ", indent = "    "),
    ))

_resolved_hub = repository_rule(
    implementation = _resolved_hub_impl,
    attrs = {
        "select_map": attr.string(mandatory = True),
        "source": attr.string(mandatory = True),
    },
)

_from_file = tag_class(attrs = {
    "lockfile": attr.label(mandatory = True),
    "basename": attr.string(mandatory = True, doc = "Release asset basename, e.g. 'notation'."),
    "url_templates": attr.string_list(default = [_URL_DEFAULT]),
})
_source = tag_class(attrs = {"target": attr.label(mandatory = True)})

def _impl(ctx):
    source = None
    lockfile_entries = []
    basename = None
    url_templates = [_URL_DEFAULT]
    hub_name = None

    for mod in ctx.modules:
        for src in mod.tags.source:
            source = str(src.target)
        for ff in mod.tags.from_file:
            hub_name = mod.name + "_resolved"
            basename = ff.basename
            url_templates = ff.url_templates
            lockfile_entries = json.decode(ctx.read(ff.lockfile))

    if source == None or hub_name == None:
        fail("prebuilt_signer requires both a source() and a from_file() tag")

    select_map = {}
    for item in lockfile_entries:
        os = item["os"]
        cpu = item["cpu"]
        if os not in _OS_CONSTRAINT or cpu not in _CPU_CONSTRAINT:
            continue
        repo = "{}_prebuilt_{}_{}".format(hub_name, os, cpu)
        _prebuilt_download(
            name = repo,
            version = item["version"],
            integrity = item["integrity"],
            basename = basename,
            os = os,
            cpu = cpu,
            url_templates = url_templates,
        )
        select_map["{}_{}".format(os, cpu)] = repo

    _resolved_hub(
        name = hub_name,
        select_map = json.encode(select_map),
        source = source,
    )

    return ctx.extension_metadata(reproducible = True)

prebuilt_signer = module_extension(
    implementation = _impl,
    tag_classes = {
        "from_file": _from_file,
        "source": _source,
    },
)
