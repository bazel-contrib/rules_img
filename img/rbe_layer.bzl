"""Public API for creating Remote Execution API Directory trees.

This module provides the `image_rbe_layer` rule for building directory trees
compatible with the Remote Execution API, instead of creating tar files.
"""

load("//img/private:rbe_layer.bzl", _image_rbe_layer = "image_rbe_layer")

image_rbe_layer = _image_rbe_layer
