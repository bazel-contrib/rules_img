"""Helper functions for the images module extension."""

load("@bazel_skylib//lib:sets.bzl", "sets")
load("@pull_hub_repo//:defs.bzl", "tool_for_repository_os")
load("//img/private:manifest_media_type.bzl", "get_media_type", manifest_kind = "kind")
load("//img/private/repository_rules:download.bzl", "download_manifest")

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

    # Merge sources dicts from both target and other
    merged_sources = {}
    if hasattr(target, "sources"):
        for repository, registries in target.sources.items():
            if repository not in merged_sources:
                merged_sources[repository] = []
            registries_set = sets.make(merged_sources[repository])
            for registry in registries:
                sets.insert(registries_set, registry)
            merged_sources[repository] = sets.to_list(registries_set)
    if hasattr(other, "sources"):
        for repository, registries in other.sources.items():
            if repository not in merged_sources:
                merged_sources[repository] = []
            registries_set = sets.make(merged_sources[repository])
            for registry in registries:
                sets.insert(registries_set, registry)
            merged_sources[repository] = sets.to_list(registries_set)
    attrs["sources"] = merged_sources

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
    """Build sources dict from an image struct.

    Args:
        img: Image struct with repository and optional registry/registries fields

    Returns:
        A sources dict with repository as key and list of registries as value
    """
    registries = get_registries_from_image(img)
    return {img.repository: registries}

def get_merged_sources_from_images(image_digests, images_by_digest):
    """Build a merged sources dict from multiple images that serve the same blob.

    When multiple images reference the same blob (layer, config, or manifest),
    this function combines all their sources to maximize download availability.
    All repositories and registries from all images are merged together.

    Args:
        image_digests: List of top-level image digests that reference a blob
        images_by_digest: Dictionary mapping digest to image struct

    Returns:
        A merged sources dict with all repositories and their registries.
        For each repository, registries from all images are combined and deduplicated.
    """
    merged_sources = {}

    for image_digest in image_digests:
        if image_digest not in images_by_digest:
            fail("Image digest '{}' not found in images_by_digest".format(image_digest))

        img = images_by_digest[image_digest]

        # Use the sources field directly from the image struct
        if not hasattr(img, "sources"):
            fail("Image digest '{}' does not have sources field".format(image_digest))

        # Merge sources into merged_sources
        for repository, registries in img.sources.items():
            if repository not in merged_sources:
                merged_sources[repository] = []

            # Add registries, avoiding duplicates using sets
            registries_set = sets.make(merged_sources[repository])
            for registry in registries:
                sets.insert(registries_set, registry)
            merged_sources[repository] = sets.to_list(registries_set)

    return merged_sources

def check_facts_for_manifest(facts, digest):
    """Check if manifest structure is cached in facts.

    Args:
        facts: Facts dictionary from previous extension evaluation
        digest: Manifest digest to check

    Returns:
        ref_graph_entry or None if not cached
    """
    return facts.get("oci_ref_graph@{}".format(digest))

def download_and_parse_manifest(ctx, digest, img, facts, downloader):
    """Download a manifest and parse it into ref graph entry.

    Args:
        ctx: Module extension context
        digest: Manifest digest to download
        img: Image struct with registry/repository info
        facts: Facts dictionary for caching structure (not blob data)
        downloader: Downloader to use

    Returns:
        Tuple of (ref_graph_entry, manifest_data_string)
    """

    # Check if structure is cached in facts
    cached_ref_graph_entry = check_facts_for_manifest(facts, digest)
    sources = get_sources_from_image(img)

    if cached_ref_graph_entry != None:
        # Structure is cached, but we still need to download the blob
        # download_manifest will use its own blob caching
        blob_info = download_manifest(
            ctx,
            downloader = downloader,
            reference = digest,
            sha256 = digest[7:],
            have_valid_digest = True,
            sources = sources,
        )
        return (cached_ref_graph_entry, blob_info.data)

    # Download and parse manifest
    blob_info = download_manifest(
        ctx,
        downloader = downloader,
        reference = digest,
        sha256 = digest[7:],  # Remove "sha256:" prefix
        have_valid_digest = True,
        sources = sources,
    )

    # Parse manifest
    manifest_data = blob_info.data
    manifest = json.decode(manifest_data)
    kind = manifest_kind(get_media_type(manifest))

    if kind not in ["manifest", "index"]:
        fail("Downloaded manifest for digest '{}' has unknown kind '{}'.".format(digest, kind))

    # Build ref graph entry (structure only, not blob data)
    ref_graph_entry = {"kind": kind}
    if kind == "manifest":
        ref_graph_entry["config"] = manifest.get("config", {}).get("digest")
        ref_graph_entry["layers"] = [
            layer.get("digest")
            for layer in manifest.get("layers", [])
            if "digest" in layer
        ]
    elif kind == "index":
        ref_graph_entry["manifests"] = [
            m.get("digest")
            for m in manifest.get("manifests", [])
            if "digest" in m
        ]

    return (ref_graph_entry, manifest_data)

