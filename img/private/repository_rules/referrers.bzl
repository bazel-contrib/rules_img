"""Fetch-time helpers for the referrers of a pulled image.

A referrer is attributed to the manifest named by the `subject` field of its own
manifest. That subject has to be part of the pulled image's tree -- the root
manifest or index, or one of the children an index lists. Anything else (no
subject, or a subject outside the tree) is an error.

Cosign's tag-scheme artifacts (`sha256-<hex>.sig` and `.att`) have no subject
field. Discovered by their tag, they are attributed to the manifest the tag names,
which the img tool checks their signed payloads to name; pinned by digest, to the
manifest of the tree their signed payloads name.
"""

load("@img_toolchain//:defs.bzl", "tool_for_repository_os")
load("//img/private:manifest_media_type.bzl", "DOCKER_MANIFEST_V2", "MEDIA_TYPE_MANIFEST", "get_media_type", manifest_kind = "kind")
load(":download.bzl", "auth_environment", "execute_kwargs")
load(":registry.bzl", "get_sources_list")

def is_valid_digest(digest):
    return type(digest) == type("") and digest.startswith("sha256:") and len(digest) == 71

def check_digests(digests, what):
    """Fail on the first digest of `digests` that is not a full sha256 digest.

    Args:
        digests: List of digest strings.
        what: Description of where the digests come from, for the error message.
    """
    for digest in digests:
        if not is_valid_digest(digest):
            fail("Invalid digest '{}' in {}. Must be of the form 'sha256:<64-hex-chars>'.".format(digest, what))

def subject_descriptors(root_digest, root_data):
    """Map every manifest a referrer of the image may apply to onto its descriptor.

    Args:
        root_digest: Digest of the pulled root manifest or index.
        root_data: Content of the root manifest or index.

    Returns:
        Dict of digest to descriptor (mediaType, digest, size): the root and, for an
        index, every child it lists -- including those a platform filter skipped.
    """
    root = json.decode(root_data)
    media_type = get_media_type(root)
    subjects = {root_digest: dict(mediaType = media_type, digest = root_digest, size = len(root_data))}
    if manifest_kind(media_type) == "index":
        for child in root.get("manifests", []):
            subjects[child["digest"]] = dict(
                mediaType = child.get("mediaType", ""),
                digest = child["digest"],
                size = child.get("size", 0),
            )
    return subjects

COSIGN_TAGS = ["sig", "att"]

def cosign_tag_types(tags, what):
    """Return the pseudo artifact types with which the img tool discovers cosign's tag-scheme artifacts.

    Args:
        tags: List of cosign tag suffixes ("sig", "att").
        what: Description of where the tags come from, for the error message.

    Returns:
        List of pseudo artifact types ("cosign:sig", "cosign:att").
    """
    for tag in tags:
        if tag not in COSIGN_TAGS:
            fail("Invalid cosign tag '{}' in {}. Must be one of {}.".format(tag, what, COSIGN_TAGS))
    return ["cosign:" + tag for tag in tags]

def discovery_types(artifact_types, cosign_tags, where):
    """Return the (pseudo) artifact types with which the img tool discovers the referrers a rule or tag asks for.

    `"*"` among the artifact types stands for every referrer: those of any artifact type, and
    cosign's tag-scheme artifacts.

    Args:
        artifact_types: The `referrer_artifact_types` attribute.
        cosign_tags: The `referrer_cosign_tags` attribute.
        where: Description of the rule or tag, for error messages.

    Returns:
        List of artifact types and cosign pseudo artifact types, without duplicates.
    """
    types = list(artifact_types)
    tags = list(cosign_tags)
    if "*" in artifact_types:
        tags.extend([tag for tag in COSIGN_TAGS if tag not in tags])
    for artifact_type in cosign_tag_types(tags, "referrer_cosign_tags of " + where):
        if artifact_type not in types:
            types.append(artifact_type)
    return types

def cosign_attribution(discovered):
    """Attribute the cosign tag-scheme artifacts of a discovery result to the manifests they were found for.

    Args:
        discovered: Dict mapping manifest digests to (pseudo) artifact types to referrer digests.

    Returns:
        Dict of referrer digest to subject digest.
    """
    subjects = {}
    for subject, found in discovered.items():
        for artifact_type, digests in found.items():
            if artifact_type.startswith("cosign:"):
                for digest in digests:
                    subjects[digest] = subject
    return subjects

