"""Public API for container oras rules."""

load("//img/private:oras_file_layer.bzl", _oras_file_layer = "oras_file_layer")
load("//img/private:oras_layer.bzl", _oras_layer = "oras_layer")

oras_file_layer = _oras_file_layer
oras_layer = _oras_layer
