"""Shared helper to run img oci-layout with optional uplevel symlinks."""

load("//img/private/common:build.bzl", "TOOLCHAIN")
load("//img/private/common:tree_symlinks.bzl", "use_tree_symlinks")

def run_oci_layout_action(ctx, *, format, output, args, inputs, mnemonic):
    """Run `img oci-layout`, emitting relative symlinks for directory outputs on Bazel >= 7.1.

    Directory TreeArtifact outputs use --symlink so that shared base-image blobs
    are symlinked rather than copied into every layout, matching rules_oci's
    shared-base model (see use_tree_symlinks for when this applies).  Tar outputs
    always embed blobs.

    Args:
        ctx: Rule context.
        format: Output format, either "directory" or "tar".
        output: The declared output artifact (TreeArtifact or File).
        args: An ctx.actions.args() object with all flags except --output.
        inputs: List of input files for the action.
        mnemonic: Action mnemonic string.
    """
    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    tool = img_toolchain_info.tool_exe

    if format == "directory" and use_tree_symlinks(tool):
        args.add("--symlink")

    args.add("--output", output.path)

    ctx.actions.run(
        inputs = inputs,
        outputs = [output],
        executable = tool,
        arguments = [args],
        env = {"RULES_IMG": "1"},
        mnemonic = mnemonic,
    )
