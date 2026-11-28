"""Automatic referrers: artifacts (SBOMs, ...) generated for every image.

The image rules call into the toolchain of type
//img:automatic_referrers_toolchain_type, if one is registered, and wrap the
artifacts it returns into OCI artifact manifests whose subject is the image.
"""

load("//img/private:stamp.bzl", "should_stamp")
load("//img/private/common:build.bzl", "TOOLCHAIN")
load("//img/private/common:layer_helper.bzl", "calculate_layer_info")
load("//img/private/common:sparse_oci_layout.bzl", "build_sparse_oci_layout_for_manifest")
load("//img/private/providers:automatic_referrers_info.bzl", "BaseSbomInfo", "ImageSbomInfo", "ReferrersInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")
load("//img/private/supply_chain_tools/gather_metadata:providers.bzl", "TargetWithMetadataInfo", "TransitiveMetadataInfo", "null_transitive_metadata_info")
load("//img/private/supply_chain_tools/gather_metadata:serialization.bzl", "metadata_info_to_json")
load(":aspects.bzl", "automatic_referrers_toolchain", "layer_scans")

_EMPTY_JSON_MEDIA_TYPE = "application/vnd.oci.empty.v1+json"

AUTOMATIC_REFERRERS_ATTRS = {
    "automatic_referrers": attr.string(
        default = "auto",
        values = ["auto", "enabled", "disabled"],
        doc = """Whether to generate automatic referrers (such as an SBOM) for this image.

Automatic referrers are only generated when an automatic referrers toolchain is registered
(for example `@rules_img_sbom//toolchains:cyclonedx`); without one, this attribute has no
effect.

* **`auto`** (default): generate them for container images. An `image_manifest` that
  describes an artifact rather than a container image (it sets `artifact_type`, `subject` or
  `config_media_type`, like a Helm chart or an SBOM) gets none.
* **`enabled`**: always generate them.
* **`disabled`**: never generate them, for example for an ORAS artifact or a Helm chart
  built from regular layers.

See [automatic referrers](/docs/automatic-referrers.md).""",
    ),
}

# Artifact types under which SBOMs are attached to images. CycloneDX and SPDX
# register IANA media types; `sbom/...` is the convention of the Notary Project
# (`notation`) and the policies built on it (for example Kyverno's).
_SBOM_ARTIFACT_TYPES = {
    "application/vnd.cyclonedx": "cyclonedx",
    "application/vnd.cyclonedx+json": "cyclonedx",
    "sbom/cyclone-dx": "cyclonedx",
    "application/spdx+json": "spdx",
    "text/spdx+json": "spdx",
    "sbom/spdx": "spdx",
}

# in-toto predicate types of SBOM attestations (`cosign attest --type cyclonedx`, ...).
_SBOM_PREDICATE_PREFIXES = {
    "https://cyclonedx.org/bom": "cyclonedx",
    "https://spdx.dev/Document": "spdx",
}

def automatic_referrers_wanted(ctx, *, is_artifact = False):
    """Whether this image should get automatic referrers.

    Args:
        ctx: Rule context with AUTOMATIC_REFERRERS_ATTRS and the optional toolchain.
        is_artifact: Whether the image describes an artifact rather than a container image.

    Returns:
        The AutomaticReferrersToolchainInfo to use, or None.
    """
    mode = ctx.attr.automatic_referrers
    if mode == "disabled" or (mode == "auto" and is_artifact):
        return None
    return automatic_referrers_toolchain(ctx)

def _predicate_format(predicate_type):
    for prefix, sbom_format in _SBOM_PREDICATE_PREFIXES.items():
        if predicate_type.startswith(prefix):
            return sbom_format
    return None

# The media types of the layers that are attestations, each a single statement, and how the SBOM
# toolchain reads them: DSSE envelopes (cosign's attestation tags, sha256-<hex>.att), and in-toto
# statements (BuildKit's attestation manifests, Docker Hardened Images' attestations).
_ATTESTATION_LAYER_ENCODINGS = {
    "application/vnd.dsse.envelope.v1+json": "dsse",
    "application/vnd.in-toto+json": "in-toto",
}

# The layer annotations that name an attestation layer's predicate type: cosign's, and the one
# BuildKit (and the in-toto attestation spec's OCI layout) uses.
_PREDICATE_TYPE_ANNOTATIONS = ["predicateType", "in-toto.io/predicate-type"]

