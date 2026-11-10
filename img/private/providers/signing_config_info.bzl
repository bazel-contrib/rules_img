"""Provider describing how `img deploy` signs images via an external plugin."""

DOC = """\
Describes how to invoke a signer plugin, produced by the `signing_config` rule
and selected globally via `//img/settings:sign_setting` or per target via the
`sign_setting` attribute of `image_push`/`image_push_spec`.
"""

FIELDS = dict(
    steps = """list[struct]: the plugins to run, in order (empty for the 'unset' sentinel). A `signing_config` has one step, a `signing_chain` the steps of its configs. Each step has:
- `config_file` (File): the deterministic sign_setting config JSON handed to `img deploy` (hashed to a content descriptor).
- `runfiles` (runfiles): the plugin executable plus its runfiles (empty for host-command plugins).
- `targets` (list[str]): sign-target selection (subset of "roots", "child_manifests", "referrers", "plugin_artifacts").
- `build_info` (bool): whether the plugin wants the build info file (the Bazel targets the deploy was built from).""",
)

SigningConfigInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
