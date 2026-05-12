"""Provider for load settings."""

DOC = """\
Collection of active load settings.
"""

FIELDS = dict(
    strategy = "The default load strategy to use",
    daemon = "The daemon to target by default",
    remote_cache = "Bazel remote cache to use for the push rule as part of the lazy push strategy. Uses the same format as Bazel's --remote_cache flag. Uses $IMG_REAPI_ENDPOINT env var if not set.",
    remote_instance_name = "Remote instance name for REAPI CAS requests. Set as instance_name field in CAS RPCs and as path prefix in ByteStream resource names. Uses $IMG_REAPI_INSTANCE_NAME env var if not set.",
    credential_helper = "Credential helper to use for the push rule. This can be the same as Bazel's credential helper. Uses $IMG_CREDENTIAL_HELPER env var or tools/credential-helper if not set.",
)

LoadSettingsInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
