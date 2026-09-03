"""Gate for referencing an action's inputs from a tree artifact with symlinks."""

load("@bazel_features//:features.bzl", "bazel_features")

def use_tree_symlinks(tool):
    """Reports whether a directory output may reference its inputs via relative symlinks.

    Actions that only copy files into a tree artifact can instead emit relative
    symlinks to the input files, so content shared between outputs (base image
    blobs, layer input files) is referenced rather than re-materialized per
    output. Bazel 7.1 is the first version that permits symlinks pointing
    outside of a tree artifact (bazelbuild/bazel#21263), and the symlinks must
    be relative to stay valid under remote execution and in runfiles trees.

    Symlinks are skipped on Windows exec platforms: Windows hardlinks to reparse
    points propagate the (now-misplaced) relative target to the new file, turning
    it into a dangling symlink when downstream tools copy files out of the tree.

    Only ask for directory (tree artifact) outputs -- tar outputs always embed
    their content.

    Args:
        tool: The executable File the action runs, used to detect the exec platform.

    Returns:
        True if the action should be asked to emit symlinks instead of copies.
    """

    # The img tool is img.exe on Windows exec platforms and img elsewhere.
    if tool.basename.endswith(".exe"):
        return False
    return bazel_features.rules.permits_treeartifact_uplevel_symlinks
