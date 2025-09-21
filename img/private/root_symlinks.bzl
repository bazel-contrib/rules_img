"""Helper functions to create a root symlink tree for pushing and loading."""

def _layer_root_symlinks_for_manifest(manifest_info, operation_index, manifest_index):
    base_path = "{}/manifests/{}/layer".format(operation_index, manifest_index)
    return {
        "{base}/{layer_index}".format(base = base_path, layer_index = layer_index): layer.blob
        for (layer_index, layer) in enumerate(manifest_info.layers)
        if layer.blob != None
    }

def _metadata_symlinks_for_manifest(manifest_info, operation_index, manifest_index):
    base_path = "{}/manifests/{}/metadata".format(operation_index, manifest_index)
    return {
        "{base}/{layer_index}".format(base = base_path, layer_index = layer_index): layer.metadata
        for (layer_index, layer) in enumerate(manifest_info.layers)
        if layer.metadata != None
    }

def _root_symlinks_for_manifest(manifest_info, *, include_layers, operation_index, manifest_index):
    base_path = "{}/manifests/{}/".format(operation_index, manifest_index)
    root_symlinks = {
        "{base}manifest.json".format(base = base_path): manifest_info.manifest,
        "{base}config.json".format(base = base_path): manifest_info.config,
    }
    if include_layers:
        root_symlinks.update(_layer_root_symlinks_for_manifest(manifest_info, operation_index, manifest_index))
        root_symlinks.update(_metadata_symlinks_for_manifest(manifest_info, operation_index, manifest_index))
    return root_symlinks

def calculate_root_symlinks(index_info, manifest_info, *, include_layers, operation_index = 0):
    """Creates a dictionary of symlinks for container image root structure.

    Generates symlinks that organize container image artifacts (manifests, configs,
    layers, and metadata) into a standardized directory structure suitable for
    pushing to registries or loading into container runtimes.

    Args:
        index_info: ImageIndexInfo provider for multi-platform images, or None
        manifest_info: ImageManifestInfo provider for single-platform images, or None
        include_layers: bool, whether to include layer blob and metadata symlinks
        operation_index: int, index of the operation in a batch (used for naming)

    Returns:
        dict: Mapping of symlink paths to target files, creating a root directory
              structure with index.json, manifest.json, config.json, and optionally
              layer/ and metadata/ subdirectories
    """
    root_symlinks = {}
    if index_info != None:
        root_symlinks["{}/index.json".format(operation_index)] = index_info.index
        for i, manifest in enumerate(index_info.manifests):
            root_symlinks.update(_root_symlinks_for_manifest(manifest, include_layers = include_layers, operation_index = operation_index, manifest_index = i))
    if manifest_info != None:
        root_symlinks.update(_root_symlinks_for_manifest(manifest_info, include_layers = include_layers, operation_index = operation_index, manifest_index = 0))
    return root_symlinks
