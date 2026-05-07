<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Public API for container image multi deploy rule.

<a id="multi_deploy"></a>

## multi_deploy

<pre>
load("@rules_img//img:multi_deploy.bzl", "multi_deploy")

multi_deploy(<a href="#multi_deploy-name">name</a>, <a href="#multi_deploy-deploy_tool">deploy_tool</a>, <a href="#multi_deploy-load_strategy">load_strategy</a>, <a href="#multi_deploy-operations">operations</a>, <a href="#multi_deploy-push_strategy">push_strategy</a>, <a href="#multi_deploy-tool_cfg">tool_cfg</a>)
</pre>

Deploys multiple container images in a single coordinated command.

Use `push_specs` and `load_specs` on your image targets to attach deployment
configuration directly, then reference the images in `operations`:

```python
load("@rules_img//img:image.bzl", "image_manifest")
load("@rules_img//img:push.bzl", "image_push_spec")
load("@rules_img//img:multi_deploy.bzl", "multi_deploy")

image_push_spec(
    name = "push_spec",
    registry = "gcr.io",
    repository = "my-project/{{.image_target_name}}",
    tag = "latest",
)

image_manifest(
    name = "frontend",
    base = "@distroless_cc",
    layers = [":frontend_layer"],
    push_specs = [":push_spec"],
)

image_manifest(
    name = "backend",
    base = "@distroless_cc",
    layers = [":backend_layer"],
    push_specs = [":push_spec"],
)

multi_deploy(
    name = "deploy_all",
    operations = [
        ":frontend",
        ":backend",
    ],
)
```

Alternatively, standalone `image_push` or `image_load` targets that already
provide `DeployInfo` can be used directly in `operations`.

Runtime usage:
```bash
bazel run //path/to:deploy_all
```

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="multi_deploy-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="multi_deploy-deploy_tool"></a>deploy_tool |  Optional label of a deploy tool target providing `DeployToolInfo` (created with `img_deploy_tool` from `@rules_img//img:deploy_tool.bzl`). When set, overrides `tool_cfg`.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="multi_deploy-load_strategy"></a>load_strategy |  Load strategy to use for all load operations in the deployment.<br><br>Available strategies: - **`auto`** (default): Uses the global default load strategy - **`eager`**: Downloads all layers during the build phase - **`lazy`**: Downloads layers only when needed during the load operation   | String | optional |  `"auto"`  |
| <a id="multi_deploy-operations"></a>operations |  List of operations to deploy together.<br><br>Each operation must provide DeployInfo (typically from image_push, image_load, image_manifest with push_specs/load_specs, or image_index with push_specs/load_specs). All operations will be merged and executed in the order specified.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | required |  |
| <a id="multi_deploy-push_strategy"></a>push_strategy |  Push strategy to use for all push operations in the deployment.<br><br>See [push strategies documentation](/docs/push-strategies.md) for detailed information.   | String | optional |  `"auto"`  |
| <a id="multi_deploy-tool_cfg"></a>tool_cfg |  Configuration of the deployer executable platform.<br><br>Available options: - **`host`** (default): Deployer executable matches the host platform. - **`target`**: Deployer executable matches the target platform(s) specified via `--platforms`.   | String | optional |  `"host"`  |


