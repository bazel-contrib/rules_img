"""Public API for writing the bytes of disk images and layers, like dd."""

load("//img/private:dd.bzl", _image_dd = "image_dd")

image_dd = _image_dd