def _layer_predicate_type(annotations):
    for key in _PREDICATE_TYPE_ANNOTATIONS:
        if annotations.get(key):
            return annotations[key]
    return ""

def _sboms_of_referrer(referrer, scope):
    """Describe the SBOMs a pulled referrer carries (usually none or one)."""
    layers = referrer.manifest.layers
    layer_annotations = getattr(referrer, "layer_annotations", None) or [{} for _ in layers]

    # Manifests of attestations, each layer a statement whose predicate type the layer's
    # annotations name: cosign's attestation tags (sha256-<hex>.att, DSSE envelopes), and
    # BuildKit's attestation manifests (in-toto statements, an SBOM next to SLSA provenance).
    attestations = []
    for (layer, annotations) in zip(layers, layer_annotations):
        encoding = _ATTESTATION_LAYER_ENCODINGS.get(layer.media_type)
        if encoding == None or layer.blob == None:
            continue
        sbom_format = _predicate_format(_layer_predicate_type(annotations))
        if sbom_format != None:
            attestations.append(struct(
                file = layer.blob,
                format = sbom_format,
                encoding = encoding,
                media_type = layer.media_type,
                artifact_type = referrer.artifact_type,
                digest = referrer.digest,
                scope = scope,
            ))
    if attestations:
        return attestations

    artifact_type = referrer.artifact_type
    encoding = None
    sbom_format = _SBOM_ARTIFACT_TYPES.get(artifact_type)
    if sbom_format != None:
        encoding = "document"
    elif artifact_type.startswith("application/vnd.dev.sigstore.bundle"):
        sbom_format = _predicate_format(referrer.annotations.get("dev.sigstore.bundle.predicateType", ""))
        encoding = "sigstore-bundle"
    elif artifact_type == "application/vnd.in-toto+json":
        sbom_format = _predicate_format(referrer.annotations.get("in-toto.io/predicate-type", ""))
        encoding = "in-toto"
    if sbom_format == None:
        return []
    layers = [layer for layer in layers if layer.blob != None]
    if len(layers) == 0:
        return []

    # The document is the layer of the SBOM's own media type, or the only layer.
    document = layers[0]
    for layer in layers:
        if layer.media_type == artifact_type or layer.media_type.endswith("+json"):
            document = layer
            break
    return [struct(
        file = document.blob,
        format = sbom_format,
        encoding = encoding,
        media_type = document.media_type,
        artifact_type = artifact_type,
        digest = referrer.digest,
        scope = scope,
    )]

_ENCODING_ORDER = {"document": 0, "in-toto": 1, "dsse": 2, "sigstore-bundle": 3}

def _pulled_base_sboms(referrers, manifest_digest):
    sboms = []

    # Referrers of the manifest itself first, then those of the index it came from,
    # which describe every platform.
    subjects = [(manifest_digest, "manifest")]
    if referrers.digest != manifest_digest:
        subjects.append((referrers.digest, "index"))
    for (subject, scope) in subjects:
        found = [sbom for referrer in referrers.referrers.get(subject, []) for sbom in _sboms_of_referrer(referrer, scope)]
        sboms.extend(sorted(found, key = lambda sbom: _ENCODING_ORDER[sbom.encoding]))
    return sboms

def _base_request(ctx, base):
    """Describe the selected base manifest for a manifest request."""
    if ctx.attr.base == None or BaseSbomInfo not in ctx.attr.base:
        return None
    info = ctx.attr.base[BaseSbomInfo]
    position = None
    if info.index_info != None:
        for (i, manifest) in enumerate(info.index_info.manifests):
            if manifest.descriptor == base.descriptor:
                position = i
                break

    if info.image_sbom != None:
        image_sbom = info.image_sbom
        if info.index_info != None:
            # A built index: use the SBOM of the child that was selected.
            image_sbom = image_sbom.manifests[position] if position != None else None
        if image_sbom == None:
            return struct(image = None, sboms = [])
        return struct(image = None, sboms = [
            struct(
                file = sbom.sbom,
                format = sbom.format,
                encoding = "document",
                media_type = sbom.media_type,
                artifact_type = sbom.artifact_type,
                digest = None,
                scope = "manifest",
            )
            for sbom in image_sbom.sboms.values()
        ])

    if info.referrers == None:
        return None
    referrers = info.referrers
    manifest_digest = referrers.digest
    if position != None:
        manifest_digest = referrers.manifest_digests[position]
    image = None
    if info.pull_info != None:
        image = struct(
            repository = info.pull_info.repository,
            registries = info.pull_info.registries,
            digest = manifest_digest,
        )
    return struct(image = image, sboms = _pulled_base_sboms(referrers, manifest_digest))

