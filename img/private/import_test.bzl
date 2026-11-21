"""Analysis tests for image_import."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("@bazel_skylib//rules:write_file.bzl", "write_file")
load("//img/private:import.bzl", "image_import")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")
load("//img/private/providers:pulled_referrers_info.bzl", "PulledReferrersInfo")

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

_SBOM_REFERRER_DIGEST = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
_SIGNATURE_REFERRER_DIGEST = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
_LOCAL_REFERRER_DIGEST = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
_STRAY_REFERRER_DIGEST = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
_SBOM_LAYER_DIGEST = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
_SIGNATURE_CONFIG_DIGEST = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
_EMPTY_JSON_DIGEST = "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"

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

def _referrer(subject, config, layer, artifact_type = None, annotations = None):
    manifest = dict(
        schemaVersion = 2,
        mediaType = "application/vnd.oci.image.manifest.v1+json",
        config = config,
        layers = [layer],
        subject = dict(
            mediaType = "application/vnd.oci.image.manifest.v1+json",
            digest = subject,
            size = 1,
        ),
    )
    if artifact_type:
        manifest["artifactType"] = artifact_type
    if annotations:
        manifest["annotations"] = annotations
    return json.encode(manifest)

_EMPTY_DESCRIPTOR = dict(
    mediaType = "application/vnd.oci.empty.v1+json",
    digest = _EMPTY_JSON_DIGEST,
    size = 2,
)

_SBOM_LAYER = dict(
    mediaType = "application/spdx+json",
    digest = _SBOM_LAYER_DIGEST,
    size = 1,
)

# Applies to the index itself, with an explicit artifactType.
_SBOM_REFERRER = _referrer(
    _INDEX_DIGEST,
    _EMPTY_DESCRIPTOR,
    _SBOM_LAYER,
    artifact_type = "application/spdx+json",
    annotations = {"org.example.kind": "sbom"},
)

# Applies to a child, and has no artifactType: the config's media type stands in.
_SIGNATURE_REFERRER = _referrer(
    _AMD64_MANIFEST_DIGEST,
    dict(mediaType = "application/vnd.example.signature.config.v1+json", digest = _SIGNATURE_CONFIG_DIGEST, size = 1),
    _EMPTY_DESCRIPTOR,
)

# Built locally from an artifact spec: no upstream source.
_LOCAL_REFERRER = _referrer(
    _ARM64_MANIFEST_DIGEST,
    _EMPTY_DESCRIPTOR,
    _SBOM_LAYER,
    artifact_type = "application/vnd.example.local",
)

# Applies to a manifest outside of the image.
_STRAY_REFERRER = _referrer(
    "sha256:1234123412341234123412341234123412341234123412341234123412341234",
    _EMPTY_DESCRIPTOR,
    _SBOM_LAYER,
    artifact_type = "application/spdx+json",
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

def _copies_source_blobs_test_impl(ctx):
    env = analysistest.begin(ctx)
    target_under_test = analysistest.target_under_test(env)
    manifest_info = target_under_test[ImageManifestInfo]
    copy_dir = "{}_blobs/sha256/".format(target_under_test.label.name)

    # Manifests and configs are small and every layout embeds them, so the
    # providers carry the copy in the output base instead of the source file.
    for (kind, file) in [("manifest", manifest_info.manifest), ("config", manifest_info.config)]:
        asserts.false(env, file.is_source, "{} should be a generated copy".format(kind))
        asserts.true(env, copy_dir in file.short_path, "{} copy is at an unexpected path: {}".format(kind, file.short_path))

    # A layer blob is large, and everything but a tree artifact layout reads it
    # directly, so it keeps pointing at the source file. The copy of it is
    # recorded on the side.
    layer = manifest_info.layers[0]
    asserts.true(env, layer.blob.is_source, "layer blob should stay the source file")
    asserts.false(env, layer.generated_blob.is_source, "generated_blob should be a generated copy")
    asserts.equals(env, _LAYER_DIGEST.removeprefix("sha256:"), layer.generated_blob.basename)

    # Two actions: one for the metadata blobs the providers hand out, one for the
    # layer blobs, which only runs when a layout referencing them is built.
    copy_actions = [
        action.mnemonic
        for action in analysistest.target_actions(env)
        if action.mnemonic.startswith("CopyImage")
    ]
    asserts.equals(env, ["CopyImageLayerBlobs", "CopyImageMetadataBlobs"], sorted(copy_actions))
    return analysistest.end(env)

_copies_source_blobs_test = analysistest.make(_copies_source_blobs_test_impl)

def _keeps_generated_blobs_test_impl(ctx):
    env = analysistest.begin(ctx)
    target_under_test = analysistest.target_under_test(env)

    # Nothing to copy: blobs that already are generated files live in the output
    # base, so a tree artifact may reference them.
    copy_actions = [
        action.mnemonic
        for action in analysistest.target_actions(env)
        if action.mnemonic.startswith("CopyImage")
    ]
    asserts.equals(env, [], copy_actions)
    for manifest in target_under_test[ImageIndexInfo].manifests:
        asserts.false(env, manifest.config.is_source, "config should not be a source file")
    return analysistest.end(env)

_keeps_generated_blobs_test = analysistest.make(_keeps_generated_blobs_test_impl)

def _attributes_referrers_by_subject_test_impl(ctx):
    env = analysistest.begin(ctx)
    target_under_test = analysistest.target_under_test(env)
    info = target_under_test[PulledReferrersInfo]

    asserts.equals(env, _INDEX_DIGEST, info.digest)
    asserts.equals(env, [_AMD64_MANIFEST_DIGEST, _ARM64_MANIFEST_DIGEST, _ATTESTATION_MANIFEST_DIGEST], info.manifest_digests)

    # Only manifests with referrers have an entry.
    asserts.equals(
        env,
        sorted([_INDEX_DIGEST, _AMD64_MANIFEST_DIGEST, _ARM64_MANIFEST_DIGEST]),
        sorted(info.referrers.keys()),
    )

    # A referrer is attributed by its own subject, not by where it was declared.
    asserts.equals(env, [_SBOM_REFERRER_DIGEST], [r.digest for r in info.referrers[_INDEX_DIGEST]])
    asserts.equals(env, [_SIGNATURE_REFERRER_DIGEST], [r.digest for r in info.referrers[_AMD64_MANIFEST_DIGEST]])
    asserts.equals(env, [_LOCAL_REFERRER_DIGEST], [r.digest for r in info.referrers[_ARM64_MANIFEST_DIGEST]])

    sbom = info.referrers[_INDEX_DIGEST][0]
    asserts.equals(env, "application/spdx+json", sbom.artifact_type)
    asserts.equals(env, "application/vnd.oci.image.manifest.v1+json", sbom.media_type)
    asserts.equals(env, {"org.example.kind": "sbom"}, sbom.annotations)
    asserts.equals(env, _INDEX_DIGEST, sbom.subject)
    asserts.equals(env, len(_SBOM_REFERRER), sbom.size)

    # The referrer's content: an artifact, not an image.
    manifest = sbom.manifest
    asserts.equals(env, "unknown", manifest.os)
    asserts.equals(env, {}, manifest.structured_config)
    asserts.equals(env, None, manifest.mtree)
    asserts.equals(env, [_SBOM_LAYER_DIGEST], [json.decode(_layer_metadata_content(env, layer))["digest"] for layer in manifest.layers])
    asserts.true(env, manifest.layers[0].blob != None, "referrer layers are always available")
    asserts.equals(env, ["registry.example.com"], [source.registry for source in manifest.layers[0].sources])

    # The descriptor is the referrers API entry.
    descriptor = json.decode(_written_content(env, manifest.descriptor))
    asserts.equals(env, "application/spdx+json", descriptor["artifactType"])
    asserts.equals(env, {"org.example.kind": "sbom"}, descriptor["annotations"])
    asserts.equals(env, _SBOM_REFERRER_DIGEST, descriptor["digest"])

    # Without an artifactType, the config's media type is the artifact type.
    signature = info.referrers[_AMD64_MANIFEST_DIGEST][0]
    asserts.equals(env, "application/vnd.example.signature.config.v1+json", signature.artifact_type)

    # A locally built artifact has nowhere upstream to fetch its blobs from.
    local = info.referrers[_ARM64_MANIFEST_DIGEST][0]
    asserts.equals(env, [], local.manifest.layers[0].sources)
    return analysistest.end(env)

def _written_content(env, file):
    for action in analysistest.target_actions(env):
        if file in action.outputs.to_list():
            return action.content
    fail("no action writes " + file.short_path)

def _layer_metadata_content(env, layer):
    return _written_content(env, layer.metadata)

_attributes_referrers_by_subject_test = analysistest.make(_attributes_referrers_by_subject_test_impl)

def _rejects_stray_referrer_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, "neither " + _INDEX_DIGEST + " nor one of its children")
    return analysistest.end(env)

_rejects_stray_referrer_test = analysistest.make(_rejects_stray_referrer_test_impl, expect_failure = True)

def _no_referrers_test_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[PulledReferrersInfo]

    # Every pulled image provides PulledReferrersInfo, even without referrers.
    asserts.equals(env, {}, info.referrers)
    asserts.equals(env, [], info.manifest_digests)
    return analysistest.end(env)

_no_referrers_test = analysistest.make(_no_referrers_test_impl)

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

    # The blobs of a pulled image are source files of the generated repository
    # (eagerly downloaded layers included). Their content is never read during
    # analysis, so one file stands in for all of them.
    source_digests = [
        _AMD64_MANIFEST_DIGEST,
        _AMD64_CONFIG_DIGEST,
        _LAYER_DIGEST,
    ]
    source_subject = name + "_source_subject"
    image_import(
        name = source_subject,
        digest = _AMD64_MANIFEST_DIGEST,
        data = {digest: _BLOBS[digest] for digest in [_AMD64_MANIFEST_DIGEST, _AMD64_CONFIG_DIGEST]},
        files = {digest: ":import_testdata/source_blob.json" for digest in source_digests},
        registries = ["registry.example.com"],
        repository = "example/image",
        tag = "latest",
        tags = ["manual"],
    )

    source_test = name + "_copies_source_blobs_test"
    _copies_source_blobs_test(
        name = source_test,
        size = "small",
        target_under_test = ":" + source_subject,
    )

    generated_test = name + "_keeps_generated_blobs_test"
    _keeps_generated_blobs_test(
        name = generated_test,
        size = "small",
        target_under_test = ":" + subject,
    )

    no_referrers_test = name + "_no_referrers_test"
    _no_referrers_test(
        name = no_referrers_test,
        size = "small",
        target_under_test = ":" + source_subject,
    )

    referrer_data = dict(_BLOBS)
    referrer_data.update({
        _SBOM_REFERRER_DIGEST: _SBOM_REFERRER,
        _SIGNATURE_REFERRER_DIGEST: _SIGNATURE_REFERRER,
        _LOCAL_REFERRER_DIGEST: _LOCAL_REFERRER,
        _STRAY_REFERRER_DIGEST: _STRAY_REFERRER,
    })
    referrer_files = dict(blob_files)
    for digest in [_SBOM_REFERRER_DIGEST, _SIGNATURE_REFERRER_DIGEST, _LOCAL_REFERRER_DIGEST, _STRAY_REFERRER_DIGEST, _SBOM_LAYER_DIGEST, _SIGNATURE_CONFIG_DIGEST, _EMPTY_JSON_DIGEST]:
        referrer_files[digest] = ":import_testdata/source_blob.json"

    referrers_subject = name + "_referrers_subject"
    image_import(
        name = referrers_subject,
        digest = _INDEX_DIGEST,
        data = referrer_data,
        files = referrer_files,
        referrers = [_SBOM_REFERRER_DIGEST, _SIGNATURE_REFERRER_DIGEST],
        local_referrers = [_LOCAL_REFERRER_DIGEST],
        registries = ["registry.example.com"],
        repository = "example/image",
        tag = "latest",
        tags = ["manual"],
    )

    referrers_test = name + "_attributes_referrers_by_subject_test"
    _attributes_referrers_by_subject_test(
        name = referrers_test,
        size = "small",
        target_under_test = ":" + referrers_subject,
    )

    stray_subject = name + "_stray_referrer_subject"
    image_import(
        name = stray_subject,
        digest = _INDEX_DIGEST,
        data = referrer_data,
        files = referrer_files,
        referrers = [_STRAY_REFERRER_DIGEST],
        registries = ["registry.example.com"],
        repository = "example/image",
        tag = "latest",
        tags = ["manual"],
    )

    stray_test = name + "_rejects_stray_referrer_test"
    _rejects_stray_referrer_test(
        name = stray_test,
        size = "small",
        target_under_test = ":" + stray_subject,
    )

    native.test_suite(
        name = name,
        tests = [":" + test, ":" + filtered_test, ":" + source_test, ":" + generated_test, ":" + no_referrers_test, ":" + referrers_test, ":" + stray_test],
    )
