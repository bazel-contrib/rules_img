"""Tests for the discovery half of the images module extension.

The context objects are stubs: a method the code is not supposed to call is simply absent, so
touching the network where it should not shows up as a failure rather than as a slow build. That
is the only way to test laziness without a registry.
"""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load("//img/private/repository_rules:repo_names.bzl", "BLOB", "LAZY_BLOB", "MANIFEST_BLOB", "blob_label")
load(":images_helpers.bzl", "GRAPH_FACT_PREFIX", "PLATFORM_FACT_PREFIX", "build_image_files_dict", "check_facts_for_manifest", "collect_blobs_to_create", "discover_platforms", "graph_from_facts", "reachable_facts", "sync_oci_ref_graph")

_ROOT = "sha256:" + "a" * 64
_CHILD = "sha256:" + "b" * 64
_OTHER = "sha256:" + "c" * 64
_CONFIG = "sha256:" + "d" * 64
_OTHER_CONFIG = "sha256:" + "e" * 64
_LAYER = "sha256:" + "f" * 64

_MANIFEST_TYPE = "application/vnd.oci.image.manifest.v1+json"

# The first child declares its platform; the second does not, so its platform has to come from
# its config, the way image_import derives it when it imports a whole index.
_AMD64_DESCRIPTOR = {
    "mediaType": _MANIFEST_TYPE,
    "digest": _CHILD,
    "platform": {"os": "linux", "architecture": "amd64"},
    "annotations": {"kept": "yes"},
}
_UNDECLARED_DESCRIPTOR = {"mediaType": _MANIFEST_TYPE, "digest": _OTHER}

_BLOBS = {
    _ROOT: {
        "mediaType": "application/vnd.oci.image.index.v1+json",
        "manifests": [_AMD64_DESCRIPTOR, _UNDECLARED_DESCRIPTOR],
    },
    _CHILD: {
        "mediaType": _MANIFEST_TYPE,
        "config": {"digest": _CONFIG},
        "layers": [{"digest": _LAYER}],
    },
    _OTHER: {
        "mediaType": _MANIFEST_TYPE,
        "config": {"digest": _OTHER_CONFIG},
        "layers": [],
    },
    _OTHER_CONFIG: {"os": "linux", "architecture": "arm64", "variant": "v8"},
}

_GRAPH = {
    _ROOT: {
        "kind": "index",
        "descriptors": [_AMD64_DESCRIPTOR, _UNDECLARED_DESCRIPTOR],
        "manifests": [_CHILD, _OTHER],
    },
    _CHILD: {"kind": "manifest", "config": _CONFIG, "layers": [_LAYER]},
    _OTHER: {"kind": "manifest", "config": _OTHER_CONFIG, "layers": []},
}

# Declared twice, from two repositories. Every download has to offer both, because a blob is
# content addressed and either location serves it.
_IMAGE = struct(
    repository = "one",
    registries = ["a.example"],
    digest = _ROOT,
    layer_handling = "eager",
    sources = {"one": ["a.example"], "two": ["b.example"]},
)

_LAZY_IMAGE = struct(
    repository = "one",
    registries = ["a.example"],
    digest = _CHILD,
    layer_handling = "lazy",
    sources = {"one": ["a.example"]},
)

_MANIFEST_PARENTS = {_CHILD: [_ROOT], _OTHER: [_ROOT]}

def _progress(_message):
    pass

def _download(url, sha256 = None, output = None, **_kwargs):
    if sha256 not in [_ROOT[7:], _CHILD[7:], _OTHER[7:], _OTHER_CONFIG[7:]]:
        # A layer, or the config of a child whose descriptor already declares its platform.
        fail("Unexpected download of {}: {}".format(sha256, url))
    if not [candidate for candidate in url if "/two/" in candidate]:
        fail("Download of {} lost a declared source: {}".format(sha256, url))
    return struct(sha256 = sha256, output = output)

def _read(path):
    for digest, blob in _BLOBS.items():
        if path.endswith(digest[7:]):
            return json.encode(blob)
    fail("Unexpected read: " + path)

