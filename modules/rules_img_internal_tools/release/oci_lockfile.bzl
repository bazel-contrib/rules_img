"""Rule producing an OCI-mode prebuilt lockfile from ORAS layer descriptors.

A prerelease publishes the tool binaries as the layers of an ORAS artifact rather
than as release assets, so the lockfile that ships with the module source has to
name blobs in a registry. The digests are only known once the layers are built,
which is what this rule bridges: it reads each layer's descriptor and writes the
lockfile that `//release/lockfile` derives from them.

The lockfile is meant to be pushed as one more layer of the same artifact, so a
consumer that has the artifact reference has everything it needs.
"""

load("@rules_img//img:providers.bzl", "LayersInfo")

def _oci_prebuilt_lockfile_impl(ctx):
    args = ctx.actions.args()
    args.add("--registry", ctx.attr.registry)
    args.add("--repository", ctx.attr.repository)
    if ctx.attr.auth_realm:
        args.add("--auth-realm", ctx.attr.auth_realm)
    if ctx.attr.auth_service:
        args.add("--auth-service", ctx.attr.auth_service)

    descriptors = []
    for (layer, platform) in ctx.attr.layers.items():
        layer_infos = layer[LayersInfo].layers
        if len(layer_infos) != 1:
            fail("{}: expected {} to be a single layer, got {}".format(ctx.label, layer.label, len(layer_infos)))
        descriptor = layer_infos[0].metadata
        descriptors.append(descriptor)
        args.add("--layer", "{}={}".format(platform, descriptor.path))

    lockfile = ctx.actions.declare_file(ctx.attr.name + ".json")
    args.add(lockfile)
    ctx.actions.run(
        outputs = [lockfile],
        inputs = descriptors,
        executable = ctx.executable._generator,
        arguments = [args],
        mnemonic = "OCIPrebuiltLockfile",
        progress_message = "Generating prebuilt lockfile %{output}",
    )
    return [DefaultInfo(files = depset([lockfile]))]

oci_prebuilt_lockfile = rule(
    implementation = _oci_prebuilt_lockfile_impl,
    doc = "Writes a prebuilt lockfile whose entries fetch each platform's binary as a blob from a registry.",
    attrs = {
        "layers": attr.label_keyed_string_dict(
            mandatory = True,
            providers = [LayersInfo],
            doc = "Maps each single-file layer holding a platform's binary to that platform's `<goos>_<goarch>`.",
        ),
        "repository": attr.string(
            mandatory = True,
            doc = "Repository the layers are pushed to, e.g. `bazel-contrib/rules_img/img`.",
        ),
        "registry": attr.string(
            mandatory = True,
            doc = "Registry the layers are pushed to, e.g. `ghcr.io`. Stated rather than derived from the push settings: it is recorded into a published artifact, so it should not follow a command line flag.",
        ),
        "auth_realm": attr.string(
            doc = "Token endpoint of the registry, as advertised by its `WWW-Authenticate` challenge. Recording it lets a consumer skip probing `GET /v2/`.",
        ),
        "auth_service": attr.string(
            doc = "Service name the registry's token endpoint expects.",
        ),
        "_generator": attr.label(
            executable = True,
            default = Label("//release/lockfile"),
            cfg = "exec",
        ),
    },
)
