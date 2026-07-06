"""`image_structure_test`: validate an image's structure from its config + mtree.

This provides a container-structure-test-compatible test rule that checks a
container image using only its image config JSON and its mtree filesystem
listing -- never the layer blobs. An aspect on the `image` attribute normalizes
any supported image (a rules_img `image_manifest`/`image_index`, or an OCI image
layout directory as produced by rules_oci) into an `ImageStructureTestInfo`, and
the test rule wraps the `img image-structure-test` subcommand with the hermetic
launcher.
"""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("@hermetic_launcher//launcher:lib.bzl", "launcher")
load("//img/private/common:build.bzl", "TOOLCHAIN", "TOOLCHAINS")
load("//img/private/providers:deploy_tool_info.bzl", "DeployToolInfo")
load("//img/private/providers:image_structure_test_info.bzl", "ImageStructureTestInfo")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")

# mtree rendering settings, needed only for the OCI-image-layout source (where the
# aspect runs `img oci-layout-metadata` to extract an mtree). The rules_img
# provider and the output-group sources supply a prebuilt mtree, so they need none
# of these.
_ASPECT_MTREE_ATTRS = {
    "_mtree_path_prefix": attr.label(
        default = Label("//img/settings:mtree_path_prefix"),
        providers = [BuildSettingInfo],
    ),
    "_mtree_options": attr.label(
        default = Label("//img/settings:mtree_options"),
        providers = [BuildSettingInfo],
    ),
    "_mtree_image_layout": attr.label(
        default = Label("//img/settings:mtree_image_layout"),
        providers = [BuildSettingInfo],
    ),
}

def _platform_struct(manifest_info):
    return struct(
        os = manifest_info.os,
        architecture = manifest_info.architecture,
        variant = manifest_info.variant,
    )

def _manifest_image(manifest_info):
    """Spec entry + runfiles files for one rules_img manifest (ImageManifestInfo).

    Uses the manifest's own `mtree` (built by the image rule) and `config` -- no
    layer blob is read and nothing is rebuilt in the aspect. `mtree` may be None
    (an image with no tar layers, or a provider from a rule that does not populate
    it), in which case only the config-based metadata checks can run.

    Returns (image_struct, files_list).
    """
    mtree_file = getattr(manifest_info, "mtree", None)
    files = [manifest_info.config]
    mtree_rlocation = ""
    if mtree_file != None:
        files.append(mtree_file)
        mtree_rlocation = launcher.to_rlocation_path(mtree_file)
    image = struct(
        platform = _platform_struct(manifest_info),
        config = launcher.to_rlocation_path(manifest_info.config),
        mtree = mtree_rlocation,
        complete = mtree_file != None,
    )
    return image, files

def _has_output_group_pairs(target):
    """Whether `target` exposes both the `mtree` and `oci_image_config` output groups."""
    if OutputGroupInfo not in target:
        return False
    output_groups = target[OutputGroupInfo]
    return hasattr(output_groups, "mtree") and hasattr(output_groups, "oci_image_config")

def _output_group_images(target):
    """Spec entries for a target exposing `mtree` and `oci_image_config` output groups.

    The files of each group are sorted (by short path) and paired positionally --
    first mtree with first config, and so on -- each pair describing one image.
    Platform metadata is not carried by output groups, so it is left empty.

    Returns (images_list, files_list).
    """
    output_groups = target[OutputGroupInfo]
    mtrees = sorted(output_groups.mtree.to_list(), key = lambda f: f.short_path)
    configs = sorted(output_groups.oci_image_config.to_list(), key = lambda f: f.short_path)
    if len(mtrees) != len(configs):
        fail("image_structure_test: `image` output groups 'mtree' ({}) and 'oci_image_config' ({}) must contain the same number of files to pair them".format(len(mtrees), len(configs)))
    images = []
    files = []
    for mtree_file, config_file in zip(mtrees, configs):
        images.append(struct(
            platform = struct(os = "", architecture = "", variant = ""),
            config = launcher.to_rlocation_path(config_file),
            mtree = launcher.to_rlocation_path(mtree_file),
            complete = True,
        ))
        files.append(config_file)
        files.append(mtree_file)
    return images, files

def _oci_layout_metadata(ctx, name, layout_dir):
    """Run `img oci-layout-metadata` over an OCI image layout tree artifact.

    Reads the layout (including its layer blobs) at build time and emits only a
    small metadata tree (per-platform config.json + image.mtree + images.json).
    The layout tree is an action input and never enters the test runfiles.

    Returns the metadata output tree File.
    """
    meta_tree = ctx.actions.declare_directory("{}.oci_meta".format(name))
    args = ctx.actions.args()
    args.add("oci-layout-metadata")
    args.add("--src", layout_dir.path)
    args.add("--output", meta_tree.path)
    args.add("--path-prefix", ctx.attr._mtree_path_prefix[BuildSettingInfo].value)
    args.add("--options", ctx.attr._mtree_options[BuildSettingInfo].value)
    args.add("--image-layout", ctx.attr._mtree_image_layout[BuildSettingInfo].value)
    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = [layout_dir],
        outputs = [meta_tree],
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        mnemonic = "OCILayoutStructureMeta",
    )
    return meta_tree

