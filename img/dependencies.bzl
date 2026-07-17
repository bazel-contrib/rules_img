"""Declare runtime dependencies

These are needed for local dev, and users must install them as well.
See https://docs.bazel.build/versions/main/skylark/deploying.html#dependencies
"""

load("@bazel_tools//tools/build_defs/repo:http.bzl", _http_archive = "http_archive")
load("@bazel_tools//tools/build_defs/repo:utils.bzl", "maybe")

def http_archive(**kwargs):
    maybe(_http_archive, **kwargs)

def rules_img_dependencies():
    """Fetches external repositories required by rules_img.

    Call this in your WORKSPACE file after loading rules_img.
    In bzlmod, these dependencies are managed automatically via MODULE.bazel.
    """
    http_archive(
        name = "bazel_skylib",
        sha256 = "bc283cdfcd526a52c3201279cda4bc298652efa898b10b4db0837dc51652756f",
        urls = [
            "https://github.com/bazelbuild/bazel-skylib/releases/download/1.7.1/bazel-skylib-1.7.1.tar.gz",
            "https://mirror.bazel.build/github.com/bazelbuild/bazel-skylib/releases/download/1.7.1/bazel-skylib-1.7.1.tar.gz",
        ],
    )

    http_archive(
        name = "platforms",
        urls = [
            "https://mirror.bazel.build/github.com/bazelbuild/platforms/releases/download/0.0.11/platforms-0.0.11.tar.gz",
            "https://github.com/bazelbuild/platforms/releases/download/0.0.11/platforms-0.0.11.tar.gz",
        ],
        sha256 = "29742e87275809b5e598dc2f04d86960cc7a55b3067d97221c9abbc9926bff0f",
    )

    http_archive(
        name = "package_metadata",
        sha256 = "5bd0cc7594ea528fd28f98d82457f157827d48cc20e07bcfdbb56072f35c8f67",
        strip_prefix = "supply-chain-0.0.6/metadata",
        url = "https://github.com/bazel-contrib/supply-chain/releases/download/v0.0.6/supply-chain-v0.0.6.tar.gz",
    )

    http_archive(
        name = "bazel_features",
        sha256 = "5f7f87f50584df12bbfe5e461d358b16a8e15d245a7e596011bf39aaee5f58a9",
        strip_prefix = "bazel_features-1.47.0",
        url = "https://github.com/bazel-contrib/bazel_features/releases/download/v1.47.0/bazel_features-v1.47.0.tar.gz",
    )

    http_archive(
        name = "rules_runfiles_group",
        sha256 = "bc9373ff5dcae2198f25474b8703f17f39926d374bc9c6422024bbcf50560f7b",
        strip_prefix = "rules_runfiles_group-0.0.1-rc.3",
        url = "https://github.com/hermeticbuild/rules_runfiles_group/releases/download/v0.0.1-rc.3/rules_runfiles_group-v0.0.1-rc.3.tar.gz",
    )

    http_archive(
        name = "hermetic_launcher",
        sha256 = "eeb520ca97697368a4d459e06f37600550971291171a1257f02a33e17fdd2df9",
        url = "https://github.com/hermeticbuild/hermetic-launcher/releases/download/v0.0.10/hermetic_launcher-v0.0.10.tar.gz",
    )
