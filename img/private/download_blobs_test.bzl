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

    test = name + "_credential_helper_cleared_test"
    _credential_helper_cleared_test(
        name = test,
        size = "small",
        target_under_test = ":" + subject,
    )

    native.test_suite(
        name = name,
        tests = [":" + test],
    )
