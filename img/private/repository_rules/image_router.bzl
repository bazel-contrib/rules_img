"""Metadata-only routing repositories for platform-selected images."""

load("//img/private/platforms:selection.bzl", "selection_configs")

def _image_router_impl(rctx):
    descriptors = json.decode(rctx.attr.descriptors)
    lines = []
    choices = {}
    for i, (constraints, position) in enumerate(selection_configs(descriptors)):
        name = "platform_{}".format(i)
        lines.append("config_setting(name = {}, constraint_values = {})".format(repr(name), repr(constraints)))
        choices[":" + name] = rctx.attr.children[position]
    choices["//conditions:default"] = str(Label("//img/base:incompatible"))
    lines.append("alias(name = 'image', actual = select({}), visibility = ['//visibility:public'])".format(repr(choices)))
    lines.append("alias(name = 'original', actual = {}, visibility = ['//visibility:public'])".format(repr(rctx.attr.original)))
    rctx.file("BUILD.bazel", "\n\n".join(lines) + "\n")

image_router = repository_rule(
    implementation = _image_router_impl,
    attrs = {
        "descriptors": attr.string(mandatory = True),
        # Strings deliberately avoid repository dependencies while generating the
        # router. These labels are resolved only by the generated BUILD file.
        "children": attr.string_list(mandatory = True),
        "original": attr.string(mandatory = True),
    },
)
