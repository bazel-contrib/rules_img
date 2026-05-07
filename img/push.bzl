"""Public API for container image push rules."""

load("//img/private:push.bzl", _image_push = "image_push")
load("//img/private:push_spec.bzl", _image_push_spec = "image_push_spec")

image_push = _image_push
image_push_spec = _image_push_spec
