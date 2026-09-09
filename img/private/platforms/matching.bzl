"""Platform spec parsing and matching for image index children.

The matching semantics mirror containerd's `platforms.OnlyStrict(platforms.Parse(spec))`:
both the requested spec and the candidate platform are normalized, then compared exactly.
No compatibility vector is applied - asking for `linux/amd64` must not download `linux/386`.
"""

def _normalize_os(os):
    os = os.lower()
    if os == "macos":
        return "darwin"
    return os

def _normalize_arch(architecture, variant):
    """Normalize an architecture/variant pair.

    Mirrors containerd's normalizeArch:
    https://github.com/containerd/platforms/blob/v1.0.0-rc.5/database.go#L76

    Args:
        architecture: The architecture, as found in an OCI platform object.
        variant: The variant, as found in an OCI platform object (may be empty).

    Returns:
        Tuple of the normalized (architecture, variant).
    """
    architecture = architecture.lower()
    variant = variant.lower()
    if architecture == "i386":
        return ("386", "")
    if architecture in ["x86_64", "x86-64", "amd64"]:
        if variant == "v1":
            variant = ""
        return ("amd64", variant)
    if architecture in ["aarch64", "arm64"]:
        if variant in ["8", "v8", "v8.0"]:
            variant = ""
        elif variant in ["9", "9.0", "v9.0"]:
            variant = "v9"
        return ("arm64", variant)
    if architecture == "armhf":
        return ("arm", "v7")
    if architecture == "armel":
        return ("arm", "v6")
    if architecture == "arm":
        if variant in ["", "7"]:
            variant = "v7"
        elif variant in ["5", "6", "8"]:
            variant = "v" + variant
        return ("arm", variant)
    return (architecture, variant)

def normalize_platform(os, architecture, variant = ""):
    """Normalize an os/architecture/variant triple for comparison.

    Args:
        os: The operating system (e.g. "linux").
        architecture: The architecture (e.g. "arm64").
        variant: The variant (e.g. "v8"). Optional.

    Returns:
        struct with normalized os, architecture and variant fields.
    """
    (architecture, variant) = _normalize_arch(architecture, variant)
    return struct(
        os = _normalize_os(os),
        architecture = architecture,
        variant = variant,
    )

def parse_platform_spec(spec):
    """Parse a platform spec of the form "os/architecture[/variant]".

    Unlike containerd's parser, this rejects shorthands that would be completed from the
    host platform ("amd64", "linux"): a repository rule must not depend on the host.

    Args:
        spec: The platform spec string.

    Returns:
        struct with normalized os, architecture and variant fields.
    """
    parts = spec.split("/")
    if len(parts) < 2 or len(parts) > 3 or not all([len(part) > 0 for part in parts]):
        fail("""invalid platform "{}": expected "os/architecture" or "os/architecture/variant" (e.g. "linux/amd64")""".format(spec))
    return normalize_platform(parts[0], parts[1], parts[2] if len(parts) == 3 else "")

def matching_spec_indices(platform, specs):
    """Find the specs an OCI platform object matches.

    Args:
        platform: The "platform" object of an index entry (dict, may be empty).
        specs: List of structs as returned by parse_platform_spec.

    Returns:
        List of indices into specs that the platform matches (empty if none do).
    """
    os = platform.get("os", "")
    architecture = platform.get("architecture", "")
    if not os or not architecture:
        # An entry without a platform cannot be selected by a filter.
        return []
    candidate = normalize_platform(os, architecture, platform.get("variant", ""))
    return [
        index
        for (index, spec) in enumerate(specs)
        if (candidate.os == spec.os and
            candidate.architecture == spec.architecture and
            candidate.variant == spec.variant)
    ]

def platform_matches_any(platform, specs):
    """Check if an OCI platform object matches any of the parsed specs.

    Args:
        platform: The "platform" object of an index entry (dict, may be empty).
        specs: List of structs as returned by parse_platform_spec.

    Returns:
        True if the platform matches at least one spec.
    """
    return len(matching_spec_indices(platform, specs)) > 0

def format_platform(platform):
    """Format an OCI platform object for error messages.

    Args:
        platform: The "platform" object of an index entry (dict, may be empty).

    Returns:
        A string of the form "os/architecture[/variant]", or "unknown" if the entry
        carries no platform information.
    """
    os = platform.get("os", "")
    architecture = platform.get("architecture", "")
    if not os or not architecture:
        return "<no platform>"
    variant = platform.get("variant", "")
    if variant:
        return "{}/{}/{}".format(os, architecture, variant)
    return "{}/{}".format(os, architecture)
