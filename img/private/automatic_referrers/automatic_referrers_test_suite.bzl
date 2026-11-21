"""Fixtures for the automatic referrers analysis tests."""

load("@bazel_features//:features.bzl", "bazel_features")
load("@package_metadata//rules:package_metadata.bzl", "package_metadata")
load("//img:image.bzl", "image_index", "image_manifest")
load("//img:layer.bzl", "image_layer")
load("//img:push.bzl", "image_push")
load("//img/private:import.bzl", "image_import")
load(":automatic_referrers_test.bzl", "dropped_children_test", "forwarded_children_test", "index_test", "manifest_test", "none_with_toolchain_test", "none_without_toolchain_test", "pulled_base_test", "push_dropping_test", "push_test")

# Bazel 8 added the package_metadata attribute to every rule (with symbolic
# macros, which mark the release). Bazel 7 only has its predecessor,
# applicable_licenses, which the metadata aspect falls back to. This package
# must load on Bazel 7, which every e2e test does with `bazel query @rules_img//...`.
_PACKAGE_METADATA_ATTR = "package_metadata" if bazel_features.globals.macro != None else "applicable_licenses"

_INDEX = "sha256:" + "0" * 64
_AMD64 = "sha256:" + "1" * 64
_AMD64_CONFIG = "sha256:" + "2" * 64
_LAYER = "sha256:" + "3" * 64
_CHILD_SBOM = "sha256:" + "a" * 64
_INDEX_SBOM = "sha256:" + "b" * 64
_INDEX_ATTESTATION = "sha256:" + "c" * 64
_SIGNATURE = "sha256:" + "d" * 64
_BLOB = "sha256:" + "e" * 64
_COSIGN_ATTESTATION = "sha256:" + "f" * 64
_COSIGN_CONFIG = "sha256:" + "5" * 64
_PROVENANCE_BLOB = "sha256:" + "6" * 64
_BUILDKIT_ATTESTATION = "sha256:" + "7" * 64
_BUILDKIT_CONFIG = "sha256:" + "8" * 64
_BUILDKIT_SBOM_BLOB = "sha256:" + "9" * 64
_EMPTY = "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
_EMPTY_DESCRIPTOR = dict(mediaType = "application/vnd.oci.empty.v1+json", digest = _EMPTY, size = 2)

def _referrer(subject, artifact_type, layer_media_type, annotations = {}):
    return json.encode(dict(
        schemaVersion = 2,
        mediaType = "application/vnd.oci.image.manifest.v1+json",
        artifactType = artifact_type,
        config = _EMPTY_DESCRIPTOR,
        layers = [dict(mediaType = layer_media_type, digest = _BLOB, size = 1)],
        subject = dict(mediaType = "application/vnd.oci.image.manifest.v1+json", digest = subject, size = 1),
        annotations = annotations,
    ))

