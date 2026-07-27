"""Derive SOCI-index pseudo-children for the deploy/push plumbing.

When an image manifest carries a SOCI Index Manifest v2 (see manifest.bzl), the
SOCI index is itself an OCI image manifest whose "layers" are ztoc blobs. To push
it alongside the image, it is modeled as an extra child manifest of the image
index: it rides the exact same sparse-layout, deploy-metadata, and layer-runfiles
machinery as a regular index child. The OCI index already cross-references it (via
`img index --soci-entry`, wired in write_index_json), so these pseudo-children are
NOT added to the index's own manifest-descriptor list -- only to the deploy
plumbing that ships and pushes their blobs.
"""

def soci_deploy_children(manifest_infos):
    """Ordered SOCI-index pseudo-children for a list of image manifests.

    Returns one struct per manifest that has a SOCI index, in manifest order. Each
    struct quacks like an ImageManifestInfo for the shared deploy code: it exposes
    `manifest` (the SOCI index JSON), `config` (the "{}" blob), `descriptor`, and
    `layers` (ztoc pseudo-layers, in SOCI-manifest order), plus platform fields.

    The ztoc layers are resolved positionally by the deploy tool (runfiles
    `manifests/<i>/layer/<j>`), so their order MUST match the SOCI index manifest's
    layer order. The SOCI index is assembled with the ztocs in this same order and
    with no min-layer-size filtering in the Bazel path (see _build_soci_index), so
    the two stay aligned.

    Args:
        manifest_infos: list of ImageManifestInfo providers.

    Returns:
        list of struct(manifest, config, descriptor, layers, os, architecture,
        variant, sources).
    """
    children = []
    for m in manifest_infos:
        if getattr(m, "soci_manifest", None) == None:
            continue
        layers = [
            struct(
                blob = pair.ztoc,
                metadata = pair.metadata,
                layer_input_files_cas = None,
                compact_stream = None,
                sources = [],
            )
            for pair in m.soci_ztocs
        ]
        children.append(struct(
            manifest = m.soci_manifest,
            config = m.soci_config,
            descriptor = m.soci_descriptor,
            layers = layers,
            os = m.os,
            architecture = m.architecture,
            variant = m.variant,
            sources = [],
        ))
    return children
