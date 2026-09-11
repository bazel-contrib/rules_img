"""Helper functions for the images module extension."""

load("@bazel_skylib//lib:sets.bzl", "sets")
load("@img_toolchain//:defs.bzl", "tool_for_repository_os")
load("//img/private:manifest_media_type.bzl", "get_media_type", manifest_kind = "kind")
load("//img/private/repository_rules:download.bzl", "auth_environment", "download_blob", "download_manifest")
load("//img/private/repository_rules:repo_names.bzl", "BLOB", "LAZY_BLOB", "MANIFEST_BLOB", "blob_label")

# Facts persist in MODULE.bazel.lock across changes to this code, so the key encodes the schema:
# v2 entries additionally carry the descriptors of an index, which is what lets the extension
# build a routing repository without reading the index blob again.
GRAPH_FACT_PREFIX = "oci_ref_graph_v2@"

# Platform of a manifest whose index descriptor declares none, read from its config once.
PLATFORM_FACT_PREFIX = "image_platform_v1@"

def pull_tag_to_struct(tag):
    """Convert a pull tag to a struct for easier attribute access.

    Args:
        tag: Pull tag object with repository, digest, and optional registry/tag fields

    Returns:
        Struct with normalized pull attributes including sources dict
    """
    registries = []
    if tag.registry:
        registries.append(tag.registry)
    if tag.registries:
        registries.extend(tag.registries)
    vals = {
        "repository": tag.repository,
        "registries": registries,
        "digest": tag.digest,
        "layer_handling": tag.layer_handling,
        "sources": {tag.repository: registries},
    }
    if tag.tag:
        vals["tag"] = tag.tag
    return struct(**vals)

def merge_pull_attrs(target, other, other_is_root):
    """Merge pull attributes into a single struct for repository rule.

    Args:
        target: Target pull attributes struct
        other: Other pull attributes struct to merge
        other_is_root: Whether the other attributes are from the root module

    Returns:
        Merged struct with combined pull attributes including merged sources
    """
    attrs = {
        "digest": target.digest or other.digest,
    }
    if other_is_root and hasattr(other, "tag"):
        attrs["tag"] = other.tag
    elif hasattr(target, "tag"):
        attrs["tag"] = target.tag
    elif hasattr(other, "tag"):
        attrs["tag"] = other.tag

    if target.repository == other.repository:
        # if the repositories match, we can merge registries
        registries = sets.make()
        if hasattr(target, "registries"):
            for reg in target.registries:
                sets.insert(registries, reg)
        if hasattr(other, "registries"):
            for reg in other.registries:
                sets.insert(registries, reg)
        registries = sorted(sets.to_list(registries))
        attrs["registries"] = registries
        attrs["repository"] = target.repository
    else:
        # otherwise, we cannot merge registries
        if other_is_root:
            if hasattr(other, "registries"):
                attrs["registries"] = other.registries
        elif hasattr(target, "registries"):
            attrs["registries"] = target.registries
        attrs["repository"] = other.repository if other_is_root else target.repository

    attrs["sources"] = merge_source_maps(
        getattr(target, "sources", {}),
        getattr(other, "sources", {}),
    )

    # Layer handling logic:
    # 1. If one is shallow and the other is not, always prefer the non-shallow one
    # 2. Between lazy and eager:
    #    - If one is from root and the other is not, prefer the root setting
    #    - Otherwise, prefer eager over lazy
    target_handling = getattr(target, "layer_handling", "shallow")
    other_handling = getattr(other, "layer_handling", "shallow")

    if target_handling == "shallow":
        # Target is shallow, prefer other regardless of what it is
        attrs["layer_handling"] = other_handling
    elif other_handling == "shallow":
        # Other is shallow, prefer target
        attrs["layer_handling"] = target_handling
    elif other_is_root:
        # Both are non-shallow (lazy or eager), and other is from root
        # Prefer the root setting
        attrs["layer_handling"] = other_handling
    else:
        # Both are non-shallow, but other is not from root
        # Use existing preference: eager > lazy
        layer_handling_priority = {
            "eager": 3,
            "lazy": 2,
        }
        if layer_handling_priority[target_handling] >= layer_handling_priority[other_handling]:
            attrs["layer_handling"] = target_handling
        else:
            attrs["layer_handling"] = other_handling

    return struct(**attrs)

