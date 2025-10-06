"""Repository rules for fetching external tools in WORKSPACE mode"""

load("//img/private/prebuilt:prebuilt.bzl", "prebuilt_collection_hub_repo", "prebuilt_pull_hub_repo")

def _img_prebuilt_tool_from_lockfile_impl(rctx):
    """Repository rule that reads lockfile and downloads tool for specific platform."""
    lockfile_content = rctx.read(rctx.attr.lockfile)
    lockfile_data = json.decode(lockfile_content)

    # Find the tool entry for our target platform
    target_tool = None
    for tool in lockfile_data:
        if tool["os"] == rctx.attr.os and tool["cpu"] == rctx.attr.cpu:
            target_tool = tool
            break

    if not target_tool:
        fail("No tool found in lockfile for platform %s_%s" % (rctx.attr.os, rctx.attr.cpu))

    # Download the tool using the same logic as prebuilt_img_tool_repo
    extension = "exe" if target_tool["os"] == "windows" else ""
    dot = "." if len(extension) > 0 else ""
    url_templates = target_tool.get("url_templates", ["https://github.com/bazel-contrib/rules_img/releases/download/{version}/img_{os}_{cpu}{dot}{extension}"])

    urls = [template.format(
        version = target_tool["version"],
        os = target_tool["os"],
        cpu = target_tool["cpu"],
        dot = dot,
        extension = extension,
    ) for template in url_templates]

    rctx.download(
        urls,
        output = "img.exe",
        executable = True,
        integrity = target_tool["integrity"],
    )

    rctx.file(
        "BUILD.bazel",
        content = """exports_files(["img.exe"])""",
    )

_img_prebuilt_tool_from_lockfile = repository_rule(
    implementation = _img_prebuilt_tool_from_lockfile_impl,
    attrs = {
        "lockfile": attr.label(mandatory = True),
        "os": attr.string(mandatory = True),
        "cpu": attr.string(mandatory = True),
    },
)

def img_register_prebuilt_toolchains(
        name = "img_toolchain",
        lockfile = Label("@rules_img//:prebuilt_lockfile.json"),
        platforms = [
            ("linux", "amd64"),
            ("linux", "arm64"),
            ("darwin", "amd64"),
            ("darwin", "arm64"),
            ("windows", "amd64"),
            ("windows", "arm64"),
        ]):
    """Register prebuilt img toolchains for WORKSPACE mode.

    This macro creates repository rules for prebuilt img tools from a lockfile
    and registers them as Bazel toolchains. This is the WORKSPACE equivalent
    of the MODULE.bazel prebuilt_img_tool extension.

    Usage in WORKSPACE:
        load("@rules_img//img:repositories.bzl", "img_register_prebuilt_toolchains")

        # Use defaults
        img_register_prebuilt_toolchains()

        # Or specify custom platforms/lockfile
        img_register_prebuilt_toolchains(
            lockfile = "@my_repo//:my_lockfile.json",
            platforms = [("linux", "amd64"), ("darwin", "amd64")]
        )

        # Then register the toolchains
        register_toolchains("@%s//:all" % name)

    Args:
        name: Name of the toolchain collection hub repository (default: "img_toolchain")
        lockfile: Label pointing to the prebuilt lockfile.json (default: "@rules_img//:prebuilt_lockfile.json")
        platforms: List of (os, cpu) tuples for platforms to support
    """

    # Create individual tool repositories for each requested platform
    tools = {}
    for (os, cpu) in platforms:
        repo_name = "%s_%s_%s" % (name, os, cpu)

        # Create repository that reads lockfile and downloads tool for this platform
        _img_prebuilt_tool_from_lockfile(
            name = repo_name,
            lockfile = lockfile,
            os = os,
            cpu = cpu,
        )

        # Track this tool for the hub repository
        platform_key = "%s_%s" % (os, cpu)
        tools[platform_key] = "@%s//:img.exe" % repo_name

    # Create the hub repository with all toolchain definitions
    prebuilt_collection_hub_repo(
        name = name,
        tools = tools,
    )

