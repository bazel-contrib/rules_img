"""Layer rule for building layers in a container image."""

load("@bazel_skylib//lib:sets.bzl", "sets")
load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("@runfilesgroupinfo.bzl", "SAME_PARTY_RUNFILES", "OTHER_PARTY_RUNFILES", "FOUNDATIONAL_RUNFILES", "DEBUG_RUNFILES", "DOCUMENTATION_RUNFILES", "RunfilesGroupInfo")
load("@fsmanifestinfo.bzl", "FSManifestInfo")
load("//img/private/common:build.bzl", "TOOLCHAIN", "TOOLCHAINS")
load("//img/private/common:layer_helper.bzl", "compression_tuning_args")
load("//img/private/providers:layer_info.bzl", "LayerInfo")
load("//img/private/providers:layer_group_info.bzl", "LayerGroupInfo")

def _file_type(f):
    type = "f"  # regular file
    if f.is_directory:
        type = "d"
    return type

def _files_arg(f):
    type = _file_type(f)
    return "{}{}".format(type, f.path)

def _to_short_path_pair(f):
    repo = f.owner.repo_name
    if repo == "":
        repo = "_main"
    type = _file_type(f)
    return "{}/{}\0{}{}".format(repo, f.short_path, type, f.path)

def _root_symlinks_arg(x):
    type = _file_type(x.target_file)
    return "{}\0{}{}".format(x.path, type, x.target_file.path)

def _symlinks_arg(x):
    type = _file_type(x.target_file)
    return "{}\0{}{}_main/{}".format(x.path, type, x.target_file.path)

def _symlink_tuple_to_arg(pair):
    source = pair[0]
    dest = pair[1]
    if source.startswith("/"):
        source = source[1:]
    return "{}\0{}".format(source, dest)

def _convert_metadata_to_json(metadata_struct):
    """Convert FSManifestInfo metadata struct to JSON string for the Go tool.

    Args:
        metadata_struct: A struct with metadata fields (mode, uid, gid, owner, group, mtime, xattrs)

    Returns:
        JSON string in the format expected by the Go tool
    """
    if metadata_struct == None:
        return None

    # Build a dict with the fields we need
    result = {}

    # mode: int -> octal string (e.g., 0o755 -> "0755")
    if hasattr(metadata_struct, "mode"):
        result["mode"] = "0%o" % metadata_struct.mode

    # uid: int -> int
    if hasattr(metadata_struct, "uid"):
        result["uid"] = metadata_struct.uid

    # gid: int -> int
    if hasattr(metadata_struct, "gid"):
        result["gid"] = metadata_struct.gid

    # owner: string -> uname: string
    if hasattr(metadata_struct, "owner"):
        result["uname"] = metadata_struct.owner

    # group: string -> gname: string
    if hasattr(metadata_struct, "group"):
        result["gname"] = metadata_struct.group

    # mtime: int (epoch seconds) -> string
    # The Go tool can handle both RFC3339 and Unix epoch seconds as string
    if hasattr(metadata_struct, "mtime"):
        result["mtime"] = str(metadata_struct.mtime)

    # xattrs: dict -> pax_records: dict
    if hasattr(metadata_struct, "xattrs"):
        result["pax_records"] = metadata_struct.xattrs

    if len(result) == 0:
        return None

    return json.encode(result)