def collect_blobs_to_create(oci_ref_graph, images_by_digest):
    """Determine which blobs need repository rules created.

    Args:
        oci_ref_graph: OCI reference graph
        images_by_digest: Dictionary of images by digest

    Returns:
        Tuple of (manifest_blobs, file_blobs, lazy_file_blobs) where each is a dict of digest -> True
    """
    manifest_blobs = {}  # Manifests and indexes
    file_blobs = {}  # Configs and eager layers
    lazy_file_blobs = {}  # Lazy layers

    # Add all manifests and indexes
    for digest in oci_ref_graph.keys():
        manifest_blobs[digest] = True

    # Add configs and conditionally add layers
    for digest, ref_graph_entry in oci_ref_graph.items():
        if ref_graph_entry["kind"] != "manifest":
            continue

        # Add config blob
        config_digest = ref_graph_entry.get("config")
        if config_digest:
            file_blobs[config_digest] = True

        # Determine layer handling for this manifest
        layer_handling = _get_layer_handling_for_manifest(
            digest,
            oci_ref_graph,
            images_by_digest,
        )

        # Create blob repos for layers based on handling strategy
        if layer_handling == "eager":
            for layer_digest in ref_graph_entry.get("layers", []):
                file_blobs[layer_digest] = True
        elif layer_handling == "lazy":
            for layer_digest in ref_graph_entry.get("layers", []):
                lazy_file_blobs[layer_digest] = True

        # If shallow, don't add layers

    return (manifest_blobs, file_blobs, lazy_file_blobs)

def _get_layer_handling_for_manifest(manifest_digest, oci_ref_graph, images_by_digest):
    """Find the layer_handling setting for a manifest.

    Args:
        manifest_digest: Digest of the manifest
        oci_ref_graph: OCI reference graph
        images_by_digest: Dictionary of images by digest

    Returns:
        Layer handling string ("shallow", "eager", or "lazy")
    """

    # Check if this manifest is a top-level image or referenced by an index
    for top_digest, top_img in images_by_digest.items():
        if manifest_digest == top_digest:
            return top_img.layer_handling
        elif top_digest in oci_ref_graph:
            top_entry = oci_ref_graph[top_digest]
            if top_entry["kind"] == "index" and manifest_digest in top_entry.get("manifests", []):
                return top_img.layer_handling

    return "shallow"  # Default

