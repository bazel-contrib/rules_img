"""Validation actions that keep a deploy's blobs in Bazel's cache.

Every strategy but `eager` leaves layer blobs out of a deploy target's runfiles
and reads them back out of Bazel's remote cache (CAS) by digest. A blob is in
that cache because some action produced or consumed it, and the cache may evict
it afterwards -- so a blob that no action of the build refers to can be gone by
the time the deploy wants it, with nothing to put it back: the actions that
uploaded it are cache hits from then on, and a remote cache re-checks a cached
action's outputs but not its inputs. A layer whose content comes from
`ctx.actions.write` is the common case.

With `--@rules_img//img/settings:validate_cas_blobs=enabled`, each deploy
operation gets an action that takes all of its blobs as inputs, which is what
makes Bazel keep them in the cache. The action only copies the operation's deploy
manifest; its output goes into Bazel's validation output group, so it is built
along with the target. One action per operation keeps each one's inputs to a
single image.
"""

load("//img/private:soci_deploy.bzl", "soci_deploy_children")
load("//img/private/common:build.bzl", "TOOLCHAIN")
load("//img/private/common:deploy_helpers.bzl", "VALIDATION_OUTPUT_GROUP")

def blobs_from_cas(strategy):
    """Whether `strategy` reads a deploy's blobs from Bazel's cache.

    Args:
        strategy: Resolved push or load strategy string (never 'auto').

    Returns:
        bool: True for every strategy but `eager`, which ships the blobs in the
        deploy target's runfiles instead.
    """
    return strategy != "eager"

def _manifests_of(image):
    """The manifests of `image`, SOCI-index pseudo-children included."""
    if hasattr(image, "manifests"):
        return image.manifests + soci_deploy_children(image.manifests)
    return [image]

def _blobs_of(images):
    """The files a deploy of `images` reads from Bazel's cache."""
    blobs = []
    for image in images:
        for manifest in _manifests_of(image):
            for layer in manifest.layers:
                # The files an eager deploy ships in its runfiles (see
                # `_layer_root_symlinks_for_manifest`), plus the compact stream a
                # compact layer is reconstructed from.
                if layer.blob != None:
                    blobs.append(layer.blob)
                if layer.compact_stream != None:
                    blobs.append(layer.compact_stream)
                if layer.layer_input_files_cas != None:
                    blobs.append(layer.layer_input_files_cas)
    return blobs

def _blob_validation_action(ctx, *, deploy_manifest, images, output_prefix, index):
    """Create one operation's validation action, or None if it has no blobs."""
    blobs = _blobs_of(images)
    if not blobs:
        # An entirely shallow image, say: nothing is read from the cache.
        return None
    blobs = depset(blobs)

    # The action's inputs are the point; copying the deploy manifest is just
    # something to do. `img copy-files` hardlinks where it can.
    validated = ctx.actions.declare_file("{}.blob_validation.{}.json".format(output_prefix, index))
    args = ctx.actions.args()
    args.add("copy-files")
    args.add(deploy_manifest)
    args.add(validated)

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = depset([deploy_manifest], transitive = [blobs]),
        outputs = [validated],
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        mnemonic = "ValidateDeployBlobs",
        progress_message = "Validating deploy blobs of %{label}",
    )
    return validated

def blob_validation_outputs(ctx, *, operations, enabled, from_cas, output_prefix):
    """Create the blob-validation actions of a deploy target, one per operation.

    Args:
        ctx: The rule context (needs the `img` toolchain).
        operations: List of the target's deploy operations, each a
            `struct(deploy_manifest = File, images = [image provider])`. The
            images are the operation's image plus its referrers, if any.
        enabled: Resolved `validate_cas_blobs` setting, as a bool.
        from_cas: Whether this target reads its blobs from Bazel's cache rather
            than from its runfiles (see `blobs_from_cas`).
        output_prefix: Prefix for the declared output files, unique per target.

    Returns:
        List of the validation output Files, empty when no action was created.
    """
    if not enabled or not from_cas:
        return []
    outputs = []
    for i, operation in enumerate(operations):
        validated = _blob_validation_action(
            ctx,
            deploy_manifest = operation.deploy_manifest,
            images = operation.images,
            output_prefix = output_prefix,
            index = i,
        )
        if validated != None:
            outputs.append(validated)
    return outputs

def add_blob_validation_outputs(output_groups, outputs):
    """Add the blob-validation outputs to Bazel's validation output group.

    Args:
        output_groups: Dict of output group name -> depset of Files, updated in
            place.
        outputs: Result of `blob_validation_outputs`; empty is a no-op.
    """
    if not outputs:
        return
    existing = output_groups.get(VALIDATION_OUTPUT_GROUP, depset())
    output_groups[VALIDATION_OUTPUT_GROUP] = depset(outputs, transitive = [existing])