def get_registries_from_image(img):
    """Extract registries list from an image struct.

    Args:
        img: Image struct with optional registry/registries fields

    Returns:
        List of registries (may be empty)
    """
    registries = []
    if hasattr(img, "registries"):
        registries = img.registries
    if hasattr(img, "registry") and img.registry:
        registries = [img.registry] + registries
    return registries

def get_sources_from_image(img):
    """Build the sources dict of an image: every location that can serve its blobs.

    A digest may be declared by several modules under different repositories, and
    `merge_pull_attrs` collects all of them. Blobs are content addressed, so any of those
    locations serves them; dropping all but the preferred one turns a mirror that happens to be
    unreachable into a failure of the whole extension.

    Args:
        img: Image struct with a sources dict, or with repository/registries fields

    Returns:
        A sources dict with repository as key and list of registries as value
    """
    if hasattr(img, "sources") and img.sources:
        return dict(img.sources)
    return {img.repository: get_registries_from_image(img)}

def merge_source_maps(target, other):
    """Combine two sources dicts, preserving the order registries were declared in.

    Args:
        target: Sources dict to merge into.
        other: Sources dict to merge.

    Returns:
        A new sources dict holding every repository and registry of both.
    """
    merged = {}
    for sources in [target, other]:
        for repository, registries in sources.items():
            known = merged.setdefault(repository, [])
            for registry in registries:
                if registry not in known:
                    known.append(registry)
    return merged

def get_merged_sources_from_images(image_digests, images_by_digest):
    """Build a merged sources dict from multiple images that serve the same blob.

    When multiple images reference the same blob (layer, config, or manifest),
    this function combines all their sources to maximize download availability.
    All repositories and registries from all images are merged together.

    Args:
        image_digests: List of top-level image digests that reference a blob
        images_by_digest: Dictionary mapping digest to image struct

    Returns:
        A merged sources dict with all repositories and their registries, in the order they were
        declared in.
    """
    merged_sources = {}

    for image_digest in image_digests:
        if image_digest not in images_by_digest:
            fail("Image digest '{}' not found in images_by_digest".format(image_digest))

        img = images_by_digest[image_digest]

        # Use the sources field directly from the image struct
        if not hasattr(img, "sources"):
            fail("Image digest '{}' does not have sources field".format(image_digest))

        merged_sources = merge_source_maps(merged_sources, img.sources)

    return merged_sources

def _list_field(entry, key):
    """Read a list field of a reference graph entry, tolerating a missing or null value."""
    value = entry.get(key)
    return value if type(value) == "list" else []

def check_facts_for_manifest(facts, digest):
    """Return the cached reference graph entry of a manifest, if it is usable.

    Args:
        facts: Facts dictionary from previous extension evaluation
        digest: Manifest digest to check

    Returns:
        ref_graph_entry, or None if it is not cached or predates the current schema
    """
    entry = facts.get(GRAPH_FACT_PREFIX + digest)
    if entry == None:
        return None
    kind = entry.get("kind")
    if kind == "manifest":
        return entry
    if kind == "index" and _list_field(entry, "manifests") == _descriptor_digests(entry):
        return entry

    # An index entry whose descriptors do not line up with its children cannot route anything, so
    # it counts as absent and gets rediscovered rather than silently degrading platform selection.
    return None

def _descriptor_digests(entry):
    """The digests of an index entry's descriptors, in index order."""
    return [descriptor.get("digest") for descriptor in _list_field(entry, "descriptors")]

