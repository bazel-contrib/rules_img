"""Rules describing the standard files under /etc."""

load("//img/private/base_images:common.bzl", "SCOPE_LINUX", "SCOPE_UNIX", "base_content_attrs", "empty_content", "expand_templates", "in_scope", "merge_sources", "run_base_verb")
load("//img/private/common:build.bzl", "TOOLCHAINS")
load("//img/private/providers:base_image_content_info.bzl", "BaseImageContentInfo")

def _etc_environment_impl(ctx):
    if not in_scope(ctx, SCOPE_LINUX):
        return empty_content()

    args = ctx.actions.args()
    templates = expand_templates(ctx, {"env": ctx.attr.env})
    inputs = []
    if templates != None:
        args.add("--templates", templates)
        inputs.append(templates)
    else:
        for key, value in ctx.attr.env.items():
            args.add("--env", "{}={}".format(key, value))

    srcs = merge_sources(ctx, ctx.attr.srcs)
    args.add_all(srcs, before_each = "--src")
    inputs.extend(srcs)

    args.add("--path", ctx.attr.path)
    args.add("--quote" if ctx.attr.quote else "--quote=false")
    if ctx.attr.mode:
        args.add("--mode", ctx.attr.mode)

    return run_base_verb(ctx, ["etc", "environment"], args, inputs = [inputs])

etc_environment = rule(
    implementation = _etc_environment_impl,
    doc = """Describes `/etc/environment`, the system-wide environment file read by PAM.

The file holds one `KEY="value"` assignment per line. Unlike a shell profile it
is not executed, so values are literal: no expansion, no `export`, no
conditionals.

This rule only applies when targeting Linux. On any other platform it is a
no-op that contributes nothing to the layer.

Example:

```python
load("@rules_img//img:base_images.bzl", "etc_environment")

etc_environment(
    name = "environment",
    env = {
        "PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
        "LANG": "C.UTF-8",
    },
)
```
""",
    attrs = base_content_attrs({
        "env": attr.string_dict(
            doc = """Environment variables to write, as a mapping of name to value.

Values support [template expansion](/docs/templating.md).""",
        ),
        "srcs": attr.label_list(
            doc = """Existing environment files to merge in.

Files are read in order and later files win. Anything set via `env` overrides
all of them.""",
            allow_files = True,
        ),
        "path": attr.string(
            default = "/etc/environment",
            doc = "Path of the file inside the image.",
        ),
        "quote": attr.bool(
            default = True,
            doc = """Whether to wrap values in double quotes.

Quoting is conventional and is what a distribution ships. Turn it off only if
something in the image parses the file naively.""",
        ),
        "mode": attr.string(
            doc = """Octal file mode, e.g. `"0644"`. Defaults to `0644`.""",
        ),
    }),
    toolchains = TOOLCHAINS,
    provides = [BaseImageContentInfo],
)

def _etc_hosts_impl(ctx):
    if not in_scope(ctx, SCOPE_LINUX):
        return empty_content()

    args = ctx.actions.args()
    templates = expand_templates(ctx, {"hosts": ctx.attr.hosts})
    inputs = []
    if templates != None:
        args.add("--templates", templates)
        inputs.append(templates)
    else:
        for address, names in ctx.attr.hosts.items():
            args.add("--host", "{}={}".format(address, names))

    srcs = merge_sources(ctx, ctx.attr.srcs)
    args.add_all(srcs, before_each = "--src")
    inputs.extend(srcs)

    args.add("--path", ctx.attr.path)
    args.add("--include-defaults" if ctx.attr.include_defaults else "--include-defaults=false")
    if ctx.attr.mode:
        args.add("--mode", ctx.attr.mode)

    return run_base_verb(ctx, ["etc", "hosts"], args, inputs = [inputs])

