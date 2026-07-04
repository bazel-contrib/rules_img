"""Rules and fixtures for the layering test suite.

`layer_contents_test` validates that the tar contents of every layer produced by
a target (via `LayersInfo`) match an exhaustive ndjson manifest, one manifest per
layer in order.

`combine_layers` concatenates the layers of several `LayersInfo` providers into a
single ordered `LayersInfo`, used to exercise the multi-layer path.

`binary_with_extra_outputs` is an executable fixture whose `DefaultInfo.files`
holds the executable plus extra default outputs at various offsets relative to
it, exercising the default-output placement of `image_layer`/`layer_from_binary`.

`generated_layer_file` writes a file of an exact size with deterministic content,
used to build a realistic, multi-file layer (the large_files case) that exercises
the compact stream with multi-block content and many CAS references.
"""

load("@hermetic_launcher//launcher:lib.bzl", "launcher")
load("@rules_img//img:providers.bzl", "LayersInfo")
load("@rules_runfiles_group//runfiles_group:providers.bzl", "RunfilesGroupInfo", "RunfilesGroupMetadataInfo")

_COMPACT_LAYERS_SETTING = "@rules_img//img/settings:experimental_compact_layers"

# Signals the verifier (via the test environment) to additionally reconstruct
# each layer from its compact stream and assert byte-for-byte equality with the
# directly-built layer blob.
_RECONSTRUCT_ENV = "RULES_IMG_LAYERING_RECONSTRUCT_COMPACT_STREAM"

# Runfiles root-symlink prefixes under which the per-layer compact stream and the
# content-addressed input-file directory are exposed for the verifier. The
# verifier resolves "<prefix>/<layer index>" via the runfiles library. These must
# stay in sync with the constants in verifier.go.
_COMPACT_STREAM_RUNFILES_PREFIX = "++rules_img_private++/compactstream"
_INPUTFILECAS_RUNFILES_PREFIX = "++rules_img_private++/inputfilecas"

def _compact_layers_transition_impl(settings, attr):
    """Split the `layer` attribute by compact-stream emission.

    When test_compact_stream is False the transition is a no-op single configuration
    that passes the incoming setting through unchanged. When True it produces two
    configurations of the same layer: one with the experimental compact stream disabled
    (a real layer blob) and one with it enabled (a compact stream plus a
    content-addressed directory of the layer's input files).
    """
    if not attr.test_compact_stream:
        return {"default": {_COMPACT_LAYERS_SETTING: settings[_COMPACT_LAYERS_SETTING]}}
    return {
        "disabled": {_COMPACT_LAYERS_SETTING: "disabled"},
        "enabled": {_COMPACT_LAYERS_SETTING: "enabled"},
    }

_compact_layers_transition = transition(
    implementation = _compact_layers_transition_impl,
    inputs = [_COMPACT_LAYERS_SETTING],
    outputs = [_COMPACT_LAYERS_SETTING],
)

def _layer_contents_test_impl(ctx):
    test_compact_stream = ctx.attr.test_compact_stream

    # The split transition exposes the layer per configuration via split_attr.
    # In no-op mode there is a single "default" configuration; in compact-stream mode
    # there are "disabled" (real blob) and "enabled" (CAS index) configurations.
    if test_compact_stream:
        disabled_target = ctx.split_attr.layer["disabled"]
        enabled_target = ctx.split_attr.layer["enabled"]
    else:
        disabled_target = ctx.split_attr.layer["default"]
        enabled_target = None

    layers = disabled_target[LayersInfo].layers

    # Each manifest label must provide exactly one file.
    manifest_files = []
    for manifest in ctx.attr.manifests:
        files = manifest[DefaultInfo].files.to_list()
        if len(files) != 1:
            fail("manifest {} must provide exactly one file, got {}".format(manifest.label, len(files)))
        manifest_files.append(files[0])

    if len(layers) != len(manifest_files):
        fail("layer {} produces {} layer(s) but {} manifest(s) were provided".format(
            disabled_target.label,
            len(layers),
            len(manifest_files),
        ))

    enabled_layers = None
    if test_compact_stream:
        enabled_layers = enabled_target[LayersInfo].layers
        if len(enabled_layers) != len(layers):
            fail("layer {} produces {} layer(s) with the compact stream disabled but {} with it enabled".format(
                disabled_target.label,
                len(layers),
                len(enabled_layers),
            ))

    # Build a native, cross-platform launcher (hermetic_launcher) that runs the
    # verifier with each layer blob and its manifest as alternating positional
    # arguments (resolved to absolute paths at runtime). A generated .sh launcher
    # is not executable on Windows; the native stub works everywhere.
    #
    # When test_compact_stream is set, each layer's compact stream (.cstream) and its
    # content-addressed input directory (.inputfilecas) are NOT passed as arguments
    # (the launcher stub has a small fixed argument budget); they are exposed as
    # runfiles symlinks keyed by layer index. At test time the verifier invokes
    # `img compact-stream reconstruct` on them and asserts the reconstructed stream
    # equals the layer blob byte-for-byte.
    embedded_args, transformed_args = launcher.args_from_entrypoint(
        executable_file = ctx.executable._verifier,
    )
    blobs = []
    root_symlinks = {}
    for i, (layer, manifest) in enumerate(zip(layers, manifest_files)):
        if layer.blob == None:
            fail("layer {} contains a shallow layer with no blob".format(disabled_target.label))
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
        if test_compact_stream:
            enabled_layer = enabled_layers[i]
            if enabled_layer.compact_stream == None or enabled_layer.layer_input_files_cas == None:
                fail(("layer {} produced no compact stream for layer {} when " +
                      "experimental_compact_layers=enabled; set " +
                      "test_compact_stream = False for layer rules that never emit a compact stream").format(
                    disabled_target.label,
                    i,
                ))
            root_symlinks["{}/{}".format(_COMPACT_STREAM_RUNFILES_PREFIX, i)] = enabled_layer.compact_stream
            root_symlinks["{}/{}".format(_INPUTFILECAS_RUNFILES_PREFIX, i)] = enabled_layer.layer_input_files_cas

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

    runfiles = ctx.runfiles(
        files = blobs + manifest_files + [ctx.executable._verifier],
        root_symlinks = root_symlinks,
    )
    runfiles = runfiles.merge(ctx.attr._verifier[DefaultInfo].default_runfiles)

    providers = [DefaultInfo(executable = exe, runfiles = runfiles)]
    if test_compact_stream:
        providers.append(RunEnvironmentInfo(environment = {_RECONSTRUCT_ENV: "1"}))
    return providers

