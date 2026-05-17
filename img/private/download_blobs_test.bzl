"""Analysis tests for download_blobs actions."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//img/private:download_blobs.bzl", "download_blobs")

_TEST_SETTING_DIGEST = "sha256_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
_TEST_ATTR_DIGEST = "sha256_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
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

def _credential_helper_setting_env_test_impl(ctx):
    env = analysistest.begin(ctx)
    action = _download_blob_action(env)
    if action != None:
        asserts.equals(
            env,
            "tools/test-credential-helper",
            action.env.get("IMG_CREDENTIAL_HELPER"),
        )
    return analysistest.end(env)

_credential_helper_setting_env_test = analysistest.make(
    _credential_helper_setting_env_test_impl,
    config_settings = {
        # buildifier: disable=canonical-repository
        "@@//img/settings:credential_helper": "tools/test-credential-helper",
    },
)

def _credential_helper_attr_env_test_impl(ctx):
    env = analysistest.begin(ctx)
    action = _download_blob_action(env)
    if action != None:
        asserts.equals(
            env,
            "/opt/rules-img/credential-helper",
            action.env.get("IMG_CREDENTIAL_HELPER"),
        )
    return analysistest.end(env)

_credential_helper_attr_env_test = analysistest.make(_credential_helper_attr_env_test_impl)

def download_blobs_test_suite(name):
    """Declare download_blobs analysis tests.

    Args:
        name: Name for the test suite.
    """
    setting_subject = name + "_credential_helper_setting_subject"
    download_blobs(
        name = setting_subject,
        digests = [_TEST_SETTING_DIGEST],
        sources = _TEST_SOURCES,
    )

    setting_test = name + "_credential_helper_setting_env_test"
    _credential_helper_setting_env_test(
        name = setting_test,
        size = "small",
        target_under_test = ":" + setting_subject,
    )

    attr_subject = name + "_credential_helper_attr_subject"
    download_blobs(
        name = attr_subject,
        credential_helper = "/opt/rules-img/credential-helper",
        digests = [_TEST_ATTR_DIGEST],
        sources = _TEST_SOURCES,
    )

    attr_test = name + "_credential_helper_attr_env_test"
    _credential_helper_attr_env_test(
        name = attr_test,
        size = "small",
        target_under_test = ":" + attr_subject,
    )

    native.test_suite(
        name = name,
        tests = [
            ":" + setting_test,
            ":" + attr_test,
        ],
    )
