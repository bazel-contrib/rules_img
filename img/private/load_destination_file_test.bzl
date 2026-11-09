"""Analysis tests for `image_load` with a `destination_file`.

With `destination_file`, the expanded load configuration names no registry and
no repository, so every action that builds the loaded image names has to read
the destination file itself: the deploy manifest the launcher runs and the
`docker save` tarball.
"""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("@bazel_skylib//rules:write_file.bzl", "write_file")
load("//img/private:layer.bzl", "image_layer")
load("//img/private:load.bzl", "image_load")
load("//img/private:manifest.bzl", "image_manifest")

def _assert_uses_destination_file(env, action, destination_file):
    asserts.true(
        env,
        "--destination-file" in action.argv,
        "{} action {} does not pass --destination-file".format(action.mnemonic, action.argv),
    )
    asserts.true(
        env,
        destination_file in action.inputs.to_list(),
        "{} action {} does not take the destination file as an input".format(action.mnemonic, action.argv),
    )

def _destination_file_test_impl(ctx):
    env = analysistest.begin(ctx)
    actions = analysistest.target_actions(env)
    destination_file = ctx.file.destination_file

    for mnemonic in ["LoadMetadata", "DockerSave"]:
        matching = [a for a in actions if a.mnemonic == mnemonic]
        asserts.equals(env, 1, len(matching), "expected exactly one {} action".format(mnemonic))
        for action in matching:
            _assert_uses_destination_file(env, action, destination_file)

    return analysistest.end(env)

_destination_file_test = analysistest.make(
    _destination_file_test_impl,
    attrs = {
        "destination_file": attr.label(allow_single_file = True, mandatory = True),
    },
)

def load_destination_file_test_suite(name):
    """Declare analysis tests for `image_load` with a `destination_file`.

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

    destination = name + "_destination"
    write_file(
        name = destination,
        out = destination + ".txt",
        content = ["example.com/acme/app\n"],
        tags = ["manual"],
    )

    layer = name + "_layer"
    image_layer(
        name = layer,
        srcs = {"/app/hello.txt": ":" + content},
        tags = ["manual"],
    )

    image = name + "_image"
    image_manifest(
        name = image,
        layers = [":" + layer],
        tags = ["manual"],
    )

    subject = name + "_subject"
    image_load(
        name = subject,
        destination_file = ":" + destination,
        image = ":" + image,
        tag = "latest",
        tags = ["manual"],
    )

    destination_file_test = name + "_analysis"
    _destination_file_test(
        name = destination_file_test,
        size = "small",
        destination_file = ":" + destination,
        target_under_test = ":" + subject,
    )

    native.test_suite(
        name = name,
        tests = [":" + destination_file_test],
    )