def check_is_manifest(digest, entry):
    """Fail unless a reference graph entry describes an image manifest.

    Args:
        digest: Digest the entry describes.
        entry: Reference graph entry.
    """
    if entry["kind"] != "manifest":
        fail("Image index referenced another index ('{}'). Nested indexes are not supported.".format(digest))

def parse_manifest_entry(digest, manifest_data):
    """Turn a downloaded manifest or index blob into a reference graph entry.

    Args:
        digest: Digest the blob was downloaded for.
        manifest_data: The raw blob.

    Returns:
        A reference graph entry: structure and index descriptors, never blob contents.
    """
    manifest = json.decode(manifest_data)
    kind = manifest_kind(get_media_type(manifest))
    if kind not in ["manifest", "index"]:
        fail("Downloaded manifest for digest '{}' has unknown kind '{}'.".format(digest, kind))

    if kind == "manifest":
        return {
            "kind": kind,
            "config": manifest.get("config", {}).get("digest"),
            "layers": [
                layer["digest"]
                for layer in manifest.get("layers", [])
                if "digest" in layer
            ],
        }

    descriptors = manifest.get("manifests", [])
    for (position, descriptor) in enumerate(descriptors):
        if not descriptor.get("digest"):
            fail("Child {} of index '{}' has no digest.".format(position, digest))
    return {
        "kind": kind,
        # Kept whole: the descriptor of a child is what the index says about it, including its
        # annotations and its media type, which a manifest omitting its own mediaType does not
        # carry. The child repository imports its manifest with exactly this descriptor.
        "descriptors": descriptors,
        "manifests": [descriptor["digest"] for descriptor in descriptors],
    }

def download_and_parse_manifest(ctx, digest, sources, facts, downloader, credential_helper = None, docker_config_path = None):
    """Return the reference graph entry of a manifest, downloading it only if it is unknown.

    Args:
        ctx: Module extension context
        digest: Manifest digest
        sources: Sources dict mapping repositories to registries
        facts: Facts dictionary from previous extension evaluation
        downloader: Downloader to use
        credential_helper: Optional credential helper path to pass to the img tool.
        docker_config_path: Optional Docker-compatible auth config path to pass to the img tool.

    Returns:
        A reference graph entry.
    """
    cached_ref_graph_entry = check_facts_for_manifest(facts, digest)
    if cached_ref_graph_entry != None:
        # The blob itself is only needed by the repositories importing it, which fetch it
        # themselves. Downloading it here would make a fully cached evaluation hit the network.
        return cached_ref_graph_entry

    blob_info = download_manifest(
        ctx,
        downloader = downloader,
        reference = digest,
        sha256 = digest[7:],  # Remove "sha256:" prefix
        have_valid_digest = True,
        sources = sources,
        credential_helper = credential_helper,
        docker_config_path = docker_config_path,
    )
    return parse_manifest_entry(digest, blob_info.data)

def image_blob_refs(digest, oci_ref_graph, layer_handling):
    """Map every blob an image refers to, to the kind of repository holding it.

    One traversal, used both to decide which blob repositories to create and to fill the files
    dict of an image repository. Deriving both from the same mapping is what keeps them
    consistent when two images share a manifest but disagree on how to handle its layers: the
    union of what they need gets created, and each image refers to the kind it asked for.

    Args:
        digest: Root digest of the image (manifest or index).
        oci_ref_graph: OCI reference graph.
        layer_handling: Layer handling mode ("shallow", "eager", or "lazy").

    Returns:
        Dictionary of blob digest -> blob repository kind.
    """
    if digest not in oci_ref_graph:
        fail("Digest '{}' not found in OCI reference graph.".format(digest))

    refs = {digest: MANIFEST_BLOB}
    entry = oci_ref_graph[digest]
    if entry["kind"] != "index":
        _add_manifest_blob_refs(refs, entry, layer_handling)
        return refs

    for child_digest in _list_field(entry, "manifests"):
        refs[child_digest] = MANIFEST_BLOB
        if child_digest in oci_ref_graph and oci_ref_graph[child_digest]["kind"] == "manifest":
            _add_manifest_blob_refs(refs, oci_ref_graph[child_digest], layer_handling)
    return refs

