"""Analysis tests for push at build time with a `destination_file`.

With `destination_file`, the expanded push configuration names no registry and
no repository, so every build-time push action has to read the destination file
itself, like the deploy manifest does.
"""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("@bazel_skylib//rules:write_file.bzl", "write_file")
load("//img/private:layer.bzl", "image_layer")
load("//img/private:manifest.bzl", "image_manifest")
load("//img/private:push.bzl", "image_push")

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

    # Every blob push (layers and configs) resolves its destination itself.
    blob_pushes = [a for a in actions if a.mnemonic == "PushImage" and a.argv[1:3] == ["push", "blob"]]
    asserts.true(env, len(blob_pushes) > 0, "expected `push blob` actions for push at build time")
    for action in blob_pushes:
        _assert_uses_destination_file(env, action, destination_file)

    # Both the `bazel run` deploy manifest and the build-time manifest push
    # request are computed with the destination file.
    metadata = [a for a in actions if a.mnemonic == "PushMetadata"]
    asserts.equals(env, 2, len(metadata), "expected a deploy manifest and a build-time push request")
    for action in metadata:
        _assert_uses_destination_file(env, action, destination_file)

    # The build-time manifest push takes its request from the PushMetadata
    # action that was emitted for it, so the destination file reaches it too.
    manifest_pushes = [a for a in actions if a.mnemonic == "PushImage" and a.argv[1:3] == ["push", "manifest"]]
    asserts.equals(env, 1, len(manifest_pushes))
    manifest_push_inputs = manifest_pushes[0].inputs.to_list()
    requests = [
        f
        for a in metadata
        for f in a.outputs.to_list()
        if f in manifest_push_inputs
    ]
    asserts.true(
        env,
        len(requests) > 0,
        "the manifest push does not read a request computed by a PushMetadata action",
    )

    return analysistest.end(env)

_destination_file_test = analysistest.make(
    _destination_file_test_impl,
    attrs = {
        "destination_file": attr.label(allow_single_file = True, mandatory = True),
    },
)

def push_at_build_time_test_suite(name):
    """Declare analysis tests for push at build time with a `destination_file`.

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
    image_push(
        name = subject,
        destination_file = ":" + destination,
        image = ":" + image,
        push_at_build_time = "enabled",
        push_at_build_time_content = "blobs_and_manifests",
        tags = ["manual"],
    )

    destination_file_test = name + "_destination_file_test"
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