def _pull_tool_prebuilt_tool_from_lockfile_impl(rctx):
    """Repository rule that reads lockfile and downloads pull_tool for specific platform."""
    lockfile_content = rctx.read(rctx.attr.lockfile)
    lockfile_data = json.decode(lockfile_content)

    # Find the tool entry for our target platform
    target_tool = None
    for tool in lockfile_data:
        if tool["os"] == rctx.attr.os and tool["cpu"] == rctx.attr.cpu:
            target_tool = tool
            break

    if not target_tool:
        fail("No pull_tool found in lockfile for platform %s_%s" % (rctx.attr.os, rctx.attr.cpu))

    # Download the tool using pull_tool URL templates
    extension = "exe" if target_tool["os"] == "windows" else ""
    dot = "." if len(extension) > 0 else ""
    url_templates = target_tool.get("url_templates", ["https://github.com/bazel-contrib/rules_img/releases/download/{version}/pull_tool_{os}_{cpu}{dot}{extension}"])

    urls = [template.format(
        version = target_tool["version"],
        os = target_tool["os"],
        cpu = target_tool["cpu"],
        dot = dot,
        extension = extension,
    ) for template in url_templates]

    rctx.download(
        urls,
        output = "pull_tool.exe",
        executable = True,
        integrity = target_tool["integrity"],
    )

    rctx.file(
        "BUILD.bazel",
        content = """exports_files(["pull_tool.exe"])""",
    )

_pull_tool_prebuilt_tool_from_lockfile = repository_rule(
    implementation = _pull_tool_prebuilt_tool_from_lockfile_impl,
    attrs = {
        "lockfile": attr.label(mandatory = True),
        "os": attr.string(mandatory = True),
        "cpu": attr.string(mandatory = True),
    },
)

def pull_tool_register_prebuilt_repositories(
        name = "pull_hub_repo",
        lockfile = Label("@rules_img//:pull_tool_lockfile.json"),
        platforms = [
            ("linux", "amd64"),
            ("linux", "arm64"),
            ("darwin", "amd64"),
            ("darwin", "arm64"),
            ("windows", "amd64"),
            ("windows", "arm64"),
        ]):
    """Register prebuilt pull_tool repositories for WORKSPACE mode.

    This macro creates repository rules for prebuilt pull_tool binaries from a lockfile
    but does NOT register them as toolchains (unlike img_register_prebuilt_toolchains).
    This is the WORKSPACE equivalent of the MODULE.bazel pull_tool extension.

    Usage in WORKSPACE:
        load("@rules_img//img:repositories.bzl", "pull_tool_register_prebuilt_repositories")

        # Use defaults
        pull_tool_register_prebuilt_repositories()

        # Or specify custom platforms/lockfile
        pull_tool_register_prebuilt_repositories(
            lockfile = "@my_repo//:my_pull_tool_lockfile.json",
            platforms = [("linux", "amd64"), ("darwin", "amd64")]
        )

    Args:
        name: Name of the pull_tool collection hub repository (default: "pull_hub_repo")
        lockfile: Label pointing to the pull_tool_lockfile.json (default: "@rules_img//:pull_tool_lockfile.json")
        platforms: List of (os, cpu) tuples for platforms to support
    """

    # Create individual tool repositories for each requested platform
    tools = {}
    for (os, cpu) in platforms:
        repo_name = "%s_%s_%s" % (name, os, cpu)

        # Create repository that reads lockfile and downloads pull_tool for this platform
        _pull_tool_prebuilt_tool_from_lockfile(
            name = repo_name,
            lockfile = lockfile,
            os = os,
            cpu = cpu,
        )

        # Track this tool for the hub repository
        platform_key = "%s_%s" % (os, cpu)
        tools[platform_key] = "@%s//:pull_tool.exe" % repo_name

    # Create the hub repository without toolchain definitions (just tool references)
    prebuilt_pull_hub_repo(
        name = name,
        tools = tools,
    )