def has_subject(manifest):
    """Whether a decoded manifest has a subject field.

    Args:
        manifest: The decoded manifest.

    Returns:
        True if it names a subject.
    """
    subject = manifest.get("subject")
    return type(subject) == type({}) and bool(subject.get("digest"))

def cosign_subjects_with_tool(ctx, *, tree, artifacts, sources = {}, credential_helper = None, docker_config_path = None, timeout = None):
    """Attribute pinned referrers without a subject field, with `img cosign-subjects`.

    Such a referrer must be one of cosign's tag-scheme signatures or attestations,
    whose signed payloads name the manifest it is about. Starlark can't decode
    a DSSE payload, so the img tool reads them.

    Args:
        ctx: Repository or module context.
        tree: Digests of the manifests the referrers may apply to.
        artifacts: List of dict(digest, manifest, blobs): the referrer's digest, the
            path of its manifest, and a dict of the paths of its downloaded layers.
        sources: Sources dict mapping repositories to registries, to fetch layers
            that weren't downloaded from.
        credential_helper: Optional credential helper, as in auth_environment.
        docker_config_path: Optional Docker-compatible auth config path, as in auth_environment.
        timeout: Optional timeout in seconds, as in execute_kwargs.

    Returns:
        Dict of referrer digest to subject digest.
    """
    if len(artifacts) == 0:
        return {}
    name = "cosign_subjects_{}".format(artifacts[0]["digest"].removeprefix("sha256:"))
    ctx.file(name + ".in.json", json.encode(dict(
        tree = tree,
        artifacts = artifacts,
        sources = get_sources_list(sources) if sources else [],
    )), executable = False)
    result = ctx.execute(
        [ctx.path(tool_for_repository_os(ctx)), "cosign-subjects", "--input=" + name + ".in.json", "--output=" + name + ".out.json"],
        environment = auth_environment(ctx, credential_helper = credential_helper, docker_config_path = docker_config_path),
        **execute_kwargs(ctx, timeout)
    )
    if result.return_code != 0:
        fail("Failed to attribute referrers without a subject: {}{}".format(result.stdout, result.stderr))
    output = json.decode(ctx.read(name + ".out.json"))
    ctx.delete(name + ".in.json")
    ctx.delete(name + ".out.json")
    return {digest: found["subject"] for digest, found in output.items()}

def referrer_subject(digest, manifest, subjects, image, attributed = {}):
    """Return the digest of the manifest a referrer applies to, or fail.

    Args:
        digest: Digest of the referrer manifest.
        manifest: The decoded referrer manifest.
        subjects: The manifests the referrer may apply to, as returned by subject_descriptors.
        image: Human-readable name of the pulled image, for error messages.
        attributed: Dict of referrer digest to subject for the referrers that have
            no subject field: cosign's tag-scheme artifacts (see cosign_attribution
            and cosign_subjects_with_tool).

    Returns:
        The digest of the referrer's subject.
    """
    media_type = get_media_type(manifest)
    allowed = [MEDIA_TYPE_MANIFEST] if has_subject(manifest) or digest not in attributed else [MEDIA_TYPE_MANIFEST, DOCKER_MANIFEST_V2]
    if media_type not in allowed:
        fail("Referrer {} of {} has media type '{}'; only image manifests ({}) are supported as referrers.".format(
            digest,
            image,
            media_type,
            MEDIA_TYPE_MANIFEST,
        ))
    subject_digest = manifest["subject"]["digest"] if has_subject(manifest) else attributed.get(digest)
    if not subject_digest:
        fail("Referrer {} of {} has no subject, so it is not attributed to any manifest of the image.".format(digest, image))
    if subject_digest not in subjects:
        fail("Referrer {} of {} applies to {}, which is neither the pulled manifest nor one of its children ({}).".format(
            digest,
            image,
            subject_digest,
            ", ".join(subjects.keys()),
        ))
    return subject_digest

def referrer_blob_digests(manifest):
    """Return the digests of the config and layers of a referrer manifest.

    Args:
        manifest: The decoded referrer manifest.

    Returns:
        List of blob digests, config first. The empty JSON blob is included: the
        downloaders write it locally instead of fetching it.
    """
    digests = []
    config_digest = manifest.get("config", {}).get("digest")
    if config_digest:
        digests.append(config_digest)
    for layer in manifest.get("layers", []):
        if layer.get("digest") and layer["digest"] not in digests:
            digests.append(layer["digest"])
    return digests

