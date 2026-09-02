"""Analysis tests for image_import."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("@bazel_skylib//rules:write_file.bzl", "write_file")
load("//img/private:import.bzl", "image_import")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")

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

    native.test_suite(
        name = name,
        tests = [":" + test],
    )
