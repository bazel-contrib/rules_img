<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Public API for signing pushed images.

<a id="signing_config"></a>

## signing_config

<pre>
load("@rules_img//img:signing.bzl", "signing_config")

signing_config(<a href="#signing_config-name">name</a>, <a href="#signing_config-args">args</a>, <a href="#signing_config-env">env</a>, <a href="#signing_config-targets">targets</a>, <a href="#signing_config-tool">tool</a>, <a href="#signing_config-tool_command">tool_command</a>)
</pre>

Configures how pushed images are signed: which signer plugin to run, and what to sign.

```python
load("@rules_img//img:signing.bzl", "signing_config")

signing_config(
    name = "sign",
    tool = "@rules_img_signer_cosign",
    targets = ["roots", "child_manifests"],
)
```

Turn signing on with `--@rules_img//img/settings:sign=enabled` and select the config with
`--@rules_img//img/settings:sign_setting=//:sign` (or the `sign` and `sign_setting` attributes of
`image_push` and `image_push_spec`). Keys and tokens come from the environment `bazel run` runs
in, never from Bazel.

See [image signing](/docs/image-signing.md) for how signing works and how to choose a plugin, and
[supply chain security](/docs/supply-chain.md) for signing and attesting SBOMs.

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="signing_config-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="signing_config-args"></a>args |  Arguments for the plugin, such as `--tlog-upload=false` for cosign. See the plugin's README.   | List of strings | optional |  `[]`  |
| <a id="signing_config-env"></a>env |  Additional (non-secret) environment variables set for the plugin. Secrets should come from the deploy-time environment instead.   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="signing_config-targets"></a>targets |  What to sign, any of:<br><br>* **`roots`** (default): the pushed image or index. * **`child_manifests`**: each platform manifest of a pushed index. * **`referrers`**: the referrers pushed with the image, such as its SBOMs. * **`sbom_attestations`**: not a signature, but a signed in-toto attestation of each SBOM about   the image it describes. Needs a plugin that can attest (cosign's, not Notation's).<br><br>Overridable at deploy time with `--sign_targets`.   | List of strings | optional |  `["roots"]`  |
| <a id="signing_config-tool"></a>tool |  The signer plugin, as a Bazel executable, such as `@rules_img_signer_cosign` or `@rules_img_signer_notation`. Mutually exclusive with `tool_command`.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="signing_config-tool_command"></a>tool_command |  The signer plugin, as the name or path of a command installed on the machine that pushes, resolved on `$PATH` at deploy time. Mutually exclusive with `tool`.   | String | optional |  `""`  |


