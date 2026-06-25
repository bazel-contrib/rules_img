"""Rules and fixtures for the layering test suite.

`layer_contents_test` validates that the tar contents of every layer produced by
a target (via `LayersInfo`) match an exhaustive ndjson manifest, one manifest per
layer in order.

`combine_layers` concatenates the layers of several `LayersInfo` providers into a
single ordered `LayersInfo`, used to exercise the multi-layer path.

`binary_with_extra_outputs` is an executable fixture whose `DefaultInfo.files`
holds the executable plus extra default outputs at various offsets relative to
it, exercising the default-output placement of `image_layer`/`layer_from_binary`.
"""

load("@hermetic_launcher//launcher:lib.bzl", "launcher")
load("@rules_img//img:providers.bzl", "LayersInfo")
load("@rules_runfiles_group//runfiles_group:providers.bzl", "RunfilesGroupInfo", "RunfilesGroupMetadataInfo")

def _layer_contents_test_impl(ctx):
    layers = ctx.attr.layer[LayersInfo].layers

    # Each manifest label must provide exactly one file.
    manifest_files = []
    for manifest in ctx.attr.manifests:
        files = manifest[DefaultInfo].files.to_list()
        if len(files) != 1:
            fail("manifest {} must provide exactly one file, got {}".format(manifest.label, len(files)))
        manifest_files.append(files[0])

    if len(layers) != len(manifest_files):
        fail("layer {} produces {} layer(s) but {} manifest(s) were provided".format(
            ctx.attr.layer.label,
            len(layers),
            len(manifest_files),
        ))

    # Build a native, cross-platform launcher (hermetic_launcher) that runs the
    # verifier with each layer blob and its manifest as alternating positional
    # arguments. A generated .sh launcher is not executable on Windows; the
    # native stub works on Linux, macOS, and Windows. Every embedded runfiles
    # path is resolved to an absolute path at runtime, so the verifier just opens
    # absolute paths.
    embedded_args, transformed_args = launcher.args_from_entrypoint(
        executable_file = ctx.executable._verifier,
    )
    blobs = []
    for layer, manifest in zip(layers, manifest_files):
        if layer.blob == None:
            fail("layer {} contains a shallow layer with no blob".format(ctx.attr.layer.label))
        blobs.append(layer.blob)
        embedded_args, transformed_args = launcher.append_runfile(
            file = layer.blob,
            embedded_args = embedded_args,
            transformed_args = transformed_args,
        )
        embedded_args, transformed_args = launcher.append_runfile(
            file = manifest,
            embedded_args = embedded_args,
            transformed_args = transformed_args,
        )

    output_basename = ctx.attr.name
    if ctx.target_platform_has_constraint(ctx.attr._windows_constraint[platform_common.ConstraintValueInfo]):
        output_basename += ".exe"
    exe = ctx.actions.declare_file(output_basename)
    launcher.compile_stub(
        ctx = ctx,
        embedded_args = embedded_args,
        transformed_args = transformed_args,
        output_file = exe,
    )

    runfiles = ctx.runfiles(files = blobs + manifest_files + [ctx.executable._verifier])
    runfiles = runfiles.merge(ctx.attr._verifier[DefaultInfo].default_runfiles)
    return [DefaultInfo(executable = exe, runfiles = runfiles)]

layer_contents_test = rule(
    implementation = _layer_contents_test_impl,
    doc = """Validates the tar contents of every layer of `layer` against `manifests`.

`layer` must provide `LayersInfo`. `manifests` is an ordered list of ndjson files
(one per layer); each line describes one tar entry with all of its tar header
fields and, for regular files, the sha256 of its content. The test fails if any
entry differs or if the number of entries does not match.""",
    attrs = {
        "layer": attr.label(
            mandatory = True,
            providers = [LayersInfo],
            doc = "A target providing LayersInfo (e.g. image_layer or layer_from_binary).",
        ),
        "manifests": attr.label_list(
            allow_files = True,
            mandatory = True,
            doc = "Ordered ndjson manifests, one per layer. Each label must provide exactly one file.",
        ),
        "_verifier": attr.label(
            default = "//tests/layering/verifier",
            executable = True,
            cfg = "target",
        ),
        "_windows_constraint": attr.label(default = "@platforms//os:windows"),
    },
    test = True,
    toolchains = [
        launcher.template_toolchain_type,
        launcher.finalizer_toolchain_type,
    ],
)

def _combine_layers_impl(ctx):
    layers = []
    for dep in ctx.attr.layers:
        layers.extend(dep[LayersInfo].layers)
    return [
        LayersInfo(layers = layers),
        DefaultInfo(files = depset([layer.blob for layer in layers if layer.blob != None])),
    ]

combine_layers = rule(
    implementation = _combine_layers_impl,
    doc = "Concatenates the layers of several LayersInfo providers into one ordered LayersInfo.",
    attrs = {
        "layers": attr.label_list(
            providers = [LayersInfo],
            doc = "Targets providing LayersInfo, concatenated in order.",
        ),
    },
    provides = [LayersInfo],
)

