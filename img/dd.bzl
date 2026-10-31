"""Public API for writing the bytes of disk images and layers, like dd."""

load("//img/private:dd_binary.bzl", _dd_binary = "dd_binary")

dd_binary = _dd_binary
