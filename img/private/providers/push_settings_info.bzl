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
    blob_repository = "When non-empty, the staging repository that image blobs are pushed to and cross-mounted from when pushing manifests. At build time every blob (layers and config) is staged here; layers are cross-mounted into the image's real repository. Empty means blobs go to the image's own repository.",
    forbid_layer_push = "Bool: when True, `img deploy` refuses to upload layer blob bytes (layers must be cross-mounted or already present).",
    deduplicated_push = "Resolved deduplicated push mode: 'disabled', 'best_effort' or 'enabled'. When not 'disabled', `img deploy` pushes in phases -- check which manifests the registry already has, upload each blob that several repositories need to just one of them (the first alphabetically, or a staging/pinned repository), then cross-mount it into the others. For registries that keep a separate blob store per repository name. 'enabled' requires cross-repository blob mounting and fails a push where mounting is refused; 'best_effort' uploads the layer the ordinary way instead. See docs/registry-support.md.",
    deduplicated_push_blob_repository = "When non-empty, the repository within the destination registry that every blob the deduplicated push shares between repositories is uploaded to and cross-mounted from, ahead of any home repository the deploy would have picked itself. Empty means the deploy picks one per blob.",
    deduplicated_push_content = "Resolved deduplicated push content: 'blobs' uploads a shared blob to its home repository and nothing else, 'blobs_and_artificial_manifests' also uploads a config blob and creates a manifest referencing the blob there, for registries that only expose a blob to other repositories once a manifest references it.",
    insecure = "Bool: when True, registries are addressed over plain HTTP and untrusted TLS certificates are accepted (like crane's --insecure). Maps to the IMG_INSECURE env var of the img tool.",
)

PushSettingsInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