def _binary_with_extra_outputs_impl(ctx):
    # The executable lives in a "bin" subdirectory so the extra outputs can be
    # placed next to it, nested under it, and one level above it.
    exe = ctx.actions.declare_file("{}/bin/launcher".format(ctx.label.name))
    ctx.actions.symlink(
        output = exe,
        target_file = ctx.file.binary,
        is_executable = True,
    )
    sidecar = ctx.actions.declare_file("{}/bin/sidecar.txt".format(ctx.label.name))
    nested = ctx.actions.declare_file("{}/bin/assets/logo.txt".format(ctx.label.name))
    parent = ctx.actions.declare_file("{}/config.yaml".format(ctx.label.name))
    ctx.actions.write(sidecar, "sidecar-content\n")
    ctx.actions.write(nested, "logo-content\n")
    ctx.actions.write(parent, "config-content\n")
    return [DefaultInfo(
        files = depset([exe, sidecar, nested, parent]),
        runfiles = ctx.runfiles(files = ctx.files.data),
        executable = exe,
    )]

binary_with_extra_outputs = rule(
    implementation = _binary_with_extra_outputs_impl,
    doc = "Executable fixture whose DefaultInfo.files holds the executable plus extra default outputs.",
    attrs = {
        "binary": attr.label(allow_single_file = True, cfg = "target"),
        "data": attr.label_list(allow_files = True),
    },
    executable = True,
)

def _files_with_symlink_impl(ctx):
    # A regular file plus a Bazel-native unresolved symlink (declare_symlink),
    # both exposed through DefaultInfo.files.
    regular = ctx.actions.declare_file(ctx.label.name + ".txt")
    ctx.actions.write(regular, "symlink target content\n")
    link = ctx.actions.declare_symlink(ctx.label.name + ".link")
    ctx.actions.symlink(output = link, target_path = ctx.attr.link_target)
    return [DefaultInfo(files = depset([regular, link]))]

files_with_symlink = rule(
    implementation = _files_with_symlink_impl,
    doc = "Fixture whose DefaultInfo.files contains a regular file and a native (declare_symlink) symlink.",
    attrs = {
        "link_target": attr.string(
            default = "target.txt",
            doc = "Literal target the unresolved symlink points at.",
        ),
    },
)

def _binary_with_runfiles_symlink_impl(ctx):
    exe = ctx.actions.declare_file(ctx.label.name)
    ctx.actions.symlink(
        output = exe,
        target_file = ctx.file.binary,
        is_executable = True,
    )

    # A Bazel-native unresolved symlink and a regular file, both placed into the
    # binary's runfiles via ctx.runfiles(files = ...).
    rf_link = ctx.actions.declare_symlink(ctx.label.name + ".runfiles_link")
    ctx.actions.symlink(output = rf_link, target_path = ctx.attr.link_target)
    rf_data = ctx.actions.declare_file(ctx.label.name + ".data.txt")
    ctx.actions.write(rf_data, "runfile data\n")

    return [DefaultInfo(
        files = depset([exe]),
        runfiles = ctx.runfiles(files = [rf_link, rf_data]),
        executable = exe,
    )]

binary_with_runfiles_symlink = rule(
    implementation = _binary_with_runfiles_symlink_impl,
    doc = "Executable fixture whose runfiles contain a native (declare_symlink) symlink and a regular file.",
    attrs = {
        "binary": attr.label(allow_single_file = True, cfg = "target"),
        "link_target": attr.string(
            default = "data/target.txt",
            doc = "Literal target the unresolved runfiles symlink points at.",
        ),
    },
    executable = True,
)

def _binary_with_runfiles_groups_impl(ctx):
    exe = ctx.actions.declare_file(ctx.label.name)
    ctx.actions.symlink(
        output = exe,
        target_file = ctx.file.binary,
        is_executable = True,
    )

    # Two runfiles groups: a stable "stdlib" group (rank 0, placed first) and a
    # frequently-changing "app" group (rank 10, placed later). layer_from_binary
    # turns each group into its own layer.
    stdlib_a = ctx.actions.declare_file(ctx.label.name + ".stdlib/libfoo.so")
    stdlib_b = ctx.actions.declare_file(ctx.label.name + ".stdlib/libbar.so")
    ctx.actions.write(stdlib_a, "foo-lib\n")
    ctx.actions.write(stdlib_b, "bar-lib\n")
    stdlib = ctx.runfiles(files = [stdlib_a, stdlib_b])

    app = ctx.actions.declare_file(ctx.label.name + ".app/data.txt")
    ctx.actions.write(app, "app-data\n")
    app_rf = ctx.runfiles(files = [app])

    return [
        DefaultInfo(
            files = depset([exe]),
            runfiles = stdlib.merge(app_rf),
            executable = exe,
        ),
        RunfilesGroupInfo(stdlib = stdlib, app = app_rf),
        RunfilesGroupMetadataInfo(groups = {
            "app": {"rank": 10},
            "stdlib": {"rank": 0},
        }),
    ]

binary_with_runfiles_groups = rule(
    implementation = _binary_with_runfiles_groups_impl,
    doc = "Executable fixture providing RunfilesGroupInfo with two ranked runfiles groups (stdlib, app).",
    attrs = {
        "binary": attr.label(allow_single_file = True, cfg = "target"),
    },
    executable = True,
)
