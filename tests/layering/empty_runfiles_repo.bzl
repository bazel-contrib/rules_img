"""Repository rule for a Python-free fixture that carries runfiles.empty_filenames.

`runfiles.empty_filenames` is the empty ``__init__.py`` glue that rules_python's
``legacy_create_init`` emits for every package directory in a Python binary's
runfiles tree. It can only be populated by the native ``py_internal`` builtin.
Bazel gates the predeclared ``py_internal`` global to a small allowlist (the
_builtins, bazel_tools, and rules_python repos, plus the tools/build_defs/python
package path), but rules_python re-exports it as a plain struct from
``@rules_python//python/private:py_internal.bzl``, which any repository may load.

This repository rule materializes a small repo whose rule loads that struct and
calls ``merge_runfiles_with_generated_inits_empty_files_supplier``. The result is
a fake "binary" whose runfiles carry empty files in two namespaces -- the main
module and this external repo -- with no Python toolchain, interpreter, or
bootstrap, so the layering golden stays small and host-independent.
"""

_DEF_BZL = '''"""Fixture rule producing a fake binary that carries runfiles.empty_filenames."""

# py_internal is re-exported here as a plain struct; loading it is permitted even
# though the underlying native global is allowlisted (see the repo rule doc).
load("@rules_python//python/private:py_internal.bzl", "py_internal")

def _empty_runfiles_binary_impl(ctx):
    exe = ctx.actions.declare_file(ctx.label.name)
    ctx.actions.write(exe, "#!/bin/sh\\nexit 0\\n", is_executable = True)

    # One regular runfile per source. The native empty-init supplier then
    # synthesizes an empty __init__.py for that directory and every parent, in
    # whichever namespace the source lives (main module vs external repo).
    runfiles = ctx.runfiles(files = [exe] + ctx.files.srcs)
    runfiles = py_internal.merge_runfiles_with_generated_inits_empty_files_supplier(
        ctx = ctx,
        runfiles = runfiles,
    )
    return [DefaultInfo(executable = exe, runfiles = runfiles)]

empty_runfiles_binary = rule(
    implementation = _empty_runfiles_binary_impl,
    executable = True,
    attrs = {"srcs": attr.label_list(allow_files = True)},
)
'''

_BUILD_BAZEL = '''load(":def.bzl", "empty_runfiles_binary")

# A fake binary whose runfiles.empty_filenames span both namespaces:
#   * this external repo -> "../<repo>/extpkg/__init__.py"   (external branch)
#   * the main module    -> "tests/.../mainpkg/__init__.py"  (main branch, no "../")
empty_runfiles_binary(
    name = "fixture",
    srcs = [
        "extpkg/mod.py",
        "@rules_img//tests/layering/testcases/empty_runfiles/mainpkg:mod.py",
    ],
    visibility = ["@rules_img//tests/layering:__subpackages__"],
)
'''

def _empty_runfiles_repo_impl(rctx):
    rctx.file("REPO.bazel", "")
    rctx.file("def.bzl", _DEF_BZL)
    rctx.file("extpkg/mod.py", "value = 1\n")
    rctx.file("BUILD.bazel", _BUILD_BAZEL)

empty_runfiles_repo = repository_rule(implementation = _empty_runfiles_repo_impl)
