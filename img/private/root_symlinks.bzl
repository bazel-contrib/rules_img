"""Helper functions to create a root symlink tree for pushing and loading."""

load("@sha256.bzl", "sha256")
load("//img/private:soci_deploy.bzl", "soci_deploy_children")

def runfiles_slot(index_info, manifest_info):
    """Returns the runfiles slot holding an image's files for `img deploy`.

    The slot is recorded in each deploy operation (`--runfiles-slot`) and names
    the directory, below the root symlinks prefix, holding the image's sparse OCI
    layout and layer blobs. It is derived from the image alone, so every
    operation deploying the same image resolves to the same files, whichever
    deploy manifest or multi_deploy the operation ends up in.

    Args:
        index_info: ImageIndexInfo provider, or None
        manifest_info: ImageManifestInfo provider, or None

    Returns:
        str: the slot name
    """
    image = index_info if index_info != None else manifest_info
    return sha256(image.sparse_oci_layout.path)[:32]

def _layer_root_symlinks_for_manifest(manifest_info, slot, manifest_index, symlink_name_prefix):
    base_path = "{}{}/manifests/{}/layer".format(symlink_name_prefix, slot, manifest_index)
    result = {}
    for (layer_index, layer) in enumerate(manifest_info.layers):
        if layer.blob != None:
            result["{base}/{layer_index}".format(base = base_path, layer_index = layer_index)] = layer.blob

        # For compact-stream layers the blob is not materialized; ship the layer's
        # content-addressed input files (sha256/<hex>) next to the layer entry so
        # the deploy tool can reconstruct the tar from its index.
        if layer.layer_input_files_cas != None:
            result["{base}/{layer_index}.inputfilecas".format(base = base_path, layer_index = layer_index)] = layer.layer_input_files_cas
    return result

def calculate_root_symlinks(index_info, manifest_info, *, include_layers, symlink_name_prefix):
    """Creates a dictionary of symlinks for container image root structure.

    Generates symlinks that organize container image artifacts into a standardized
    directory structure suitable for pushing to registries or loading into container
    runtimes. Uses a single sparse OCI layout tree artifact for manifests, configs,
    and layer descriptors, plus optional individual layer blob symlinks.

    Args:
        index_info: ImageIndexInfo provider for multi-platform images, or None
        manifest_info: ImageManifestInfo provider for single-platform images, or None
        include_layers: bool, whether to include layer blob symlinks
        symlink_name_prefix: str, prefix for naming symlinks

    Returns:
        dict: Mapping of symlink paths to target files
    """
    root_symlinks = {}
    slot = runfiles_slot(index_info, manifest_info)
    if index_info != None:
        root_symlinks["{}{}/sparse_oci_layout".format(symlink_name_prefix, slot)] = index_info.sparse_oci_layout
        if include_layers:
            for i, manifest in enumerate(index_info.manifests):
                root_symlinks.update(_layer_root_symlinks_for_manifest(manifest, slot, i, symlink_name_prefix))

            # SOCI index pseudo-children are pushed as extra index children after
            # the real manifests; ship their ztoc blobs at the matching positional
            # manifest indices so the deploy tool resolves them (the deploy metadata
            # lists them at the same indices; see compute_push_metadata).
            soci_children = soci_deploy_children(index_info.manifests)
            for offset, child in enumerate(soci_children):
                manifest_index = len(index_info.manifests) + offset
                root_symlinks.update(_layer_root_symlinks_for_manifest(child, slot, manifest_index, symlink_name_prefix))
    if manifest_info != None:
        root_symlinks["{}{}/sparse_oci_layout".format(symlink_name_prefix, slot)] = manifest_info.sparse_oci_layout
        if include_layers:
            root_symlinks.update(_layer_root_symlinks_for_manifest(manifest_info, slot, 0, symlink_name_prefix))
    return root_symlinks

def symlink_name_prefix(ctx):
    canonical_repo_name = ctx.label.repo_name if len(ctx.label.repo_name) > 0 else "_main"

    # Hash label_str to avoid deeply nested names exceeding the limit of 256 bytes.
    label_str = "{}//{}:{}".format(canonical_repo_name, ctx.label.package, ctx.label.name)
    return "++rules_img_private++/{}/".format(sha256(label_str))
