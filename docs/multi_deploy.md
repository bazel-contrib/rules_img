<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Public API for container image multi deploy rule.

<a id="multi_deploy"></a>

## multi_deploy

<pre>
load("@rules_img//img:multi_deploy.bzl", "multi_deploy")

multi_deploy(<a href="#multi_deploy-name">name</a>, <a href="#multi_deploy-deploy_tool">deploy_tool</a>, <a href="#multi_deploy-load_strategy">load_strategy</a>, <a href="#multi_deploy-operations">operations</a>, <a href="#multi_deploy-push_strategy">push_strategy</a>, <a href="#multi_deploy-tool_cfg">tool_cfg</a>)
</pre>

Merges multiple deploy operations into a single unified deployment command.

This rule takes multiple operations (typically from image_push or image_load rules)
that provide DeployInfo and merges them into a single command that can deploy all
operations in parallel. This is useful for scenarios where you need to push and/or
load multiple related images as a coordinated deployment.

The rule produces an executable that can be run with `bazel run`.

Example:

```python
load("@rules_img//img:push.bzl", "image_push")
load("@rules_img//img:load.bzl", "image_load")
load("@rules_img//img:multi_deploy.bzl", "multi_deploy")

# Individual operations
image_push(
    name = "push_frontend",
    image = ":frontend",
    registry = "gcr.io",
    repository = "my-project/frontend",
    tag = "latest",
)

image_push(
    name = "push_backend",
    image = ":backend",
    registry = "gcr.io",
    repository = "my-project/backend",
    tag = "latest",
)

image_load(
    name = "load_database",
    image = ":database",
    tag = "my-database:latest",
)

# Unified deployment
multi_deploy(
    name = "deploy_all",
    operations = [
        ":push_frontend",
        ":push_backend",
        ":load_database",
    ],
    push_strategy = "lazy",
    load_strategy = "eager",
)
```

Runtime usage:
```bash
# Deploy all operations together
bazel run //path/to:deploy_all
```

The deploy-merge subcommand will execute all push and load operations in sequence,
allowing for coordinated deployment of related container images.

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="multi_deploy-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="multi_deploy-deploy_tool"></a>deploy_tool |  Optional label of a deploy tool target providing `DeployToolInfo` (created with `img_deploy_tool` from `@rules_img//img:deploy_tool.bzl`). When set, overrides `tool_cfg`.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="multi_deploy-load_strategy"></a>load_strategy |  Load strategy to use for all load operations in the deployment.<br><br>Available strategies: - **`auto`** (default): Uses the global default load strategy - **`eager`**: Downloads all layers during the build phase - **`lazy`**: Downloads layers only when needed during the load operation   | String | optional |  `"auto"`  |
| <a id="multi_deploy-operations"></a>operations |  List of operations to deploy together.<br><br>Each operation must provide DeployInfo (typically from image_push or image_load rules). All operations will be merged and executed in the order specified.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | required |  |
| <a id="multi_deploy-push_strategy"></a>push_strategy |  Push strategy to use for all push operations in the deployment.<br><br>See [push strategies documentation](/docs/push-strategies.md) for detailed information.   | String | optional |  `"auto"`  |
| <a id="multi_deploy-tool_cfg"></a>tool_cfg |  Configuration of the deployer executable platform.<br><br>Available options: - **`host`** (default): Deployer executable matches the host platform. - **`target`**: Deployer executable matches the target platform(s) specified via `--platforms`.   | String | optional |  `"host"`  |


