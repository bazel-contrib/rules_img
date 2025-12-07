"""Platform constraint utilities for container images."""

def map_os_arch_to_constraints(os_arch_pairs):
    """Map OS/architecture pairs to Bazel constraint labels.

    Args:
        os_arch_pairs: List of strings in format "os_arch" (e.g., ["linux_amd64", "darwin_arm64"])

    Returns:
        String representation of a select expression for target_compatible_with
    """
    if not os_arch_pairs:
        return "[]"

    # If there's only one platform, return its constraints directly
    if len(os_arch_pairs) == 1:
        return '["@rules_img//img/constraints:{}"]'.format(os_arch_pairs[0])

    # For multiple platforms, create a select expression
    select_dict = {}
    for os_arch in sorted(os_arch_pairs):
        select_dict['"@rules_img//img/constraints:{}"'.format(os_arch)] = "[]"
    select_dict['"//conditions:default"'] = '["{}"]'.format(str(Label("@platforms//:incompatible")))

    # Build the select expression string
    select_items = []
    for key, value in select_dict.items():
        select_items.append("        {}: {},".format(key, value))

    return "select({{\n{}\n    }})".format("\n".join(select_items))
