"""rule to import OCI images from a local directory."""

load("//img/private/common:transitions.bzl", "reset_platform_transition")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
load("//img/private/providers:layer_info.bzl", "LayerInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")
load("//img/private/providers:pull_info.bzl", "PullInfo")
load(":manifest_media_type.bzl", "get_media_type")

def _digest_to_file(ctx, digest):
    """Get a starlark File object for a digest."""
    if not digest in ctx.attr.files:
        # this is a missing blob
        return None
    label = ctx.attr.files[digest]
    files = label[DefaultInfo].files.to_list()
    if len(files) != 1:
        fail("invalid number of files for digest: {}".format(digest))
    return files[0]

def _write_layer_info(ctx, manifest, config, layer_index, index_position = None):
    """Write layer info to file and return LayerInfo provider."""
    layers = manifest.get("layers", [])
    if layer_index >= len(layers):
        fail("layer index out of range for manifest: {}".format(layer_index))
    layer = layers[layer_index]
    media_type = layer.get("mediaType", "unknown")
    digest = layer.get("digest", "unknown")
    if not digest.startswith("sha256:"):
        fail("invalid digest: {}".format(digest))
    size = layer.get("size", 0)
    if type(size) != type(0):
        fail("invalid size: {}".format(size))

    rootfs = config.get("rootfs", {})
    diff_ids = rootfs.get("diff_ids", [])
    if layer_index >= len(diff_ids):
        fail("layer index out of range for config: {}".format(layer_index))
    diff_id = diff_ids[layer_index]
    if not diff_id.startswith("sha256:"):
        fail("invalid diff_id: {}".format(diff_id))

    if index_position == None:
        name = """{} :: layer[{}]""".format(ctx.label, layer_index)
    else:
        name = """{} :: manifest[{}] < os = {}, architecture = {} > :: layer[{}]""".format(
            ctx.label,
            index_position,
            config.get("os", "unknown"),
            config.get("architecture", "unknown"),
            layer_index,
        )
    metadata = dict(
        name = name,
        diff_id = diff_id,
        mediaType = media_type,
        digest = digest,
        size = size,
        annotations = layer.get("annotations", {}),
    )
    index_position_str = "" if index_position == None else str(index_position) + "_"
    layer_metadata = ctx.actions.declare_file(ctx.attr.name + "_{}{}_layer_metadata.json".format(index_position_str, layer_index))
    ctx.actions.write(layer_metadata, json.encode(metadata))
    return LayerInfo(
        blob = _digest_to_file(ctx, digest),
        metadata = layer_metadata,
        media_type = media_type,
        estargz = layer.get("annotations", {}).get(TOC_JSON_DIGEST_ANNOTATION) != None,
    )

def _write_manifest_descriptor(ctx, digest, manifest, platform, descriptor = None, index_position = None):
    filename_suffix = "_descriptor.json" if index_position == None else "_{}_descriptor.json".format(index_position)
    out = ctx.actions.declare_file(ctx.attr.name + filename_suffix)
    if descriptor == None:
        # we don't have a prebuilt descriptor from an image index.
        # let's build our own.
        descriptor = dict(
            mediaType = get_media_type(manifest),
            size = len(ctx.attr.data[digest]),
            digest = digest,
            platform = platform,
        )
    ctx.actions.write(out, json.encode(descriptor))
    return out

def _build_manifest_info(ctx, digest, descriptor = None, index_position = None, platform = None):
    if not digest in ctx.attr.data:
        fail("missing blob for digest: " + digest)
    manifest = json.decode(ctx.attr.data[digest])
    media_type = get_media_type(manifest)
    if not media_type in [MEDIA_TYPE_MANIFEST, DOCKER_MANIFEST_V2]:
        fail("invalid mediaType in manifest: {}".format(media_type))
    config_digest = manifest.get("config", {}).get("digest", "missing config digest")
    if not config_digest in ctx.attr.data:
        fail("missing blob for config digest: " + config_digest)
    config = json.decode(ctx.attr.data[config_digest])
    if platform == None:
        platform = dict(
            architecture = config.get("architecture", "unknown"),
            os = config.get("os", "unknown"),
        )
    missing_blobs = []
    layers = []
    for (layer_index, layer) in enumerate(manifest.get("layers", [])):
        layer_info = _write_layer_info(ctx, manifest, config, layer_index, index_position)
        if layer_info.blob == None:
            missing_blobs.append(layer["digest"].removeprefix("sha256:"))
        layers.append(layer_info)
    return ImageManifestInfo(
        base_image = None,
        descriptor = _write_manifest_descriptor(ctx, digest, manifest, platform, descriptor, index_position),
        manifest = _digest_to_file(ctx, digest),
        config = _digest_to_file(ctx, config_digest),
        structured_config = config,
        architecture = config.get("architecture", "unknown"),
        os = config.get("os", "unknown"),
        platform = platform,
        layers = layers,
        missing_blobs = missing_blobs,
    )

def _image_import_impl(ctx):
    root_blob = json.decode(ctx.attr.data[ctx.attr.digest])
    media_type = get_media_type(root_blob)
    if not media_type in [MEDIA_TYPE_MANIFEST, DOCKER_MANIFEST_V2, MEDIA_TYPE_INDEX, DOCKER_MANIFEST_LIST_V2]:
        fail("invalid mediaType in root blob: {}".format(media_type))

    providers = [
        DefaultInfo(files = depset([_digest_to_file(ctx, ctx.attr.digest)])),
        PullInfo(
            registries = ctx.attr.registries,
            repository = ctx.attr.repository,
            tag = ctx.attr.tag,
            digest = ctx.attr.digest,
        ),
    ]
    if media_type in [MEDIA_TYPE_MANIFEST, DOCKER_MANIFEST_V2]:
        # this is a single-platform manifest
        providers.append(_build_manifest_info(ctx, ctx.attr.digest))
    elif media_type in [MEDIA_TYPE_INDEX, DOCKER_MANIFEST_LIST_V2]:
        # this is a multi-platform index
        manifests = [
            _build_manifest_info(ctx, manifest["digest"], descriptor = manifest, index_position = position, platform = manifest.get("platform"))
            for (position, manifest) in enumerate(root_blob.get("manifests", []))
        ]
        providers.append(ImageIndexInfo(
            index = _digest_to_file(ctx, ctx.attr.digest),
            manifests = manifests,
        ))
    return providers

image_import = rule(
    implementation = _image_import_impl,
    attrs = {
        "digest": attr.string(),
        "data": attr.string_dict(),
        "files": attr.string_keyed_label_dict(
            allow_files = True,
        ),
        "registries": attr.string_list(
            doc = "List of registry mirrors used to pull the image.",
        ),
        "repository": attr.string(
            doc = "Repository name of the image.",
        ),
        "tag": attr.string(
            doc = "Tag of the image.",
        ),
    },
    cfg = reset_platform_transition,
)

MEDIA_TYPE_INDEX = "application/vnd.oci.image.index.v1+json"
DOCKER_MANIFEST_LIST_V2 = "application/vnd.docker.distribution.manifest.list.v2+json"
MEDIA_TYPE_MANIFEST = "application/vnd.oci.image.manifest.v1+json"
DOCKER_MANIFEST_V2 = "application/vnd.docker.distribution.manifest.v2+json"
MEDIA_TYPE_CONFIG = "application/vnd.oci.image.config.v1+json"
TOC_JSON_DIGEST_ANNOTATION = "containerd.io/snapshot/stargz/toc.digest"
STORE_UNCOMPRESSED_SIZE_ANNOTATION = "io.containers.estargz.uncompressed-size"
