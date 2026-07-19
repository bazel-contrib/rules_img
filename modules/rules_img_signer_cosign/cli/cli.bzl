"""Downloads the official prebuilt cosign CLI for each supported platform.

This is independent of the signer *plugin* (see //prebuilt, which resolves the
`sign-oci-artifact` plugin). This extension fetches the upstream sigstore/cosign
release binary — the version is kept in sync with the sigstore libraries pinned
in go.mod — so the integration tests can verify signatures with the real CLI.

The per-platform binaries are exposed through `@rules_img_signer_cosign//cosign`
via a `select()`. Driven by `cosign_cli.lock.json` (an object with a `version`
and a list of `{os, cpu, integrity}` platform entries).
"""

# cosign publishes raw (un-archived) release binaries.
_URL = "https://github.com/sigstore/cosign/releases/download/{version}/cosign-{os}-{cpu}{ext}"

def _cosign_cli_download_impl(rctx):
    ext = ".exe" if rctx.attr.os == "windows" else ""
    url = _URL.format(
        version = rctx.attr.version,
        os = rctx.attr.os,
        cpu = rctx.attr.cpu,
        ext = ext,
    )
    rctx.download(
        url = url,
        output = "downloaded",
        executable = True,
        integrity = rctx.attr.integrity,
    )

    # Wrap the downloaded file in native_binary so it is a runnable executable
    # target (usable for `bazel run` and as an executable data dependency).
    out = "cosign" + ext
    rctx.file("BUILD.bazel", """\
load("@bazel_skylib//rules:native_binary.bzl", "native_binary")

native_binary(
    name = "binary",
    src = "downloaded",
    out = "{out}",
    visibility = ["//visibility:public"],
)
""".format(out = out))

_cosign_cli_download = repository_rule(
    implementation = _cosign_cli_download_impl,
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
                _cosign_cli_download(
                    name = "cosign_cli_{}_{}".format(platform["os"], platform["cpu"]),
                    version = version,
                    integrity = platform["integrity"],
                    os = platform["os"],
                    cpu = platform["cpu"],
                )
    return ctx.extension_metadata(reproducible = True)

cosign_cli = module_extension(
    implementation = _impl,
    tag_classes = {"from_file": _from_file},
)
