"""Public API for container image layer rules."""

load("//img/private:file_metadata.bzl", _file_metadata = "file_metadata")
load("//img/private:layer.bzl", _image_layer = "image_layer")
load("//img/private:layer_from_file.bzl", _layer_from_file = "layer_from_file")
load("//img/private:layer_from_tar.bzl", _layer_from_tar = "layer_from_tar")

image_layer = _image_layer
layer_from_file = _layer_from_file
layer_from_tar = _layer_from_tar
file_metadata = _file_metadata
