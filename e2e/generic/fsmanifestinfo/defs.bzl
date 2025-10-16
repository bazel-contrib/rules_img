"""Custom rules for testing FSManifestInfo with image_layer."""

load("@fsmanifestinfo.bzl", "FSManifestInfo")

def _fake_manifest_impl(ctx):
    """A fake rule that produces FSManifestInfo for testing.

    This rule simulates a filesystem manifest provider, mapping destination
    paths to source files. It's useful for testing how image_layer handles
    FSManifestInfo providers.
    """

    # Build the entries dictionary mapping destination paths to entry structs
    entries = {}

    for dest_path, src_file in ctx.attr.files.items():
        # Get the actual File object from the target
        files_list = src_file[DefaultInfo].files.to_list()
        if len(files_list) != 1:
            fail("Expected exactly one file for {}, got {}".format(dest_path, len(files_list)))

        file = files_list[0]

        # Create an entry struct with kind="file" and target=File
        entries[dest_path] = struct(
            kind = "file",
            target = file,
        )

    # Add symlinks from the symlinks attribute
    for symlink_path, target_path in ctx.attr.symlinks.items():
        entries[symlink_path] = struct(
            kind = "symlink",
            target = target_path,
        )

    # Add empty directories from the empty_dirs attribute
    for empty_dir_path in ctx.attr.empty_dirs:
        entries[empty_dir_path] = struct(
            kind = "empty_dir",
        )

    # Build the metadata dictionary if provided
    metadata = None
    if ctx.attr.file_metadata_mode or ctx.attr.file_metadata_uid or ctx.attr.file_metadata_gid or ctx.attr.file_metadata_owner or ctx.attr.file_metadata_group or ctx.attr.file_metadata_mtime or ctx.attr.file_metadata_xattrs:
        metadata = {}

        # Helper to build metadata struct for a path
        def get_metadata_for_path(path):
            meta_dict = {}
            if path in ctx.attr.file_metadata_mode:
                # Parse octal string to int
                meta_dict["mode"] = int(ctx.attr.file_metadata_mode[path], 8)
            if path in ctx.attr.file_metadata_uid:
                meta_dict["uid"] = int(ctx.attr.file_metadata_uid[path])
            if path in ctx.attr.file_metadata_gid:
                meta_dict["gid"] = int(ctx.attr.file_metadata_gid[path])
            if path in ctx.attr.file_metadata_owner:
                meta_dict["owner"] = ctx.attr.file_metadata_owner[path]
            if path in ctx.attr.file_metadata_group:
                meta_dict["group"] = ctx.attr.file_metadata_group[path]
            if path in ctx.attr.file_metadata_mtime:
                meta_dict["mtime"] = int(ctx.attr.file_metadata_mtime[path])
            if path in ctx.attr.file_metadata_xattrs:
                # Parse JSON string to dict
                meta_dict["xattrs"] = json.decode(ctx.attr.file_metadata_xattrs[path])
            return struct(**meta_dict) if meta_dict else None

        # Build metadata for each file that has any metadata set
        for path in entries.keys():
            meta = get_metadata_for_path(path)
            if meta:
                metadata[path] = meta

    # Build defaults if provided
    defaults = None
    if ctx.attr.default_mode or ctx.attr.default_uid or ctx.attr.default_gid or ctx.attr.default_owner or ctx.attr.default_group or ctx.attr.default_mtime or ctx.attr.default_xattrs:
        defaults_dict = {}
        if ctx.attr.default_mode:
            defaults_dict["mode"] = int(ctx.attr.default_mode, 8)
        if ctx.attr.default_uid:
            defaults_dict["uid"] = int(ctx.attr.default_uid)
        if ctx.attr.default_gid:
            defaults_dict["gid"] = int(ctx.attr.default_gid)
        if ctx.attr.default_owner:
            defaults_dict["owner"] = ctx.attr.default_owner
        if ctx.attr.default_group:
            defaults_dict["group"] = ctx.attr.default_group
        if ctx.attr.default_mtime:
            defaults_dict["mtime"] = int(ctx.attr.default_mtime)
        if ctx.attr.default_xattrs:
            defaults_dict["xattrs"] = json.decode(ctx.attr.default_xattrs)
        defaults = struct(**defaults_dict)

    # Create FSManifestInfo
    fs_manifest_info = FSManifestInfo(
        entries = entries,
        metadata = metadata,
        defaults = defaults,
    )

    # Return the provider along with DefaultInfo
    # DefaultInfo provides the source files for dependency tracking
    all_files = []
    for target in ctx.attr.files.values():
        all_files.extend(target[DefaultInfo].files.to_list())

    return [
        DefaultInfo(files = depset(all_files)),
        fs_manifest_info,
    ]