etc_hosts = rule(
    implementation = _etc_hosts_impl,
    doc = """Describes `/etc/hosts`, the static hostname table.

Names accumulate per address rather than replacing each other, so the default
loopback entries and your own names for `127.0.0.1` end up on the same line.

This rule only applies when targeting Linux. On any other platform it is a
no-op that contributes nothing to the layer.

Example:

```python
load("@rules_img//img:base_images.bzl", "etc_hosts")

etc_hosts(
    name = "hosts",
    hosts = {
        "10.0.0.7": "database db.internal",
        "127.0.0.1": "myapp.local",
    },
)
```
""",
    attrs = base_content_attrs({
        "hosts": attr.string_dict(
            doc = """Host mappings, as a mapping of IP address to space-separated host names.

Values support [template expansion](/docs/templating.md).""",
        ),
        "srcs": attr.label_list(
            doc = "Existing hosts files to merge in, read in order.",
            allow_files = True,
        ),
        "include_defaults": attr.bool(
            default = True,
            doc = """Whether to include the standard loopback entries.

These are the `127.0.0.1 localhost` and IPv6 `::1`/`ff02::` entries that
`netbase` ships. Without them, resolving `localhost` inside the container
depends on the resolver's own fallbacks.""",
        ),
        "path": attr.string(
            default = "/etc/hosts",
            doc = "Path of the file inside the image.",
        ),
        "mode": attr.string(
            doc = """Octal file mode, e.g. `"0644"`. Defaults to `0644`.""",
        ),
    }),
    toolchains = TOOLCHAINS,
    provides = [BaseImageContentInfo],
)

def _etc_release_impl(ctx):
    if not in_scope(ctx, SCOPE_LINUX):
        return empty_content()

    args = ctx.actions.args()
    templates = expand_templates(ctx, {
        "lsb_release": ctx.attr.lsb_release,
        "os_release": ctx.attr.os_release,
    })
    inputs = []
    if templates != None:
        args.add("--templates", templates)
        inputs.append(templates)
    else:
        for key, value in ctx.attr.os_release.items():
            args.add("--os-release", "{}={}".format(key, value))
        for key, value in ctx.attr.lsb_release.items():
            args.add("--lsb-release", "{}={}".format(key, value))

    os_release_srcs = merge_sources(ctx, ctx.attr.os_release_srcs)
    lsb_release_srcs = merge_sources(ctx, ctx.attr.lsb_release_srcs)
    args.add_all(os_release_srcs, before_each = "--os-release-src")
    args.add_all(lsb_release_srcs, before_each = "--lsb-release-src")
    inputs.extend(os_release_srcs)
    inputs.extend(lsb_release_srcs)

    args.add("--os-release-path", ctx.attr.os_release_path)
    args.add("--lsb-release-path", ctx.attr.lsb_release_path)
    args.add("--usr-lib-path", ctx.attr.usr_lib_path)
    args.add("--write-lsb-release" if ctx.attr.write_lsb_release else "--write-lsb-release=false")
    args.add("--usr-lib-symlink" if ctx.attr.usr_lib_symlink else "--usr-lib-symlink=false")
    if ctx.attr.mode:
        args.add("--mode", ctx.attr.mode)

    return run_base_verb(ctx, ["etc", "release"], args, inputs = [inputs])