def _add_manifest_blob_refs(refs, entry, layer_handling):
    config_digest = entry.get("config")
    if config_digest:
        refs[config_digest] = BLOB
    if layer_handling == "eager":
        for layer_digest in _list_field(entry, "layers"):
            refs[layer_digest] = BLOB
    elif layer_handling == "lazy":
        for layer_digest in _list_field(entry, "layers"):
            refs[layer_digest] = LAZY_BLOB

def collect_blobs_to_create(oci_ref_graph, images_by_digest):
    """Determine which blob repositories need to be created.

    Args:
        oci_ref_graph: OCI reference graph
        images_by_digest: Dictionary of images by digest

    Returns:
        Tuple of (manifest_blobs, file_blobs, lazy_file_blobs) where each is a dict of digest -> True
    """
    manifest_blobs = {digest: True for digest in oci_ref_graph.keys()}
    file_blobs = {}
    lazy_file_blobs = {}

    for digest, img in images_by_digest.items():
        for blob_digest, kind in image_blob_refs(digest, oci_ref_graph, img.layer_handling).items():
            if kind == BLOB:
                file_blobs[blob_digest] = True
            elif kind == LAZY_BLOB:
                lazy_file_blobs[blob_digest] = True

    return (manifest_blobs, file_blobs, lazy_file_blobs)

def build_image_files_dict(digest, oci_ref_graph, layer_handling):
    """Build the files dict mapping digests to blob repo labels for a specific image.

    Args:
        digest: Root digest of the image
        oci_ref_graph: OCI reference graph
        layer_handling: Layer handling mode ("shallow", "eager", or "lazy")

    Returns:
        Dictionary of digest -> label string for referenced blobs
    """
    return {
        blob_digest: blob_label(blob_digest, kind)
        for blob_digest, kind in image_blob_refs(digest, oci_ref_graph, layer_handling).items()
    }

def build_reverse_blob_mappings(oci_ref_graph, images_by_digest):
    """Build reverse mappings from blobs to top-level images that reference them.

    Args:
        oci_ref_graph: OCI reference graph mapping digests to their references
        images_by_digest: Dictionary of top-level images by digest

    Returns:
        Tuple of (file_blob_to_images, manifest_blob_to_images) where:
        - file_blob_to_images: dict mapping layer/config digest -> list of top-level image digests
        - manifest_blob_to_images: dict mapping manifest/index digest -> list of top-level image digests
    """
    file_blob_to_images = {}  # layers and configs -> top-level images
    manifest_blob_to_images = {}  # manifests and indexes -> top-level images

    # Iterate through each top-level image and map all its blobs back to it
    for top_digest in images_by_digest.keys():
        if top_digest not in oci_ref_graph:
            fail("Top-level image digest '{}' not found in OCI reference graph.".format(top_digest))

        entry = oci_ref_graph[top_digest]

        if entry["kind"] == "manifest":
            # Add the manifest itself
            if top_digest not in manifest_blob_to_images:
                manifest_blob_to_images[top_digest] = []
            manifest_blob_to_images[top_digest].append(top_digest)

            # Add config blob
            config = entry.get("config")
            if config:
                if config not in file_blob_to_images:
                    file_blob_to_images[config] = []
                file_blob_to_images[config].append(top_digest)

            # Add layer blobs
            for layer in entry.get("layers", []):
                if layer not in file_blob_to_images:
                    file_blob_to_images[layer] = []
                file_blob_to_images[layer].append(top_digest)

        elif entry["kind"] == "index":
            # Add the index itself
            if top_digest not in manifest_blob_to_images:
                manifest_blob_to_images[top_digest] = []
            manifest_blob_to_images[top_digest].append(top_digest)

            # Process each child manifest
            for child_digest in entry.get("manifests", []):
                # Add child manifest
                if child_digest not in manifest_blob_to_images:
                    manifest_blob_to_images[child_digest] = []
                manifest_blob_to_images[child_digest].append(top_digest)

                # Process the child manifest's contents
                if child_digest in oci_ref_graph:
                    child_entry = oci_ref_graph[child_digest]
                    if child_entry["kind"] == "manifest":
                        # Add config blob
                        config = child_entry.get("config")
                        if config:
                            if config not in file_blob_to_images:
                                file_blob_to_images[config] = []
                            file_blob_to_images[config].append(top_digest)

                        # Add layer blobs
                        for layer in child_entry.get("layers", []):
                            if layer not in file_blob_to_images:
                                file_blob_to_images[layer] = []
                            file_blob_to_images[layer].append(top_digest)

    return (file_blob_to_images, manifest_blob_to_images)

