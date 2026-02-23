"""Host platform tools resolution for cross-platform RBE support.

Resolves the img tool and launcher template toolchains for the host platform
using a platform transition, replacing the "host" exec_group pattern that fails
when the host platform has no registered execution platform (e.g., macOS host
with Linux-only RBE remote executors).

See https://github.com/bazel-contrib/rules_img/issues/417 for background.
"""

load("//img/private/common:build.bzl", "TOOLCHAIN")

# Toolchain type labels from @hermetic_launcher, referenced as strings to avoid
# a load-time dependency on @hermetic_launcher. This is critical because this
# file is loaded from host_tools/BUILD.bazel, and `bazel query @rules_img//...`
# (used by integration tests) must be able to load all BUILD files without
# requiring @hermetic_launcher to be available (it is not declared by
# rules_img_dependencies() for WORKSPACE mode consumers).
#
# These strings are resolved lazily: in rule(toolchains=[...]) they are stored
# and only resolved at analysis time, and in ctx.toolchains[key] they are
# resolved when the function is called.
_TEMPLATE_TOOLCHAIN_TYPE = "@hermetic_launcher//launcher:template_toolchain_type"
_FINALIZER_TOOLCHAIN_TYPE = "@hermetic_launcher//launcher:finalizer_toolchain_type"

HostToolsInfo = provider(
    doc = "Provides host-platform resolved tool binaries for cross-platform RBE.",
    fields = {
        "img_tool_exe": "The img tool executable resolved for the host platform.",
        "template_exe": "The launcher template executable resolved for the host platform.",
    },
)

def _host_tools_impl(ctx):
    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    template_toolchain = ctx.toolchains[_TEMPLATE_TOOLCHAIN_TYPE]
    return [HostToolsInfo(
        img_tool_exe = img_toolchain_info.tool_exe,
        # Note: "tempalte" is a known typo in the upstream hermetic_launcher API.
        template_exe = template_toolchain.tempaltetoolchaininfo.template_exe,
    )]

host_tools = rule(
    implementation = _host_tools_impl,
    doc = """Resolves img tool and launcher template for the host platform.

    This rule is intended to be used with `host_platform_transition` as a
    `cfg` on a private attribute. It resolves the img tool and launcher
    template toolchains via `target_compatible_with` matching against the
    host platform, which works even when the host is not a registered
    execution platform (as is common in cross-platform RBE setups).
    """,
    attrs = {},
    toolchains = [TOOLCHAIN, _TEMPLATE_TOOLCHAIN_TYPE],
)

def compile_host_stub(ctx, *, embedded_args, transformed_args, output_file, template):
    """Compile a hermetic launcher stub with a pre-resolved host-platform template.

    This is equivalent to `launcher.compile_stub` but accepts a pre-resolved
    template binary (from `HostToolsInfo`) instead of resolving it from an
    exec_group or toolchain. This is necessary because the host-platform
    template is resolved via a target-platform transition rather than an
    exec_group.

    Args:
        ctx: The rule context. Must have `launcher.finalizer_toolchain_type`
            in its toolchains.
        embedded_args: List of embedded arguments for the launcher.
        transformed_args: List of transformed argument indices.
        output_file: The output file for the compiled stub.
        template: The pre-resolved launcher template binary (File).

    Returns:
        The output file.
    """
    finalizer = ctx.toolchains[_FINALIZER_TOOLCHAIN_TYPE].finalizer_info.finalizer
    args = ctx.actions.args()
    args.add("--template", template)
    args.add("-o", output_file)
    args.add_joined("--transform", transformed_args, join_with = ",")
    args.add("--")
    args.add_all(embedded_args)
    ctx.actions.run(
        outputs = [output_file],
        executable = finalizer,
        arguments = [args],
        inputs = [template],
        mnemonic = "CompileHostStub",
    )
    return output_file
