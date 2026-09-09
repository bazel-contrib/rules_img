"""Unit tests for platform spec parsing and matching."""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load("//img/private/platforms:matching.bzl", "format_platform", "matching_spec_indices", "parse_platform_spec", "platform_matches_any")

def _platform(os, architecture, variant = None):
    platform = {"os": os, "architecture": architecture}
    if variant != None:
        platform["variant"] = variant
    return platform

def _matches(specs, os, architecture, variant = None):
    return platform_matches_any(
        _platform(os, architecture, variant),
        [parse_platform_spec(spec) for spec in specs],
    )

def _matching_test_impl(ctx):
    env = unittest.begin(ctx)

    asserts.true(env, _matches(["linux/amd64"], "linux", "amd64"), "an exact match")
    asserts.false(env, _matches(["linux/amd64"], "linux", "arm64"), "a different architecture")
    asserts.false(env, _matches(["linux/amd64"], "windows", "amd64"), "a different os")

    # Docker Hub lists arm64 with variant v8, which normalizes to the empty variant.
    asserts.true(env, _matches(["linux/arm64"], "linux", "arm64", "v8"), "arm64/v8 is plain arm64")
    asserts.true(env, _matches(["linux/arm64/v8"], "linux", "arm64"), "plain arm64 is arm64/v8")
    asserts.true(env, _matches(["linux/amd64"], "linux", "amd64", "v1"), "amd64/v1 is plain amd64")
    asserts.true(env, _matches(["linux/arm"], "linux", "arm", "v7"), "arm defaults to v7")
    asserts.true(env, _matches(["linux/aarch64"], "linux", "arm64"), "aarch64 is an alias of arm64")
    asserts.true(env, _matches(["linux/x86_64"], "linux", "amd64"), "x86_64 is an alias of amd64")
    asserts.true(env, _matches(["LINUX/AMD64"], "linux", "amd64"), "specs are case insensitive")

    # Unlike containerd's Only(), no compatibility vector is applied: a filter must not
    # download platforms the caller did not ask for.
    asserts.false(env, _matches(["linux/amd64"], "linux", "amd64", "v3"), "amd64 is not amd64/v3")
    asserts.false(env, _matches(["linux/amd64"], "linux", "386"), "amd64 does not imply 386")
    asserts.true(env, _matches(["linux/amd64/v3"], "linux", "amd64", "v3"), "an explicit variant matches")

    # Attestation manifests only match when they are requested explicitly.
    asserts.false(env, _matches(["linux/amd64"], "unknown", "unknown"), "attestations are not a platform")
    asserts.true(env, _matches(["unknown/unknown"], "unknown", "unknown"), "attestations can be requested")

    asserts.false(
        env,
        platform_matches_any({}, [parse_platform_spec("linux/amd64")]),
        "an entry without platform information cannot be selected",
    )
    asserts.true(
        env,
        _matches(["windows/amd64", "linux/amd64"], "linux", "amd64"),
        "any of the specs may match",
    )

    return unittest.end(env)

_matching_test = unittest.make(_matching_test_impl)

def _matching_spec_indices_test_impl(ctx):
    env = unittest.begin(ctx)

    specs = [parse_platform_spec(spec) for spec in ["linux/arm64", "linux/arm64/v8", "linux/amd64"]]
    asserts.equals(
        env,
        [0, 1],
        matching_spec_indices(_platform("linux", "arm64", "v8"), specs),
        "every spec an entry matches is reported, so an unused spec can be detected",
    )
    asserts.equals(env, [], matching_spec_indices(_platform("linux", "s390x"), specs), "no match")

    return unittest.end(env)

_matching_spec_indices_test = unittest.make(_matching_spec_indices_test_impl)

def _format_platform_test_impl(ctx):
    env = unittest.begin(ctx)

    asserts.equals(env, "linux/amd64", format_platform(_platform("linux", "amd64")))
    asserts.equals(env, "linux/arm64/v8", format_platform(_platform("linux", "arm64", "v8")))
    asserts.equals(env, "<no platform>", format_platform({}))

    return unittest.end(env)

_format_platform_test = unittest.make(_format_platform_test_impl)

def matching_test_suite(name):
    """Declare the platform matching unit tests.

    Args:
        name: Name for the test suite.
    """
    unittest.suite(
        name,
        _matching_test,
        _matching_spec_indices_test,
        _format_platform_test,
    )
