"""Release build utilities for rules_img.

This module provides utilities for building release artifacts, including
platform-specific binaries, source bundles, and BCR (Bazel Central Registry)
packages.
"""

load("@rules_img_private//release_platforms:defs.bzl", "PLATFORM_NAMES", "release_platforms_transition")
load("@rules_pkg//pkg:mappings.bzl", "pkg_attributes")
load("@rules_pkg//pkg:providers.bzl", "PackageFilesInfo")
load("//img/private/config:defs.bzl", "ModuleVersionInfo")

DEFAULT_ATTRIBUTES = pkg_attributes(mode = "0644")
EXECUTABLE_ATTRIBUTES = pkg_attributes(mode = "0755")

OverrideSourceFilesInfo = provider(
    doc = """Provider representing overrides for a rules_pkg PackageFilesInfo""",
    fields = {
        "attributes": """Attribute information, represented as a `dict`.

Keys are strings representing attribute identifiers, values are
arbitrary data structures that represent the associated data.  These are
most often strings, but are not explicitly defined.

For known attributes and data type expectations, see the Common
Attributes documentation in the `rules_pkg` reference.
        """,
        "dest_src_map": """Map of file destinations to sources.

Sources are represented by bazel `File` structures.""",
    },
)

OfflineBuildDistdirInfo = provider(
    doc = """Provider representing the contents of a Bazel "--distdir".""",
    fields = {
        "basename_file_map": """Map of basename to File""",
        "files": "Depset of File whose basename shall be used as-is",
    },
)

BCRModuleVersionInfo = provider(
    doc = """Provider representing a version of a BCR module.""",
    fields = {
        "module_name": "Name of the module",
        "version": "The module version",
        "source_archive": "An archive File containing the module source",
        "source_archive_basename": "A basename for the source archive",
        "metadata_template": "A File containing a base template for metadata.json",
    },
)

def _release_files(ctx):
    output_group_info = {}
    version = ctx.attr.version[ModuleVersionInfo].version

    # The git tag the prebuilt binaries are published under. For the img tool
    # this is "v<version>" (e.g. v0.0.1); the independently-versioned signer
    # plugins override it to their per-module tag (e.g.
    # rules_img_signer_cosign-v<version>). It is recorded verbatim in the
    # lockfile so the prebuilt download URL resolves to the right release.
    tag = ctx.attr.tag_template.format(version = version)
    module_version = ctx.actions.declare_file("%s_module_version" % ctx.attr.name)
    git_tag = ctx.actions.declare_file("%s_git_tag" % ctx.attr.name)
    ctx.actions.write(module_version, content = version)
    ctx.actions.write(git_tag, content = tag)
    output_group_info["version"] = depset([module_version, git_tag])
    lockfile_args = ctx.actions.args()
    lockfile_args.add("--tag", tag)
    dest_src_map = {}
    attributes = {}
    distdir_contents = {}
    for platform in PLATFORM_NAMES:
        src = ctx.split_attr.executable[platform]
        executable = src[DefaultInfo].files_to_run.executable
        basename = ctx.attr.basename if len(ctx.attr.basename) > 0 else "img"

        # ensure we copy the extension from the executable (for Windows)
        dot_extension = ""
        if len(executable.extension) > 0 and not basename.endswith("." + executable.extension):
            dot_extension = "." + executable.extension
        filename_basename = "%s_%s%s" % (basename, platform, dot_extension)
        filename = filename_basename
        dest_src_map[filename] = executable
        attributes[filename] = EXECUTABLE_ATTRIBUTES
        distdir_contents[filename_basename] = executable
        output_group_info["%s_files" % platform] = depset([executable])
        lockfile_args.add("--tool", "%s=%s" % (platform, executable.path))
    override = ctx.files.lockfile_override
    if len(override) > 1:
        fail("{}: lockfile_override must provide at most one file, but {} provides {}".format(
            ctx.label,
            ctx.attr.lockfile_override.label,
            len(override),
        ))
    lockfile = override[0] if len(override) == 1 else None
    if lockfile == None:
        # No externally produced lockfile: hash the binaries built above into one.
        # The arguments naming them were collected in the loop.
        lockfile = ctx.actions.declare_file("%s_lockfile.json" % ctx.attr.name)
        lockfile_args.add(lockfile)
        ctx.actions.run(
            outputs = [lockfile],
            inputs = [ctx.split_attr.executable[p][DefaultInfo].files_to_run.executable for p in PLATFORM_NAMES],
            executable = ctx.executable.lockfile_generator,
            arguments = [lockfile_args],
        )
    output_group_info["lockfile"] = depset([lockfile])

    # Build the override map for source files
    override_attributes = {ctx.attr.lockfile_name: DEFAULT_ATTRIBUTES}
    override_dest_src_map = {ctx.attr.lockfile_name: lockfile}

    # Add MODULE.bazel override if provided
    if ctx.file.module_bazel:
        override_attributes["MODULE.bazel"] = DEFAULT_ATTRIBUTES
        override_dest_src_map["MODULE.bazel"] = ctx.file.module_bazel

    return [
        DefaultInfo(files = depset(dest_src_map.values())),
        OutputGroupInfo(**output_group_info),
        PackageFilesInfo(attributes = attributes, dest_src_map = dest_src_map),
        OverrideSourceFilesInfo(
            attributes = override_attributes,
            dest_src_map = override_dest_src_map,
        ),
        OfflineBuildDistdirInfo(
            basename_file_map = distdir_contents,
            files = depset(),
        ),
    ]