def _stamp(ctx):
    """The workspace status files to stamp from, or None if this image is not stamped.

    The same decision as for the image's own template expansion: the target's
    `stamp` attribute, the global stamp setting, and Bazel's --stamp.
    """
    if not should_stamp(ctx = ctx, template_strings = ["{{.BUILD_TIMESTAMP}}"]).stamp:
        return None
    return struct(stable_status = ctx.info_file, volatile_status = ctx.version_file)

def _image_name(label):
    """The image's display name: its label, as //package:name (@@repo//package:name outside the main repository).

    Deliberately not shaped like a repository path: where the image is pushed isn't known when its
    referrers are built.
    """
    repo = "@@" + label.repo_name if label.repo_name else ""
    return "{}//{}:{}".format(repo, label.package, label.name)

def _metadata_graph(ctx, deps):
    """Serialize the package metadata of deps, with the image as the root target."""
    infos = [dep[TransitiveMetadataInfo] for dep in deps if TransitiveMetadataInfo in dep]
    direct = [dep.label for dep in deps if TransitiveMetadataInfo in dep and dep[TransitiveMetadataInfo] != null_transitive_metadata_info]
    transitive = [info.transitive for info in infos if info != null_transitive_metadata_info]
    root = TargetWithMetadataInfo(target = ctx.label, metadata = depset(), direct_deps = tuple(direct))
    metadata = TransitiveMetadataInfo(
        transitive = depset([root], transitive = transitive),
        top_level_target = ctx.label,
    )
    graph = ctx.actions.declare_file(ctx.label.name + "_automatic_referrers.graph.json")
    ctx.actions.write(graph, metadata_info_to_json(metadata)[0])
    files = []
    for target in metadata.transitive.to_list():
        for provider in target.metadata.to_list():
            if hasattr(provider, "files"):
                files.append(provider.files)
    return struct(graph = graph, metadata = metadata, files = depset(transitive = files))

def _wrap_artifact(ctx, artifact, subject_descriptor, suffix):
    """Wrap an artifact into an OCI artifact manifest whose subject is the image.

    suffix (appended to the target's name) makes the declared files unique.
    """
    prefix = ctx.label.name + suffix
    layer = calculate_layer_info(
        ctx = ctx,
        media_type = artifact.media_type,
        tar_file = artifact.file,
        metadata_file = ctx.actions.declare_file(prefix + "_layer.json"),
        estargz = False,
        zstd_chunked = False,
        annotations = {"org.opencontainers.image.title": artifact.file.basename},
        digest_modes = ["digest"],
    )
    manifest_out = ctx.actions.declare_file(prefix + "_manifest.json")
    config_out = ctx.actions.declare_file(prefix + "_config.json")
    descriptor_out = ctx.actions.declare_file(prefix + "_descriptor.json")
    digest_out = ctx.actions.declare_file(prefix + "_digest")
    args = ctx.actions.args()
    args.add("manifest")
    args.add("--config-media-type", _EMPTY_JSON_MEDIA_TYPE)
    args.add("--artifact-type", artifact.artifact_type)
    args.add("--omit-platform")
    args.add("--layer-from-metadata", layer.metadata)
    args.add("--subject-descriptor", subject_descriptor)
    for key, value in sorted(getattr(artifact, "annotations", {}).items()):
        args.add("--annotation", "{}={}".format(key, value))
    args.add("--manifest", manifest_out)
    args.add("--config", config_out)
    args.add("--descriptor", descriptor_out)
    args.add("--digest", digest_out)
    ctx.actions.run(
        inputs = [layer.metadata, subject_descriptor],
        outputs = [manifest_out, config_out, descriptor_out, digest_out],
        executable = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo.tool_exe,
        arguments = [args],
        mnemonic = "AutomaticReferrerManifest",
        progress_message = "Creating %s referrer of %%{label}" % artifact.artifact_type,
    )
    return ImageManifestInfo(
        descriptor = descriptor_out,
        manifest = manifest_out,
        config = config_out,
        structured_config = {},
        architecture = "unknown",
        os = "unknown",
        variant = "",
        layers = [layer],
        mtree = None,
        sparse_oci_layout = build_sparse_oci_layout_for_manifest(ctx, manifest_out, config_out, [layer], suffix = suffix),
        soci_manifest = None,
        soci_config = None,
        soci_descriptor = None,
        soci_ztocs = [],
    )