layer_contents_test = rule(
    implementation = _layer_contents_test_impl,
    doc = """Validates the tar contents of every layer of `layer` against `manifests`.

`layer` must provide `LayersInfo`. `manifests` is an ordered list of ndjson files
(one per layer); each line describes one tar entry with all of its tar header
fields and, for regular files, the sha256 of its content. The test fails if any
entry differs or if the number of entries does not match.

When `test_compact_stream` is True (the default), the layer is additionally built with
`experimental_compact_layers=enabled`. At test time the verifier reconstructs
each layer from its compact stream (resolving content against the layer's
content-addressed input directory) via `img compact-stream reconstruct` and asserts the
result is byte-for-byte identical to the layer blob built with the compact stream
disabled. Set `test_compact_stream = False` for layer rules that never emit a compact stream.""",
    attrs = {
        "layer": attr.label(
            mandatory = True,
            providers = [LayersInfo],
            cfg = _compact_layers_transition,
            doc = "A target providing LayersInfo (e.g. image_layer or layer_from_binary).",
        ),
        "manifests": attr.label_list(
            allow_files = True,
            mandatory = True,
            doc = "Ordered ndjson manifests, one per layer. Each label must provide exactly one file.",
        ),
        "test_compact_stream": attr.bool(
            default = True,
            doc = """Whether to also validate compact stream reconstruction.

When True, the layer is built twice (compact stream disabled and enabled); the tar
reconstructed from the enabled build's compact stream must be byte-for-byte
identical to the disabled build's layer blob. Set to False only for layer rules
that never emit a compact stream (e.g. layer_from_tar).""",
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

def _generated_layer_file_impl(ctx):
    # Deterministic test data of an exact size, generated at build time so the CAS
    # stream index can be exercised with realistic, multi-block files without
    # committing large blobs to the repo. The content is a per-file-unique prefix
    # followed by a repeating ASCII pattern, sliced to size_bytes. Because the bytes
    # are fully determined by (seed, size_bytes), the golden manifest is stable and
    # two targets sharing both attributes produce byte-identical files, which the
    # layer tool deduplicates into a single CAS blob plus a hardlink.
    out = ctx.actions.declare_file(ctx.attr.out)
    size = ctx.attr.size_bytes
    if size <= 0:
        content = ""
    else:
        base = "{}:0123456789abcdefghijklmnopqrstuvwxyz-".format(ctx.attr.seed)
        content = (base * (size // len(base) + 1))[:size]
    ctx.actions.write(out, content)
    return [DefaultInfo(files = depset([out]))]

generated_layer_file = rule(
    implementation = _generated_layer_file_impl,
    doc = """Generate a single file of an exact byte size with deterministic content.

Used by the large_files case to exercise the compact stream with realistic
multi-block content and many CAS references without committing large binaries.
The bytes are fully determined by `seed` and `size_bytes`, so the golden manifest
is stable across rebuilds; two targets sharing both attributes are byte-identical
and are therefore hardlink-deduplicated by the layer tool.""",
    attrs = {
        "out": attr.string(mandatory = True, doc = "Output filename, relative to the package."),
        "size_bytes": attr.int(mandatory = True, doc = "Exact size of the generated file, in bytes."),
        "seed": attr.string(
            mandatory = True,
            doc = "Mixed into the content so distinct files get distinct digests; equal seeds + sizes yield identical files.",
        ),
    },
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
