"""Rules to convert OCI layout directories and container image archives to container images.

Use `image_manifest_from_oci_layout` to convert an OCI layout directory,
OCI layout tar archive, or `docker save` tar archive to a single-platform
container image manifest (optionally flattening all layers into a single layer),
and `image_index_from_oci_layout` to convert an OCI layout directory
to a multi-platform container image index.
"""

load("//img/private/conversion:index_from_oci_layout.bzl", _image_index_from_oci_layout = "image_index_from_oci_layout")
load("//img/private/conversion:manifest_from_oci_layout.bzl", _image_manifest_from_oci_layout = "image_manifest_from_oci_layout")

image_manifest_from_oci_layout = _image_manifest_from_oci_layout
image_index_from_oci_layout = _image_index_from_oci_layout
