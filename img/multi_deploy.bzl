"""Public API for container image multi deploy rule."""

load(
    "//img/private:multi_deploy.bzl",
    _MULTI_DEPLOY_ACTION_ATTRS = "MULTI_DEPLOY_ACTION_ATTRS",
    _MULTI_DEPLOY_ACTION_TOOLCHAINS = "MULTI_DEPLOY_ACTION_TOOLCHAINS",
    _multi_deploy = "multi_deploy",
    _multi_deploy_action = "multi_deploy_action",
)

multi_deploy = _multi_deploy
multi_deploy_action = _multi_deploy_action
multi_deploy_action_attrs = _MULTI_DEPLOY_ACTION_ATTRS
multi_deploy_action_toolchains = _MULTI_DEPLOY_ACTION_TOOLCHAINS
