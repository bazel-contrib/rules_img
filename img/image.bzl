"""Rules to build container images from layers.

Use `image_from_binary` to package any `*_binary` target into a container image,
`image_manifest` to create a single-platform container image from layers,
and `image_index` to compose a multi-platform container image index.
"""

load("//img/private:image_from_binary.bzl", _image_from_binary = "image_from_binary")
load("//img/private:index.bzl", _image_index = "image_index")
load("//img/private:manifest.bzl", _image_manifest = "image_manifest")

image_manifest = _image_manifest
image_index = _image_index
image_from_binary = _image_from_binary
