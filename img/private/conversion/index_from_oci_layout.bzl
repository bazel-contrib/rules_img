"""Rule to convert an OCI layout to an image index."""

load("//img/private/common:build.bzl", "TOOLCHAIN", "TOOLCHAINS")
load("//img/private/common:media_types.bzl", "GZIP_LAYER", "LAYER_TYPES", "UNCOMPRESSED_LAYER", "ZSTD_LAYER")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
load("//img/private/providers:layer_info.bzl", "LayerInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")

_layer_extension = {
    UNCOMPRESSED_LAYER: "tar",
    GZIP_LAYER: "tgz",
    ZSTD_LAYER: "tzst",
}

def _image_index_from_oci_layout(ctx):
    src_dir = ctx.file.src
    layer_media_types = ctx.attr.layers
    manifest_platforms_raw = ctx.attr.manifests

    if len(layer_media_types) == 0:
        fail("At least one layer media type must be specified.")

    for media_type in layer_media_types:
        if media_type not in LAYER_TYPES:
            fail("Unsupported layer media type: {}".format(media_type))

    if len(manifest_platforms_raw) == 0:
        fail("At least one manifest must be specified.")

    # Parse manifest platform specifications from "os/architecture" or "os/architecture/variant" format
    manifest_platforms = {}
    for idx, platform_str in enumerate(manifest_platforms_raw):
        parts = platform_str.split("/")
        if len(parts) < 2 or len(parts) > 3:
            fail("Manifest platform must be in format 'os/architecture' or 'os/architecture/variant', got: {}".format(platform_str))
        os = parts[0]
        arch = parts[1]
        variant = parts[2] if len(parts) == 3 else ""

        # ARM64 defaults to v8 variant
        # See: https://github.com/containerd/platforms/blob/2e51fd9435bd985e1753954b24f4b0453f4e4767/platforms.go#L290
        if arch == "arm64" and variant == "":
            variant = "v8"

        manifest_platforms[str(idx)] = [os, arch, variant]

    output_index = ctx.actions.declare_file("{}_index.json".format(ctx.attr.name))
    output_digest = ctx.actions.declare_file("{}_digest".format(ctx.attr.name))
    outputs = [output_index, output_digest]

    # For each manifest, we need to output:
    # - manifest JSON
    # - config JSON
    # - descriptor JSON
    # - layer blobs
    # - layer metadata JSONs
    manifest_outputs = {}
    all_layer_infos = {}

    for idx_str in sorted(manifest_platforms.keys()):
        idx = int(idx_str)
        platform_spec = manifest_platforms[idx_str]
        os = platform_spec[0]
        arch = platform_spec[1]
        variant = platform_spec[2]

        manifest_file = ctx.actions.declare_file("{}_{}_manifest.json".format(ctx.attr.name, idx))
        config_file = ctx.actions.declare_file("{}_{}_config.json".format(ctx.attr.name, idx))
        descriptor_file = ctx.actions.declare_file("{}_{}_descriptor.json".format(ctx.attr.name, idx))

        outputs.extend([manifest_file, config_file, descriptor_file])

        layer_blobs = [
            ctx.actions.declare_file("{}_{}_layer_blob_{}.{}".format(
                ctx.attr.name,
                idx,
                i,
                _layer_extension[layer_media_types[i]],
            ))
            for i in range(len(layer_media_types))
        ]
        metadata_jsons = [
            ctx.actions.declare_file("{}_{}_metadata_{}.json".format(ctx.attr.name, idx, i))
            for i in range(len(layer_media_types))
        ]

        outputs.extend(layer_blobs)
        outputs.extend(metadata_jsons)

        manifest_outputs[idx_str] = {
            "manifest": manifest_file,
            "config": config_file,
            "descriptor": descriptor_file,
            "layer_blobs": layer_blobs,
            "metadata_jsons": metadata_jsons,
            "os": os,
            "arch": arch,
            "variant": variant,
        }

        layer_infos = [
            LayerInfo(
                blob = layer_blobs[i],
                estargz = False,
                media_type = layer_media_types[i],
                metadata = metadata_jsons[i],
            )
            for i in range(len(layer_media_types))
        ]
        all_layer_infos[idx_str] = layer_infos

    # Build the command arguments
    args = [
        "index-from-oci-layout",
        "--src",
        src_dir.path,
        "--index",
        output_index.path,
        "--digest",
        output_digest.path,
    ]

    for idx_str in sorted(manifest_platforms.keys()):
        info = manifest_outputs[idx_str]
        args += [
            "--manifest={}={}".format(idx_str, info["manifest"].path),
            "--config={}={}".format(idx_str, info["config"].path),
            "--descriptor={}={}".format(idx_str, info["descriptor"].path),
            "--os={}={}".format(idx_str, info["os"]),
            "--architecture={}={}".format(idx_str, info["arch"]),
        ]
        if info["variant"] != "":
            args.append("--variant={}={}".format(idx_str, info["variant"]))

        for i in range(len(layer_media_types)):
            args += [
                "--layer_media_type={},{}={}".format(idx_str, i, layer_media_types[i]),
                "--layer_blob={},{}={}".format(idx_str, i, info["layer_blobs"][i].path),
                "--layer_metadata_json={},{}={}".format(idx_str, i, info["metadata_jsons"][i].path),
            ]

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = [src_dir],
        outputs = outputs,
        arguments = args,
        executable = img_toolchain_info.tool_exe,
        mnemonic = "ConvertOCILayoutToImageIndex",
        progress_message = "Converting OCI layout at {} to image index {}".format(src_dir.path, output_index.path),
    )

    # Build ImageManifestInfo providers for each manifest
    manifest_infos = []
    for idx_str in sorted(manifest_platforms.keys()):
        info = manifest_outputs[idx_str]
        manifest_infos.append(ImageManifestInfo(
            base_image = None,
            descriptor = info["descriptor"],
            manifest = info["manifest"],
            config = info["config"],
            structured_config = {"architecture": info["arch"], "os": info["os"]},
            architecture = info["arch"],
            os = info["os"],
            variant = info["variant"],
            layers = all_layer_infos[idx_str],
            missing_blobs = [],
        ))

    return [
        DefaultInfo(files = depset([output_index])),
        OutputGroupInfo(
            digest = depset([output_digest]),
            oci_layout = depset([src_dir]),
        ),
        ImageIndexInfo(
            index = output_index,
            manifests = manifest_infos,
        ),
    ]

image_index_from_oci_layout = rule(
    implementation = _image_index_from_oci_layout,
    attrs = {
        "src": attr.label(
            doc = "The directory containing the OCI layout to convert from.",
            mandatory = True,
            allow_single_file = True,
        ),
        "layers": attr.string_list(
            doc = "A list of layer media types. This applies to all manifests. Use the well-defined media types in @rules_img//img:media_types.bzl.",
            mandatory = True,
        ),
        "manifests": attr.string_list(
            doc = """An ordered list of platform specifications in 'os/architecture' or 'os/architecture/variant' format.
            Example: ["linux/arm64", "linux/amd64/v3"]""",
            mandatory = True,
        ),
    },
    provides = [ImageIndexInfo],
    toolchains = TOOLCHAINS,
)
