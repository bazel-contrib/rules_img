"""Defines providers for settings of push rules."""

DOC = """\
Collection of active push settings.
"""

FIELDS = dict(
    strategy = "The strategy of the push rule. This can be one of the following: 'eager', 'lazy', 'cas_registry', or 'bes'.",
    remote_cache = "Bazel remote cache to use for the push rule as part of the lazy push strategy. Uses the same format as Bazel's --remote_cache flag. Uses $IMG_REAPI_ENDPOINT env var if not set.",
    remote_instance_name = "Remote instance name for REAPI CAS requests. Set as instance_name field in CAS RPCs and as path prefix in ByteStream resource names. Uses $IMG_REAPI_INSTANCE_NAME env var if not set.",
    credential_helper = "Credential helper to use for registry requests and push-strategy gRPC connections. See docs/credential-helpers.md for details. Uses $IMG_CREDENTIAL_HELPER env var or tools/credential-helper if not set.",
    credential_helper_oci_registry = "Credential helper used only for OCI registry operations (push, tag). Takes precedence over credential_helper for registry auth. Uses $IMG_CREDENTIAL_HELPER_OCI_REGISTRY env var if not set. See docs/credential-helpers.md.",
    credential_helper_remote_cache = "Credential helper used only to authenticate gRPC calls to the remote cache / remote execution API. Takes precedence over credential_helper for those calls. Uses $IMG_CREDENTIAL_HELPER_REMOTE_CACHE env var if not set. See docs/credential-helpers.md.",
    cross_mount = "Cross-mount configuration. Either 'same_registry', 'cross_registry' or 'disabled'.",
)

PushSettingsInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