def build_facts_to_store(oci_ref_graph):
    """Build facts dictionary to store in the lockfile.

    Only stores the OCI reference graph structure and the descriptors of an index, never blob
    contents. Blob data caching is handled by download_manifest and download_blob.

    Args:
        oci_ref_graph: OCI reference graph (structure only)

    Returns:
        Dictionary of facts to store
    """
    return {GRAPH_FACT_PREFIX + digest: entry for digest, entry in oci_ref_graph.items()}

def normalize_repository_name(name, repository):
    """Normalize a friendly name and repository into a valid Bazel repository name.

    Bazel repository names must:
    - Start with a letter (A-Z, a-z) or number (0-9)
    - Contain only: letters, digits, dash (-), underscore (_), dot (.)
    - Match pattern: [a-zA-Z0-9][-.\\w]*

    Args:
        name: Optional friendly name (like "ubuntu"). Can be empty or None.
        repository: Repository path (like "library/ubuntu")

    Returns:
        A valid Bazel repository name
    """

    # Use name if provided and non-empty, otherwise use repository
    base = name if name else repository
    if not base:
        fail("Both name and repository cannot be empty")

    # Replace invalid characters with underscores
    # Valid chars are: letters, digits, dash, underscore, dot
    # Invalid chars include: slash, colon, @, etc.
    normalized = ""
    for char in base.elems():
        if char.isalnum() or char in "-_.":
            normalized += char
        else:
            normalized += "_"

    # Ensure it starts with a letter or digit
    if normalized and not (normalized[0].isalnum()):
        fail("Normalized repository name '{}' must start with a letter or digit.".format(normalized))

    # Handle edge case of empty result
    if not normalized:
        fail("Normalized repository name cannot be empty.")

    return normalized

def graph_from_facts(images_by_digest, facts):
    """Return the complete reference graph, if previous evaluations already discovered all of it.

    Args:
        images_by_digest: Dictionary mapping digest to image struct
        facts: Facts dictionary from previous extension evaluation

    Returns:
        Dictionary mapping digest to ref_graph_entry, or None if any part is still unknown.
    """
    oci_ref_graph = {}
    children = []
    for digest in images_by_digest.keys():
        entry = check_facts_for_manifest(facts, digest)
        if entry == None:
            return None
        oci_ref_graph[digest] = entry
        if entry["kind"] == "index":
            children.extend(_list_field(entry, "manifests"))

    for digest in children:
        if digest in oci_ref_graph:
            continue
        entry = check_facts_for_manifest(facts, digest)
        if entry == None:
            return None
        check_is_manifest(digest, entry)
        oci_ref_graph[digest] = entry

    return oci_ref_graph