release_files = rule(
    implementation = _release_files,
    attrs = {
        "executable": attr.label(
            cfg = release_platforms_transition,
            mandatory = True,
        ),
        "basename": attr.string(),
        "lockfile_generator": attr.label(
            executable = True,
            default = Label("@rules_img_internal_tools//release/lockfile"),
            cfg = "exec",
        ),
        "lockfile_override": attr.label(
            allow_files = True,
            default = Label("//img/private/release:prebuilt_lockfile_override"),
            doc = "A lockfile produced outside this build, packaged instead of the generated one. " +
                  "Set through the //img/private/release:prebuilt_lockfile_override flag; when it " +
                  "provides no file (the default), the lockfile is generated from the binaries built here.",
        ),
        "lockfile_name": attr.string(
            mandatory = True,
        ),
        "tag_template": attr.string(
            default = "v{version}",
            doc = "Template for the release tag the prebuilt binaries are published under. " +
                  "'{version}' is substituted with the module version. Defaults to 'v{version}' " +
                  "(the img tool release tag); independently-versioned modules such as the signer " +
                  "plugins set it to their per-module tag, e.g. 'rules_img_signer_cosign-v{version}'.",
        ),
        "module_bazel": attr.label(
            allow_single_file = True,
            doc = "Optional MODULE.bazel file to override in the release",
        ),
        "version": attr.label(
            default = "@rules_img_version",
            providers = [ModuleVersionInfo],
        ),
    },
)

def _offline_bundle_impl(ctx):
    contents = {}

    # Handle multiple distdir_contents inputs
    for distdir_content in ctx.attr.distdir_contents:
        if distdir_content:  # Check if not None
            mapped_contents = distdir_content[OfflineBuildDistdirInfo].basename_file_map
            extra_files = distdir_content[OfflineBuildDistdirInfo].files
            for f in extra_files.to_list():
                contents[f.basename] = f
            for basename, f in mapped_contents.items():
                contents[basename] = f

    distdir_args = ctx.actions.args()
    for basename, f in contents.items():
        distdir_args.add("--file", "%s=%s" % (basename, f.path))

    distdir_tree_artifact = ctx.actions.declare_directory(ctx.attr.name + ".distdir")
    distdir_args.add(distdir_tree_artifact.path)
    ctx.actions.run(
        outputs = [distdir_tree_artifact],
        inputs = contents.values(),
        executable = ctx.executable.distdir_generator,
        arguments = [distdir_args],
    )

    return [DefaultInfo(files = depset([distdir_tree_artifact]))]

offline_bundle = rule(
    implementation = _offline_bundle_impl,
    attrs = {
        "distdir_contents": attr.label_list(
            providers = [OfflineBuildDistdirInfo],
            mandatory = True,
        ),
        "distdir_generator": attr.label(
            executable = True,
            default = Label("@rules_img_internal_tools//release/distdir"),
            cfg = "exec",
        ),
    },
)

