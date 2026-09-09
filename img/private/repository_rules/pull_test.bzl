"""Recording-downloader tests for the pull repository implementation."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load(":pull.bzl", "pull_test")

_INDEX = "application/vnd.oci.image.index.v1+json"
_MANIFEST = "application/vnd.oci.image.manifest.v1+json"
_PullResultInfo = provider("Recorded downloads and generated repository files.", fields = ["calls", "files", "root", "selected", "blobs"])

def _digest(number):
    return "sha256:" + "0" * (64 - len(str(number))) + str(number)

def _fixture(single, nested):
    descriptors = []
    blobs = {}
    platforms = [
        {"os": "linux", "architecture": "amd64"},
        {"os": "linux", "architecture": "arm64"},
        {"os": "linux", "architecture": "arm64", "variant": "v9"},
        {"os": "unknown", "architecture": "unknown"},
        None,
    ]
    for i, platform in enumerate(platforms):
        manifest = _digest(i * 3 + 1)
        config = _digest(i * 3 + 2)
        layer = _digest(i * 3 + 3)
        descriptor = dict(mediaType = _MANIFEST, digest = manifest, size = 1)
        if platform != None:
            descriptor["platform"] = platform
        descriptors.append(descriptor)
        blobs[manifest] = json.encode(dict(
            mediaType = _MANIFEST,
            config = {"digest": config},
            layers = [{"digest": layer}],
        ))
        blobs[config] = json.encode(platform or {"os": "windows", "architecture": "amd64"})
        blobs[layer] = "layer"
    if nested:
        descriptors.append(dict(mediaType = _INDEX, digest = _digest(99), platform = {"os": "linux", "architecture": "s390x"}))
    blobs[_digest(0)] = json.encode(dict(mediaType = _INDEX, manifests = descriptors))
    return struct(blobs = blobs, root = _digest(1) if single else _digest(0))

def _record_download(rctx, digest, downloader):
    if downloader != rctx.attr.downloader:
        fail("incorrect downloader")
    path = "blobs/sha256/" + digest.removeprefix("sha256:")
    if path not in rctx.files:
        rctx.calls.append(digest)
        rctx.files[path] = rctx.blobs[digest]
    return struct(digest = digest, path = path, data = rctx.blobs[digest])

def _record_prefetch(rctx, reference):
    if reference not in rctx.blobs:
        fail("unknown prefetch reference")
    rctx.calls.append("bulk-pull")
    for digest, content in rctx.blobs.items():
        if content != "layer" or rctx.attr.layer_handling == "eager":
            _record_download(rctx, digest, rctx.attr.downloader)

_DOWNLOADS = struct(
    manifest = lambda rctx, reference, downloader, **kwargs: _record_download(rctx, reference, downloader),
    blob = lambda rctx, digest, downloader, **kwargs: _record_download(rctx, digest, downloader),
    layers = lambda rctx, digests, downloader, **kwargs: [_record_download(rctx, digest, downloader) for digest in digests],
    prefetch = _record_prefetch,
)

def _subject_impl(ctx):
    fixture = _fixture(ctx.attr.single, ctx.attr.nested)
    files = {}
    calls = []
    rctx = struct(
        attr = struct(
            name = ctx.label.name,
            platforms = ctx.attr.platforms,
            digest = fixture.root,
            tag = "",
            registry = "registry.example.com",
            registries = [],
            repository = "image",
            downloader = ctx.attr.downloader,
            layer_handling = ctx.attr.layer_handling,
            unsafe_allow_tag_without_digest = False,
            docker_config_path = "",
        ),
        files = files,
        calls = calls,
        blobs = fixture.blobs,
        file = lambda path, content: files.update({path: content}),
    )
    pull_test.implementation(rctx, downloads = _DOWNLOADS)
    return [_PullResultInfo(calls = calls, files = files, root = fixture.root, selected = ctx.attr.selected, blobs = fixture.blobs)]

_subject = rule(
    implementation = _subject_impl,
    attrs = {
        "platforms": attr.string_list(),
        "selected": attr.int_list(),
        "downloader": attr.string(default = "bazel"),
        "layer_handling": attr.string(default = "shallow"),
        "single": attr.bool(),
        "nested": attr.bool(),
    },
)

def _downloads_test_impl(ctx):
    env = analysistest.begin(ctx)
    result = analysistest.target_under_test(env)[_PullResultInfo]
    expected = [result.root]
    for i in result.selected:
        manifest = _digest(i * 3 + 1)
        if manifest != result.root:
            expected.append(manifest)
        expected.append(_digest(i * 3 + 2))
    if ctx.attr.eager:
        expected.extend([_digest(i * 3 + 3) for i in result.selected])
    asserts.equals(env, sorted(expected), sorted([call for call in result.calls if call != "bulk-pull"]))
    asserts.equals(env, ctx.attr.bulk, "bulk-pull" in result.calls)
    build = result.files["BUILD.bazel"]
    selected = [_digest(i * 3 + 1) for i in result.selected] if ctx.attr.filtered_index else []
    asserts.true(env, "selected_manifest_digests = {},".format(repr(selected)) in build)
    root_path = "blobs/sha256/" + result.root.removeprefix("sha256:")
    asserts.equals(env, result.blobs[result.root], result.files[root_path])
    for i in range(5):
        layer = _digest(i * 3 + 3)
        if ctx.attr.lazy:
            asserts.equals(env, i in result.selected, layer.replace("sha256:", "sha256_") in build)
    if result.selected == [0]:
        asserts.true(env, "img/constraints:linux_amd64" in build)
        asserts.false(env, "img/constraints:linux_arm64" in build)
    return analysistest.end(env)

_downloads_test = analysistest.make(
    _downloads_test_impl,
    attrs = {
        "eager": attr.bool(),
        "lazy": attr.bool(),
        "bulk": attr.bool(),
        "filtered_index": attr.bool(default = True),
    },
)

def _failure_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, ctx.attr.message)
    return analysistest.end(env)

_failure_test = analysistest.make(_failure_test_impl, expect_failure = True, attrs = {"message": attr.string()})

def pull_test_suite(name):
    """Declare pull download and validation tests.

    Args:
        name: Test suite name.
    """
    tests = []
    for downloader in ["bazel", "img_tool"]:
        for handling in ["shallow", "eager", "lazy"]:
            for case, platforms, selected in [
                ("one", ["linux/amd64"], [0]),
                ("variants", ["linux/arm64"], [1, 2]),
                ("exact", ["linux/arm64/v8"], [1]),
                ("multiple", ["linux/arm64/v9", "linux/amd64", "linux/amd64"], [0, 2]),
                ("attestation", ["unknown/unknown"], [3]),
                ("all", [], [0, 1, 2, 3, 4]),
            ]:
                test = "{}_{}_{}_{}".format(name, downloader, handling, case)
                _subject(
                    name = test + "_subject",
                    platforms = platforms,
                    selected = selected,
                    downloader = downloader,
                    layer_handling = handling,
                    tags = ["manual"],
                )
                _downloads_test(
                    name = test,
                    size = "small",
                    target_under_test = ":" + test + "_subject",
                    eager = handling == "eager",
                    lazy = handling == "lazy",
                    bulk = downloader == "img_tool" and not platforms,
                    filtered_index = bool(platforms),
                )
                tests.append(test)
        for case, kwargs in [
            ("single", {"single": True}),
            ("excluded_nested", {"nested": True}),
        ]:
            test = "{}_{}_{}".format(name, downloader, case)
            _subject(name = test + "_subject", platforms = ["linux/amd64"], selected = [0], downloader = downloader, tags = ["manual"], **kwargs)
            _downloads_test(name = test, size = "small", target_under_test = ":" + test + "_subject", filtered_index = case != "single")
            tests.append(test)
        for case, platforms, single, nested, message in [
            ("missing", ["linux/amd64", "linux/ppc64le"], False, False, "requested platforms not found in image: linux/ppc64le"),
            ("missing_metadata", ["windows/amd64"], False, False, "requested platforms not found"),
            ("wrong_variant", ["linux/arm64/v7"], False, False, "requested platforms not found"),
            ("wrong_single", ["linux/arm64"], True, False, "requested platforms not found"),
            ("selected_nested", ["linux/s390x"], False, True, "Nested indexes are not supported"),
        ] + [("invalid_" + str(i), [platform], False, False, "invalid platform") for i, platform in enumerate(["linux", "linux/", "/amd64", "linux/arm64/", "linux/arm64/v8/extra", " linux/amd64", "linux/arm 64"])]:
            test = "{}_{}_{}".format(name, downloader, case)
            _subject(name = test + "_subject", platforms = platforms, downloader = downloader, single = single, nested = nested, tags = ["manual"])
            _failure_test(name = test, size = "small", target_under_test = ":" + test + "_subject", message = message)
            tests.append(test)
    native.test_suite(name = name, tests = tests)
