"""Common build utilities for container image rules."""

TOOLCHAIN = str(Label("//img:toolchain_type"))
DATA_TOOLCHAIN = str(Label("//img:data_toolchain_type"))
TOOLCHAINS = [TOOLCHAIN]