def _wrap_artifacts(ctx, artifacts, subject_info, suffix):
    referrers = []
    sboms = {}
    for (position, artifact) in enumerate(artifacts):
        manifest_info = _wrap_artifact(ctx, artifact, subject_info.descriptor, "{}_{}".format(suffix, position))
        referrers.append(struct(
            manifest_info = manifest_info,
            index_info = None,
            artifact_type = artifact.artifact_type,
            subject = subject_info,
        ))
        sbom_format = getattr(artifact, "sbom_format", None)
        if sbom_format != None:
            if sbom_format in sboms:
                fail("The automatic referrers toolchain returned two {} SBOMs for {}".format(sbom_format, ctx.label))
            sboms[sbom_format] = struct(
                format = sbom_format,
                media_type = artifact.media_type,
                artifact_type = artifact.artifact_type,
                sbom = artifact.file,
                referrer = manifest_info,
            )
    return referrers, sboms

def _sbom_files(sbom_info):
    return [sbom.sbom for sbom in sbom_info.sboms.values()] if sbom_info != None else []

def manifest_automatic_referrers(ctx, *, toolchain, manifest_info, digest, base):
    """Generate the automatic referrers of an image manifest.

    Args:
        ctx: Rule context of the image_manifest.
        toolchain: AutomaticReferrersToolchainInfo (see automatic_referrers_wanted), or None.
        manifest_info: ImageManifestInfo of the image.
        digest: File containing the image's digest.
        base: The selected base ImageManifestInfo, or None.

    Returns:
        struct(providers, output_groups, referrers, info): `info` is the image's
        ReferrersInfo (see referrers_for_destination), or None.
    """
    if toolchain == None:
        return struct(providers = [], output_groups = {}, referrers = [], info = None)
    graph = _metadata_graph(ctx, ctx.attr.layers)
    scans = layer_scans([ctx.attr.base] + ctx.attr.layers) if getattr(toolchain, "scan_layer", None) != None else {}
    base_layer_count = len(base.layers) if base != None else 0
    layers = []
    for (position, layer) in enumerate(manifest_info.layers):
        scan = scans.get(layer.metadata) or (scans.get(layer.blob) if layer.blob != None else None)
        layers.append(struct(
            layer = layer,
            origin = scan.origin if scan != None else ("pulled" if layer.sources else "built"),
            base = position < base_layer_count,
            scan = scan.scan if scan != None else None,
        ))
    request = struct(
        kind = "manifest",
        label = ctx.label,
        name = _image_name(ctx.label),
        stamp = _stamp(ctx),
        digest = digest,
        descriptor = manifest_info.descriptor,
        manifest = manifest_info,
        metadata_graph = graph.graph,
        metadata = graph.metadata,
        metadata_files = graph.files,
        layers = layers,
        base = _base_request(ctx, base) if base != None else None,
    )
    artifacts = toolchain.generate(ctx, toolchain, request)
    referrers, sboms = _wrap_artifacts(ctx, artifacts, manifest_info, "_automatic_referrer")
    sbom_info = ImageSbomInfo(sboms = sboms, manifests = []) if sboms else None
    info = ReferrersInfo(
        referrers = referrers,
        destination_state = struct(
            kind = "manifest",
            toolchain = toolchain,
            label = request.label,
            name = request.name,
            stamp = request.stamp,
            digest = digest,
            descriptor = manifest_info.descriptor,
            subject = manifest_info,
            artifacts = artifacts,
            referrers = referrers,
            sbom_info = sbom_info,
        ),
    )
    providers = [info]
    output_groups = {}
    layer_scan_files = [layer.scan for layer in layers if layer.scan != None]
    if layer_scan_files:
        output_groups["layer_scans"] = depset(layer_scan_files)
    if sbom_info != None:
        providers.append(sbom_info)
        output_groups["sbom"] = depset(_sbom_files(sbom_info))
    return struct(providers = providers, output_groups = output_groups, referrers = referrers, info = info)

