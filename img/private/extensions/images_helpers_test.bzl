"""Facts and dependency tests for the lazy images extension."""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load(":images_helpers.bzl", "build_image_files_dict", "check_facts_for_manifest", "collect_blobs_to_create", "discover_root_platforms", "download_and_parse_manifest", "sync_oci_ref_graph")

_ROOT = "sha256:" + "a" * 64
_CHILD = "sha256:" + "b" * 64
_OTHER = "sha256:" + "c" * 64
_CONFIG = "sha256:" + "d" * 64
_LAYER = "sha256:" + "e" * 64

def _progress(_message):
    pass

def _download(url, sha256, **_kwargs):
    # Configs and manifests are allowed; a request for a layer fails this test.
    if sha256 not in [_ROOT[7:], _CHILD[7:], _OTHER[7:], _CONFIG[7:]]:
        fail("Unexpected blob download: " + repr(url))
    return struct(sha256 = sha256)

def _read(path):
    if path.endswith(_ROOT[7:]):
        return json.encode({"mediaType": "application/vnd.oci.image.index.v1+json", "manifests": [{"digest": _CHILD, "platform": {"os": "linux", "architecture": "amd64"}}, {"digest": _OTHER}]})
    if path.endswith(_CONFIG[7:]):
        return json.encode({"os": "linux", "architecture": "amd64"})
    return json.encode({"mediaType": "application/vnd.oci.image.manifest.v1+json", "config": {"digest": _CONFIG}, "layers": [{"digest": _LAYER}]})

def _path(_value):
    return struct(exists = False)

def _images_helpers_test_impl(ctx):
    env = unittest.begin(ctx)
    descriptor = {"digest": _CHILD, "platform": {"os": "linux", "architecture": "amd64"}}
    index = {"kind": "index", "manifests": [_CHILD], "descriptors": [descriptor]}
    manifest = {"kind": "manifest", "config": _CONFIG, "layers": [_LAYER]}
    graph = {_ROOT: index, _CHILD: manifest}
    facts = {"oci_ref_graph_v2@" + digest: entry for digest, entry in graph.items()}
    img = struct(repository = "library/test", sources = {"library/test": ["invalid.example"]}, layer_handling = "eager")

    # No download/path/execute methods: any IO on a hit immediately fails.
    fake_ctx = struct(report_progress = _progress)
    for downloader in ["bazel", "img_tool"]:
        asserts.equals(env, (manifest, None), download_and_parse_manifest(fake_ctx, _CHILD, img, facts, downloader))
        asserts.equals(env, graph, sync_oci_ref_graph(fake_ctx, {_ROOT: img}, facts, downloader))
    asserts.equals(env, None, check_facts_for_manifest({"oci_ref_graph@" + _ROOT: index}, _ROOT))
    asserts.equals(env, None, check_facts_for_manifest({"oci_ref_graph_v2@" + _ROOT: {"kind": "index", "manifests": [_CHILD]}}, _ROOT))
    platform_facts = {"image_platform_v1@" + _CHILD: descriptor["platform"]}
    asserts.equals(env, platform_facts, discover_root_platforms(fake_ctx, {_CHILD: img}, graph, platform_facts, "bazel"))
    shallow = build_image_files_dict(_CHILD, graph, "shallow")
    asserts.equals(env, sorted([_CHILD, _CONFIG]), sorted(shallow.keys()))
    eager = build_image_files_dict(_CHILD, graph, "eager")
    asserts.equals(env, sorted([_CHILD, _CONFIG, _LAYER]), sorted(eager.keys()))
    asserts.false(env, _ROOT in eager, "selected manifest does not depend on its parent")
    graph[_OTHER] = manifest
    lazy_img = struct(layer_handling = "lazy")
    manifests, files, lazy_files = collect_blobs_to_create(graph, {_ROOT: img, _OTHER: lazy_img})
    asserts.true(env, _LAYER in files)
    asserts.true(env, _LAYER in lazy_files, "mixed handling must declare both dependency kinds")
    asserts.equals(env, sorted(graph.keys()), sorted(manifests.keys()))

    # The native downloader discovers roots/children without requesting layers.
    cold_ctx = struct(report_progress = _progress, download = _download, read = _read, path = _path)
    cold = sync_oci_ref_graph(cold_ctx, {_ROOT: img}, {}, "bazel")
    asserts.equals(env, manifest, cold[_CHILD])
    asserts.equals(env, [_CHILD, _OTHER], cold[_ROOT]["manifests"])
    asserts.equals(env, descriptor, cold[_ROOT]["descriptors"][0])
    asserts.equals(env, {"image_platform_v1@" + _CHILD: {"os": "linux", "architecture": "amd64", "variant": ""}}, discover_root_platforms(cold_ctx, {_CHILD: img}, cold, {}, "bazel"))

    # Missing root facts can still reuse existing facts for a newly discovered child.
    partial = {"oci_ref_graph_v2@" + _OTHER: {"kind": "manifest", "config": _CONFIG, "layers": []}}
    restored = sync_oci_ref_graph(cold_ctx, {_ROOT: img}, partial, "bazel")
    asserts.equals(env, [], restored[_OTHER]["layers"], "cached child should not be downloaded again")
    return unittest.end(env)

images_helpers_test = unittest.make(_images_helpers_test_impl)
