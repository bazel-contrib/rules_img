"""Factory for creating release_files test targets."""

def _release_files_test_impl(ctx):
    """Run the update_release_files tool in test mode."""

    # Create wrapper script
    wrapper = ctx.actions.declare_file(ctx.label.name + ".sh")

    substitutions = {
        "{TOOL}": ctx.executable.update_release_files_binary.short_path,
        "{MODULE_BAZEL}": ctx.file.module_bazel.short_path,  # MODULE.bazel
    }

    ctx.actions.expand_template(
        template = ctx.file._wrapper_template,
        output = wrapper,
        substitutions = substitutions,
        is_executable = True,
    )

    # Gather runfiles including the tool
    runfiles = ctx.runfiles(files = [wrapper, ctx.file.module_bazel])
    runfiles = runfiles.merge(ctx.attr.update_release_files_binary[DefaultInfo].default_runfiles)

    return [DefaultInfo(
        executable = wrapper,
        runfiles = runfiles,
    )]

_release_files_test = rule(
    implementation = _release_files_test_impl,
    test = True,
    attrs = {
        "update_release_files_binary": attr.label(
            mandatory = True,
            executable = True,
            cfg = "exec",
        ),
        "module_bazel": attr.label(
            allow_single_file = True,
        ),
        "_wrapper_template": attr.label(
            default = "//img/private/release/gazelle_plugin:test_wrapper.sh",
            allow_single_file = True,
        ),
    },
)

def release_files_test(
        name,
        update_release_files_binary,
        tags = None,
        **kwargs):
    """Factory function that creates a test to check if release_files is up to date.

    Args:
        name: name of the test
        update_release_files_binary: the update_release_files binary target to test with
        tags: additional tags for the test
        **kwargs: additional arguments to pass to the test rule
    """

    if tags == None:
        tags = []
    else:
        # Make a copy so we don't modify the input
        tags = list(tags)

    # Add tags that prevent remote execution
    tags.extend(["no-sandbox", "no-remote", "no-cache", "local"])

    _release_files_test(
        name = name,
        update_release_files_binary = update_release_files_binary,
        tags = tags,
        module_bazel = kwargs.pop("module_bazel", "//:MODULE.bazel"),
        **kwargs
    )
