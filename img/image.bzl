"""Rules to build container images from layers.

Use `image_from_binary` to package any `*_binary` target into a container image,
`image_manifest` to create a single-platform container image from layers,
and `image_index` to compose a multi-platform container image index.

`INHERIT_FROM_BASE` is a sentinel that can be used as (or inside) the `user`,
`working_dir`, `stop_signal`, `entrypoint`, and `cmd` attributes of
`image_manifest` / `image_from_binary` to explicitly inherit that config field
from the base image.
"""

load("//img/private:image_from_binary.bzl", _image_from_binary = "image_from_binary")
load("//img/private:index.bzl", _image_index = "image_index")
load("//img/private:manifest.bzl", _image_manifest = "image_manifest")
load("//img/private:optimize.bzl", _image_optimize = "image_optimize")
load("//img/private/common:inherit.bzl", _INHERIT_FROM_BASE = "INHERIT_FROM_BASE")

image_manifest = _image_manifest
image_index = _image_index
image_from_binary = _image_from_binary
image_optimize = _image_optimize

# Sentinel that explicitly inherits an image config field from the base image.
# Usable as (or inside) the `user`, `working_dir`, `stop_signal`, `entrypoint`,
# and `cmd` attributes of `image_manifest` / `image_from_binary`. See the
# attribute docs and img/private/common/inherit.bzl for the exact semantics.
INHERIT_FROM_BASE = _INHERIT_FROM_BASE
