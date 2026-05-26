"""Shared sparse OCI layout builder functions."""

load(":build.bzl", "TOOLCHAIN")

def build_sparse_oci_layout_for_manifest(ctx, manifest_out, config_out, layers, suffix = ""):
    """Build a sparse OCI layout tree artifact for a single-platform manifest.

    Args:
        ctx: Rule context.
        manifest_out: The manifest file.
        config_out: The config file.
        layers: List of SingleLayerInfo providers.
        suffix: Optional suffix for the output directory name (to avoid conflicts).

    Returns:
        Tree artifact containing the sparse OCI layout.
    """
    output = ctx.actions.declare_directory(ctx.label.name + "_sparse_oci_layout" + suffix)

    args = ctx.actions.args()
    args.add("sparse-oci-layout")
    args.add("--format", "directory")
    args.add("--manifest", manifest_out.path)
    args.add("--config", config_out.path)
    args.add("--output", output.path)

    inputs = [manifest_out, config_out]

    for layer in layers:
        args.add("--layer", layer.metadata.path)
        inputs.append(layer.metadata)

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = inputs,
        outputs = [output],
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        mnemonic = "SparseOCILayout",
    )

    return output

def build_sparse_oci_layout_for_index(ctx, index_out, manifests):
    """Build a sparse OCI layout tree artifact for a multi-platform image index.

    Args:
        ctx: Rule context.
        index_out: The index file.
        manifests: List of ImageManifestInfo providers.

    Returns:
        Tree artifact containing the sparse OCI layout.
    """
    output = ctx.actions.declare_directory(ctx.label.name + "_sparse_oci_layout")

    args = ctx.actions.args()
    args.add("sparse-oci-layout")
    args.add("--format", "directory")
    args.add("--index", index_out.path)
    args.add("--output", output.path)

    inputs = [index_out]

    for manifest in manifests:
        args.add("--manifest-path", manifest.manifest.path)
        args.add("--config-path", manifest.config.path)
        inputs.append(manifest.manifest)
        inputs.append(manifest.config)

        for layer in manifest.layers:
            args.add("--layer", layer.metadata.path)
            inputs.append(layer.metadata)

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = inputs,
        outputs = [output],
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        mnemonic = "SparseOCIIndexLayout",
    )

    return output
