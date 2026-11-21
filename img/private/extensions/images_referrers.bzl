"""Referrer handling for the images module extension.

Referrers are requested per anchor manifest: the image of an `images.pull` tag, or
the `manifest` of an `images.referrers` tag. The anchor may be the root of a pulled
image or a child of a pulled index; its tree is the anchor and, for an index, its
children. A referrer is attributed to the manifest its own `subject` names, which
must be in the tree of the anchor it was requested for.

Cosign's tag-scheme artifacts (`sha256-<hex>.sig` and `.att`) have no subject
field: the img tool records the manifest whose tag they were found under as their
subject, after checking that their signed payloads name it.

The facts record, per manifest, the referrers discovered for it by artifact type
(`referrer_discovery@`, where `cosign:sig` and `cosign:att` stand for cosign's
tags), next to the structure of every referrer manifest (`oci_ref_graph@`,
including its subject).
"""

load("//img/private/repository_rules:referrers.bzl", "check_digests", "discovery_types", "is_valid_digest")

DISCOVERY_FACT_PREFIX = "referrer_discovery@"

def referrer_request(*, anchor, tag, is_root, where):
    """Describe the referrers a tag requests for an anchor manifest.

    Args:
        anchor: Digest of the manifest the referrers apply to (or whose tree they apply to).
        tag: An `images.pull` or `images.referrers` tag.
        is_root: Whether the tag comes from the root module.
        where: Human-readable location of the tag, for error messages.

    Returns:
        A struct, or None if the tag requests no referrers.
    """
    if not tag.referrer_digests and not tag.referrer_artifact_types and not tag.referrer_cosign_tags and not tag.referrer_artifacts:
        return None
    if not is_valid_digest(anchor):
        fail("Invalid manifest digest '{}' in {}. Must be of the form 'sha256:<64-hex-chars>'.".format(anchor, where))
    check_digests(tag.referrer_digests, "referrer_digests of " + where)
    return struct(
        anchor = anchor,
        digests = list(tag.referrer_digests),
        # The img tool discovers cosign's tag-scheme artifacts for pseudo artifact types.
        artifact_types = discovery_types(tag.referrer_artifact_types, tag.referrer_cosign_tags, where),
        artifacts = [str(label) for label in tag.referrer_artifacts],
        is_root = is_root,
        where = where,
    )

def tree_of(anchor, oci_ref_graph):
    """Return the anchor and, if it is an index, its children.

    Args:
        anchor: Digest of a manifest or index.
        oci_ref_graph: OCI reference graph.

    Returns:
        List of digests.
    """
    entry = oci_ref_graph.get(anchor)
    if entry != None and entry["kind"] == "index":
        return [anchor] + entry.get("manifests", [])
    return [anchor]

def images_containing(digest, images_by_digest, oci_ref_graph):
    """Return the digests of the pulled images whose tree contains a manifest.

    Args:
        digest: Digest of a manifest or index.
        images_by_digest: Dictionary of pulled images by digest.
        oci_ref_graph: OCI reference graph.

    Returns:
        List of root digests.
    """
    return [root for root in images_by_digest.keys() if digest in tree_of(root, oci_ref_graph)]

def referrers_tool_input(requests):
    """Build the --referrers input of `img sync-oci-ref-graph`.

    Args:
        requests: List of referrer requests.

    Returns:
        A dict with the pinned referrers and the discovery requests.
    """
    return {
        "referrers": [
            {"digest": digest, "anchor": request.anchor}
            for request in requests
            for digest in request.digests
        ],
        "discover": [
            {"anchor": request.anchor, "artifact_types": request.artifact_types}
            for request in requests
            if request.artifact_types
        ],
    }

def resolve_referrers(requests, images_by_digest, oci_ref_graph, discovered):
    """Attribute the requested referrers to the manifests they apply to.

    Fails if a referrer is not an image manifest, has no subject, or applies to a
    manifest outside the tree of its anchor. A request whose anchor is not part of
    any pulled image is an error in the root module, and ignored otherwise: a
    module may annotate images that only some of its users pull.

    Args:
        requests: List of referrer requests.
        images_by_digest: Dictionary of pulled images by digest.
        oci_ref_graph: OCI reference graph, including the referrer manifests.
        discovered: Dict mapping manifest digests to artifact types to discovered referrers.

    Returns:
        struct(by_manifest, artifacts): `by_manifest` maps a manifest digest to the
        sorted digests of its referrers; `artifacts` maps an anchor digest to the
        labels of its referrer artifact specs.
    """
    by_manifest = {}
    artifacts = {}
    for request in requests:
        if len(images_containing(request.anchor, images_by_digest, oci_ref_graph)) == 0:
            if request.is_root:
                fail("{}: manifest {} is not part of any pulled image (neither a pulled manifest or index, nor a child of a pulled index).".format(request.where, request.anchor))
            continue
        tree = tree_of(request.anchor, oci_ref_graph)
        digests = list(request.digests)
        for manifest in tree:
            for artifact_type in request.artifact_types:
                digests.extend(discovered.get(manifest, {}).get(artifact_type, []))
        for digest in digests:
            entry = oci_ref_graph.get(digest)
            if entry == None:
                fail("Referrer {} in {} was not fetched.".format(digest, request.where))
            if entry["kind"] != "manifest":
                fail("Referrer {} in {} is an image index; only image manifests are supported as referrers.".format(digest, request.where))
            subject = entry.get("subject")
            if not subject:
                fail("Referrer {} in {} has no subject, so it is not attributed to any manifest of {}.".format(digest, request.where, request.anchor))
            if subject not in tree:
                fail("Referrer {} in {} applies to {}, which is neither {} nor one of its children.".format(digest, request.where, subject, request.anchor))
            referrers = by_manifest.setdefault(subject, [])
            if digest not in referrers:
                referrers.append(digest)
        for label in request.artifacts:
            labels = artifacts.setdefault(request.anchor, [])
            if label not in labels:
                labels.append(label)
    return struct(
        by_manifest = {manifest: sorted(referrers) for manifest, referrers in by_manifest.items()},
        artifacts = artifacts,
    )

def referrers_for_image(digest, oci_ref_graph, resolved):
    """Return the referrers and artifact specs that apply to a pulled image.

    Args:
        digest: Root digest of the image.
        oci_ref_graph: OCI reference graph.
        resolved: Result of resolve_referrers.

    Returns:
        struct(referrers, artifacts): the sorted referrer digests of every manifest in
        the image's tree, and a dict mapping each anchor in the tree to its spec labels.
    """
    referrers = {}
    artifacts = {}
    for manifest in tree_of(digest, oci_ref_graph):
        for referrer in resolved.by_manifest.get(manifest, []):
            referrers[referrer] = True
        if manifest in resolved.artifacts:
            artifacts[manifest] = resolved.artifacts[manifest]
    return struct(referrers = sorted(referrers.keys()), artifacts = artifacts)

def discovery_facts(discovered):
    """Build the facts that record the discovered referrers in the lockfile.

    Args:
        discovered: Dict mapping manifest digests to artifact types to discovered referrers.

    Returns:
        Dictionary of facts.
    """
    return {DISCOVERY_FACT_PREFIX + manifest: found for manifest, found in discovered.items()}