def index_automatic_referrers(ctx, *, toolchain, index_info, digest, manifests):
    """Generate the automatic referrers of an image index, and collect its children's.

    The children's automatic referrers and SBOMs are forwarded, so pushing the
    index pushes them along, unless the toolchain drops them (its
    `child_referrers` is "drop") and the index has referrers of its own (an
    index without its own never drops its children's).

    Args:
        ctx: Rule context of the image_index.
        toolchain: AutomaticReferrersToolchainInfo (see automatic_referrers_wanted), or None.
        index_info: ImageIndexInfo of the index.
        digest: File containing the index's digest.
        manifests: The child manifest targets, in the order of index_info.manifests.

    Returns:
        struct(providers, output_groups, referrers, info): `info` is the index's
        ReferrersInfo (see referrers_for_destination), or None.
    """
    child_sboms = [manifest[ImageSbomInfo] if ImageSbomInfo in manifest else None for manifest in manifests]
    child_referrers = []
    for manifest in manifests:
        if ReferrersInfo in manifest:
            child_referrers.extend(manifest[ReferrersInfo].referrers)
    child_sbom_files = [
        sbom.sbom
        for child in child_sboms
        if child != None
        for sbom in child.sboms.values()
    ]

    referrers = []
    artifacts = []
    sbom_info = None
    if toolchain != None:
        request = struct(
            kind = "index",
            label = ctx.label,
            name = _image_name(ctx.label),
            stamp = _stamp(ctx),
            digest = digest,
            descriptor = index_info.descriptor,
            index = index_info,
            manifests = [
                struct(manifest = manifest_info, digest = _digest_file(manifest), sbom = child_sbom)
                for (manifest_info, manifest, child_sbom) in zip(index_info.manifests, manifests, child_sboms)
            ],
        )
        artifacts = toolchain.generate(ctx, toolchain, request)
        referrers, sboms = _wrap_artifacts(ctx, artifacts, index_info, "_automatic_referrer")
        if sboms:
            sbom_info = ImageSbomInfo(sboms = sboms, manifests = child_sboms)
    drop_children = toolchain != None and getattr(toolchain, "child_referrers", "forward") == "drop"
    if referrers and drop_children:
        # The index's own referrers take the place of its children's (an
        # index SBOM aggregating theirs, say): they are not published with it.
        child_referrers = []
        child_sbom_files = []

    providers = []
    info = None
    if referrers or child_referrers:
        info = ReferrersInfo(
            referrers = referrers + child_referrers,
            destination_state = struct(
                kind = "index",
                toolchain = toolchain,
                label = ctx.label,
                name = _image_name(ctx.label),
                stamp = _stamp(ctx),
                digest = digest,
                descriptor = index_info.descriptor,
                subject = index_info,
                artifacts = artifacts,
                own_referrers = referrers,
                sbom_info = sbom_info,
                drop_children = drop_children,
                children = [
                    struct(
                        info = manifest[ReferrersInfo] if ReferrersInfo in manifest else None,
                        sbom = child_sbom,
                        digest = _digest_file(manifest),
                        manifest = manifest_info,
                    )
                    for (manifest_info, manifest, child_sbom) in zip(index_info.manifests, manifests, child_sboms)
                ],
            ),
        )
        providers.append(info)
    output_groups = {}
    if sbom_info != None:
        providers.append(sbom_info)
    sbom_files = _sbom_files(sbom_info) + child_sbom_files
    if sbom_files:
        output_groups["sbom"] = depset(sbom_files)
    child_layer_scans = [
        manifest[OutputGroupInfo].layer_scans
        for manifest in manifests
        if OutputGroupInfo in manifest and hasattr(manifest[OutputGroupInfo], "layer_scans")
    ]
    if child_layer_scans:
        output_groups["layer_scans"] = depset(transitive = child_layer_scans)
    return struct(providers = providers, output_groups = output_groups, referrers = referrers + child_referrers, info = info)

def _destination_request(ctx, state, destination, suffix, manifests = None):
    """The request of the toolchain's `for_destination` for one image of state."""
    fields = dict(
        kind = state.kind,
        label = state.label,
        name = state.name,
        stamp = state.stamp,
        digest = state.digest,
        descriptor = state.descriptor,
        destination = destination,
        output_prefix = ctx.label.name + suffix,
        artifacts = state.artifacts,
    )
    if state.kind == "manifest":
        fields["manifest"] = state.subject
    else:
        fields["index"] = state.subject
        fields["manifests"] = manifests
    return struct(**fields)

def _hook(state):
    return getattr(state.toolchain, "for_destination", None) if state.toolchain != None else None