def _handle_groups(ctx):
    if ctx.attr.layer_for_group and not ctx.attr.layer_ids:
        fail("layer_for_group requires explicit layer_ids to be set")
    if ctx.attr.layer_for_group:
        for group_name, layer_id in ctx.attr.layer_for_group.items():
            if ctx.attr.include_groups and group_name not in ctx.attr.include_groups:
                fail("layer_for_group contains mapping for {} -> {}, which is not in include_groups (and therefore excluded)".format(group_name, layer_id))
            if ctx.attr.exclude_groups and group_name in ctx.attr.exclude_groups:
                fail("layer_for_group contains mapping for {} -> {}, which is in exclude_groups".format(group_name, layer_id))
            if layer_id not in ctx.attr.layer_ids:
                fail("layer_for_group contains mapping for {} -> {}, but layer {} is not in layer_ids".format(group_name, layer_id, layer_id))
    if ctx.attr.include_groups and ctx.attr.exclude_groups:
        fail("Cannot set both include_groups and exclude_groups")
    # TODO: switch to builtin set (requires Bazel 8+)
    # groups = set()
    skip_groups = sets.make(ctx.attr.exclude_groups)
    include_groups = sets.make(ctx.attr.include_groups)
    groups_uses_allowlist = True if ctx.attr.include_groups else False
    groups = sets.make([SAME_PARTY_RUNFILES])  # Default group is always present
    layer_ids = sets.make(ctx.attr.layer_ids if ctx.attr.layer_ids else [])

    # Find groups that are present in srcs while applying include/exclude filters
    for files in ctx.attr.srcs.values():
        runfiles_group_info = files[RunfilesGroupInfo] if RunfilesGroupInfo in files else None
        if runfiles_group_info:
            # Extract group names from RunfilesGroupInfo
            for group_name in dir(runfiles_group_info):
                # Apply include/exclude filters
                if groups_uses_allowlist:
                    if not sets.contains(include_groups, group_name):
                        continue
                elif sets.contains(skip_groups, group_name):
                    continue
                # Add to discovered groups
                sets.insert(groups, group_name)

    layer_id_for_group = {}
    if not ctx.attr.layer_ids:
        # If explicit layer ids are not set,
        # we derive them from the default grouping.
        if ctx.attr.default_grouping == "layer_per_group":
            layer_ids = sets.copy(groups)
            for g in sets.to_list(layer_ids):
                # identity mapping
                layer_id_for_group[g] = g
        else:
            # merge all into the default layer
            layer_ids = sets.make([SAME_PARTY_RUNFILES])
            for g in sets.to_list(groups):
                layer_id_for_group[g] = SAME_PARTY_RUNFILES
    elif ctx.attr.layer_for_group:
        # We were given explicit layer id to group mappings.
        # Now we use the layer_for_group mapping.
        layer_id_for_group.update(ctx.attr.layer_for_group)

    default_layer_id = SAME_PARTY_RUNFILES
    if ctx.attr.default_layer_id:
        # Ensure the default layer id is in the list of layer ids.
        sets.insert(layer_ids, ctx.attr.default_layer_id)
        default_layer_id = ctx.attr.default_layer_id

    sorted_layer_ids = []
    if ctx.attr.layer_ids:
        # Use the user-specified order.
        sorted_layer_ids.extend(ctx.attr.layer_ids)
    else:
        # Use a deterministic order according to a ranking of well-known groups:
        #  - Foundational files (interpreter, standard libraries, ...)
        #  - Other-party files (third-party dependencies, ... )
        #  - Documentation files (README, man pages, ...)
        #  - Debug files (debugging tools, ...)
        #  - Custom (unranked) groups
        #  - Same-party files (application code, ...)
        # This order is chosen to optimize caching and sharing of layers.
        # Foundational layers are most likely to be shared across images,
        # while same-party layers are least likely to be shared.
        # The order also reflects the likelihood of changes, with
        # foundational layers changing least frequently and same-party layers most frequently.
        # This ordering helps to minimize the amount of data that needs to be pulled
        # when images are updated, as changes to same-party layers will not affect
        # the foundational layers.
        # If another ordering is desired, the user can specify explicit layer_ids.
        _ranking = {
            FOUNDATIONAL_RUNFILES: 0,
            OTHER_PARTY_RUNFILES: 1,
            DOCUMENTATION_RUNFILES: 2,
            DEBUG_RUNFILES: 3,
            # 4 is for unranked groups
            SAME_PARTY_RUNFILES: 5,
        }
        sorted_layer_ids.extend(sorted(sets.to_list(layer_ids), key = lambda x: (_ranking[x] if x in _ranking else 4, x)))
    return struct(
        groups = groups,
        layer_ids = layer_ids,
        layer_id_for_group = layer_id_for_group,
        default_layer_id = default_layer_id,
        sorted_layer_ids = sorted_layer_ids,
    )

