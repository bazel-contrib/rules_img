"""Rules for cleaning MODULE.bazel files before release."""

def clean_module_bazel(name, src, **kwargs):
    """Clean a MODULE.bazel file by removing REMOVE_BEFORE_RELEASE sections.

    Args:
        name: Name of the target
        src: Source MODULE.bazel file
        **kwargs: Additional arguments to pass to genrule
    """
    native.genrule(
        name = name,
        srcs = [src],
        outs = [name + ".bazel"],
        cmd = "$(location @rules_img_internal_tools//release/module_bazel_release_prep:module_bazel_release_prep) $(SRCS) $@",
        tools = ["@rules_img_internal_tools//release/module_bazel_release_prep:module_bazel_release_prep"],
        **kwargs
    )
