"""Rules to convert OCI layouts and container archives into container images.

Use `image_manifest_from_oci_layout` and `image_index_from_oci_layout` to import
OCI layout directories. Use `image_manifest_from_archive` and
`image_index_from_archive` to import uncompressed Docker save or OCI layout
`.tar` archives when the layer count is known. Use `image_flatten` to squash a
single-platform image or multi-platform index, OCI layout, or `.tar` archive
into one layer per platform without requiring source layer counts at Bazel
analysis time.
"""

load("//img/private/conversion:archive.bzl", _image_index_from_archive = "image_index_from_archive", _image_manifest_from_archive = "image_manifest_from_archive")
load("//img/private/conversion:flatten.bzl", _image_flatten = "image_flatten")
load("//img/private/conversion:index_from_oci_layout.bzl", _image_index_from_oci_layout = "image_index_from_oci_layout")
load("//img/private/conversion:manifest_from_oci_layout.bzl", _image_manifest_from_oci_layout = "image_manifest_from_oci_layout")

image_manifest_from_oci_layout = _image_manifest_from_oci_layout
image_index_from_oci_layout = _image_index_from_oci_layout
image_manifest_from_archive = _image_manifest_from_archive
image_index_from_archive = _image_index_from_archive
image_flatten = _image_flatten
