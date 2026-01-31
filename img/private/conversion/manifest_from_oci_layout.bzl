"""Rule to convert an OCI layout to an image manifest."""

load("//img/private/common:build.bzl", "TOOLCHAIN", "TOOLCHAINS")
load("//img/private/common:media_types.bzl", "GZIP_LAYER", "LAYER_TYPES", "UNCOMPRESSED_LAYER", "ZSTD_LAYER")
load("//img/private/providers:layer_info.bzl", "LayerInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")

_layer_extension = {
    UNCOMPRESSED_LAYER: "tar",
    GZIP_LAYER: "tgz",
    ZSTD_LAYER: "tzst",
}

def _image_manifest_from_oci_layout(ctx):
    src_dir = ctx.file.src
    architecture = ctx.attr.architecture
    os = ctx.attr.os
    variant = ctx.attr.variant
    layer_media_types = ctx.attr.layers

    # ARM64 defaults to v8 variant
    # See: https://github.com/containerd/platforms/blob/2e51fd9435bd985e1753954b24f4b0453f4e4767/platforms.go#L290
    if architecture == "arm64" and variant == "":
        variant = "v8"

    if len(layer_media_types) == 0:
        fail("At least one layer media type must be specified.")

    for media_type in layer_media_types:
        if media_type not in LAYER_TYPES:
            fail("Unsupported layer media type: {}".format(media_type))

    output_manifest = ctx.actions.declare_file("{}_image_manifest.json".format(ctx.attr.name))
    output_config = ctx.actions.declare_file("{}_image_config.json".format(ctx.attr.name))
    output_descriptor = ctx.actions.declare_file("{}_image_descriptor.json".format(ctx.attr.name))
    output_digest = ctx.actions.declare_file("{}_digest".format(ctx.attr.name))
    outputs = [
        output_manifest,
        output_config,
        output_descriptor,
        output_digest,
    ]

    layer_blobs = [
        ctx.actions.declare_file("{}_layer_blob_{}.{extension}".format(ctx.attr.name, i, extension = _layer_extension[layer_media_types[i]]))
        for i in range(len(layer_media_types))
    ]
    metadata_jsons = [
        ctx.actions.declare_file("{}_metadata_{}.json".format(ctx.attr.name, i))
        for i in range(len(layer_media_types))
    ]
    layer_infos = [
        LayerInfo(
            blob = layer_blobs[i],
            estargz = False,
            media_type = layer_media_types[i],
            metadata = metadata_jsons[i],
        )
        for i in range(len(layer_media_types))
    ]
    outputs.extend(layer_blobs)
    outputs.extend(metadata_jsons)

    args = [
        "manifest-from-oci-layout",
        "--src",
        src_dir.path,
        "--manifest",
        output_manifest.path,
        "--config",
        output_config.path,
        "--descriptor",
        output_descriptor.path,
        "--digest",
        output_digest.path,
        "--architecture",
        architecture,
        "--os",
        os,
    ]
    if variant != "":
        args += ["--variant", variant]
    for i in range(len(layer_media_types)):
        args += [
            "--layer_media_type={}={}".format(i, layer_media_types[i]),
            "--layer_blob={}={}".format(i, layer_blobs[i].path),
            "--layer_metadata_json={}={}".format(i, metadata_jsons[i].path),
        ]

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = [src_dir],
        outputs = outputs,
        arguments = args,
        executable = img_toolchain_info.tool_exe,
        mnemonic = "ConvertOCILayoutToImageManifest",
        progress_message = "Converting OCI layout at {} to image manifest {}".format(src_dir.path, output_manifest.path),
    )

    return [
        DefaultInfo(files = depset([output_manifest, output_config])),
        OutputGroupInfo(
            descriptor = depset([output_descriptor]),
            digest = depset([output_digest]),
            oci_layout = depset([src_dir]),
        ),
        ImageManifestInfo(
            descriptor = output_descriptor,
            manifest = output_manifest,
            config = output_config,
            structured_config = {"architecture": architecture, "os": os},
            architecture = architecture,
            os = os,
            variant = variant,
            layers = layer_infos,
            missing_blobs = [],
        ),
    ]

image_manifest_from_oci_layout = rule(
    implementation = _image_manifest_from_oci_layout,
    attrs = {
        "src": attr.label(
            doc = "The directory containing the OCI layout to convert from.",
            mandatory = True,
            allow_single_file = True,
        ),
        "architecture": attr.string(
            doc = "The target architecture for the image manifest.",
            mandatory = True,
            values = [
                "386",
                "amd64",
                "arm",
                "arm64",
                "mips64",
                "ppc64le",
                "riscv64",
            ],
        ),
        "os": attr.string(
            doc = "The target operating system for the image manifest.",
            mandatory = True,
            values = [
                "android",
                "darwin",
                "freebsd",
                "ios",
                "linux",
                "netbsd",
                "openbsd",
                "wasip1",
                "windows",
            ],
        ),
        "layers": attr.string_list(
            doc = "A list of layer media types. Use the well-defined media types in @rules_img//img:media_types.bzl.",
            mandatory = True,
        ),
        "variant": attr.string(
            doc = "The platform variant (e.g., 'v3' for amd64/v3, 'v8' for arm64/v8).",
            default = "",
        ),
    },
    provides = [ImageManifestInfo],
    toolchains = TOOLCHAINS,
)
