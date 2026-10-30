"""Analysis tests for the compression_level fallback."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("@bazel_skylib//rules:write_file.bzl", "write_file")
load("//img/private:layer.bzl", "image_layer")

# buildifier: disable=canonical-repository
_COMPRESS_SETTING = "@@//img/settings:compress"

# buildifier: disable=canonical-repository
_LEVEL_SETTING = "@@//img/settings:compression_level"

def _flag_value(env, flag):
    for action in analysistest.target_actions(env):
        if action.mnemonic == "LayerTar":
            argv = action.argv
            for i, arg in enumerate(argv):
                if arg == flag:
                    return argv[i + 1]
    return None

def _gzip_level_on_zstd_test_impl(ctx):
    env = analysistest.begin(ctx)

    # The gzip-only level 6 is ignored, leaving fastbuild's default of 1.
    asserts.equals(env, "1", _flag_value(env, "--compression-level"))
    return analysistest.end(env)

_gzip_level_on_zstd_test = analysistest.make(
    _gzip_level_on_zstd_test_impl,
    config_settings = {
        "//command_line_option:compilation_mode": "fastbuild",
        _COMPRESS_SETTING: "zstd",
        _LEVEL_SETTING: "6",
    },
)

def _gzip_level_kept_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.equals(env, "6", _flag_value(env, "--compression-level"))
    return analysistest.end(env)

_gzip_level_kept_test = analysistest.make(
    _gzip_level_kept_test_impl,
    config_settings = {
        "//command_line_option:compilation_mode": "fastbuild",
        _COMPRESS_SETTING: "gzip",
        _LEVEL_SETTING: "6",
    },
)

def compression_tuning_test_suite(name):
    """Declare analysis tests for compression_tuning_args.

    Args:
        name: Name for the test suite.
    """
    content = name + "_content"
    write_file(
        name = content,
        out = content + ".txt",
        content = ["hello\n"],
        tags = ["manual"],
    )

    subject = name + "_subject"
    image_layer(
        name = subject,
        srcs = {"/app/hello.txt": ":" + content},
        tags = ["manual"],
    )

    tests = {
        "gzip_level_on_zstd": _gzip_level_on_zstd_test,
        "gzip_level_kept": _gzip_level_kept_test,
    }
    for test_name, test_rule in tests.items():
        test_rule(
            name = name + "_" + test_name + "_test",
            size = "small",
            target_under_test = ":" + subject,
        )

    native.test_suite(
        name = name,
        tests = [":" + name + "_" + test_name + "_test" for test_name in tests],
    )