def build_image_files_dict(digest, oci_ref_graph, layer_handling):
    """Build the files dict mapping digests to blob repo labels for a specific image.

    Args:
        digest: Root digest of the image
        oci_ref_graph: OCI reference graph
        layer_handling: Layer handling mode ("shallow", "eager", or "lazy")

    Returns:
        Dictionary of digest -> label string for referenced blobs
    """
    files = {}

    if digest not in oci_ref_graph:
        fail("Digest '{}' not found in OCI reference graph.".format(digest))

    # Add the root manifest/index
    blob_repo = "blob_{}".format(digest.replace("sha256:", "").replace(":", "_"))
    files[digest] = "@{}//:manifest.json".format(blob_repo)

    entry = oci_ref_graph[digest]

    # Determine if we should include layer files and which repo prefix to use
    include_layers = layer_handling in ["eager", "lazy"]
    layer_repo_prefix = "lazy" if layer_handling == "lazy" else "blob"

    if entry["kind"] == "manifest":
        # Single-platform manifest: add config and optionally layers
        config_digest = entry.get("config")
        if config_digest:
            blob_repo = "blob_{}".format(config_digest.replace("sha256:", "").replace(":", "_"))
            files[config_digest] = "@{}//:blob".format(blob_repo)

        if include_layers:
            for layer_digest in entry.get("layers", []):
                blob_repo = "{}_{}".format(layer_repo_prefix, layer_digest.replace("sha256:", "").replace(":", "_"))
                files[layer_digest] = "@{}//:blob".format(blob_repo)

    elif entry["kind"] == "index":
        # Multi-platform index: add all child manifests, their configs, and optionally their layers
        for child_digest in entry.get("manifests", []):
            # Add child manifest
            blob_repo = "blob_{}".format(child_digest.replace("sha256:", "").replace(":", "_"))
            files[child_digest] = "@{}//:manifest.json".format(blob_repo)

            # Add child manifest's config and optionally layers
            if child_digest in oci_ref_graph:
                child_entry = oci_ref_graph[child_digest]
                if child_entry["kind"] == "manifest":
                    child_config_digest = child_entry.get("config")
                    if child_config_digest:
                        blob_repo = "blob_{}".format(child_config_digest.replace("sha256:", "").replace(":", "_"))
                        files[child_config_digest] = "@{}//:blob".format(blob_repo)

                    if include_layers:
                        for layer_digest in child_entry.get("layers", []):
                            blob_repo = "{}_{}".format(layer_repo_prefix, layer_digest.replace("sha256:", "").replace(":", "_"))
                            files[layer_digest] = "@{}//:blob".format(blob_repo)

    return files

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

    Only stores the OCI reference graph structure (metadata), not actual blob data.
    Blob data caching is handled by download_manifest and download_blob.

    Args:
        oci_ref_graph: OCI reference graph (structure only)

    Returns:
        Dictionary of facts to store
    """
    facts_to_store = {}
    for digest in oci_ref_graph:
        facts_to_store["oci_ref_graph@{}".format(digest)] = oci_ref_graph[digest]
    return facts_to_store

def reachable_facts_to_dict(images_by_digest, facts):
    """Extract reachable facts for top-level images into a dictionary.

    Args:
        images_by_digest: Dictionary mapping digest to image struct
        facts: Facts dictionary from previous extension evaluation

    Returns:
        Dictionary of reachable facts for top-level images
    """
    reachable_facts = {}
    children = sets.make()
    for digest in images_by_digest.keys():
        fact_key = "oci_ref_graph@{}".format(digest)
        if fact_key in facts:
            reachable_facts[fact_key] = facts[fact_key]
            if facts[fact_key]["kind"] == "index":
                for child_digest in facts[fact_key].get("manifests", []):
                    sets.insert(children, child_digest)

    # Now also include all reachable child manifests
    for child_digest in sets.to_list(children):
        fact_key = "oci_ref_graph@{}".format(child_digest)
        if fact_key in facts:
            reachable_facts[fact_key] = facts[fact_key]

    return reachable_facts

def sync_oci_ref_graph(ctx, images_by_digest, facts, downloader):
    """Sync the OCI reference graph by downloading manifests.

    Uses parallel downloading with the img_tool, or falls back to sequential
    downloading for the bazel downloader.

    Args:
        ctx: Module extension context
        images_by_digest: Dictionary mapping digest to image struct
        facts: Facts dictionary from previous extension evaluation
        downloader: Downloader to use ("img_tool" or "bazel")

    Returns:
        Dictionary mapping digest to ref_graph_entry
    """
    ctx.report_progress("Syncing OCI reference graph...")
    oci_ref_graph = {}

    # Use pull_tool to prefetch the full OCI ref graph in parallel
    if downloader == "img_tool":
        # Prepare facts JSON (convert facts to a format expected by the tool)
        facts_json_content = json.encode(reachable_facts_to_dict(images_by_digest, facts))
        ctx.file("facts_input.json", facts_json_content)

        # Prepare images JSON (convert images_by_digest to JSON)
        # Convert struct objects to dicts for JSON encoding
        images_for_json = {}
        for digest, img in images_by_digest.items():
            images_for_json[digest] = {
                "repository": img.repository,
                "registries": img.registries if hasattr(img, "registries") else [],
                "digest": img.digest,
                "tag": img.tag if hasattr(img, "tag") else "",
                "layer_handling": img.layer_handling,
                "sources": img.sources,
            }
        images_json_content = json.encode(images_for_json)
        ctx.file("images_input.json", images_json_content)

        # Execute sync-oci-ref-graph command
        tool = tool_for_repository_os(ctx)
        tool_path = ctx.path(tool)

        result = ctx.execute([
            tool_path,
            "sync-oci-ref-graph",
            "--facts",
            "facts_input.json",
            "--images",
            "images_input.json",
            "--output",
            "facts_output.json",
        ])

        if result.return_code != 0:
            fail("Failed to sync OCI ref graph: {}{}".format(result.stdout, result.stderr))

        # Read updated facts
        updated_facts_json = ctx.read("facts_output.json")
        updated_facts = json.decode(updated_facts_json)

        # Parse updated facts into oci_ref_graph
        for key, value in updated_facts.items():
            if key.startswith("oci_ref_graph@"):
                digest = key.removeprefix("oci_ref_graph@")
                oci_ref_graph[digest] = value
        return oci_ref_graph

    # Fallback to sequential downloading for "bazel" downloader
    # Download top-level manifests/indexes
    for digest, img in images_by_digest.items():
        ref_graph_entry, _manifest_data = download_and_parse_manifest(ctx, digest, img, facts, downloader)
        oci_ref_graph[digest] = ref_graph_entry

    # Download child manifests referenced by indexes
    manifest_to_download_from_index = {}
    for parent_digest, ref_graph_entry in oci_ref_graph.items():
        if ref_graph_entry["kind"] == "index":
            for child_digest in ref_graph_entry["manifests"]:
                if child_digest not in oci_ref_graph:
                    manifest_to_download_from_index[child_digest] = parent_digest

    for digest, index_digest in manifest_to_download_from_index.items():
        img = images_by_digest[index_digest]
        ref_graph_entry, _manifest_data = download_and_parse_manifest(ctx, digest, img, facts, downloader)
        if ref_graph_entry["kind"] != "manifest":
            fail("Expected manifest for digest '{}' but got '{}'.".format(digest, ref_graph_entry["kind"]))
        oci_ref_graph[digest] = ref_graph_entry

    ctx.report_progress("OCI reference graph synced with {} entries.".format(len(oci_ref_graph)))
    return oci_ref_graph