def reachable_facts(images_by_digest, facts):
    """Collect the reference graph facts of the declared images and of their known children.

    `ctx.facts` cannot be enumerated, so the reachable set is walked instead. A child recorded by
    some other index is part of it, which is what lets a newly declared index reuse the manifests
    it shares with an image discovered earlier.

    Args:
        images_by_digest: Dictionary mapping digest to image struct
        facts: Facts from a previous extension evaluation

    Returns:
        Dictionary of fact key -> reference graph entry.
    """
    collected = {}
    children = []
    for digest in images_by_digest.keys():
        entry = check_facts_for_manifest(facts, digest)
        if entry == None:
            continue
        collected[GRAPH_FACT_PREFIX + digest] = entry
        if entry["kind"] == "index":
            children.extend(_list_field(entry, "manifests"))

    for digest in children:
        if GRAPH_FACT_PREFIX + digest in collected:
            continue
        entry = check_facts_for_manifest(facts, digest)
        if entry != None:
            collected[GRAPH_FACT_PREFIX + digest] = entry

    return collected

def sync_oci_ref_graph(ctx, images_by_digest, facts, downloader, credential_helper = None, docker_config_path = None):
    """Sync the OCI reference graph, downloading the manifests that are not known yet.

    Args:
        ctx: Module extension context
        images_by_digest: Dictionary mapping digest to image struct
        facts: Facts dictionary from previous extension evaluation
        downloader: Downloader to use ("img_tool" or "bazel")
        credential_helper: Optional credential helper path to pass to the img tool.
        docker_config_path: Optional Docker-compatible auth config path to pass to the img tool.

    Returns:
        Dictionary mapping digest to ref_graph_entry
    """
    known_graph = graph_from_facts(images_by_digest, facts)
    if known_graph != None:
        # Nothing left to discover. Returning before touching the downloader also avoids
        # resolving the img toolchain, so an evaluation with an up-to-date lockfile needs
        # neither the network nor the pull tool.
        return known_graph

    ctx.report_progress("Syncing OCI reference graph...")
    if downloader == "img_tool":
        oci_ref_graph = _sync_with_tool(ctx, images_by_digest, facts, credential_helper, docker_config_path)
    else:
        oci_ref_graph = _sync_with_bazel(ctx, images_by_digest, facts, downloader, credential_helper, docker_config_path)
    ctx.report_progress("OCI reference graph synced with {} entries.".format(len(oci_ref_graph)))
    return oci_ref_graph

def _sync_with_tool(ctx, images_by_digest, facts, credential_helper, docker_config_path):
    """Discover the reference graph with the img tool, which downloads manifests in parallel."""

    ctx.file("facts_input.json", json.encode(reachable_facts(images_by_digest, facts)))

    # Convert struct objects to dicts for JSON encoding
    images_for_json = {}
    for digest, img in images_by_digest.items():
        images_for_json[digest] = {
            "repository": img.repository,
            "registries": img.registries if hasattr(img, "registries") else [],
            "digest": img.digest,
            "tag": img.tag if hasattr(img, "tag") else "",
            "layer_handling": img.layer_handling,
            "sources": get_sources_from_image(img),
        }
    ctx.file("images_input.json", json.encode(images_for_json))

    tool_path = ctx.path(tool_for_repository_os(ctx))
    result = ctx.execute(
        [
            tool_path,
            "sync-oci-ref-graph",
            "--facts",
            "facts_input.json",
            "--images",
            "images_input.json",
            "--output",
            "facts_output.json",
        ],
        environment = auth_environment(
            ctx,
            credential_helper = credential_helper,
            docker_config_path = docker_config_path,
        ),
    )
    if result.return_code != 0:
        fail("Failed to sync OCI ref graph: {}{}".format(result.stdout, result.stderr))

    updated_facts = json.decode(ctx.read("facts_output.json"))
    oci_ref_graph = {}
    for key, value in updated_facts.items():
        if key.startswith(GRAPH_FACT_PREFIX):
            oci_ref_graph[key.removeprefix(GRAPH_FACT_PREFIX)] = value
    for digest in images_by_digest.keys():
        if digest not in oci_ref_graph:
            fail(("The img tool reported no reference graph entry for '{}'. This usually means " +
                  "it is older than the ruleset and writes its facts under a different key; " +
                  "check that rules_img_tool matches rules_img.").format(digest))
    return oci_ref_graph