fake_manifest = rule(
    implementation = _fake_manifest_impl,
    doc = """Creates a fake FSManifestInfo provider for testing image_layer.

    This rule creates a filesystem manifest that maps destination paths to
    source files. It's useful for testing how rules_img handles FSManifestInfo.

    Example:
        fake_manifest(
            name = "my_manifest",
            files = {
                "/etc/app/config.yaml": ":config.yaml",
                "/usr/share/app/data.txt": ":data.txt",
            },
            metadata = {
                "/etc/app/config.yaml": '{"mode": "0644"}',
            },
        )
    """,
    attrs = {
        "files": attr.string_keyed_label_dict(
            mandatory = True,
            allow_files = True,
            doc = """Mapping of destination paths to source file labels.
            Keys are absolute paths where files will be placed in the image.
            Values are labels pointing to the source files.""",
        ),
        "symlinks": attr.string_dict(
            default = {},
            doc = """Symlinks to create in the layer. Keys are symlink paths,
            values are the targets they point to.""",
        ),
        "empty_dirs": attr.string_list(
            default = [],
            doc = """List of empty directory paths to create in the layer.""",
        ),
        # File-specific metadata attributes
        "file_metadata_mode": attr.string_dict(
            default = {},
            doc = "Per-file mode metadata as octal strings (e.g., '0644'). Keys are destination paths.",
        ),
        "file_metadata_uid": attr.string_dict(
            default = {},
            doc = "Per-file UID metadata as strings. Keys are destination paths.",
        ),
        "file_metadata_gid": attr.string_dict(
            default = {},
            doc = "Per-file GID metadata as strings. Keys are destination paths.",
        ),
        "file_metadata_owner": attr.string_dict(
            default = {},
            doc = "Per-file owner name metadata. Keys are destination paths.",
        ),
        "file_metadata_group": attr.string_dict(
            default = {},
            doc = "Per-file group name metadata. Keys are destination paths.",
        ),
        "file_metadata_mtime": attr.string_dict(
            default = {},
            doc = "Per-file mtime metadata as Unix epoch seconds (strings). Keys are destination paths.",
        ),
        "file_metadata_xattrs": attr.string_dict(
            default = {},
            doc = "Per-file extended attributes as JSON strings (e.g., '{\"user.key\": \"value\"}'). Keys are destination paths.",
        ),
        # Default metadata attributes
        "default_mode": attr.string(
            default = "",
            doc = "Default mode for all files as octal string (e.g., '0644').",
        ),
        "default_uid": attr.string(
            default = "",
            doc = "Default UID for all files as string.",
        ),
        "default_gid": attr.string(
            default = "",
            doc = "Default GID for all files as string.",
        ),
        "default_owner": attr.string(
            default = "",
            doc = "Default owner name for all files.",
        ),
        "default_group": attr.string(
            default = "",
            doc = "Default group name for all files.",
        ),
        "default_mtime": attr.string(
            default = "",
            doc = "Default mtime for all files as Unix epoch seconds (string).",
        ),
        "default_xattrs": attr.string(
            default = "",
            doc = "Default extended attributes for all files as JSON string (e.g., '{\"user.key\": \"value\"}').",
        ),
    },
)
