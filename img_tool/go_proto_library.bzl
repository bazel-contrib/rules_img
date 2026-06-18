"""Go protobuf library rules for generating and managing .pb.go files.

This module provides a wrapper around rules_go's go_proto_library that automatically
generates and writes source files for both standard protobuf (.pb.go) and gRPC
(_grpc.pb.go) generated code. This is used to vendor the generated Go protobuf
source files into the repository for consistency with the project's build system.
"""

load("@bazel_lib//lib:write_source_files.bzl", "write_source_files")
load("@bazel_skylib//rules:diff_test.bzl", "diff_test")
load("@rules_go//proto:def.bzl", _go_proto_library = "go_proto_library")

_grpc_compiler = Label("@rules_go//proto:go_grpc_v2")

def _go_proto_library_impl(name, compilers, filenames, **kwargs):
    grpc_enabled = False
    for compiler in compilers:
        if str(compiler) == str(_grpc_compiler):
            grpc_enabled = True
            break
    filenames = _filenames_from_args(filenames, name, grpc_enabled)
    _go_proto_library(name = name, compilers = compilers, **kwargs)
    for filename in filenames:
        target_name = _target_name_from_filename(name, filename)
        pb_go(
            name = target_name,
            go_proto_library = ":" + name,
            visibility = [":__pkg__"],
            filename = filename,
        )

go_proto_library_rule = macro(
    inherit_attrs = _go_proto_library,
    attrs = {
        "compilers": attr.label_list(mandatory = True, configurable = False),
        "filenames": attr.string_list(
            default = [],
            configurable = False,
            doc = "Explicit list of generated .pb.go and _grpc.pb.go filenames. If empty (default), filenames are inferred from the rule name.",
        ),
    },
    implementation = _go_proto_library_impl,
)

def go_proto_library(*, name, compilers, filenames = [], **kwargs):
    """Creates a Go protobuf library and writes generated source files to the repository.

    This macro wraps rules_go's go_proto_library to automatically generate and vendor
    the resulting .pb.go files into the source tree using write_source_files.

    Args:
        name: Name of the target and base name for generated files.
        compilers: List of protobuf compilers to use (e.g., ["@rules_go//proto:go_proto", "@rules_go//proto:go_grpc_v2"]).
        filenames: Optional list of explicit generated filenames. If empty, filenames are inferred from name and compilers.
        **kwargs: Additional arguments passed to the underlying go_proto_library rule.
    """
    grpc_enabled = "@rules_go//proto:go_grpc_v2" in compilers
    filenames = _filenames_from_args(filenames, name, grpc_enabled)
    generated_files = {
        f: _target_name_from_filename(name, f)
        for f in filenames
    }
    go_proto_library_rule(name = name, compilers = compilers, filenames = filenames, **kwargs)
    write_source_files(
        name = name + ".write_source_files",
        files = generated_files,
        diff_test = False,
    )

    # Assert the checked-in .pb.go files are up to date. We use bazel_skylib's
    # diff_test (rather than write_source_files' built-in diff_test) because the
    # latter resolves the source file label against the main repository, which
    # breaks when this module is consumed as a bazel_dep. Package-relative labels
    # below resolve against this module's repository as intended.
    for out_file, generated_target in generated_files.items():
        diff_test(
            name = "{}_check".format(_target_name_from_filename(name, out_file)),
            file1 = ":" + out_file,
            file2 = ":" + generated_target,
            failure_message = (
                "Generated file {} is out of date. ".format(out_file) +
                "Run `bazel run //{}:{}.write_source_files` to update it.".format(
                    native.package_name(),
                    name,
                )
            ),
        )

def _generated_pb_files(ctx):
    output_groups = ctx.attr.go_proto_library[OutputGroupInfo]
    go_generated_srcs = getattr(output_groups, "go_generated_srcs")
    if go_generated_srcs == None:
        fail("go_proto_library does not have output group 'go_generated_srcs'")
    list = go_generated_srcs.to_list()
    if len(list) < 1:
        fail("Expected at least one file in go_generated_srcs, got 0")
    return list

def _pb_go_impl(ctx):
    generated_pb_files = _generated_pb_files(ctx)
    pg_go = [f for f in generated_pb_files if f.basename == ctx.attr.filename]
    if len(pg_go) != 1:
        fail("Expected exactly one matching .pb.go file with name %s from go_proto_library, got %d" % (ctx.attr.filename, len(pg_go)))
    return [DefaultInfo(files = depset(pg_go))]

pb_go = rule(
    implementation = _pb_go_impl,
    attrs = {
        "go_proto_library": attr.label(providers = [OutputGroupInfo]),
        "filename": attr.string(mandatory = True),
    },
)

def _target_name_from_filename(name, filename):
    mangled_name = filename.replace(".", "_").removesuffix(".go")
    if not mangled_name.startswith(name):
        return name + "_" + mangled_name
    return mangled_name

def _filenames_from_args(filenames, name, grpc_enabled):
    if len(filenames) == 0:
        filenames = [name + ".pb.go"]
        if grpc_enabled:
            filenames.append(name + "_grpc.pb.go")
    return filenames