def _create_layer(ctx, out, metadata_out, srcs_for_layer, compression, estargz_enabled, manifest_metadata, merged_symlinks, manifest_directories):
    """Creates a single layer from the given sources.

    Args:
        ctx: The rule context.
        out: The output file for the layer tar.
        metadata_out: The output file for layer metadata.
        srcs_for_layer: List of (path_in_image, target, group_names, include_executable) tuples
                        where group_names can be a string or a list of strings.
        compression: Compression format (gzip or zstd).
        estargz_enabled: Whether estargz is enabled.
        manifest_metadata: Dict of path -> JSON metadata string from FSManifestInfo
        merged_symlinks: Dict of symlink path -> target path (merged from ctx.attr.symlinks and FSManifestInfo)
        manifest_directories: List of directory paths from FSManifestInfo
    """
    args = ["layer", "--name", str(ctx.label), "--metadata", metadata_out.path, "--format", compression]

    # Set compressor defaults based on compilation mode for gzip
    args.extend(compression_tuning_args(ctx, compression, estargz_enabled))
    if estargz_enabled:
        args.append("--estargz")
    for key, value in ctx.attr.annotations.items():
        args.extend(["--annotation", "{}={}".format(key, value)])
    if ctx.attr.default_metadata:
        args.extend(["--default-metadata", ctx.attr.default_metadata])

    # Merge ctx.attr.file_metadata with manifest_metadata
    # ctx.attr.file_metadata takes precedence
    all_file_metadata = {}
    all_file_metadata.update(manifest_metadata)
    all_file_metadata.update(ctx.attr.file_metadata)

    for path, metadata in all_file_metadata.items():
        path = path.removeprefix("/")  # the "/" is not included in the tar file.
        args.extend(["--file-metadata", "{}={}".format(path, metadata)])

    files_args = ctx.actions.args()
    files_args.set_param_file_format("multiline")
    files_args.use_param_file("--add-from-file=%s", use_always = True)

    inputs = []

    for (path_in_image, files, group_names, include_executable) in srcs_for_layer:
        path_in_image = path_in_image.removeprefix("/")  # the "/" is not included in the tar file.
        # Get DefaultInfo from the target
        if hasattr(files, "DefaultInfo"):
            default_info = files.DefaultInfo
        else:
            default_info = files[DefaultInfo]
        files_to_run = default_info.files_to_run
        executable = None
        runfiles = None

        # Normalize group_names to always be a list
        if type(group_names) == type(""):
            group_names = [group_names]

        # Check if this target has RunfilesGroupInfo
        runfiles_group_info = files[RunfilesGroupInfo] if (type(files) != type(struct()) and RunfilesGroupInfo in files) else None

        inputs.append(default_info.files)
        if files_to_run != None and files_to_run.executable != None and not files_to_run.executable.is_source:
            # This is an executable.
            # Add the executable with the runfiles tree, but ignore any other files.
            executable = files_to_run.executable
            runfiles = default_info.default_runfiles

            # Create the runfiles args
            executable_runfiles_args = ctx.actions.args()
            executable_runfiles_args.set_param_file_format("multiline")

            if include_executable:
                # Add the executable itself and use --runfiles flag
                args.append("--executable={}={}".format(path_in_image, executable.path))
                executable_runfiles_args.use_param_file("--runfiles={}=%s".format(executable.path), use_always = True)
            else:
                # Use --runfiles-only flag for runfiles without the executable
                executable_runfiles_args.use_param_file("--runfiles-only={}=%s".format(path_in_image), use_always = True)

            if runfiles_group_info:
                # Collect files from all groups for this layer
                all_group_files = []
                for group_name in group_names:
                    group_files = getattr(runfiles_group_info, group_name, depset())
                    all_group_files.append(group_files)

                # Merge all group files
                merged_files = depset(transitive = all_group_files)
                executable_runfiles_args.add_all(merged_files, map_each = _to_short_path_pair, expand_directories = False)
                inputs.append(merged_files)
            else:
                # Use all runfiles (default behavior)
                executable_runfiles_args.add_all(runfiles.files, map_each = _to_short_path_pair, expand_directories = False)
                executable_runfiles_args.add_all(runfiles.symlinks, map_each = _symlinks_arg)
                executable_runfiles_args.add_all(runfiles.root_symlinks, map_each = _root_symlinks_arg)
                inputs.append(runfiles.files)
                inputs.append(runfiles.symlinks)
                inputs.append(runfiles.root_symlinks)

            args.append(executable_runfiles_args)
            continue

        # This isn't an executable.
        # Let's add all files instead.
        if default_info.files == None:
            fail("Expected {} ({}) to contain an executable or files, got None".format(path_in_image, files))

        if runfiles_group_info:
            # Collect files from all groups for this layer
            all_group_files = []
            for group_name in group_names:
                group_files = getattr(runfiles_group_info, group_name, depset())
                all_group_files.append(group_files)

            # Merge all group files
            merged_files = depset(transitive = all_group_files)
            files_args.add_all(merged_files, map_each = _files_arg, format_each = "{}\0%s".format(path_in_image), expand_directories = False)
            inputs.append(merged_files)
        else:
            # Use all files (default behavior)
            files_args.add_all(default_info.files, map_each = _files_arg, format_each = "{}\0%s".format(path_in_image), expand_directories = False)

    if len(merged_symlinks) > 0:
        symlink_args = ctx.actions.args()
        symlink_args.set_param_file_format("multiline")
        symlink_args.use_param_file("--symlinks-from-file=%s", use_always = True)
        symlink_args.add_all(merged_symlinks.items(), map_each = _symlink_tuple_to_arg)
        args.append(symlink_args)

    if len(manifest_directories) > 0:
        directory_args = ctx.actions.args()
        directory_args.set_param_file_format("multiline")
        directory_args.use_param_file("--directories-from-file=%s", use_always = True)
        directory_args.add_all(manifest_directories)
        args.append(directory_args)

    args.append(files_args)
    args.append(out.path)

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        outputs = [out, metadata_out],
        inputs = depset(transitive = inputs),
        executable = img_toolchain_info.tool_exe,
        arguments = args,
        mnemonic = "LayerTar",
    )

