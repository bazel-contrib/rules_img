"""Analysis tests for the overridable `history` attribute on `image_layer`."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("@bazel_skylib//rules:write_file.bzl", "write_file")
load("//img/private:layer.bzl", "image_layer")

def _history_test_impl(ctx):
    env = analysistest.begin(ctx)
    actions = [a for a in analysistest.target_actions(env) if a.mnemonic == "LayerTar"]
    asserts.true(env, len(actions) == 1, "expected a LayerTar action")
    got = actions[0].argv[actions[0].argv.index("--history") + 1]
    if ctx.attr.expect_default:
        name = ctx.attr.target_under_test.label.name
        asserts.true(
            env,
            got.startswith("bazel build ") and got.endswith(":" + name),
            "expected the default 'bazel build <label>' history entry, got %r" % got,
        )
    else:
        asserts.equals(env, ctx.attr.expected, got)
    return analysistest.end(env)

_history_test = analysistest.make(
    _history_test_impl,
    attrs = {
        "expected": attr.string(),
        "expect_default": attr.bool(),
    },
)

def layer_history_test_suite(name):
    """Declare analysis tests for the `history` attribute on `image_layer`.

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

    default_subject = name + "_default_subject"
    image_layer(
        name = default_subject,
        srcs = {"/app/hello.txt": ":" + content},
        tags = ["manual"],
    )

    empty_subject = name + "_empty_subject"
    image_layer(
        name = empty_subject,
        srcs = {"/app/hello.txt": ":" + content},
        history = [],
        tags = ["manual"],
    )

    explicit_subject = name + "_explicit_subject"
    image_layer(
        name = explicit_subject,
        srcs = {"/app/hello.txt": ":" + content},
        history = ["custom created_by entry"],
        tags = ["manual"],
    )

    _history_test(
        name = name + "_default_test",
        target_under_test = default_subject,
        expect_default = True,
    )
    _history_test(
        name = name + "_empty_test",
        target_under_test = empty_subject,
        expected = "",
    )
    _history_test(
        name = name + "_explicit_test",
        target_under_test = explicit_subject,
        expected = "custom created_by entry",
    )

    native.test_suite(
        name = name,
        tests = [
            name + "_default_test",
            name + "_empty_test",
            name + "_explicit_test",
        ],
    )
