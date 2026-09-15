"""Analysis tests for experimental_compact_layers_materialize_blob."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("@bazel_skylib//rules:write_file.bzl", "write_file")
load("//img/private:layer.bzl", "image_layer")
load("//img/private/providers:layers_info.bzl", "LayersInfo")

# buildifier: disable=canonical-repository
_COMPACT_LAYERS_SETTING = "@@//img/settings:experimental_compact_layers"

# buildifier: disable=canonical-repository
_MATERIALIZE_BLOB_SETTING = "@@//img/settings:experimental_compact_layers_materialize_blob"

def _reconstruct_action(env):
    for action in analysistest.target_actions(env):
        if action.mnemonic == "LayerReconstruct":
            return action
    return None

def _disabled_by_default_test_impl(ctx):
    env = analysistest.begin(ctx)
    layer = analysistest.target_under_test(env)[LayersInfo].layers[0]
    asserts.true(env, layer.blob == None, "blob should stay None when materialize_blob is left at its default")
    asserts.equals(env, None, _reconstruct_action(env))
    return analysistest.end(env)

_disabled_by_default_test = analysistest.make(
    _disabled_by_default_test_impl,
    config_settings = {_COMPACT_LAYERS_SETTING: "enabled"},
)

def _materializes_blob_test_impl(ctx):
    env = analysistest.begin(ctx)
    layer = analysistest.target_under_test(env)[LayersInfo].layers[0]
    asserts.true(env, layer.blob != None, "blob should be populated when materialize_blob is enabled")

    action = _reconstruct_action(env)
    asserts.true(env, action != None, "expected a LayerReconstruct action")
    if action == None:
        return analysistest.end(env)

    outputs = action.outputs.to_list()
    asserts.equals(env, 1, len(outputs))
    asserts.equals(env, layer.blob, outputs[0])

    inputs = [f.path for f in action.inputs.to_list()]
    asserts.true(env, layer.compact_stream.path in inputs, "compact stream should be an action input")
    asserts.true(env, layer.layer_input_files_cas.path in inputs, "CAS directory should be an action input")

    return analysistest.end(env)

_materializes_blob_test = analysistest.make(
    _materializes_blob_test_impl,
    config_settings = {
        _COMPACT_LAYERS_SETTING: "enabled",
        _MATERIALIZE_BLOB_SETTING: "enabled",
    },
)

def tar_layer_materialize_blob_test_suite(name):
    """Declare analysis tests for experimental_compact_layers_materialize_blob.

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

    disabled_test = name + "_disabled_by_default_test"
    _disabled_by_default_test(
        name = disabled_test,
        size = "small",
        target_under_test = ":" + subject,
    )

    materializes_test = name + "_materializes_blob_test"
    _materializes_blob_test(
        name = materializes_test,
        size = "small",
        target_under_test = ":" + subject,
    )

    native.test_suite(
        name = name,
        tests = [
            ":" + disabled_test,
            ":" + materializes_test,
        ],
    )
