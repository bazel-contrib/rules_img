"""Analysis tests for the blob validation of deploy targets.

The actions these assert on exist for their inputs: every strategy but eager
reads layer blobs back out of Bazel's cache, so the blobs have to be inputs of
*some* action for the cache to keep them. See blob_validation.bzl.
"""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("@bazel_skylib//rules:write_file.bzl", "write_file")
load("//img/private:layer.bzl", "image_layer")
load("//img/private:load.bzl", "image_load")
load("//img/private:manifest.bzl", "image_manifest")
load("//img/private:multi_deploy.bzl", "multi_deploy")
load("//img/private:push.bzl", "image_push")
load("//img/private/common:deploy_helpers.bzl", "VALIDATION_OUTPUT_GROUP")
load("//img/private/providers:deploy_info.bzl", "DeployInfo")

# buildifier: disable=canonical-repository
_PUSH_STRATEGY = "@@//img/settings:push_strategy"

# buildifier: disable=canonical-repository
_LOAD_STRATEGY = "@@//img/settings:load_strategy"

# buildifier: disable=canonical-repository
_VALIDATE_CAS_BLOBS = "@@//img/settings:validate_cas_blobs"

def _validation_actions(env):
    return [
        action
        for action in analysistest.target_actions(env)
        if action.mnemonic == "ValidateDeployBlobs"
    ]

def _assert_outputs_validated(env, actions):
    """Bazel's validation output group holds each action's single output."""
    output_groups = analysistest.target_under_test(env)[OutputGroupInfo]
    validated = getattr(output_groups, VALIDATION_OUTPUT_GROUP, depset()).to_list()
    for action in actions:
        asserts.equals(env, 1, len(action.outputs.to_list()))
        asserts.true(
            env,
            action.outputs.to_list()[0] in validated,
            "the validation output group does not hold the blob validation output",
        )

def _blob_validation_test_impl(ctx):
    env = analysistest.begin(ctx)
    actions = _validation_actions(env)

    # One deploy operation, so one validation action.
    asserts.equals(env, 1, len(actions), "expected exactly one ValidateDeployBlobs action")
    if len(actions) != 1:
        return analysistest.end(env)
    action = actions[0]

    # The image and its deploy manifest are taken from the target under test, so
    # the Files compare equal to the action's (they share its configuration).
    deploy_info = analysistest.target_under_test(env)[DeployInfo]
    inputs = action.inputs.to_list()
    asserts.true(
        env,
        deploy_info.deploy_manifest in inputs,
        "the deploy manifest is not an input of the validation action",
    )
    for layer in deploy_info.image.layers:
        asserts.true(
            env,
            layer.blob in inputs,
            "layer blob {} is not an input of the validation action".format(layer.blob.short_path),
        )

    _assert_outputs_validated(env, actions)

    return analysistest.end(env)

def _no_blob_validation_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.equals(
        env,
        [],
        _validation_actions(env),
        "expected no ValidateDeployBlobs action",
    )
    return analysistest.end(env)

def _multi_deploy_blob_validation_test_impl(ctx):
    env = analysistest.begin(ctx)
    actions = _validation_actions(env)

    # One action per deploy operation, each scoped to that operation's own
    # (unmerged) deploy manifest -- rather than one action with every blob of
    # every image. multi_deploy provides no DeployInfo of its own, so the
    # operations are identified by the basename of their deploy manifest.
    asserts.equals(
        env,
        len(ctx.attr.expected_deploy_manifests),
        len(actions),
        "expected one ValidateDeployBlobs action per deploy operation",
    )
    for basename in ctx.attr.expected_deploy_manifests:
        owners = [
            action
            for action in actions
            if basename in [input.basename for input in action.inputs.to_list()]
        ]
        asserts.equals(
            env,
            1,
            len(owners),
            "expected exactly one validation action for the operation of {}".format(basename),
        )
        if len(owners) == 1:
            asserts.true(
                env,
                len(owners[0].inputs.to_list()) > 1,
                "the validation action of {} takes no blobs besides the deploy manifest".format(basename),
            )

    _assert_outputs_validated(env, actions)

    return analysistest.end(env)

def _push_strategy_validation_test(strategy):
    """A test that a push with `strategy` validates its blobs."""
    return analysistest.make(
        _blob_validation_test_impl,
        config_settings = {
            _PUSH_STRATEGY: strategy,
            _VALIDATE_CAS_BLOBS: "enabled",
        },
    )

# Every push strategy that reads its blobs back out of Bazel's cache.
_lazy_push_validation_test = _push_strategy_validation_test("lazy")