def _path(_value):
    return struct(exists = False)

_WARM = struct(report_progress = _progress)
_COLD = struct(report_progress = _progress, download = _download, read = _read, path = _path)

def _known_facts():
    return {GRAPH_FACT_PREFIX + digest: entry for digest, entry in _GRAPH.items()}

def _complete_facts_need_no_io_test_impl(ctx):
    env = unittest.begin(ctx)

    # _WARM has no download, read, path, file or execute method, so any attempt at IO fails.
    for downloader in ["bazel", "img_tool"]:
        asserts.equals(
            env,
            _GRAPH,
            sync_oci_ref_graph(_WARM, {_ROOT: _IMAGE}, _known_facts(), downloader),
            "a complete lockfile needs neither the network nor the pull tool: " + downloader,
        )

    return unittest.end(env)

_complete_facts_need_no_io_test = unittest.make(_complete_facts_need_no_io_test_impl)

def _facts_schema_test_impl(ctx):
    env = unittest.begin(ctx)

    facts = _known_facts()
    asserts.equals(env, _GRAPH[_ROOT], check_facts_for_manifest(facts, _ROOT))

    # Descriptors have to survive the JSON roundtrip through the lockfile whole: the annotations
    # of a child are recorded nowhere else.
    restored = json.decode(json.encode(facts))
    asserts.equals(env, _GRAPH[_ROOT], check_facts_for_manifest(restored, _ROOT))
    asserts.equals(env, {"kept": "yes"}, check_facts_for_manifest(restored, _ROOT)["descriptors"][0]["annotations"])

    # An index entry from before descriptors were recorded cannot route anything.
    asserts.equals(
        env,
        None,
        check_facts_for_manifest({GRAPH_FACT_PREFIX + _ROOT: {"kind": "index", "manifests": [_CHILD]}}, _ROOT),
    )
    asserts.equals(
        env,
        None,
        check_facts_for_manifest({"oci_ref_graph@" + _ROOT: _GRAPH[_ROOT]}, _ROOT),
        "facts of the previous schema are not read",
    )

    return unittest.end(env)

_facts_schema_test = unittest.make(_facts_schema_test_impl)

def _reachable_facts_test_impl(ctx):
    env = unittest.begin(ctx)

    facts = _known_facts()
    asserts.equals(env, _GRAPH, graph_from_facts({_ROOT: _IMAGE}, facts))

    # A root whose children are not all recorded is not usable as-is: discovery has to run.
    asserts.equals(
        env,
        None,
        graph_from_facts({_ROOT: _IMAGE}, {GRAPH_FACT_PREFIX + _ROOT: _GRAPH[_ROOT]}),
    )

    # A newly declared image whose own manifest is unknown still hands the discovery tool the
    # children another index already recorded, so a shared manifest costs no download.
    unknown = "sha256:" + "9" * 64
    new_image = struct(
        repository = "three",
        registries = ["c.example"],
        digest = unknown,
        layer_handling = "shallow",
        sources = {"three": ["c.example"]},
    )
    asserts.equals(
        env,
        sorted(facts.keys()),
        sorted(reachable_facts({unknown: new_image, _ROOT: _IMAGE}, facts).keys()),
    )
    asserts.equals(env, {}, reachable_facts({_ROOT: _IMAGE}, {}))

    return unittest.end(env)

_reachable_facts_test = unittest.make(_reachable_facts_test_impl)

def _cold_discovery_test_impl(ctx):
    env = unittest.begin(ctx)

    graph = sync_oci_ref_graph(_COLD, {_ROOT: _IMAGE}, {}, "bazel")
    asserts.equals(env, _GRAPH, graph, "discovery reproduces what the facts describe")

    # Partial facts: a child that is already known is not downloaded again. _read would return
    # a manifest with one layer for it, so an empty layer list can only come from the fact.
    partial = {GRAPH_FACT_PREFIX + _CHILD: {"kind": "manifest", "config": _CONFIG, "layers": []}}
    graph = sync_oci_ref_graph(_COLD, {_ROOT: _IMAGE}, partial, "bazel")
    asserts.equals(env, [], graph[_CHILD]["layers"], "a known child is reused, not rediscovered")
    asserts.equals(env, _GRAPH[_OTHER], graph[_OTHER], "the unknown child still gets discovered")

    return unittest.end(env)

