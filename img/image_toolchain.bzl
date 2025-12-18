"""Rules to define and register the img toolchain."""

load("//img/private:image_toolchain.bzl", _DATA_TOOLCHAIN_TYPE = "DATA_TOOLCHAIN_TYPE", _TOOLCHAIN_TYPE = "TOOLCHAIN_TYPE", _image_toolchain = "image_toolchain")

image_toolchain = _image_toolchain
DATA_TOOLCHAIN_TYPE = _DATA_TOOLCHAIN_TYPE
TOOLCHAIN_TYPE = _TOOLCHAIN_TYPE
