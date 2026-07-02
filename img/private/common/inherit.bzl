"""The sentinel value used to request inheriting a config field from the base image."""

# INHERIT_FROM_BASE is the default value of several `image_manifest` string and
# string_list attributes (`user`, `working_dir`, `stop_signal`, `entrypoint`,
# `cmd`). It lets the rule tell the difference between an attribute that was left
# untouched (which should inherit the base image's value) and one that was
# explicitly set to an empty value (which should unset the field).
#
# It is forwarded verbatim to the `img` tool as a normal flag value, where it is
# expanded against the base image config:
#
#   - Leaving an attribute at its default (the sentinel) inherits the base value.
#   - Setting an attribute to an explicit empty value ("" or []) unsets the field
#     instead of inheriting it.
#   - For string_list attributes, an INHERIT_FROM_BASE item is replaced in place by
#     the base image's list, so `[INHERIT_FROM_BASE, "--flag"]` appends "--flag"
#     to the inherited value. `[INHERIT_FROM_BASE]` on its own is exactly "inherit".
#
# The value is a human-readable placeholder chosen to be extremely unlikely to
# collide with a real config value; a literal entrypoint/user/etc. equal to this
# string would be treated as "inherit". Load it via `@rules_img//img:image.bzl`
# to inherit explicitly, e.g. `entrypoint = [INHERIT_FROM_BASE, "--verbose"]`.
INHERIT_FROM_BASE = "<inherit from base>"