def _image_structure_test_aspect_impl(target, ctx):
    if ImageManifestInfo in target:
        image, runfiles_files = _manifest_image(target[ImageManifestInfo])
        spec = struct(images = [image])
    elif ImageIndexInfo in target:
        images = []
        runfiles_files = []
        for manifest_info in target[ImageIndexInfo].manifests:
            image, files = _manifest_image(manifest_info)
            images.append(image)
            runfiles_files.extend(files)
        spec = struct(images = images)
    elif _has_output_group_pairs(target):
        # A non-rules_img source that publishes prebuilt mtree + config JSON via
        # output groups. Preferred over extracting from an OCI layout directory.
        images, runfiles_files = _output_group_images(target)
        spec = struct(images = images)
    else:
        default_files = target[DefaultInfo].files.to_list()
        if len(default_files) == 1 and default_files[0].is_directory:
            meta_tree = _oci_layout_metadata(ctx, "{}.structuretest".format(target.label.name), default_files[0])
            spec = struct(layout_trees = [launcher.to_rlocation_path(meta_tree)])
            runfiles_files = [meta_tree]
        else:
            fail("image_structure_test: `image` must provide ImageManifestInfo or " +
                 "ImageIndexInfo (rules_img image_manifest/image_index), expose `mtree` " +
                 "and `oci_image_config` output groups, or be a single tree-artifact " +
                 "DefaultInfo (an OCI image layout directory, e.g. rules_oci oci_image). " +
                 "Got: {}".format(target.label))

    spec_file = ctx.actions.declare_file("{}.image_structure_spec.json".format(target.label.name))
    ctx.actions.write(spec_file, json.encode(spec))
    return [ImageStructureTestInfo(spec = spec_file, files = depset(runfiles_files))]

_image_structure_test_aspect = aspect(
    implementation = _image_structure_test_aspect_impl,
    attr_aspects = [],  # inspect the `image` target itself; do not propagate to its deps.
    attrs = _ASPECT_MTREE_ATTRS,
    toolchains = TOOLCHAINS,
    provides = [ImageStructureTestInfo],
)

def _image_structure_test_impl(ctx):
    input = ctx.attr.image[ImageStructureTestInfo]
    deploy_tool_info = ctx.attr._deploy_tool[DeployToolInfo]

    request = ctx.actions.declare_file(ctx.label.name + ".request.json")
    ctx.actions.write(request, json.encode(struct(
        spec = launcher.to_rlocation_path(input.spec),
        configs = [launcher.to_rlocation_path(config) for config in ctx.files.configs],
    )))

    stub = ctx.actions.declare_file(ctx.label.name + ".exe")
    embedded_args, transformed_args = launcher.args_from_entrypoint(executable_file = deploy_tool_info.img_deploy_exe)
    embedded_args.extend(["image-structure-test", "--request"])
    embedded_args, transformed_args = launcher.append_runfile(
        file = request,
        embedded_args = embedded_args,
        transformed_args = transformed_args,
    )
    launcher.compile_stub(
        ctx = ctx,
        embedded_args = embedded_args,
        transformed_args = transformed_args,
        output_file = stub,
        template_file = deploy_tool_info.launcher_template,
    )

    runfiles = ctx.runfiles(
        files = [deploy_tool_info.img_deploy_exe, request, input.spec] + ctx.files.configs,
        transitive_files = input.files,
    )
    return [DefaultInfo(executable = stub, runfiles = runfiles)]

image_structure_test = rule(
    implementation = _image_structure_test_impl,
    doc = """Validates a container image's structure using its config JSON and mtree.

`image_structure_test` is a lightweight, hermetic analogue of
[container-structure-test](https://github.com/GoogleContainerTools/container-structure-test):
it accepts the same YAML/JSON config files, but validates the image using only its
image config JSON and its mtree filesystem listing -- it never materializes the
layer blobs into the test. Because of that, checks that require a running container
or file contents are not supported and cause a clear failure (so a migrated config
tells you exactly what won't port):

- **Supported** (`metadataTest`): env / envVars, labels, entrypoint, cmd,
  exposedPorts, volumes, workdir, user -- validated against the image config JSON.
- **Supported** (`fileExistenceTests`): path, shouldExist, permissions, uid, gid,
  isExecutableBy -- validated against the image mtree.
- **Rejected**: `commandTests` (require running the container), `fileContentTests`
  (require file bytes), `licenseTests` (require scanning a running container).

The `image` may be a rules_img `image_manifest` or `image_index`, a target that
exposes prebuilt `mtree` and `oci_image_config` output groups (paired positionally
in sorted order), or an OCI image layout directory (a tree artifact, e.g. a
rules_oci `oci_image`). For a multi-platform `image_index`, every config is
validated against each single-platform image.

Example:

```python
load("@rules_img//img:test.bzl", "image_structure_test")

image_structure_test(
    name = "hello_test",
    configs = ["testdata/hello.yaml"],
    image = ":hello",
)
```
""",
    attrs = {
        "image": attr.label(
            doc = "Image to validate: a rules_img image_manifest/image_index, a target exposing `mtree` + `oci_image_config` output groups, or an OCI image layout directory (rules_oci).",
            mandatory = True,
            aspects = [_image_structure_test_aspect],
        ),
        "configs": attr.label_list(
            doc = "container-structure-test config files (YAML or JSON).",
            mandatory = True,
            allow_files = [".yaml", ".yml", ".json"],
        ),
        "_deploy_tool": attr.label(
            default = Label("//img/deploy_tool/for_host"),
            providers = [DeployToolInfo],
        ),
    },
    test = True,
    toolchains = [launcher.finalizer_toolchain_type] + TOOLCHAINS,
)
