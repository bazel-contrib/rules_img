"""Defines providers for the referrers of a pulled image."""

REFERRER_DOC = """\
A referrer of a manifest: an artifact (an SBOM, a signature, an attestation, ...)
whose `subject` is that manifest.

The descriptor-level fields mirror an entry of the
[referrers API](https://github.com/opencontainers/distribution-spec/blob/main/spec.md#listing-referrers)
response, so they are known during analysis and can be used to pick artifacts
(for example in an aspect) without reading any file.
"""

REFERRER_FIELDS = dict(
    digest = "Digest of the referrer manifest.",
    media_type = "Media type of the referrer manifest (`application/vnd.oci.image.manifest.v1+json`).",
    size = "Size of the referrer manifest in bytes.",
    artifact_type = """\
Artifact type of the referrer: the manifest's `artifactType`, or the media type of its config
when that is unset, as the referrers API reports it.""",
    annotations = "Dict of the referrer manifest's annotations.",
    subject = "Digest of the manifest this referrer applies to.",
    manifest = """\
ImageManifestInfo of the referrer. Its `manifest`, `config` and the `blob` of each layer are
always available: referrers are fetched regardless of the image's `layer_handling`.
`descriptor` holds the referrers API descriptor (including `artifactType` and `annotations`).
Artifacts have no platform, so `os` and `architecture` are `"unknown"`, and `structured_config`
is empty.""",
)

ReferrerInfo = provider(
    doc = REFERRER_DOC,
    fields = REFERRER_FIELDS,
)

REFERRERS_DOC = """\
The referrers attached to a pulled image, grouped by the manifest they apply to.

Pulled images always provide it, with no referrers unless some were requested. A referrer
is attributed to the manifest named by its `subject`: the root manifest or index, or for an
index, one of its child manifests.
"""

REFERRERS_FIELDS = dict(
    digest = "Digest of the pulled root manifest or index.",
    manifest_digests = """\
List of the digests of the child manifests, in the order of `ImageIndexInfo.manifests`, so a child
picked from the index can be looked up in `referrers`. Empty for a single-platform image.""",
    referrers = """\
Dict mapping the digest of a manifest in the pulled image (the root and, for an index, each
child it lists) to the list of its ReferrerInfo. Only manifests with referrers have an entry, so
look a manifest up with `referrers.get(digest, [])`. Referrers fetched from registries come first, followed by those built from referrer
artifact specs.""",
)

PulledReferrersInfo = provider(
    doc = REFERRERS_DOC,
    fields = REFERRERS_FIELDS,
)
