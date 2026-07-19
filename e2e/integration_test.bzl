"""`img_integration_tests`: a thin wrapper around `bazel_integration_tests` that
adds a `supports_signing` flag.

When `supports_signing = True`, every generated per-version test carries the
`RULES_IMG_E2E_SIGNING=1` environment variable. The integration test runner
reads it to decide whether to run the extra signed-push + signature-verification
phases (cosign/notation) after the plain `bazel run //:push`.

The upstream plural `bazel_integration_tests` macro does not forward a per-test
`env`, but the singular `bazel_integration_test` does. So we reimplement the
version fan-out over the singular macro, preserving the exact per-version target
naming (`integration_test_utils.bazel_integration_test_name`) so existing
`test_suite`s that enumerate the generated names keep resolving.
"""

load(
    "@rules_bazel_integration_test//bazel_integration_test:defs.bzl",
    "bazel_integration_test",
    "integration_test_utils",
)

def img_integration_tests(
        name,
        test_runner,
        bazel_versions = [],
        workspace_path = None,
        workspace_files = None,
        tags = integration_test_utils.DEFAULT_INTEGRATION_TEST_TAGS,
        timeout = "long",
        additional_env_inherit = [],
        bazel_binaries = None,
        startup_options = "",
        supports_signing = False,
        **kwargs):
    """Defines one `bazel_integration_test` per Bazel version.

    Args:
        name: Base name; each test is `<name>_bazel_<version>`.
        test_runner: Label of the test runner binary.
        bazel_versions: Bazel versions to test against.
        workspace_path: Relative path to the child workspace.
        workspace_files: Optional explicit workspace file list.
        tags: Tags applied to each generated test.
        timeout: Bazel test timeout.
        additional_env_inherit: Extra env var names inherited by the runner.
        bazel_binaries: `bazel_binaries` repo handle (bzlmod).
        startup_options: Startup flags passed via `BIT_STARTUP_OPTIONS`.
        supports_signing: When True, sets `RULES_IMG_E2E_SIGNING=1` so the runner
            exercises the cosign/notation signing + verification phases.
        **kwargs: Forwarded to `bazel_integration_test` (e.g. target_compatible_with).
    """
    if not bazel_versions:
        fail("One or more Bazel versions must be specified.")

    env = {"RULES_IMG_E2E_SIGNING": "1"} if supports_signing else {}

    for bazel_version in bazel_versions:
        bazel_integration_test(
            name = integration_test_utils.bazel_integration_test_name(
                name,
                bazel_version,
            ),
            test_runner = test_runner,
            bazel_version = bazel_version,
            workspace_path = workspace_path,
            workspace_files = workspace_files,
            tags = tags,
            timeout = timeout,
            additional_env_inherit = additional_env_inherit,
            bazel_binaries = bazel_binaries,
            startup_options = startup_options,
            env = env,
            **kwargs
        )