def _image_layer_impl(ctx):
    compression = ctx.attr.compress
    if compression == "auto":
        compression = ctx.attr._default_compression[BuildSettingInfo].value

    estargz = ctx.attr.estargz
    if estargz == "auto":
        estargz = ctx.attr._default_estargz[BuildSettingInfo].value
    estargz_enabled = estargz == "enabled"

    if compression == "gzip":
        out_ext = ".tgz"
        media_type = "application/vnd.oci.image.layer.v1.tar+gzip"
    elif compression == "zstd":
        out_ext = ".tar.zst"
        media_type = "application/vnd.oci.image.layer.v1.tar+zstd"
    else:
        fail("Unsupported compression: {}".format(compression))

    group_settings = _handle_groups(ctx)

    # Organize srcs by layer_id based on group information
    # layer_id -> [(path_in_image, target, group_name, include_executable)]
    srcs_by_layer = {layer_id: [] for layer_id in group_settings.sorted_layer_ids}

    for (path_in_image, files) in ctx.attr.srcs.items():
        # Check if the target has RunfilesGroupInfo
        runfiles_group_info = files[RunfilesGroupInfo] if RunfilesGroupInfo in files else None

        if runfiles_group_info:
            # Use the groups from RunfilesGroupInfo
            # Collect all groups for this executable organized by layer_id
            groups_by_layer = {}
            for group_name in dir(runfiles_group_info):
                if group_name.startswith("_"):
                    continue
                layer_id = group_settings.layer_id_for_group.get(group_name, group_settings.default_layer_id)
                if layer_id not in groups_by_layer:
                    groups_by_layer[layer_id] = []
                groups_by_layer[layer_id].append(group_name)

            # Determine which layer should contain the executable itself
            # We choose the last layer in sorted order that contains any group from this executable
            last_layer_id = None
            for layer_id in reversed(group_settings.sorted_layer_ids):
                if layer_id in groups_by_layer:
                    last_layer_id = layer_id
                    break

            # Add one entry per layer, combining all groups that go to that layer
            for layer_id, group_names in groups_by_layer.items():
                include_executable = (layer_id == last_layer_id)
                srcs_by_layer[layer_id].append((path_in_image, files, group_names, include_executable))
        else:
            # Default to SAME_PARTY_RUNFILES
            layer_id = group_settings.layer_id_for_group.get(SAME_PARTY_RUNFILES, group_settings.default_layer_id)
            srcs_by_layer[layer_id].append((path_in_image, files, SAME_PARTY_RUNFILES, True))

    # Collect metadata, symlinks, and directories from FSManifestInfo
    manifest_metadata = {}
    manifest_symlinks = {}
    manifest_directories = []

    # Process FSManifestInfo entries if any
    for manifest_target in ctx.attr.manifests:
        fs_manifest = manifest_target[FSManifestInfo]

        # Add all entries from the manifest to the default layer
        default_layer_id = group_settings.default_layer_id

        for path_in_image, entry in fs_manifest.entries.items():
            if entry.kind == "file":
                # entry.target should be a File
                # Create a struct with DefaultInfo so it can be processed like any other target
                file_info_provider = struct(DefaultInfo = DefaultInfo(files = depset([entry.target])))
                srcs_by_layer[default_layer_id].append((path_in_image, file_info_provider, SAME_PARTY_RUNFILES, True))

                # Check if there's file-specific metadata for this path
                if fs_manifest.metadata and path_in_image in fs_manifest.metadata:
                    metadata_json = _convert_metadata_to_json(fs_manifest.metadata[path_in_image])
                    if metadata_json:
                        manifest_metadata[path_in_image] = metadata_json
                elif fs_manifest.defaults:
                    # Use defaults if no file-specific metadata
                    metadata_json = _convert_metadata_to_json(fs_manifest.defaults)
                    if metadata_json:
                        manifest_metadata[path_in_image] = metadata_json
            elif entry.kind == "symlink":
                # entry.target is the target path the symlink points to
                # path_in_image is where the symlink should be created
                manifest_symlinks[path_in_image] = entry.target
            elif entry.kind == "empty_dir":
                # Add empty directory to the list (remove leading slash)
                dir_path = path_in_image.removeprefix("/")
                manifest_directories.append(dir_path)

    # Merge symlinks: ctx.attr.symlinks takes precedence over manifest symlinks
    merged_symlinks = {}
    merged_symlinks.update(manifest_symlinks)
    merged_symlinks.update(ctx.attr.symlinks)

    # Create layers for each layer_id
    layer_infos = []
    all_outputs = []
    all_metadata = []

    for i, layer_id in enumerate(group_settings.sorted_layer_ids):
        if len(group_settings.sorted_layer_ids) == 1:
            # Single layer - use simple naming
            out = ctx.actions.declare_file(ctx.attr.name + out_ext)
            metadata_out = ctx.actions.declare_file(ctx.attr.name + "_metadata.json")
        else:
            # Multiple layers - include index and layer_id in name
            out = ctx.actions.declare_file("{}_{}_{}{}".format(ctx.attr.name, i, layer_id, out_ext))
            metadata_out = ctx.actions.declare_file("{}_{}_{}{}".format(ctx.attr.name, i, layer_id, "_metadata.json"))

        # Symlinks should only be added to the default layer
        layer_symlinks = merged_symlinks if layer_id == group_settings.default_layer_id else {}

        _create_layer(
            ctx,
            out,
            metadata_out,
            srcs_by_layer[layer_id],
            compression,
            estargz_enabled,
            manifest_metadata,
            layer_symlinks,
            manifest_directories,
        )

        layer_infos.append(LayerInfo(
            blob = out,
            metadata = metadata_out,
            media_type = media_type,
            estargz = estargz_enabled,
        ))
        all_outputs.append(out)
        all_metadata.append(metadata_out)

    # Build providers list
    providers = [
        DefaultInfo(files = depset(all_outputs)),
        OutputGroupInfo(
            layer = depset(all_outputs),
            metadata = depset(all_metadata),
        ),
    ]

    # Return LayerInfo iff exactly one layer, otherwise LayerGroupInfo
    if len(layer_infos) == 1:
        providers.append(layer_infos[0])
    else:
        providers.append(LayerGroupInfo(layers = layer_infos))

    return providers

