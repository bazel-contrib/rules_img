"""Shared plumbing for the rules that describe base image content.

Every content rule follows the same shape: check whether the rule applies to the
target platform at all, expand any templated attributes, then run one
`img base <verb>` action that writes a single metadata stream. The helpers here
own that shape so the rules themselves only have to build their own flags.
"""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private:stamp.bzl", "expand_or_write")
load("//img/private/common:build.bzl", "TOOLCHAIN")
load("//img/private/config:defs.bzl", "TargetPlatformInfo")
load("//img/private/providers:base_image_content_info.bzl", "BaseImageContentInfo")
load("//img/private/providers:stamp_setting_info.bzl", "StampSettingInfo")

# Scopes decide which target platforms a rule applies to. A rule outside its
# scope is not an error -- it is a no-op, so the same BUILD file can describe a
# base image for several platforms without a select() around every rule.
SCOPE_ALL = "all"
SCOPE_UNIX = "unix"
SCOPE_LINUX = "linux"

# The GOOS values that count as Unix for SCOPE_UNIX. Windows is the only
# supported platform that is not on this list, but naming the members rather
# than excluding windows keeps a future non-Unix platform from silently
# acquiring an /etc/passwd.
_UNIX_OS = [
    "android",
    "darwin",
    "dragonfly",
    "freebsd",
    "illumos",
    "ios",
    "linux",
    "netbsd",
    "openbsd",
    "solaris",
]

def in_scope(ctx, scope):
    """Reports whether a rule applies to the target platform.

    Args:
        ctx: Rule context. Must have the attributes from `base_content_attrs()`.
        scope: One of SCOPE_ALL, SCOPE_UNIX or SCOPE_LINUX.

    Returns:
        True when the rule should produce content, False when it is a no-op.
    """
    if scope == SCOPE_ALL:
        return True
    os = ctx.attr._os_cpu[TargetPlatformInfo].os
    if scope == SCOPE_LINUX:
        return os == "linux"
    if scope == SCOPE_UNIX:
        return os in _UNIX_OS
    fail("unknown scope: {}".format(scope))

def empty_content():
    """Returns the providers of a rule that is a no-op on the target platform.

    The rule still analyses and still returns a BaseImageContentInfo, so a
    `base_image_layer` depending on it needs no platform-specific plumbing; the
    provider is simply empty.
    """
    return [
        DefaultInfo(files = depset()),
        BaseImageContentInfo(metadata = depset(), files = depset()),
    ]

# Attributes every content rule needs: the target platform (for scope checks)
# and the template expansion / stamping pair that mirrors image_manifest.
_BASE_CONTENT_ATTRS = dict(
    build_settings = attr.string_keyed_label_dict(
        doc = """Build settings for template expansion.

Maps template variable names to `string_flag` targets. The values can be
referenced from this rule's templated attributes with `{{.VARIABLE_NAME}}`
(Go template syntax).

See [template expansion](/docs/templating.md) for more details.
""",
        providers = [BuildSettingInfo],
    ),
    stamp = attr.string(
        doc = """Controls build stamping for template expansion.

- **`auto`** (default): Defers to the global `--@rules_img//img/settings:stamp` setting.
- **`force`**: Always stamp if templates contain `{{}}` placeholders, ignoring Bazel's `--stamp` flag.
- **`disabled`**: Never include stamp information.

See [template expansion](/docs/templating.md) for available stamp variables.
""",
        default = "auto",
        values = ["auto", "force", "disabled"],
    ),
    _os_cpu = attr.label(
        default = Label("//img/private/config:target_os_cpu"),
        providers = [TargetPlatformInfo],
    ),
    _stamp_settings = attr.label(
        default = Label("//img/private/settings:stamp"),
        providers = [StampSettingInfo],
    ),
)

def base_content_attrs(extra = {}):
    """Returns the attribute dict of a content rule.

    Args:
        extra: Rule-specific attributes, merged over the shared ones.

    Returns:
        A dict suitable for a rule's `attrs`.
    """
    return dict(_BASE_CONTENT_ATTRS) | extra

def expand_templates(ctx, templates):
    """Expands the rule's templated attributes, if any need it.

    Args:
        ctx: Rule context.
        templates: Dict of template name to value (a string, a list of strings,
            or a dict of strings), matching what `img expand-template` accepts.

    Returns:
        The expanded JSON File, or None when nothing needed expanding.
    """
    if not templates:
        return None
    return expand_or_write(
        ctx = ctx,
        templates = templates,
        output_name = ctx.label.name + "_templates.json",
        only_if_stamping = True,
    )

def run_base_verb(ctx, verb, args, inputs = [], referenced_files = []):
    """Runs one `img base <verb>` action and returns the rule's providers.

    Args:
        ctx: Rule context.
        verb: The subcommand, as a list of words, e.g. `["etc", "hosts"]`.
        args: A `ctx.actions.args()` object with the verb's own flags, or a list
            of such objects and strings.
        inputs: List of File or depset of File the action reads.
        referenced_files: List of depsets of File referenced by path from the
            emitted metadata. These are not action inputs (the action only
            records their paths); they are propagated so the layer action that
            eventually reads the metadata can open them.

    Returns:
        The list of providers the rule should return.
    """
    output = ctx.actions.declare_file(ctx.label.name + ".basemeta.zst")

    base_args = ctx.actions.args()
    base_args.add("base")
    base_args.add_all(verb)
    base_args.add("--output", output)

    # The producing label is recorded on every entry so that a conflict between
    # two content rules can name both sides.
    base_args.add("--producer", str(ctx.label))

    all_args = [base_args]
    if type(args) == "list":
        all_args.extend(args)
    else:
        all_args.append(args)

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        outputs = [output],
        inputs = depset(transitive = [i if type(i) == "depset" else depset(i) for i in inputs]),
        executable = img_toolchain_info.tool_exe,
        arguments = all_args,
        mnemonic = "BaseImageContent",
        progress_message = "Describing base image content %{label}",
    )

    return [
        DefaultInfo(files = depset([output])),
        OutputGroupInfo(basemeta = depset([output])),
        BaseImageContentInfo(
            metadata = depset([output]),
            files = depset(transitive = referenced_files),
        ),
    ]

def merge_sources(ctx, targets):
    """Collects the files of a label_list attribute as a flat list.

    Args:
        ctx: Rule context (unused, accepted for symmetry with the other helpers).
        targets: List of Target from a label_list attribute.

    Returns:
        A list of File, in attribute order.
    """
    _ = ctx  # buildifier: disable=unused-variable
    files = []
    for target in targets:
        files.extend(target[DefaultInfo].files.to_list())
    return files
