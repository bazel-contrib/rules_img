<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Public API for container image signing configuration.

See the `signing_config` rule for how `img deploy` signs images by delegating to
external signer plugins.

<a id="signing_config"></a>

## signing_config

<pre>
load("@rules_img//img:signing.bzl", "signing_config")

signing_config(<a href="#signing_config-name">name</a>, <a href="#signing_config-args">args</a>, <a href="#signing_config-env">env</a>, <a href="#signing_config-targets">targets</a>, <a href="#signing_config-tool">tool</a>, <a href="#signing_config-tool_command">tool_command</a>)
</pre>

Describes how `img deploy` signs images by invoking a signer plugin.

`img deploy` runs the plugin as `<tool> sign-oci-artifact [args...]`, writes the
JSON descriptor of the artifact to sign to the plugin's stdin, and expects an OCI
image layout tar (the signature artifact) on stdout. The signature is then
pushed to the image's repository as an OCI referrer. `img` itself performs no
cryptography; all signing logic lives in the plugin.

The plugin is referenced either as a Bazel executable (`tool`, shipped in the
push binary's runfiles) or as a host-installed command (`tool_command`, resolved
on `$PATH` at deploy time). Secrets and signing hardware are provided by the
environment `bazel run` executes in — never by Bazel.

Example:

```python
load("@rules_img//img:signing.bzl", "signing_config")

# Use a Bazel-built plugin.
signing_config(
    name = "notary",
    tool = "@rules_img//img/signer:notation",
    args = ["--key", "release"],
)

# Use a host-installed tool.
signing_config(
    name = "corp",
    tool_command = "corp-signer",
    args = ["--profile", "prod"],
)
```

Referenced globally via `--@rules_img//img/settings:sign_setting=//path:notary`,
or per target via the `sign_setting` attribute of `image_push`/`image_push_spec`.
Signing is enabled with the `sign` attribute or the `//img/settings:sign` flag.

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="signing_config-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="signing_config-args"></a>args |  Arguments passed to the plugin after the `sign-oci-artifact` subcommand.   | List of strings | optional |  `[]`  |
| <a id="signing_config-env"></a>env |  Additional (non-secret) environment variables set for the plugin. Secrets should come from the deploy-time environment instead.   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="signing_config-targets"></a>targets |  Default set of descriptors to sign: any of "roots" (the pushed root, the default), "child_manifests" (each child of an index), and "referrers" (referrer artifacts such as SBOMs). Overridable at deploy time via `--sign_targets`.   | List of strings | optional |  `["roots"]`  |
| <a id="signing_config-tool"></a>tool |  A Bazel executable implementing the `sign-oci-artifact` protocol. Shipped in the push binary's runfiles. Mutually exclusive with `tool_command`.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="signing_config-tool_command"></a>tool_command |  Name or path of a host-installed signer tool, resolved on `$PATH` at deploy time. Mutually exclusive with `tool`.   | String | optional |  `""`  |