etc_release = rule(
    implementation = _etc_release_impl,
    doc = """Describes `/etc/os-release` (and optionally `/etc/lsb-release`).

`os-release` is the standard way for software in the image to identify the
distribution it is running on. Values are always written double-quoted, which is
valid for every field.

By default the real file is written to `/usr/lib/os-release` with
`/etc/os-release` as a relative symlink to it, which is what
[os-release(5)](https://www.freedesktop.org/software/systemd/man/os-release.html)
specifies so the identity travels with `/usr`.

This rule only applies when targeting Linux. On any other platform it is a
no-op that contributes nothing to the layer.

Example:

```python
load("@rules_img//img:base_images.bzl", "etc_release")

etc_release(
    name = "release",
    os_release = {
        "ID": "acme-base",
        "NAME": "ACME Base Image",
        "PRETTY_NAME": "ACME Base Image {{.VERSION}}",
        "VERSION_ID": "{{.VERSION}}",
    },
    build_settings = {"VERSION": "//settings:version"},
)
```
""",
    attrs = base_content_attrs({
        "os_release": attr.string_dict(
            doc = """`os-release` entries, as a mapping of key to value.

Values support [template expansion](/docs/templating.md).""",
        ),
        "lsb_release": attr.string_dict(
            doc = """`lsb-release` entries, as a mapping of key to value.

Only written when `write_lsb_release` is True. Values support
[template expansion](/docs/templating.md).""",
        ),
        "os_release_srcs": attr.label_list(
            doc = "Existing os-release files to merge in, read in order.",
            allow_files = True,
        ),
        "lsb_release_srcs": attr.label_list(
            doc = "Existing lsb-release files to merge in, read in order.",
            allow_files = True,
        ),
        "write_lsb_release": attr.bool(
            default = False,
            doc = """Whether to also write the older LSB release file.

Most images do not need it; `os-release` is what current software reads.""",
        ),
        "usr_lib_symlink": attr.bool(
            default = True,
            doc = """Whether to write the real file under `/usr/lib` and make `/etc/os-release` a symlink.

Set to False to write a plain file at `os_release_path` instead.""",
        ),
        "os_release_path": attr.string(
            default = "/etc/os-release",
            doc = "Path of the os-release file (or its symlink) inside the image.",
        ),
        "usr_lib_path": attr.string(
            default = "/usr/lib/os-release",
            doc = "Path of the real os-release file when `usr_lib_symlink` is True.",
        ),
        "lsb_release_path": attr.string(
            default = "/etc/lsb-release",
            doc = "Path of the lsb-release file inside the image.",
        ),
        "mode": attr.string(
            doc = """Octal file mode, e.g. `"0644"`. Defaults to `0644`.""",
        ),
    }),
    toolchains = TOOLCHAINS,
    provides = [BaseImageContentInfo],
)

def _etc_passwd_impl(ctx):
    if not in_scope(ctx, SCOPE_UNIX):
        return empty_content()

    args = ctx.actions.args()
    templates = expand_templates(ctx, {
        "groups": ctx.attr.groups,
        "users": ctx.attr.users,
    })
    inputs = []
    if templates != None:
        args.add("--templates", templates)
        inputs.append(templates)
    else:
        args.add_all(ctx.attr.users, before_each = "--user")
        args.add_all(ctx.attr.groups, before_each = "--group")

    passwd_srcs = merge_sources(ctx, ctx.attr.passwd_srcs)
    group_srcs = merge_sources(ctx, ctx.attr.group_srcs)
    shadow_srcs = merge_sources(ctx, ctx.attr.shadow_srcs)
    args.add_all(passwd_srcs, before_each = "--passwd-src")
    args.add_all(group_srcs, before_each = "--group-src")
    args.add_all(shadow_srcs, before_each = "--shadow-src")
    inputs.extend(passwd_srcs)
    inputs.extend(group_srcs)
    inputs.extend(shadow_srcs)

    args.add("--passwd-path", ctx.attr.passwd_path)
    args.add("--group-path", ctx.attr.group_path)
    args.add("--shadow-path", ctx.attr.shadow_path)
    args.add("--create-home-directories" if ctx.attr.create_home_directories else "--create-home-directories=false")
    args.add("--write-shadow" if ctx.attr.write_shadow else "--write-shadow=false")
    args.add("--home-mode", ctx.attr.home_directory_mode)
    if ctx.attr.mode:
        args.add("--mode", ctx.attr.mode)
    if ctx.attr.shadow_mode:
        args.add("--shadow-mode", ctx.attr.shadow_mode)

    return run_base_verb(ctx, ["etc", "passwd"], args, inputs = [inputs])