_BLOBS = {
    _INDEX: json.encode(dict(
        schemaVersion = 2,
        mediaType = "application/vnd.oci.image.index.v1+json",
        manifests = [
            dict(
                mediaType = "application/vnd.oci.image.manifest.v1+json",
                digest = _AMD64,
                size = 1,
                platform = dict(os = "linux", architecture = "amd64"),
            ),
            # BuildKit's attestation manifest of the amd64 manifest.
            dict(
                mediaType = "application/vnd.oci.image.manifest.v1+json",
                digest = _BUILDKIT_ATTESTATION,
                size = 1,
                annotations = {
                    "vnd.docker.reference.digest": _AMD64,
                    "vnd.docker.reference.type": "attestation-manifest",
                },
                platform = dict(os = "unknown", architecture = "unknown"),
            ),
        ],
    )),
    _AMD64: json.encode(dict(
        schemaVersion = 2,
        mediaType = "application/vnd.oci.image.manifest.v1+json",
        config = dict(mediaType = "application/vnd.oci.image.config.v1+json", digest = _AMD64_CONFIG, size = 1),
        layers = [dict(mediaType = "application/vnd.oci.image.layer.v1.tar+gzip", digest = _LAYER, size = 1)],
    )),
    _AMD64_CONFIG: json.encode(dict(architecture = "amd64", os = "linux", rootfs = dict(type = "layers", diff_ids = ["sha256:" + "4" * 64]))),
    _CHILD_SBOM: _referrer(_AMD64, "application/spdx+json", "application/spdx+json"),
    _INDEX_SBOM: _referrer(_INDEX, "application/vnd.cyclonedx+json", "application/vnd.cyclonedx+json"),
    _INDEX_ATTESTATION: _referrer(
        _INDEX,
        "application/vnd.dev.sigstore.bundle.v0.3+json",
        "application/vnd.dev.sigstore.bundle.v0.3+json",
        {"dev.sigstore.bundle.predicateType": "https://cyclonedx.org/bom"},
    ),
    _SIGNATURE: _referrer(_INDEX, "application/vnd.cncf.notary.signature", "application/jose+json"),
    # cosign's tag scheme (sha256-<hex>.att): no subject, a stub config, and a DSSE
    # envelope per attestation whose predicate type the layer's annotations name.
    _COSIGN_ATTESTATION: json.encode(dict(
        schemaVersion = 2,
        mediaType = "application/vnd.oci.image.manifest.v1+json",
        config = dict(mediaType = "application/vnd.oci.image.config.v1+json", digest = _COSIGN_CONFIG, size = 1),
        layers = [
            dict(mediaType = "application/vnd.dsse.envelope.v1+json", digest = _PROVENANCE_BLOB, size = 1, annotations = {"predicateType": "https://slsa.dev/provenance/v0.2"}),
            dict(mediaType = "application/vnd.dsse.envelope.v1+json", digest = _BLOB, size = 1, annotations = {"predicateType": "https://spdx.dev/Document"}),
        ],
    )),
    _COSIGN_CONFIG: json.encode(dict(architecture = "", os = "", rootfs = dict(type = "layers", diff_ids = [_PROVENANCE_BLOB, _BLOB]))),
    # BuildKit's attestation manifest (oci-artifact=false: no subject, the index attributes it),
    # an in-toto statement per attestation whose predicate type the layer's annotations name.
    _BUILDKIT_ATTESTATION: json.encode(dict(
        schemaVersion = 2,
        mediaType = "application/vnd.oci.image.manifest.v1+json",
        config = dict(mediaType = "application/vnd.oci.image.config.v1+json", digest = _BUILDKIT_CONFIG, size = 1),
        layers = [
            dict(mediaType = "application/vnd.in-toto+json", digest = _BUILDKIT_SBOM_BLOB, size = 1, annotations = {"in-toto.io/predicate-type": "https://spdx.dev/Document"}),
            dict(mediaType = "application/vnd.in-toto+json", digest = _PROVENANCE_BLOB, size = 1, annotations = {"in-toto.io/predicate-type": "https://slsa.dev/provenance/v1"}),
        ],
    )),
    _BUILDKIT_CONFIG: json.encode(dict(architecture = "unknown", os = "unknown", rootfs = dict(type = "layers", diff_ids = [_BUILDKIT_SBOM_BLOB, _PROVENANCE_BLOB]))),
}

