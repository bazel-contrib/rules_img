"""Custom rules for testing RunfilesGroupInfo with image_layer."""

load("@runfilesgroupinfo.bzl", "SAME_PARTY_RUNFILES", "OTHER_PARTY_RUNFILES", "FOUNDATIONAL_RUNFILES", "DEBUG_RUNFILES", "RunfilesGroupInfo")

def _fake_split_binary_impl(ctx):
    """A fake split_binary rule that produces an executable with RunfilesGroupInfo.

    This rule simulates a binary that has been split into multiple groups for
    layering purposes. It creates an executable and organizes its runfiles into
    different groups based on the split_config attribute.
    """

    # Create the executable script
    executable = ctx.actions.declare_file(ctx.attr.name)
    ctx.actions.write(
        output = executable,
        content = ctx.attr.script_content,
        is_executable = True,
    )

    # Collect all input files
    all_files = []
    for dep in ctx.attr.deps:
        all_files.extend(dep[DefaultInfo].files.to_list())

    # Split files into groups based on the split_config
    # Format: {"group_name": ["pattern1", "pattern2", ...]}
    groups = {}
    remaining_files = list(all_files)

    # Process split_config to assign files to groups
    for group_name, patterns in ctx.attr.split_config.items():
        group_files = []
        for pattern in patterns:
            matched = []
            for f in remaining_files:
                if pattern in f.path or pattern in f.basename:
                    group_files.append(f)
                    matched.append(f)
            # Remove matched files from remaining
            for f in matched:
                remaining_files.remove(f)

        if group_files:
            groups[group_name] = depset(group_files)

    # Assign remaining files to the default group
    if remaining_files:
        default_group = ctx.attr.default_group
        if default_group in groups:
            # Merge with existing
            groups[default_group] = depset(transitive = [groups[default_group], depset(remaining_files)])
        else:
            groups[default_group] = depset(remaining_files)

    # Create RunfilesGroupInfo
    runfiles_group_info = RunfilesGroupInfo(**groups)

    # Create standard runfiles (union of all groups)
    all_runfiles = []
    for group_files in groups.values():
        all_runfiles.append(group_files)

    runfiles = ctx.runfiles(
        files = [executable],
        transitive_files = depset(transitive = all_runfiles),
    )

    return [
        DefaultInfo(
            executable = executable,
            files = depset([executable]),
            runfiles = runfiles,
        ),
        runfiles_group_info,
    ]

fake_split_binary = rule(
    implementation = _fake_split_binary_impl,
    doc = """Creates a fake executable with RunfilesGroupInfo for testing layer grouping.

    This rule simulates a binary that has been split into multiple groups. It takes
    a set of dependencies and splits their files into different groups based on
    patterns, then exposes these groups via RunfilesGroupInfo.

    Example:
        fake_split_binary(
            name = "my_app",
            script_content = "#!/bin/bash\\necho 'Hello'",
            deps = [":lib1", ":lib2", ":lib3"],
            split_config = {
                "FOUNDATIONAL_RUNFILES": ["stdlib", "runtime"],
                "OTHER_PARTY_RUNFILES": ["third_party"],
                "DEBUG_RUNFILES": ["debug", "test"],
            },
            default_group = "SAME_PARTY_RUNFILES",
        )
    """,
    attrs = {
        "script_content": attr.string(
            mandatory = True,
            doc = "The content of the executable script",
        ),
        "deps": attr.label_list(
            allow_files = True,
            doc = "Dependencies whose files will be included as runfiles",
        ),
        "split_config": attr.string_list_dict(
            default = {},
            doc = """Configuration for splitting files into groups.
            Keys are group names (e.g., SAME_PARTY_RUNFILES, OTHER_PARTY_RUNFILES).
            Values are lists of patterns to match against file paths.""",
        ),
        "default_group": attr.string(
            default = SAME_PARTY_RUNFILES,
            doc = "The default group for files that don't match any pattern",
        ),
    },
    executable = True,
)