image_layer = rule(
    implementation = _image_layer_impl,
    doc = """Creates a container image layer from files, executables, and directories.

This rule packages files into a layer that can be used in container images. It supports:
- Adding files at specific paths in the image
- Setting file permissions and ownership
- Creating symlinks
- Including executables with their runfiles
- Compression (gzip, zstd) and eStargz optimization

While this rule creates a single layer by default, some configurations of the attributes will result in multiple sub-layers being created for splitting files into separate groups.
This rule returns either a LayerInfo provider (for a single layer) or a LayerGroupInfo provider (for multiple layers).

Example:

```python
load("@rules_img//img:layer.bzl", "image_layer", "file_metadata")

# Simple layer with files
image_layer(
    name = "app_layer",
    srcs = {
        "/app/bin/server": "//cmd/server",
        "/app/config.json": ":config.json",
    },
)

# Layer with custom permissions
image_layer(
    name = "secure_layer",
    srcs = {
        "/etc/app/config": ":config",
        "/etc/app/secret": ":secret",
    },
    default_metadata = file_metadata(
        mode = "0644",
        uid = 1000,
        gid = 1000,
    ),
    file_metadata = {
        "/etc/app/secret": file_metadata(mode = "0600"),
    },
)

# Layer with symlinks
image_layer(
    name = "bin_layer",
    srcs = {
        "/usr/local/bin/app": "//cmd/app",
    },
    symlinks = {
        "/usr/bin/app": "/usr/local/bin/app",
    },
)
```
""",
    attrs = {
        "srcs": attr.string_keyed_label_dict(
            doc = """Files to include in the layer. Keys are paths in the image (e.g., "/app/bin/server"),
values are labels to files or executables. Executables automatically include their runfiles.""",
            allow_files = True,
        ),
        "symlinks": attr.string_dict(
            doc = """Symlinks to create in the layer. Keys are symlink paths in the image,
values are the targets they point to.""",
        ),
        "manifests": attr.label_list(
            doc = """List of targets providing FSManifestInfo. Files from these manifests will be added to the default layer/group.""",
            providers = [FSManifestInfo],
            default = [],
        ),
        "compress": attr.string(
            default = "auto",
            values = ["auto", "gzip", "zstd"],
            doc = """Compression algorithm to use. If set to 'auto', uses the global default compression setting.""",
        ),
        "estargz": attr.string(
            default = "auto",
            values = ["auto", "enabled", "disabled"],
            doc = """Whether to use estargz format. If set to 'auto', uses the global default estargz setting.
When enabled, the layer will be optimized for lazy pulling and will be compatible with the estargz format.""",
        ),
        "annotations": attr.string_dict(
            default = {},
            doc = """Annotations to add to the layer metadata as key-value pairs.""",
        ),
        "default_metadata": attr.string(
            default = "",
            doc = """JSON-encoded default metadata to apply to all files in the layer.
Can include fields like mode, uid, gid, uname, gname, mtime, and pax_records.""",
        ),
        "file_metadata": attr.string_dict(
            default = {},
            doc = """Per-file metadata overrides as a dict mapping file paths to JSON-encoded metadata.
The path should match the path in the image (the key in srcs attribute).
Metadata specified here overrides any defaults from default_metadata.""",
        ),
        "default_grouping": attr.string(
            default = "layer_per_group",
            values = ["layer_per_group", "merge_all"],
            doc = """Determines how files are grouped into layers when multiple groups are present.
If layer_ids is specified, this attribute is ignored.

- layer_per_group: Creates one layer per unique group found in srcs (or a single layer if no groups are found). This is the default.
- merge_all: Merges all files into a single layer (ignoring groups).
""",
        ),
        "layer_ids": attr.string_list(
            doc = """Ordered list of layer IDs to create for the purpose of *grouping*.
If unspecified, the following defaults are used based on default_grouping:

- layer_per_group: One layer per unique group in srcs
- merge_all: A single layer containing all files

If specified, the layers will be created in the order given.""",
        ),
        "default_layer_id": attr.string(
            doc = """Default layer ID to assign to files that do not specify a group.
If empty, files without a group are assigned to the last layer id.""",
        ),
        "include_groups": attr.string_list(
            doc = """Allowlist of group names to include. If empty, all groups are included. Mutually exclusive with exclude_groups.""",
        ),
        "exclude_groups": attr.string_list(
            doc = """Denylist of group names to exclude. If empty, no groups are excluded. Mutually exclusive with include_groups.""",
        ),
        "layer_for_group": attr.string_dict(
            doc = """Mapping of group names to layer IDs. Files in a group will be assigned to the specified layer.
If a group is not listed here, files in that group will be assigned to the default layer (refer to default_layer_id for more information).
If not specified, the following default behavior is used based on default_grouping and layer_ids:

- layer_ids set: Groups with names matching layer_ids are assigned to those layers; others go to the default layer.
- layer_per_group is set, layer_ids is unset: Each group is assigned to its own layer and layer_for_group is ignored.
- merge_all is set, layer_ids is unset: All groups are merged into a single layer.
""",
        ),
        "_default_compression": attr.label(
            default = Label("//img/settings:compress"),
            providers = [BuildSettingInfo],
        ),
        "_default_estargz": attr.label(
            default = Label("//img/settings:estargz"),
            providers = [BuildSettingInfo],
        ),
        "_compression_jobs": attr.label(
            default = Label("//img/settings:compression_jobs"),
            providers = [BuildSettingInfo],
        ),
        "_compression_level": attr.label(
            default = Label("//img/settings:compression_level"),
            providers = [BuildSettingInfo],
        ),
    },
    toolchains = TOOLCHAINS,
)