def _manifest_for_destination(ctx, state, destination, suffix):
    """The automatic referrers of an image manifest, for a push to destination (see _for_destination)."""
    sbom_info = state.sbom_info
    referrers = state.referrers
    hook = _hook(state)
    if hook != None:
        artifacts = hook(ctx, state.toolchain, _destination_request(ctx, state, destination, suffix))
        if artifacts != None:
            referrers, sboms = _wrap_artifacts(ctx, artifacts, state.subject, "_automatic_referrer" + suffix)
            sbom_info = ImageSbomInfo(sboms = sboms, manifests = []) if sboms else None
    return struct(referrers = referrers, sbom_info = sbom_info, sbom_files = _sbom_files(sbom_info))

def _for_destination(ctx, state, destination, suffix):
    """The automatic referrers of the image of state, for a push to destination.

    Returns:
        struct(referrers, sbom_info, sbom_files): the referrers to push, the
        image's ImageSbomInfo for the destination (or None), and the SBOM files
        pushed with it (the children's too, for an index that publishes them).
    """
    if state.kind == "manifest":
        return _manifest_for_destination(ctx, state, destination, suffix)
    hook = _hook(state)

    # An index: first its children (always manifests), whose SBOMs (and their
    # referrers) the index's own SBOM refers to, then the index.
    children = []
    for (position, child) in enumerate(state.children):
        child_state = getattr(child.info, "destination_state", None) if child.info != None else None
        if child_state != None and child_state.kind == "manifest":
            children.append(_manifest_for_destination(ctx, child_state, destination, "{}_m{}".format(suffix, position)))
        else:
            children.append(struct(
                referrers = child.info.referrers if child.info != None else [],
                sbom_info = child.sbom,
                sbom_files = _sbom_files(child.sbom),
            ))
    referrers = state.own_referrers
    sbom_info = state.sbom_info
    if hook != None and state.artifacts:
        manifests = [
            struct(manifest = child.manifest, digest = child.digest, sbom = result.sbom_info)
            for (child, result) in zip(state.children, children)
        ]
        artifacts = hook(ctx, state.toolchain, _destination_request(ctx, state, destination, suffix, manifests = manifests))
        if artifacts != None:
            referrers, sboms = _wrap_artifacts(ctx, artifacts, state.subject, "_automatic_referrer" + suffix)
            sbom_info = ImageSbomInfo(sboms = sboms, manifests = [result.sbom_info for result in children]) if sboms else None
    sbom_files = _sbom_files(sbom_info)
    if not (referrers and state.drop_children):
        for result in children:
            referrers = referrers + result.referrers
            sbom_files = sbom_files + result.sbom_files
    return struct(referrers = referrers, sbom_info = sbom_info, sbom_files = sbom_files)

def referrers_for_destination(ctx, info, *, configuration, destination_file = None, key):
    """The automatic referrers to push with an image to one destination.

    When the automatic referrers toolchain has a `for_destination` function, the
    referrers pushed to a destination (an SBOM that names the image there, say)
    are made from those generated for the image, for that destination: for an
    image index, those of each child manifest too. Otherwise they are the image's
    automatic referrers.

    Args:
        ctx: Rule context of the target that pushes the image (an image with
            push_specs, or image_push). Files are declared in its package.
        info: The image's ReferrersInfo, or None.
        configuration: File with the push's configuration: its expanded templates,
            with the registry and repository pushed to.
        destination_file: File with the destination as `registry/repository`,
            which wins over the configuration's, or None.
        key: A string unique among the destinations of ctx, for file names.

    Returns:
        struct(referrers, sbom_files): the referrers (as in ReferrersInfo.referrers)
        and the SBOM files among them.
    """
    if info == None:
        return struct(referrers = [], sbom_files = [])
    state = getattr(info, "destination_state", None)
    if state == None:
        return struct(referrers = info.referrers, sbom_files = [])
    destination = struct(configuration = configuration, destination_file = destination_file)
    result = _for_destination(ctx, state, destination, "_destination_" + key)
    return struct(referrers = result.referrers, sbom_files = result.sbom_files)

def _digest_file(manifest):
    """The digest output of a child manifest target, if it exposes one."""
    if OutputGroupInfo in manifest and hasattr(manifest[OutputGroupInfo], "digest"):
        files = manifest[OutputGroupInfo].digest.to_list()
        if len(files) == 1:
            return files[0]
    return None