etc_passwd = rule(
    implementation = _etc_passwd_impl,
    doc = """Describes `/etc/passwd`, `/etc/group`, `/etc/shadow` and home directories.

Users and groups are declared with the `passwd_entry()` and `group_entry()`
helpers, and may also be merged in from existing files. Duplicate user names,
UIDs, group names or GIDs are an error rather than a last-one-wins merge.

Shadow entries are always written locked (`!*`). This rule never accepts a
password hash: anything it wrote would be readable by anyone who can pull the
image or read the Bazel cache.

This rule applies when targeting any Unix-like platform (Linux, macOS, the
BSDs). On Windows it is a no-op that contributes nothing to the layer.

Example:

```python
load("@rules_img//img:base_images.bzl", "etc_passwd", "group_entry", "passwd_entry")

etc_passwd(
    name = "users",
    users = [
        passwd_entry(username = "root", uid = 0, gid = 0, home = "/root", shell = "/bin/sh"),
        passwd_entry(username = "app", uid = 1000, gid = 1000, home = "/home/app"),
    ],
    groups = [
        group_entry(name = "root", gid = 0),
        group_entry(name = "app", gid = 1000, users = ["app"]),
    ],
)
```
""",
    attrs = base_content_attrs({
        "users": attr.string_list(
            doc = """Users to define, as JSON strings built by `passwd_entry()`.

Values support [template expansion](/docs/templating.md).""",
        ),
        "groups": attr.string_list(
            doc = """Groups to define, as JSON strings built by `group_entry()`.

Values support [template expansion](/docs/templating.md).""",
        ),
        "passwd_srcs": attr.label_list(
            doc = "Existing passwd files to merge in.",
            allow_files = True,
        ),
        "group_srcs": attr.label_list(
            doc = "Existing group files to merge in.",
            allow_files = True,
        ),
        "shadow_srcs": attr.label_list(
            doc = """Existing shadow files to merge in.

Records found here are carried over verbatim, ageing fields included. Users with
no record get a locked entry.""",
            allow_files = True,
        ),
        "create_home_directories": attr.bool(
            default = True,
            doc = """Whether to create a directory for each user's home.

Users whose home is `/` (the convention for a system account without one) are
skipped. Root's home is always created with mode `0700`.""",
        ),
        "write_shadow": attr.bool(
            default = True,
            doc = "Whether to write `/etc/shadow` with a locked entry per user.",
        ),
        "home_directory_mode": attr.string(
            default = "0750",
            doc = "Octal mode of created home directories.",
        ),
        "passwd_path": attr.string(
            default = "/etc/passwd",
            doc = "Path of the passwd file inside the image.",
        ),
        "group_path": attr.string(
            default = "/etc/group",
            doc = "Path of the group file inside the image.",
        ),
        "shadow_path": attr.string(
            default = "/etc/shadow",
            doc = "Path of the shadow file inside the image.",
        ),
        "mode": attr.string(
            doc = """Octal mode of passwd and group, e.g. `"0644"`. Defaults to `0644`.""",
        ),
        "shadow_mode": attr.string(
            doc = """Octal mode of the shadow file. Defaults to `0640`.""",
        ),
    }),
    toolchains = TOOLCHAINS,
    provides = [BaseImageContentInfo],
)

def passwd_entry(
        *,
        username,
        uid,
        gid,
        gecos = None,
        home = None,
        shell = None):
    """Describes one user for the `users` attribute of `etc_passwd`.

    Args:
        username: Login name. String.
        uid: Numeric user ID. Integer.
        gid: Numeric ID of the user's primary group. Integer.
        gecos: Human-readable description (the GECOS field). String.
        home: Home directory. Defaults to `/`, meaning the user has none.
        shell: Login shell. Defaults to `/sbin/nologin`, which denies interactive login.

    Returns:
        A JSON-encoded string describing the user.
    """
    entry = {
        "gid": gid,
        "uid": uid,
        "username": username,
    }
    if gecos != None:
        entry["gecos"] = gecos
    if home != None:
        entry["home"] = home
    if shell != None:
        entry["shell"] = shell
    return json.encode(entry)

def group_entry(*, name, gid, users = None):
    """Describes one group for the `groups` attribute of `etc_passwd`.

    Args:
        name: Group name. String.
        gid: Numeric group ID. Integer.
        users: Names of users who are members of this group beyond the ones that
            have it as their primary group. List of strings.

    Returns:
        A JSON-encoded string describing the group.
    """
    entry = {
        "gid": gid,
        "name": name,
    }
    if users != None:
        entry["users"] = users
    return json.encode(entry)