def _source_bundle_impl(ctx):
    attributes = {}
    dest_src_map = {}
    for file in ctx.files.srcs:
        if not file.is_source:
            fail("Bundling non-source file %s" % file.path)
        dest_src_map[file.path] = file
        attributes[file.path] = DEFAULT_ATTRIBUTES
        if file.extension in ["exe", "sh"] or file.path in ["cmd/img/img"]:
            attributes[file.path] = EXECUTABLE_ATTRIBUTES
    for override in ctx.attr.overrides:
        override = override[OverrideSourceFilesInfo]
        attributes.update(override.attributes)
        dest_src_map.update(override.dest_src_map)

    # Override MODULE.bazel if provided (e.g. a release-cleaned version). Keyed
    # with the strip_prefix so the stripping step below lands it at MODULE.bazel.
    if ctx.file.module_bazel:
        module_key = (ctx.attr.strip_prefix or "") + "MODULE.bazel"
        dest_src_map[module_key] = ctx.file.module_bazel
        attributes[module_key] = DEFAULT_ATTRIBUTES

    # Strip a leading path prefix from every destination (both source files and
    # override entries) so a module living in a subdirectory (e.g. a signer
    # plugin under modules/rules_img_signer_*) is packaged relative to its own
    # module root. Callers set matching override keys (e.g. release_files'
    # lockfile_name) to the pre-strip path so the override lands correctly.
    strip_prefix = ctx.attr.strip_prefix
    if strip_prefix:
        stripped_dest_src = {}
        stripped_attributes = {}
        for dest, f in dest_src_map.items():
            stripped = dest.removeprefix(strip_prefix)
            stripped_dest_src[stripped] = f
            stripped_attributes[stripped] = attributes[dest]
        dest_src_map = stripped_dest_src
        attributes = stripped_attributes

    # The srcs come from globs that must tolerate an empty match, so a pattern
    # that stops matching would silently drop files from the archive.
    missing = [dest for dest in ctx.attr.required_files if dest not in dest_src_map]
    if missing:
        fail("source_bundle {} would package an incomplete module: missing {}".format(
            ctx.label,
            ", ".join(missing),
        ))

    return [
        DefaultInfo(files = depset(dest_src_map.values())),
        PackageFilesInfo(attributes = attributes, dest_src_map = dest_src_map),
    ]

source_bundle = rule(
    implementation = _source_bundle_impl,
    attrs = {
        "srcs": attr.label_list(allow_files = True),
        "overrides": attr.label_list(providers = [OverrideSourceFilesInfo]),
        "strip_prefix": attr.string(
            doc = "Leading path prefix stripped from every packaged destination.",
        ),
        "module_bazel": attr.label(
            allow_single_file = True,
            doc = "Optional MODULE.bazel to substitute in the bundle (e.g. a release-cleaned version).",
        ),
        "required_files": attr.string_list(
            doc = "Destinations (relative to the packaged module root) that must be present in the bundle.",
        ),
    },
)

