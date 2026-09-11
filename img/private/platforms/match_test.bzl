"""Unit tests for platform matching.

The load-bearing property is the last one: the loading-phase matcher used to generate a pulled
image's routing repository and the analysis-phase matcher used by `select_base` must always make
the same choice. `select_base` accepts an `ImageManifestInfo` base without re-checking it, so if
they diverge, a build silently uses a manifest for the wrong platform.
"""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load(":match.bzl", "match_manifest", "match_platform_index", "no_match_message", "normalized_variant", "platform_vector")

def _platform(os, architecture, variant = ""):
    return {"os": os, "architecture": architecture, "variant": variant}

def _manifest(identifier, platform):
    """Build what `image_import` would report for a manifest with this platform."""
    architecture = platform["architecture"]
    return struct(
        id = identifier,
        os = platform["os"],
        architecture = architecture,
        variant = normalized_variant(architecture, platform.get("variant", "")),
    )

def _variants_test_impl(ctx):
    env = unittest.begin(ctx)

    asserts.equals(env, "v8", normalized_variant("arm64", ""))
    asserts.equals(env, "v8", normalized_variant("arm64", "v8"))
    asserts.equals(env, "v9.1", normalized_variant("arm64", "v9.1"))
    asserts.equals(env, "", normalized_variant("amd64", ""))
    asserts.equals(env, "v7", normalized_variant("arm", "v7"))

    asserts.equals(
        env,
        ["v3", "v2", "v1", ""],
        [entry["variant"] for entry in platform_vector("linux", "amd64", "v3")],
        "amd64 falls back to lower microarchitecture levels and then to no variant",
    )
    asserts.equals(
        env,
        ["v7", "v6", "v5"],
        [entry["variant"] for entry in platform_vector("linux", "arm", "v7")],
        "32-bit arm falls back down to v5, and never to no variant",
    )
    asserts.equals(
        env,
        ["v9.2", "v9.1", "v9", "v8"],
        [entry["variant"] for entry in platform_vector("linux", "arm64", "v9.2")],
        "arm64 v9.x falls back through v9 to v8",
    )

    return unittest.end(env)

_variants_test = unittest.make(_variants_test_impl)

def _match_platform_index_test_impl(ctx):
    env = unittest.begin(ctx)

    # A lone manifest that requires a microarchitecture level must not be selected for a target
    # that does not have it. Selecting it would produce an image the target CPU cannot run.
    only_v3 = [_platform("linux", "amd64", "v3")]
    asserts.equals(env, None, match_platform_index(only_v3, "linux", "amd64", ""))
    asserts.equals(env, None, match_platform_index(only_v3, "linux", "amd64", "v2"))
    asserts.equals(env, 0, match_platform_index(only_v3, "linux", "amd64", "v3"))
    asserts.equals(env, 0, match_platform_index(only_v3, "linux", "amd64", "v4"))

    # Variants declared by the index decide, even between manifests of the same architecture.
    v2_v3 = [_platform("linux", "amd64", "v2"), _platform("linux", "amd64", "v3")]
    asserts.equals(env, None, match_platform_index(v2_v3, "linux", "amd64", ""))
    asserts.equals(env, 0, match_platform_index(v2_v3, "linux", "amd64", "v2"))
    asserts.equals(env, 1, match_platform_index(v2_v3, "linux", "amd64", "v3"))
    asserts.equals(env, 1, match_platform_index(v2_v3, "linux", "amd64", "v4"))

    # An attestation manifest is never an image for any platform.
    asserts.equals(env, None, match_platform_index([_platform("unknown", "unknown")], "linux", "amd64", ""))

    # A descriptor with no platform, whose config could not be read either, is skipped rather
    # than matching everything.
    asserts.equals(env, 1, match_platform_index([None, _platform("linux", "amd64")], "linux", "amd64", ""))
    asserts.equals(env, None, match_platform_index([None], "linux", "amd64", ""))
    asserts.equals(env, None, match_platform_index([], "linux", "amd64", ""))

    # The first of two equally good occurrences wins, so a duplicated platform resolves
    # deterministically to the earlier entry of the index.
    twice = [_platform("linux", "arm64", "v8"), _platform("linux", "arm64", "")]
    asserts.equals(env, 0, match_platform_index(twice, "linux", "arm64", ""))

    return unittest.end(env)

_match_platform_index_test = unittest.make(_match_platform_index_test_impl)

_PLATFORM_SETS = [
    [_platform("linux", "amd64"), _platform("linux", "arm64")],
    [_platform("linux", "amd64", "v2"), _platform("linux", "amd64", "v3")],
    [_platform("linux", "amd64", "v3")],
    [_platform("linux", "arm", "v6"), _platform("linux", "arm", "v7")],
    [_platform("linux", "arm64", "v8"), _platform("linux", "arm64", "")],
    [_platform("linux", "arm64", "v9"), _platform("linux", "arm64", "v8")],
    [_platform("unknown", "unknown")],
    [_platform("windows", "amd64"), _platform("linux", "amd64"), _platform("darwin", "arm64")],
]

_TARGETS = [
    ("linux", "amd64", ""),
    ("linux", "amd64", "v1"),
    ("linux", "amd64", "v2"),
    ("linux", "amd64", "v3"),
    ("linux", "amd64", "v4"),
    ("linux", "arm64", ""),
    ("linux", "arm64", "v8"),
    ("linux", "arm64", "v8.5"),
    ("linux", "arm64", "v9"),
    ("linux", "arm64", "v9.3"),
    ("linux", "arm", "v5"),
    ("linux", "arm", "v6"),
    ("linux", "arm", "v7"),
    ("linux", "s390x", ""),
    ("windows", "amd64", ""),
    ("darwin", "arm64", "v8"),
]

def _loading_and_analysis_agree_test_impl(ctx):
    env = unittest.begin(ctx)

    for platforms in _PLATFORM_SETS:
        manifests = [_manifest(position, platform) for (position, platform) in enumerate(platforms)]
        for (os, architecture, variant) in _TARGETS:
            where = "{} for {}/{}/{}".format(platforms, os, architecture, variant)
            position = match_platform_index(platforms, os, architecture, variant)
            match = match_manifest(manifests, os, architecture, variant)
            if position == None:
                asserts.equals(env, None, match, "no descriptor matches, so no manifest may: " + where)
                continue
            asserts.true(env, match != None, "a descriptor matches, so a manifest must: " + where)
            if match != None:
                asserts.equals(env, position, match.id, "both phases pick the same manifest: " + where)

    return unittest.end(env)

_loading_and_analysis_agree_test = unittest.make(_loading_and_analysis_agree_test_impl)

def _no_match_message_test_impl(ctx):
    env = unittest.begin(ctx)
    asserts.equals(
        env,
        "no matching base image found for os=linux architecture=amd64",
        no_match_message("linux", "amd64", ""),
    )
    asserts.equals(
        env,
        "no matching base image found for os=linux architecture=arm64 variant=v9",
        no_match_message("linux", "arm64", "v9"),
    )
    return unittest.end(env)

_no_match_message_test = unittest.make(_no_match_message_test_impl)

def match_test_suite(name):
    """Declare the platform matching unit tests.

    Args:
        name: Name of the test suite.
    """
    unittest.suite(
        name,
        _variants_test,
        _match_platform_index_test,
        _loading_and_analysis_agree_test,
        _no_match_message_test,
    )