_cold_discovery_test = unittest.make(_cold_discovery_test_impl)

def _discover_platforms_test_impl(ctx):
    env = unittest.begin(ctx)

    # Only the child whose descriptor declares no platform costs a config download; _download
    # fails on any other blob.
    expected = {PLATFORM_FACT_PREFIX + _OTHER: {"os": "linux", "architecture": "arm64", "variant": "v8"}}
    asserts.equals(
        env,
        expected,
        discover_platforms(_COLD, _GRAPH, {_ROOT: _IMAGE}, _MANIFEST_PARENTS, {}, "bazel"),
    )

    # Recorded once, read from the lockfile afterwards.
    asserts.equals(
        env,
        expected,
        discover_platforms(_WARM, _GRAPH, {_ROOT: _IMAGE}, _MANIFEST_PARENTS, expected, "bazel"),
    )

    # A single-platform image has no descriptors, so nothing to fall back for.
    asserts.equals(
        env,
        {},
        discover_platforms(_WARM, _GRAPH, {_CHILD: _LAZY_IMAGE}, _MANIFEST_PARENTS, {}, "bazel"),
    )

    return unittest.end(env)

_discover_platforms_test = unittest.make(_discover_platforms_test_impl)

def _blob_dependencies_test_impl(ctx):
    env = unittest.begin(ctx)

    # A selected child depends on its own blobs, and on nothing of its siblings or its parent.
    child_files = build_image_files_dict(_CHILD, _GRAPH, "eager")
    asserts.equals(env, sorted([_CHILD, _CONFIG, _LAYER]), sorted(child_files.keys()))
    asserts.equals(env, blob_label(_CHILD, MANIFEST_BLOB), child_files[_CHILD])
    asserts.equals(env, blob_label(_LAYER, BLOB), child_files[_LAYER])

    asserts.equals(
        env,
        sorted([_CHILD, _CONFIG]),
        sorted(build_image_files_dict(_CHILD, _GRAPH, "shallow").keys()),
        "a shallow image does not depend on its layers",
    )

    # The whole index depends on every child, so `:original` stays complete.
    asserts.equals(
        env,
        sorted([_ROOT, _CHILD, _CONFIG, _LAYER, _OTHER, _OTHER_CONFIG]),
        sorted(build_image_files_dict(_ROOT, _GRAPH, "eager").keys()),
    )

    # Two images sharing a manifest but disagreeing on layer handling need both kinds of blob
    # repository, and each has to refer to the one it asked for.
    manifests, files, lazy_files = collect_blobs_to_create(_GRAPH, {_ROOT: _IMAGE, _CHILD: _LAZY_IMAGE})
    asserts.equals(env, sorted(_GRAPH.keys()), sorted(manifests.keys()))
    asserts.true(env, _LAYER in files, "the eager parent needs an eagerly fetched layer")
    asserts.true(env, _LAYER in lazy_files, "the lazy parent needs a lazily fetched layer")
    asserts.equals(env, blob_label(_LAYER, LAZY_BLOB), build_image_files_dict(_CHILD, _GRAPH, "lazy")[_LAYER])

    return unittest.end(env)

_blob_dependencies_test = unittest.make(_blob_dependencies_test_impl)

def images_helpers_test_suite(name):
    """Declare the module extension discovery unit tests.

    Args:
        name: Name of the test suite.
    """
    unittest.suite(
        name,
        _complete_facts_need_no_io_test,
        _facts_schema_test,
        _reachable_facts_test,
        _cold_discovery_test,
        _discover_platforms_test,
        _blob_dependencies_test,
    )