def _versioned_filename_info_impl(ctx):
    file = ctx.file.src
    basename = file.basename
    destdir = ctx.attr.destdir
    slash = "/" if len(destdir) > 0 else ""
    extension = ctx.attr.extension if len(ctx.attr.extension) > 0 else file.extension
    dot = "." if len(extension) > 0 else ""
    path = file.path
    stem = basename.removesuffix(dot + extension)
    dest = ctx.attr.path_template.format(
        basename = basename,
        destdir = destdir,
        slash = slash,
        extension = extension,
        dot = dot,
        stem = stem,
        path = path,
        version = ctx.attr.version[ModuleVersionInfo].version,
    )
    dest_basename = ctx.attr.path_template.format(
        basename = basename,
        destdir = "",
        slash = "",
        extension = extension,
        dot = dot,
        stem = stem,
        path = path,
        version = ctx.attr.version[ModuleVersionInfo].version,
    )
    dest_src_map = {dest: file}

    # Generate release notes if requested (for source archives)
    output_group_info = {}
    if ctx.attr.generate_release_notes:
        release_notes = ctx.actions.declare_file("%s_release_notes.md" % ctx.attr.name)
        version = ctx.attr.version[ModuleVersionInfo].version
        version_with_v = "v" + version

        ctx.actions.run(
            outputs = [release_notes],
            inputs = [ctx.file.src],
            executable = ctx.executable._release_notes_generator,
            arguments = [ctx.file.src.path, version_with_v, release_notes.path],
            mnemonic = "GenerateReleaseNotes",
        )
        output_group_info["release_notes"] = depset([release_notes])

    return [
        DefaultInfo(files = depset(dest_src_map.values())),
        OutputGroupInfo(**output_group_info),
        PackageFilesInfo(attributes = {dest: ctx.attr.attributes}, dest_src_map = dest_src_map),
        BCRModuleVersionInfo(
            module_name = ctx.attr.module_name,
            version = ctx.attr.version[ModuleVersionInfo].version,
            source_archive = ctx.file.src,
            source_archive_basename = dest_basename,
            metadata_template = ctx.file._metadata_template,
        ),
    ]

versioned_filename_info = rule(
    implementation = _versioned_filename_info_impl,
    attrs = {
        "module_name": attr.string(),
        "src": attr.label(allow_single_file = True),
        "destdir": attr.string(),
        "extension": attr.string(),
        "path_template": attr.string(default = "{destdir}{slash}{stem}-v{version}{dot}{extension}"),
        "attributes": attr.string(),
        "generate_release_notes": attr.bool(default = False),
        "version": attr.label(
            default = "@rules_img_version",
            providers = [ModuleVersionInfo],
        ),
        "_metadata_template": attr.label(
            allow_single_file = True,
            default = "//:.bcr/metadata.template.json",
        ),
        "_release_notes_generator": attr.label(
            executable = True,
            default = Label("@rules_img_internal_tools//release/release_notes"),
            cfg = "exec",
        ),
    },
)

def _offline_bcr_impl(ctx):
    bcr_args = ctx.actions.args()
    inputs = []
    output_group_info = {}
    for src_tar in ctx.attr.src_tars:
        bcr_info = src_tar[BCRModuleVersionInfo]
        request = {
            "module_name": bcr_info.module_name,
            "version": bcr_info.version,
            "source_path": bcr_info.source_archive.path,
            "override_source_basename": bcr_info.source_archive_basename,
            "metadata_template_path": bcr_info.metadata_template.path,
        }
        request_file = ctx.actions.declare_file(ctx.attr.name + "_local_module_" + bcr_info.module_name + ".json")
        inputs.append(request_file)
        inputs.append(bcr_info.source_archive)
        inputs.append(bcr_info.metadata_template)
        ctx.actions.write(request_file, content = json.encode(request))
        bcr_args.add("--add-local-module", request_file.path)
        bazel_dep = ctx.actions.declare_file(ctx.attr.name + "_local_module_" + bcr_info.module_name + ".bazel_dep")
        ctx.actions.write(bazel_dep, content = """bazel_dep(
    name = "{name}",
    version = "{version}",
)
""".format(name = bcr_info.module_name, version = bcr_info.version))
        output_group_info[bcr_info.module_name] = depset([bazel_dep])
    bcr_tree_artifact = ctx.actions.declare_directory(ctx.attr.name + ".local")
    bcr_args.add(bcr_tree_artifact.path)
    ctx.actions.run(
        outputs = [bcr_tree_artifact],
        inputs = inputs,
        executable = ctx.executable.bcr_generator,
        arguments = [bcr_args],
    )
    bcr = depset([bcr_tree_artifact])
    output_group_info["bcr"] = bcr
    return [
        DefaultInfo(files = bcr),
        OutputGroupInfo(**output_group_info),
    ]

offline_bcr = rule(
    implementation = _offline_bcr_impl,
    attrs = {
        "src_tars": attr.label_list(
            providers = [BCRModuleVersionInfo],
            mandatory = True,
        ),
        "bcr_generator": attr.label(
            executable = True,
            default = Label("@rules_img_internal_tools//release/bcr"),
            cfg = "exec",
        ),
    },
)
