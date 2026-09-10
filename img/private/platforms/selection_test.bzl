"""Selection tests shared by imported images and generated routing repositories."""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load(":selection.bzl", "select_descriptor", "selection_configs")

def _descriptor(arch, variant = "", os = "linux"):
    return {"platform": {"os": os, "architecture": arch, "variant": variant}}

def _selection_test_impl(ctx):
    env = unittest.begin(ctx)
    descriptors = [
        _descriptor("amd64"),
        _descriptor("amd64", "v2"),
        _descriptor("arm64"),
        _descriptor("arm", "v6"),
        _descriptor("unknown", os = "unknown"),
        _descriptor("amd64", "v2"),
    ]
    asserts.equals(env, 0, select_descriptor(descriptors, "linux", "amd64", ""))
    asserts.equals(env, 1, select_descriptor(descriptors, "linux", "amd64", "v3"))
    asserts.equals(env, 2, select_descriptor(descriptors, "linux", "arm64", ""))
    asserts.equals(env, 2, select_descriptor(descriptors, "linux", "arm64", "v9"))
    asserts.equals(env, 3, select_descriptor(descriptors, "linux", "arm", "v7"))
    asserts.equals(env, None, select_descriptor(descriptors, "windows", "amd64", ""))
    asserts.equals(env, None, select_descriptor(descriptors, "linux", "s390x", ""))
    asserts.equals(env, None, select_descriptor([{}], "linux", "amd64", ""))
    configs = selection_configs(descriptors)
    keys = [tuple(constraints) for constraints, _ in configs]
    asserts.equals(env, len(keys), len({key: True for key in keys}), "branches must be disjoint")
    asserts.false(env, any([position == 4 for _, position in configs]), "attestations cannot be selected")
    asserts.false(env, any([position == 5 for _, position in configs]), "first descriptor wins ties")
    asserts.equals(env, [], selection_configs([{}]))
    return unittest.end(env)

selection_test = unittest.make(_selection_test_impl)
