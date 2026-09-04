"""Sparse OCI layout builder for rules that produce ImageManifestInfo outside rules_img."""

load(
    "//img/private/common:sparse_oci_layout.bzl",
    _build_sparse_oci_layout_for_manifest = "build_sparse_oci_layout_for_manifest",
)

build_sparse_oci_layout_for_manifest = _build_sparse_oci_layout_for_manifest