_cas_registry_push_validation_test = _push_strategy_validation_test("cas_registry")

_bes_push_validation_test = _push_strategy_validation_test("bes")

_lazy_push_setting_disabled_test = analysistest.make(
    _no_blob_validation_test_impl,
    config_settings = {
        _PUSH_STRATEGY: "lazy",
        _VALIDATE_CAS_BLOBS: "disabled",
    },
)

# The eager strategy ships the blobs in the target's runfiles, so the build
# already refers to them.
_eager_push_validation_test = analysistest.make(
    _no_blob_validation_test_impl,
    config_settings = {
        _PUSH_STRATEGY: "eager",
        _VALIDATE_CAS_BLOBS: "enabled",
    },
)

_lazy_load_validation_test = analysistest.make(
    _blob_validation_test_impl,
    config_settings = {
        _LOAD_STRATEGY: "lazy",
        _VALIDATE_CAS_BLOBS: "enabled",
    },
)

_lazy_multi_deploy_validation_test = analysistest.make(
    _multi_deploy_blob_validation_test_impl,
    attrs = {
        "expected_deploy_manifests": attr.string_list(mandatory = True),
    },
    config_settings = {
        _PUSH_STRATEGY: "lazy",
        _VALIDATE_CAS_BLOBS: "enabled",
    },
)

# A cache-reading strategy for a kind of operation this target does not deploy
# does not count: the push-only multi_deploy below keeps its eager push.
_other_operation_lazy_test = analysistest.make(
    _no_blob_validation_test_impl,
    config_settings = {
        _PUSH_STRATEGY: "eager",
        _LOAD_STRATEGY: "lazy",
        _VALIDATE_CAS_BLOBS: "enabled",
    },
)

def _image(name, content):
    """An image_manifest with a single layer holding `content`."""
    write_file(
        name = name + "_content",
        out = name + "_content.txt",
        content = [content],
        tags = ["manual"],
    )
    image_layer(
        name = name + "_layer",
        srcs = {"/app/hello.txt": ":" + name + "_content"},
        tags = ["manual"],
    )
    image_manifest(
        name = name,
        layers = [":" + name + "_layer"],
        tags = ["manual"],
    )

def blob_validation_test_suite(name):
    """Declare analysis tests for the blob validation of lazy deploy targets.

    Args:
        name: Name for the test suite.
    """
    pushes = []
    for i in range(2):
        image = "{}_image_{}".format(name, i)
        _image(image, "hello {}\n".format(i))

        push = "{}_push_{}".format(name, i)
        image_push(
            name = push,
            image = ":" + image,
            registry = "registry.example.com",
            repository = "acme/app" + str(i),
            tag = "latest",
            tags = ["manual"],
        )
        pushes.append(push)

    load_target = name + "_load"
    image_load(
        name = load_target,
        image = ":{}_image_0".format(name),
        tag = "acme/app:latest",
        tags = ["manual"],
    )

    deploy = name + "_deploy"
    multi_deploy(
        name = deploy,
        operations = [":" + push for push in pushes],
        tags = ["manual"],
    )

    push_only_deploy = name + "_push_only_deploy"
    multi_deploy(
        name = push_only_deploy,
        deploy_operations = ["push"],
        operations = [":" + pushes[0]],
        tags = ["manual"],
    )

    tests = []
    for test_name, test_rule, subject in [
        ("lazy_push", _lazy_push_validation_test, pushes[0]),
        ("cas_registry_push", _cas_registry_push_validation_test, pushes[0]),
        ("bes_push", _bes_push_validation_test, pushes[0]),
        ("lazy_push_setting_disabled", _lazy_push_setting_disabled_test, pushes[0]),
        ("eager_push", _eager_push_validation_test, pushes[0]),
        ("lazy_load", _lazy_load_validation_test, load_target),
        ("other_operation_lazy", _other_operation_lazy_test, push_only_deploy),
    ]:
        test = "{}_{}_test".format(name, test_name)
        test_rule(
            name = test,
            size = "small",
            target_under_test = ":" + subject,
        )
        tests.append(":" + test)

    multi_deploy_test = name + "_lazy_multi_deploy_test"
    _lazy_multi_deploy_validation_test(
        name = multi_deploy_test,
        size = "small",
        # image_push names its deploy manifest after the target.
        expected_deploy_manifests = [push + ".json" for push in pushes],
        target_under_test = ":" + deploy,
    )
    tests.append(":" + multi_deploy_test)

    native.test_suite(
        name = name,
        tests = tests,
    )
