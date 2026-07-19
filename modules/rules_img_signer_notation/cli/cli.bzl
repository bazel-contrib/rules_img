"""Downloads the official prebuilt notation CLI for each supported platform.

This is independent of the signer *plugin* (see //prebuilt, which resolves the
`sign-oci-artifact` plugin). This extension fetches the upstream
notaryproject/notation release binary — the version is kept in sync with the
notation-core-go library pinned in go.mod — so the integration tests can verify
signatures with the real CLI.

The per-platform binaries are exposed through `@rules_img_signer_notation//notation`
via a `select()`. Driven by `notation_cli.lock.json` (an object with a `version`
and a list of `{os, cpu, integrity}` platform entries).
"""

# notation ships gzip tarballs (Unix) and zip archives (Windows); the binary
# sits at the archive root as `notation` / `notation.exe`.
_URL = "https://github.com/notaryproject/notation/releases/download/{version}/notation_{ver}_{os}_{cpu}.{aext}"

def _notation_cli_download_impl(rctx):
    windows = rctx.attr.os == "windows"
    aext = "zip" if windows else "tar.gz"
    version = rctx.attr.version
    ver = version[1:] if version.startswith("v") else version
    url = _URL.format(
        version = version,
        ver = ver,
        os = rctx.attr.os,
        cpu = rctx.attr.cpu,
        aext = aext,
    )
    rctx.download_and_extract(
        url = url,
        integrity = rctx.attr.integrity,
    )

    # The extracted binary is at the archive root.
    binary = "notation.exe" if windows else "notation"
    rctx.file("BUILD.bazel", """\
load("@bazel_skylib//rules:native_binary.bzl", "native_binary")

native_binary(
    name = "binary",
    src = "{binary}",
    out = "{binary}",
    visibility = ["//visibility:public"],
)
""".format(binary = binary))

_notation_cli_download = repository_rule(
    implementation = _notation_cli_download_impl,
    attrs = {
        "version": attr.string(mandatory = True),
        "integrity": attr.string(mandatory = True),
        "os": attr.string(mandatory = True, values = ["darwin", "linux", "windows"]),
        "cpu": attr.string(mandatory = True, values = ["amd64", "arm64"]),
    },
)

_from_file = tag_class(attrs = {
    "lockfile": attr.label(mandatory = True),
})

def _impl(ctx):
    for mod in ctx.modules:
        for ff in mod.tags.from_file:
            lock = json.decode(ctx.read(ff.lockfile))
            version = lock["version"]
            for platform in lock["platforms"]:
                _notation_cli_download(
                    name = "notation_cli_{}_{}".format(platform["os"], platform["cpu"]),
                    version = version,
                    integrity = platform["integrity"],
                    os = platform["os"],
                    cpu = platform["cpu"],
                )
    return ctx.extension_metadata(reproducible = True)

notation_cli = module_extension(
    implementation = _impl,
    tag_classes = {"from_file": _from_file},
)