def automatic_referrers_test_suite(name):
    """Declare the automatic referrers analysis tests.

    Args:
        name: Name of the test suite.
    """
    native.platform(
        name = name + "_linux_amd64",
        constraint_values = ["@platforms//os:linux", "@platforms//cpu:x86_64"],
    )
    native.platform(
        name = name + "_linux_arm64",
        constraint_values = ["@platforms//os:linux", "@platforms//cpu:aarch64"],
    )
    linux_amd64 = ":" + name + "_linux_amd64"

    package_metadata(
        name = name + "_metadata",
        purl = "pkg:generic/example@1.0.0",
        tags = ["manual"],
    )
    native.genrule(
        name = name + "_hello",
        outs = [name + "_hello.txt"],
        cmd = "echo hello > $@",
        tags = ["manual"],
    )
    image_layer(
        name = name + "_layer",
        srcs = {"/hello.txt": ":" + name + "_hello"},
        tags = ["manual"],
        **{_PACKAGE_METADATA_ATTR: [":" + name + "_metadata"]}
    )
    layers = [":" + name + "_layer"]

    image_manifest(name = name + "_image", layers = layers, platform = linux_amd64, tags = ["manual"])
    image_manifest(name = name + "_image_arm64", layers = layers, platform = ":" + name + "_linux_arm64", tags = ["manual"])
    image_manifest(name = name + "_stamped", layers = layers, platform = linux_amd64, stamp = "force", tags = ["manual"])
    image_manifest(name = name + "_opted_out", layers = layers, platform = linux_amd64, automatic_referrers = "disabled", tags = ["manual"])
    image_manifest(
        name = name + "_artifact",
        layers = layers,
        artifact_type = "application/vnd.example.artifact",
        config_media_type = "application/vnd.oci.empty.v1+json",
        platform = linux_amd64,
        tags = ["manual"],
    )
    image_index(name = name + "_index", manifests = [":" + name + "_image", ":" + name + "_image_arm64"], tags = ["manual"])
    image_index(
        name = name + "_opted_out_index",
        automatic_referrers = "disabled",
        manifests = [":" + name + "_image", ":" + name + "_image_arm64"],
        tags = ["manual"],
    )

    files = {digest: ":testdata/blob.json" for digest in [_AMD64, _AMD64_CONFIG, _LAYER, _CHILD_SBOM, _INDEX_SBOM, _INDEX_ATTESTATION, _SIGNATURE, _BLOB, _EMPTY, _COSIGN_ATTESTATION, _COSIGN_CONFIG, _PROVENANCE_BLOB, _BUILDKIT_ATTESTATION, _BUILDKIT_CONFIG, _BUILDKIT_SBOM_BLOB]}
    files[_INDEX] = ":testdata/blob.json"
    image_import(
        name = name + "_pulled",
        digest = _INDEX,
        data = _BLOBS,
        files = files,
        referrers = [_CHILD_SBOM, _INDEX_SBOM, _INDEX_ATTESTATION, _SIGNATURE, _COSIGN_ATTESTATION, _BUILDKIT_ATTESTATION],
        referrer_subjects = {_COSIGN_ATTESTATION: _AMD64, _BUILDKIT_ATTESTATION: _AMD64},
        # As a pull with a platform filter leaves the attestation manifest: not a child.
        omitted_manifests = [_BUILDKIT_ATTESTATION],
        registries = ["registry.example.com"],
        repository = "example/base",
        tags = ["manual"],
    )
    image_manifest(name = name + "_on_pulled", base = ":" + name + "_pulled", layers = layers, platform = linux_amd64, tags = ["manual"])

    image_push(name = name + "_push", image = ":" + name + "_image", registry = "registry.example.com", repository = "example/image", tags = ["manual"])
    image_push(name = name + "_push_index", image = ":" + name + "_index", registry = "registry.example.com", repository = "example/image", tags = ["manual"])
    image_push(
        name = name + "_push_without",
        image = ":" + name + "_image",
        push_automatic_referrers = "disabled",
        registry = "registry.example.com",
        repository = "example/image",
        tags = ["manual"],
    )

    tests = []

    def _test(rule, suffix, target, **kwargs):
        rule(name = name + "_" + suffix, target_under_test = ":" + name + "_" + target, size = "small", **kwargs)
        tests.append(":" + name + "_" + suffix)

    _test(manifest_test, "manifest_test", "image")
    _test(manifest_test, "stamped_manifest_test", "stamped", stamp = ["stable-status.txt", "volatile-status.txt"])
    _test(none_without_toolchain_test, "no_toolchain_test", "image")
    _test(none_with_toolchain_test, "opted_out_test", "opted_out")
    _test(none_with_toolchain_test, "artifact_test", "artifact")
    _test(index_test, "index_test", "index")
    _test(
        pulled_base_test,
        "pulled_base_test",
        "on_pulled",
        base_manifest = _AMD64,
        child_sbom = _CHILD_SBOM,
        index_sbom = _INDEX_SBOM,
        index_attestation = _INDEX_ATTESTATION,
        cosign_attestation = _COSIGN_ATTESTATION,
        buildkit_attestation = _BUILDKIT_ATTESTATION,
    )
    _test(push_test, "push_test", "push", expected_referrers = 2)
    _test(push_test, "push_index_test", "push_index", expected_referrers = 6)
    _test(push_test, "push_disabled_test", "push_without", expected_referrers = 0)
    _test(dropped_children_test, "dropped_children_test", "index")
    _test(forwarded_children_test, "forwarded_children_test", "opted_out_index")
    _test(push_dropping_test, "push_index_dropping_test", "push_index", expected_referrers = 2)

    native.test_suite(name = name, tests = tests)