def build_referrer_artifacts(rctx, *, specs, subjects, default_subject):
    """Build the referrer artifacts described by spec files, with the img tool.

    The manifests and their blobs are written to `blobs/sha256/` of the repository.

    Args:
        rctx: Repository context.
        specs: List of labels of referrer artifact spec files.
        subjects: The manifests an artifact may apply to, as returned by subject_descriptors.
        default_subject: Subject of the artifacts that don't name one.

    Returns:
        List of struct(digest, subject), one per built artifact, in spec order.
    """
    if len(specs) == 0:
        return []
    tool_path = rctx.path(tool_for_repository_os(rctx))
    rctx.file("referrer_subjects.json", json.encode(subjects), executable = False)
    args = [
        tool_path,
        "referrer-artifacts",
        "--subjects=referrer_subjects.json",
        "--default-subject=" + default_subject,
        "--output=.",
        "--result=referrer_artifacts.json",
    ] + ["--spec={}".format(rctx.path(spec)) for spec in specs]
    result = rctx.execute(args, **execute_kwargs(rctx))
    if result.return_code != 0:
        fail("Failed to build referrer artifacts: {}{}".format(result.stdout, result.stderr))
    output = json.decode(rctx.read("referrer_artifacts.json"))
    rctx.delete("referrer_subjects.json")
    rctx.delete("referrer_artifacts.json")

    # The specs are watched as label attributes. The files they package are not,
    # so register them: a changed SBOM must not leave a stale artifact behind.
    if hasattr(rctx, "watch"):
        for path in output["inputs"]:
            rctx.watch(path)
    return [struct(digest = artifact["digest"], subject = artifact["subject"]) for artifact in output["artifacts"]]

def discover_referrers_with_tool(ctx, *, subjects, artifact_types, sources, credential_helper = None, docker_config_path = None, timeout = None):
    """Discover the referrers of manifests by artifact type, with `img discover-referrers`.

    Listing referrers per the distribution spec needs the response status (only a
    404 from the referrers API falls back to the referrers tag schema) and headers
    (Link pagination), which Bazel's downloader does not expose. So discovery
    always runs the img tool, whichever downloader fetches the blobs.

    Args:
        ctx: Repository or module context.
        subjects: Digests of the manifests whose referrers to list.
        artifact_types: List of artifact types to keep.
        sources: Sources dict mapping repositories to registries, tried in order.
        credential_helper: Optional credential helper, as in auth_environment.
        docker_config_path: Optional Docker-compatible auth config path, as in auth_environment.
        timeout: Optional timeout in seconds, as in execute_kwargs.

    Returns:
        Dict mapping each subject to a dict of every requested artifact type to the
        sorted digests of its referrers.
    """
    if len(subjects) == 0 or len(artifact_types) == 0:
        return {subject: {artifact_type: [] for artifact_type in artifact_types} for subject in subjects}
    output = "discovered_referrers_{}.json".format(subjects[0].removeprefix("sha256:"))
    args = [ctx.path(tool_for_repository_os(ctx)), "discover-referrers", "--output=" + output]
    args.extend(["--subject=" + subject for subject in subjects])
    args.extend(["--artifact-type=" + artifact_type for artifact_type in artifact_types])
    args.extend(["--source=" + source for source in get_sources_list(sources)])
    result = ctx.execute(
        args,
        environment = auth_environment(ctx, credential_helper = credential_helper, docker_config_path = docker_config_path),
        **execute_kwargs(ctx, timeout)
    )
    if result.return_code != 0:
        fail("Failed to discover referrers: {}{}".format(result.stdout, result.stderr))
    discovered = json.decode(ctx.read(output))
    ctx.delete(output)
    return discovered

def flatten_discovered(found):
    """Return the sorted, deduplicated referrer digests of a discovery result.

    Args:
        found: Dict mapping artifact types to referrer digests.

    Returns:
        Sorted list of digests.
    """
    digests = {}
    for referrers in found.values():
        for digest in referrers:
            digests[digest] = True
    return sorted(digests.keys())
