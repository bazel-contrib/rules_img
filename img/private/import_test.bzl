"""Analysis tests for image_import."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("@bazel_skylib//rules:write_file.bzl", "write_file")
load("//img/private:import.bzl", "image_import")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")

# Digests are opaque keys here: nothing verifies that a blob hashes to the digest
# it is registered under during analysis, so these are readable placeholders.
_INDEX_DIGEST = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
_AMD64_MANIFEST_DIGEST = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
_AMD64_CONFIG_DIGEST = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
_ARM64_MANIFEST_DIGEST = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
_ARM64_CONFIG_DIGEST = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
_ATTESTATION_MANIFEST_DIGEST = "sha256:5555555555555555555555555555555555555555555555555555555555555555"
_ATTESTATION_CONFIG_DIGEST = "sha256:6666666666666666666666666666666666666666666666666666666666666666"
_LAYER_DIGEST = "sha256:7777777777777777777777777777777777777777777777777777777777777777"
_DIFF_ID = "sha256:8888888888888888888888888888888888888888888888888888888888888888"
_ATTESTATION_LAYER_DIGEST = "sha256:9999999999999999999999999999999999999999999999999999999999999999"

def _image_config(architecture):
    return json.encode(dict(
        architecture = architecture,
        os = "linux",
        rootfs = dict(type = "layers", diff_ids = [_DIFF_ID]),
    ))

def _image_manifest(config_digest):
    return json.encode(dict(
        schemaVersion = 2,
        mediaType = "application/vnd.oci.image.manifest.v1+json",
        config = dict(
            mediaType = "application/vnd.oci.image.config.v1+json",
            digest = config_digest,
            size = 1,
        ),
        layers = [dict(
            mediaType = "application/vnd.oci.image.layer.v1.tar+gzip",
            digest = _LAYER_DIGEST,
            size = 1,
        )],
    ))

# A buildx attestation manifest: an empty config (no rootfs.diff_ids) with in-toto
# layers. Describing it as an image fails, which is what regressed image_import.
_ATTESTATION_CONFIG = json.encode(dict())

_ATTESTATION_MANIFEST = json.encode(dict(
    schemaVersion = 2,
    mediaType = "application/vnd.oci.image.manifest.v1+json",
    config = dict(
        mediaType = "application/vnd.oci.image.config.v1+json",
        digest = _ATTESTATION_CONFIG_DIGEST,
        size = 1,
    ),
    layers = [dict(
        mediaType = "application/vnd.in-toto+json",
        digest = _ATTESTATION_LAYER_DIGEST,
        size = 1,
    )],
))

_INDEX = json.encode(dict(
    schemaVersion = 2,
    mediaType = "application/vnd.oci.image.index.v1+json",
    manifests = [
        dict(
            mediaType = "application/vnd.oci.image.manifest.v1+json",
            digest = _AMD64_MANIFEST_DIGEST,
            size = 1,
            platform = dict(architecture = "amd64", os = "linux"),
        ),
        # No platform in the descriptor: the platform is read from the config.
        # This must not be mistaken for an attestation.
        dict(
            mediaType = "application/vnd.oci.image.manifest.v1+json",
            digest = _ARM64_MANIFEST_DIGEST,
            size = 1,
        ),
        dict(
            mediaType = "application/vnd.oci.image.manifest.v1+json",
            digest = _ATTESTATION_MANIFEST_DIGEST,
            size = 1,
            platform = dict(architecture = "unknown", os = "unknown"),
            annotations = {"vnd.docker.reference.type": "attestation-manifest"},
        ),
    ],
))

# What the images module extension hands a repository importing one child of an index. Its
# platform declares a microarchitecture level the manifest's own config does not mention, and it
# carries annotations that exist nowhere but in the index.
_VARIANT_DESCRIPTOR = dict(
    mediaType = "application/vnd.oci.image.manifest.v1+json",
    digest = _AMD64_MANIFEST_DIGEST,
    size = 1,
    platform = dict(architecture = "amd64", os = "linux", variant = "v3"),
    annotations = {"org.opencontainers.image.ref.name": "kept"},
)

_BLOBS = {
    _INDEX_DIGEST: _INDEX,
    _AMD64_MANIFEST_DIGEST: _image_manifest(_AMD64_CONFIG_DIGEST),
    _AMD64_CONFIG_DIGEST: _image_config("amd64"),
    _ARM64_MANIFEST_DIGEST: _image_manifest(_ARM64_CONFIG_DIGEST),
    _ARM64_CONFIG_DIGEST: _image_config("arm64"),
    _ATTESTATION_MANIFEST_DIGEST: _ATTESTATION_MANIFEST,
    _ATTESTATION_CONFIG_DIGEST: _ATTESTATION_CONFIG,
}

def _layer_metadata(env):
    """Returns the written layer metadata JSON, keyed by output basename."""
    metadata = {}
    for action in analysistest.target_actions(env):
        for output in action.outputs.to_list():
            if output.basename.endswith("_layer_metadata.json"):
                metadata[output.basename] = json.decode(action.content)
    return metadata

def _imports_attestation_manifests_test_impl(ctx):
    env = analysistest.begin(ctx)
    target_under_test = analysistest.target_under_test(env)
    index_info = target_under_test[ImageIndexInfo]

    # Every child of the index is imported, including the attestation: the index
    # blob is used verbatim, so a deploy has to be able to carry them all.
    platforms = [
        "{}/{}".format(manifest.os, manifest.architecture)
        for manifest in index_info.manifests
    ]
    asserts.equals(env, ["linux/amd64", "linux/arm64", "unknown/unknown"], platforms)

    # The attestation's config declares no rootfs layers, so its layer carries no
    # diff_id -- while a real image layer still does.
    prefix = target_under_test.label.name
    metadata = _layer_metadata(env)
    asserts.equals(
        env,
        _DIFF_ID,
        metadata.get(prefix + "_0_0_layer_metadata.json", {}).get("diff_id"),
    )
    asserts.equals(
        env,
        None,
        metadata.get(prefix + "_2_0_layer_metadata.json", {}).get("diff_id"),
    )
    return analysistest.end(env)

_imports_attestation_manifests_test = analysistest.make(_imports_attestation_manifests_test_impl)

def _descriptors(env):
    """Returns the written descriptor JSON, keyed by output basename."""
    descriptors = {}
    for action in analysistest.target_actions(env):
        for output in action.outputs.to_list():
            if output.basename.endswith("_descriptor.json"):
                descriptors[output.basename] = json.decode(action.content)
    return descriptors

def _imports_child_with_descriptor_test_impl(ctx):
    env = analysistest.begin(ctx)
    target_under_test = analysistest.target_under_test(env)
    manifest_info = target_under_test[ImageManifestInfo]

    # What the index says about a child outranks what can be derived from the child alone. Here
    # the config declares no variant at all, so losing the descriptor would make this manifest
    # look usable on a baseline amd64 target.
    asserts.equals(env, "linux", manifest_info.os)
    asserts.equals(env, "amd64", manifest_info.architecture)
    asserts.equals(env, "v3", manifest_info.variant)

    # The descriptor is written exactly as the index had it. Nothing else records the
    # annotations of a child, or its media type when its manifest omits its own.
    asserts.equals(
        env,
        _VARIANT_DESCRIPTOR,
        _descriptors(env).get(target_under_test.label.name + "_descriptor.json"),
    )
    return analysistest.end(env)

_imports_child_with_descriptor_test = analysistest.make(_imports_child_with_descriptor_test_impl)

def _child_descriptor_without_platform_test_impl(ctx):
    env = analysistest.begin(ctx)
    manifest_info = analysistest.target_under_test(env)[ImageManifestInfo]

    # `platform` is optional in an index descriptor. A child that omits it is still a perfectly
    # usable image, described by its config.
    asserts.equals(env, "linux", manifest_info.os)
    asserts.equals(env, "arm64", manifest_info.architecture)
    asserts.equals(env, "v8", manifest_info.variant, "arm64 defaults to v8")
    return analysistest.end(env)

_child_descriptor_without_platform_test = analysistest.make(_child_descriptor_without_platform_test_impl)

def _skips_omitted_manifests_test_impl(ctx):
    env = analysistest.begin(ctx)
    index_info = analysistest.target_under_test(env)[ImageIndexInfo]

    # A platform-filtered pull downloads only some children, but stores the index blob
    # verbatim. The omitted children are skipped instead of failing on their missing blobs.
    platforms = [
        "{}/{}".format(manifest.os, manifest.architecture)
        for manifest in index_info.manifests
    ]
    asserts.equals(env, ["linux/amd64"], platforms)
    return analysistest.end(env)

_skips_omitted_manifests_test = analysistest.make(_skips_omitted_manifests_test_impl)

def import_test_suite(name):
    """Declare image_import analysis tests.

    Args:
        name: Name for the test suite.
    """
    blob_files = {}
    for digest, content in _BLOBS.items():
        blob = "{}_blob_{}".format(name, digest.removeprefix("sha256:")[:4])
        write_file(
            name = blob,
            out = blob + ".json",
            content = [content],
            tags = ["manual"],
        )
        blob_files[digest] = ":" + blob

    subject = name + "_subject"
    image_import(
        name = subject,
        digest = _INDEX_DIGEST,
        data = _BLOBS,
        files = blob_files,
        registries = ["registry.example.com"],
        repository = "example/image",
        tag = "latest",
        tags = ["manual"],
    )

    test = name + "_imports_attestation_manifests_test"
    _imports_attestation_manifests_test(
        name = test,
        size = "small",
        target_under_test = ":" + subject,
    )

    # Everything an amd64-only pull would have fetched: the index, and the amd64 child.
    omitted = [_ARM64_MANIFEST_DIGEST, _ATTESTATION_MANIFEST_DIGEST]
    filtered_digests = [
        _INDEX_DIGEST,
        _AMD64_MANIFEST_DIGEST,
        _AMD64_CONFIG_DIGEST,
    ]
    filtered_subject = name + "_filtered_subject"
    image_import(
        name = filtered_subject,
        digest = _INDEX_DIGEST,
        data = {digest: _BLOBS[digest] for digest in filtered_digests},
        files = {digest: blob_files[digest] for digest in filtered_digests},
        omitted_manifests = omitted,
        registries = ["registry.example.com"],
        repository = "example/image",
        tag = "latest",
        tags = ["manual"],
    )

    filtered_test = name + "_skips_omitted_manifests_test"
    _skips_omitted_manifests_test(
        name = filtered_test,
        size = "small",
        target_under_test = ":" + filtered_subject,
    )

    # One child manifest of the index, imported on its own, as the images module extension
    # creates it for the platform a build selects.
    child_digests = [_AMD64_MANIFEST_DIGEST, _AMD64_CONFIG_DIGEST]
    child_subject = name + "_child_subject"
    image_import(
        name = child_subject,
        digest = _AMD64_MANIFEST_DIGEST,
        descriptor = json.encode(_VARIANT_DESCRIPTOR),
        data = {digest: _BLOBS[digest] for digest in child_digests},
        files = {digest: blob_files[digest] for digest in child_digests},
        registries = ["registry.example.com"],
        repository = "example/image",
        tag = "latest",
        tags = ["manual"],
    )

    child_test = name + "_imports_child_with_descriptor_test"
    _imports_child_with_descriptor_test(
        name = child_test,
        size = "small",
        target_under_test = ":" + child_subject,
    )

    platformless_digests = [_ARM64_MANIFEST_DIGEST, _ARM64_CONFIG_DIGEST]
    platformless_subject = name + "_platformless_child_subject"
    image_import(
        name = platformless_subject,
        digest = _ARM64_MANIFEST_DIGEST,
        descriptor = json.encode(json.decode(_INDEX)["manifests"][1]),
        data = {digest: _BLOBS[digest] for digest in platformless_digests},
        files = {digest: blob_files[digest] for digest in platformless_digests},
        registries = ["registry.example.com"],
        repository = "example/image",
        tag = "latest",
        tags = ["manual"],
    )

    platformless_test = name + "_child_descriptor_without_platform_test"
    _child_descriptor_without_platform_test(
        name = platformless_test,
        size = "small",
        target_under_test = ":" + platformless_subject,
    )

    native.test_suite(
        name = name,
        tests = [
            ":" + test,
            ":" + filtered_test,
            ":" + child_test,
            ":" + platformless_test,
        ],
    )
