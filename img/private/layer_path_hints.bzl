"""Layer path hints for deploy metadata to support lazy push/load fallback."""

def layer_hints_for_deploy_metadata(ctx, *, index_info, manifest_info, strategy, args, inputs, outputs):
    """Generates layer path hints for deploy metadata when using lazy push strategy.

    This function creates a hints file that maps layer blobs to their metadata files,
    which is used to avoid "split brain" scenarios where some layers exist only in
    remote cache and others only locally when using the lazy push strategy.

    Args:
        ctx: The rule context.
        index_info: ImageIndexInfo provider for multi-platform images, or None.
        manifest_info: ImageManifestInfo provider for single-platform images, or None.
        strategy: Push strategy name. Only "lazy" strategy generates hints.
        args: List to append hint flags to.
        inputs: List of input files to append layer blobs to.
        outputs: List of output files.

    Returns:
        File object containing layer hints, or None if strategy is not "lazy".
    """
    if strategy != "lazy":
        # Only the lazy strategy has the risk of "split brain" where
        # some layers only exist in the remote cache, and some only locally.
        # For all other strategies, no hints are needed.
        return None
    layer_hints_args_file = ctx.actions.args()
    layer_hints_args_file.set_param_file_format("multiline")
    layer_hints_args_file.use_param_file("--layer-hints-paths-file-input=%s", use_always = True)

    # File format of the params file:
    # One line per layer blob path that may exist locally (hence the word hint).
    # (even if layer.blob != None, there's no guarantee it exists locally due to BwoB)
    #
    # Each line looks like this:
    # /path/to/layer/blob.tar.gz\0/path/to/layer/metadata.json
    layers_with_local_blobs = []

    if index_info != None:
        for manifest in index_info.manifests:
            for layer in manifest.layers:
                if layer.blob != None:
                    layers_with_local_blobs.append(layer)
    if manifest_info != None:
        for layer in manifest_info.layers:
            if layer.blob != None:
                layers_with_local_blobs.append(layer)

    for layer in layers_with_local_blobs:
        layer_hints_args_file.add_joined(
            [layer.blob, layer.metadata],
            join_with = "\0",
            uniquify = True,
        )
        inputs.append(layer.metadata)

    layer_hints_file = ctx.actions.declare_file(ctx.label.name + ".layer_path_hints")
    output_args = ctx.actions.args()
    output_args.add("--layer-hints-paths-output", layer_hints_file)
    outputs.append(layer_hints_file)
    args.append(layer_hints_args_file)
    args.append(output_args)
    return layer_hints_file
