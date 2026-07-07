"""Provider describing how `img deploy` signs images via an external plugin."""

DOC = """\
Describes how to invoke a signer plugin, produced by the `signing_config` rule
and selected globally via `//img/settings:sign_setting` or per target via the
`sign_setting` attribute of `image_push`/`image_push_spec`.
"""

FIELDS = dict(
    config_file = "File: the deterministic sign_setting config JSON handed to `img deploy` (hashed to a content descriptor), or None for the 'unset' sentinel.",
    runfiles = "runfiles: the plugin executable plus its runfiles (empty for host-command plugins or the unset sentinel).",
    targets = "list[str]: default sign-target selection (subset of \"roots\", \"child_manifests\", \"referrers\").",
)

SigningConfigInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
