"""Analysis tests for download_blobs actions."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//img/private:download_blobs.bzl", "download_blobs")

_TEST_DIGEST = "sha256_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
_TEST_SOURCES = {"example/image": ["registry.example.com"]}

def _download_blob_action(env):
    matches = []
    for action in analysistest.target_actions(env):
        if action.mnemonic == "DownloadBlob":
            matches.append(action)

    asserts.equals(env, 1, len(matches), "expected exactly one DownloadBlob action")
    if len(matches) != 1:
        return None

    return matches[0]

def _credential_helper_cleared_test_impl(ctx):
    env = analysistest.begin(ctx)
    action = _download_blob_action(env)
    if action != None:
        for var in [
            "IMG_CREDENTIAL_HELPER",
            "IMG_CREDENTIAL_HELPER_OCI_REGISTRY",
            "IMG_CREDENTIAL_HELPER_REMOTE_CACHE",
        ]:
            asserts.equals(
                env,
                "",
                action.env.get(var),
            )
    return analysistest.end(env)

_credential_helper_cleared_test = analysistest.make(
    _credential_helper_cleared_test_impl,
    config_settings = {
        # buildifier: disable=canonical-repository
        "@@//img/settings:credential_helper": "tools/test-credential-helper",
        # buildifier: disable=canonical-repository
        "@@//img/settings:credential_helper_oci_registry": "tools/test-credential-helper",
        # buildifier: disable=canonical-repository
        "@@//img/settings:credential_helper_remote_cache": "tools/test-credential-helper",
    },
)

def _path_mapping_test_impl(ctx):
    env = analysistest.begin(ctx)
    action = _download_blob_action(env)
    if action == None:
        return analysistest.end(env)

    outputs = action.outputs.to_list()
    asserts.equals(env, 1, len(outputs), "expected exactly one output")
    if len(outputs) != 1 or "--output" not in action.argv:
        asserts.true(env, False, "expected an --output argument in {}".format(action.argv))
        return analysistest.end(env)

    output = outputs[0]
    output_arg = action.argv[action.argv.index("--output") + 1]
    asserts.true(
        env,
        output_arg.endswith("/" + output.basename),
        "expected --output to point at {}, got {}".format(output.basename, output_arg),
    )
    asserts.true(
        env,
        output_arg != output.path,
        "expected the output path to be mapped, got the unmapped {}".format(output_arg),
    )
    return analysistest.end(env)

_path_mapping_test = analysistest.make(
    _path_mapping_test_impl,
    config_settings = {
        "//command_line_option:experimental_output_paths": "strip",
    },
)

def download_blobs_test_suite(name):
    """Declare download_blobs analysis tests.

    Args:
        name: Name for the test suite.
    """
    subject = name + "_subject"
    download_blobs(
        name = subject,
        digests = [_TEST_DIGEST],
        sources = _TEST_SOURCES,
        tags = ["manual"],
    )

    tests = []

    test = name + "_credential_helper_cleared_test"
    _credential_helper_cleared_test(
        name = test,
        size = "small",
        target_under_test = ":" + subject,
    )
    tests.append(":" + test)

    test = name + "_path_mapping_test"
    _path_mapping_test(
        name = test,
        size = "small",
        target_under_test = ":" + subject,
    )
    tests.append(":" + test)

    native.test_suite(
        name = name,
        tests = tests,
    )
