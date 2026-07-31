"""Shared helper to run img oci-layout with optional uplevel symlinks."""

load("@bazel_features//:features.bzl", "bazel_features")
load("//img/private/common:build.bzl", "TOOLCHAIN")

def run_oci_layout_action(ctx, *, format, output, args, inputs, mnemonic):
    """Run `img oci-layout`, emitting relative symlinks for directory outputs on Bazel >= 7.1.

    Directory TreeArtifact outputs use --symlink so that shared base-image blobs
    are symlinked rather than copied into every layout, matching rules_oci's
    shared-base model.  The img tool produces relative symlinks, which are safe
    under remote execution and runfiles trees.  Tar outputs always embed blobs.

    Args:
        ctx: Rule context.
        format: Output format, either "directory" or "tar".
        output: The declared output artifact (TreeArtifact or File).
        args: An ctx.actions.args() object with all flags except --output.
        inputs: List of input files for the action.
        mnemonic: Action mnemonic string.
    """
    if format == "directory" and bazel_features.rules.permits_treeartifact_uplevel_symlinks:
        args.add("--symlink")

    args.add("--output", output.path)

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = inputs,
        outputs = [output],
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        env = {"RULES_IMG": "1"},
        mnemonic = mnemonic,
    )