def _sync_with_bazel(ctx, images_by_digest, facts, downloader, credential_helper, docker_config_path):
    """Discover the reference graph with Bazel's downloader, one manifest at a time."""
    oci_ref_graph = {}
    for digest, img in images_by_digest.items():
        oci_ref_graph[digest] = download_and_parse_manifest(
            ctx,
            digest,
            get_sources_from_image(img),
            facts,
            downloader,
            credential_helper = credential_helper,
            docker_config_path = docker_config_path,
        )

    # A child listed by several indexes can be fetched from any of their locations.
    child_sources = {}
    for parent_digest, entry in oci_ref_graph.items():
        if entry["kind"] != "index":
            continue
        parent_sources = get_sources_from_image(images_by_digest[parent_digest])
        for child_digest in _list_field(entry, "manifests"):
            if child_digest in oci_ref_graph:
                continue
            child_sources[child_digest] = merge_source_maps(
                child_sources.get(child_digest, {}),
                parent_sources,
            )

    for child_digest, sources in child_sources.items():
        entry = download_and_parse_manifest(
            ctx,
            child_digest,
            sources,
            facts,
            downloader,
            credential_helper = credential_helper,
            docker_config_path = docker_config_path,
        )
        check_is_manifest(child_digest, entry)
        oci_ref_graph[child_digest] = entry

    return oci_ref_graph

def discover_platforms(ctx, oci_ref_graph, images_by_digest, manifest_blob_to_images, facts, downloader, credential_helper = None, docker_config_path = None):
    """Read the platform of index children whose descriptor declares none.

    The `platform` field of an index descriptor is optional. When it is missing, `image_import`
    falls back to the manifest's config, and so must the routing: without a platform, a
    perfectly usable child could not be selected for any target platform at all. Only the
    configs of those children are read, so an index that declares its platforms - virtually all
    of them do - costs nothing here.

    Args:
        ctx: Module extension context.
        oci_ref_graph: Discovered OCI reference graph.
        images_by_digest: Top-level images keyed by digest.
        manifest_blob_to_images: Manifest digest -> digests of the images referencing it.
        facts: Previously persisted facts, accessed by key.
        downloader: Downloader implementation to use.
        credential_helper: Optional credential helper override.
        docker_config_path: Optional Docker authentication config override.

    Returns:
        Digest-keyed platform facts, for the children that needed one.
    """
    needed = {}
    for digest in images_by_digest.keys():
        entry = oci_ref_graph[digest]
        if entry["kind"] != "index":
            continue
        for descriptor in _list_field(entry, "descriptors"):
            if not descriptor.get("platform"):
                needed[descriptor["digest"]] = digest

    stored = {}
    for digest in sorted(needed.keys()):
        key = PLATFORM_FACT_PREFIX + digest
        platform = facts.get(key)
        if platform == None:
            child = oci_ref_graph.get(digest)
            if child == None:
                fail("Child manifest '{}' of index '{}' was never discovered.".format(digest, needed[digest]))
            config_digest = child.get("config")
            if not config_digest:
                # Nothing declares a platform for this child and it has no config to read one
                # from, so it stays part of the index but is never selected.
                continue
            if digest not in manifest_blob_to_images:
                fail("Could not find source image for manifest digest '{}'.".format(digest))
            blob = download_blob(
                ctx,
                downloader = downloader,
                digest = config_digest,
                sources = get_merged_sources_from_images(manifest_blob_to_images[digest], images_by_digest),
                credential_helper = credential_helper,
                docker_config_path = docker_config_path,
            )
            config = json.decode(blob.data)
            platform = {field: config.get(field, "") for field in ["os", "architecture", "variant"]}
        stored[key] = platform
    return stored
