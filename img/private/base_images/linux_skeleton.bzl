"""Rule describing the empty directory skeleton of a Linux base image."""

load("//img/private/base_images:common.bzl", "SCOPE_LINUX", "base_content_attrs", "empty_content", "in_scope", "run_base_verb")
load("//img/private/common:build.bzl", "TOOLCHAINS")
load("//img/private/providers:base_image_content_info.bzl", "BaseImageContentInfo")

# The directory groups the rule exposes, each as its own tri-state attribute.
# The names must match the group names the `img base skeleton` verb knows.
_GROUPS = [
    "etc",
    "bin_and_lib",
    "home",
    "root",
    "tmp",
    "var",
    "run",
    "mount_points",
    "opt_srv",
]

def _group_attr(doc):
    return attr.string(
        default = "auto",
        values = ["auto", "enabled", "disabled"],
        doc = doc,
    )

def _linux_skeleton_impl(ctx):
    if not in_scope(ctx, SCOPE_LINUX):
        return empty_content()

    args = ctx.actions.args()
    for group in _GROUPS:
        # "auto" means enabled: the point of the rule is a working skeleton, and
        # a caller who wants less says so explicitly.
        value = getattr(ctx.attr, group)
        if value == "disabled":
            args.add("--group", "{}=disabled".format(group))

    args.add("--usr-merged" if ctx.attr.usr_merged else "--usr-merged=false")
    for path, metadata in ctx.attr.extra_directories.items():
        args.add("--directory", "{}={}".format(path, metadata))

    return run_base_verb(ctx, ["skeleton"], args)

linux_skeleton = rule(
    implementation = _linux_skeleton_impl,
    doc = """Describes the empty directory skeleton of a Linux base image.

The defaults are a working Filesystem Hierarchy Standard layout with the modes a
stock distribution uses: `0755` for anything the system owns, `1777` for the
shared scratch directories, `0700` for `/root`, and `0555` for the kernel
pseudo-filesystem mount points. Each group of directories can be turned off
individually.

This rule only applies when targeting Linux. On any other platform it is a
no-op that contributes nothing to the layer.

Example:

```python
load("@rules_img//img:base_images.bzl", "linux_skeleton")

# A minimal skeleton for a static binary that needs nowhere to write.
linux_skeleton(
    name = "skeleton",
    home = "disabled",
    opt_srv = "disabled",
    var = "disabled",
)
```
""",
    attrs = base_content_attrs({
        "usr_merged": attr.bool(
            default = True,
            doc = """Whether to use the merged-`/usr` layout.

When True, `/bin`, `/sbin`, `/lib` and `/lib64` are symlinks into `/usr`, as
every current distribution does. When False they are real directories.

Set the same value on `system_libraries`, which places libraries accordingly.
Mismatching them fails the build rather than producing an image whose libraries
sit inside a symlink and cannot be found.""",
        ),
        "etc": _group_attr("Whether to create `/etc`."),
        "bin_and_lib": _group_attr(
            """Whether to create the binary and library directories.

Covers `/usr` and its subdirectories, plus the top-level `/bin`, `/sbin`, `/lib`
and `/lib64` (as symlinks or real directories, per `usr_merged`).""",
        ),
        "home": _group_attr("Whether to create `/home`."),
        "root": _group_attr("Whether to create `/root`, with mode `0700`."),
        "tmp": _group_attr("Whether to create `/tmp`, with the sticky mode `1777`."),
        "var": _group_attr(
            """Whether to create `/var` and its standard subdirectories.

Covers `/var/log`, `/var/tmp` (sticky), `/var/cache`, `/var/lib` and
`/var/spool`. When the `run` group is enabled too, `/var/run` and `/var/lock`
are added as symlinks into `/run`.""",
        ),
        "run": _group_attr("Whether to create `/run` and `/run/lock`."),
        "mount_points": _group_attr(
            """Whether to create the standard mount points.

Covers `/dev`, `/proc` and `/sys` (the kernel pseudo-filesystems a runtime
mounts over) plus `/mnt` and `/media`.""",
        ),
        "opt_srv": _group_attr("Whether to create `/opt` and `/srv`."),
        "extra_directories": attr.string_dict(
            doc = """Additional directories to create, mapping path to JSON-encoded metadata.

Build the metadata with `file_metadata()` from `@rules_img//img:layer.bzl`. An
empty value uses mode `0755`, owned by root.

```python
extra_directories = {
    "/var/lib/myapp": file_metadata(mode = "0750", uid = 1000, gid = 1000),
}
```""",
        ),
    }),
    toolchains = TOOLCHAINS,
    provides = [BaseImageContentInfo],
)
