"""Shared stamping utilities for Bazel rules."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private/common:build.bzl", "TOOLCHAIN")
load("//img/private/providers:stamp_setting_info.bzl", "StampSettingInfo")

def get_build_settings(ctx):
    """Extract build settings values from the context.

    Args:
        ctx: The rule context

    Returns:
        A dictionary mapping setting names to their values
    """
    settings = {}
    for setting_name, setting_label in ctx.attr.build_settings.items():
        settings[setting_name] = setting_label[BuildSettingInfo].value
    return settings

def should_stamp(*, ctx, template_strings):
    """Get the stamp configuration from the context.

    Args:
        ctx: The rule context
        template_strings: List of strings to check for Go template placeholders ({{...}})

    Returns:
        A struct containing stamp, can_stamp, and want_stamp boolean fields
    """
    stamp_settings = ctx.attr._stamp_settings[StampSettingInfo]
    can_stamp = stamp_settings.bazel_setting
    global_user_preference = stamp_settings.user_preference
    target_stamp = ctx.attr.stamp

    contains_template_placeholders = False
    for template in template_strings:
        # search for "{{" followed by "}}" (Go template syntax)
        # ensure {{ comes before }} in the string
        open_pos = template.find("{{")
        if open_pos >= 0:
            close_pos = template.find("}}", open_pos + 2)
            if close_pos >= 0:
                contains_template_placeholders = True
                break

    want_stamp = False
    if target_stamp == "disabled":
        want_stamp = False
    elif target_stamp == "enabled":
        want_stamp = contains_template_placeholders
    elif target_stamp == "auto":
        want_stamp = global_user_preference and contains_template_placeholders
    return struct(
        stamp = can_stamp and want_stamp,
        can_stamp = can_stamp,
        want_stamp = want_stamp,
    )

def expand_or_write(*, ctx, templates, output_name):
    """Either expand templates or write JSON directly based on build_settings.

    Args:
        ctx: The rule context
        templates: The templates dictionary (dict of template name to value (str) or values (list of str))
        output_name: The name for the output file

    Returns:
        The File object for the final JSON
    """
    build_settings = get_build_settings(ctx)
    stamp_settings = should_stamp(ctx = ctx, template_strings = [json.encode(v) for v in templates.values()])
    final_json = ctx.actions.declare_file(output_name)

    if build_settings or stamp_settings.want_stamp:
        # Add build settings to the request for template expansion
        request = dict(
            templates = templates,
            build_settings = build_settings,
        )

        # Write the template JSON
        template_name = output_name.replace(".json", ".template_request.json")
        template_json = ctx.actions.declare_file(template_name)
        ctx.actions.write(
            template_json,
            json.encode(request),
        )

        # Run expand-template to create the final JSON

        # Build arguments for expand-template
        args = []
        inputs = [template_json]

        # Add stamp files if stamping is enabled
        if stamp_settings.stamp:
            if ctx.version_file:
                args.extend(["--stamp", ctx.version_file.path])
                inputs.append(ctx.version_file)
            if ctx.info_file:
                args.extend(["--stamp", ctx.info_file.path])
                inputs.append(ctx.info_file)

        args.extend([template_json.path, final_json.path])

        img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
        ctx.actions.run(
            inputs = inputs,
            outputs = [final_json],
            executable = img_toolchain_info.tool_exe,
            arguments = ["expand-template"] + args,
            mnemonic = "ExpandTemplate",
        )
        return final_json
    else:
        # No templates to expand, create JSON directly
        ctx.actions.write(
            final_json,
            json.encode(templates),
        )
        return final_json
