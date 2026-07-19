"""Analysis tests for `resolve_load_destination`'s destination_registry fallback.

These live under //img/private (not //tests) so the probe rule's implicit
dependency on //img/settings:destination_registry (visible only to
//img/private:__subpackages__) resolves, and so the package is naturally
excluded from the release source tarball (which lists packages explicitly).
"""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private/common:deploy_helpers.bzl", "resolve_load_destination")

_ProbeInfo = provider(
    doc = "Captures the resolved (registry, repository) for testing.",
    fields = ["registry", "repository"],
)

def _probe_impl(ctx):
    registry, repository = resolve_load_destination(ctx)
    return [_ProbeInfo(registry = registry, repository = repository)]

_load_destination_probe = rule(
    implementation = _probe_impl,
    attrs = {
        "registry": attr.string(),
        "repository": attr.string(),
        "_destination_registry": attr.label(
            default = Label("//img/settings:destination_registry"),
            providers = [BuildSettingInfo],
        ),
    },
)

_DESTINATION_REGISTRY = str(Label("//img/settings:destination_registry"))

def _assert_resolved(env, want_registry, want_repository):
    info = analysistest.target_under_test(env)[_ProbeInfo]
    asserts.equals(env, want_registry, info.registry)
    asserts.equals(env, want_repository, info.repository)

# An explicit `registry` wins; the global fallback is not consulted.
def _explicit_registry_impl(ctx):
    env = analysistest.begin(ctx)
    _assert_resolved(env, "explicit.example.com", "repo/app")
    return analysistest.end(env)

_explicit_registry_test = analysistest.make(_explicit_registry_impl)

# `repository` set + `registry` empty + destination_registry flag set -> injected.
def _fallback_impl(ctx):
    env = analysistest.begin(ctx)
    _assert_resolved(env, "injected.example.com", "repo/app")
    return analysistest.end(env)

_fallback_test = analysistest.make(
    _fallback_impl,
    config_settings = {_DESTINATION_REGISTRY: "injected.example.com"},
)

# Legacy full-reference form (neither registry nor repository) is never injected,
# even when the destination_registry flag is set.
def _legacy_impl(ctx):
    env = analysistest.begin(ctx)
    _assert_resolved(env, "", "")
    return analysistest.end(env)

_legacy_test = analysistest.make(
    _legacy_impl,
    config_settings = {_DESTINATION_REGISTRY: "injected.example.com"},
)

# XOR guard: registry set, repository empty, no fallback available -> analysis fails.
def _xor_failure_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, "must be set together")
    return analysistest.end(env)

_xor_failure_test = analysistest.make(_xor_failure_impl, expect_failure = True)

def load_destination_test_suite(name):
    """Declares the probe targets, analysis tests, and a wrapping test_suite.

    Args:
      name: Name of the wrapping test_suite.
    """
    _load_destination_probe(
        name = "explicit_probe",
        registry = "explicit.example.com",
        repository = "repo/app",
        tags = ["manual"],
    )
    _explicit_registry_test(name = "explicit_registry_test", target_under_test = ":explicit_probe", size = "small")

    _load_destination_probe(
        name = "fallback_probe",
        repository = "repo/app",
        tags = ["manual"],
    )
    _fallback_test(name = "fallback_test", target_under_test = ":fallback_probe", size = "small")

    _load_destination_probe(
        name = "legacy_probe",
        tags = ["manual"],
    )
    _legacy_test(name = "legacy_test", target_under_test = ":legacy_probe", size = "small")

    _load_destination_probe(
        name = "xor_probe",
        registry = "only.example.com",
        tags = ["manual"],
    )
    _xor_failure_test(name = "xor_failure_test", target_under_test = ":xor_probe", size = "small")

    native.test_suite(
        name = name,
        tests = [
            ":explicit_registry_test",
            ":fallback_test",
            ":legacy_test",
            ":xor_failure_test",
        ],
    )
