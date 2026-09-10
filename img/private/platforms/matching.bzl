"""Platform spec parsing and selection of image index children.

Both a requested spec and a candidate platform are normalized before they are compared,
mirroring containerd. No compatibility vector is applied: asking for `linux/amd64` must
not download `linux/386`.

A spec that spells out no variant (`linux/arm64`) matches every variant of that
OS/architecture; one that does (`linux/arm64/v9`) matches only that variant. That keeps a
single invariant: adding a filter never changes which child `select_base` picks for a
given target platform, it only drops platforms that were not asked for. Comparing a bare
`os/architecture` strictly would silently downgrade a build whose target platform asks for
a variant, and where the fallback vector does not end at the variant-less entry
(`linux/arm`) it would break the build outright.
"""

load("//img/private:manifest_media_type.bzl", "kind")
load(":platforms.bzl", "has_constraint_setting")

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

def normalize_platform(os, architecture, variant):
    """Normalize an os/architecture/variant triple, mirroring containerd.

    Args:
        os: The OS, as found in an OCI platform object.
        architecture: The architecture, as found in an OCI platform object.
        variant: The variant, as found in an OCI platform object (may be empty).

    Returns:
        struct with the normalized os, architecture and variant.
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
        struct with the spec as written, its normalized os, architecture and variant, and
        any_variant telling whether the spec left the variant open.
    """
    parts = spec.split("/")
    if len(parts) < 2 or len(parts) > 3 or not all([len(part) > 0 for part in parts]):
        fail("""invalid platform "{}": expected "os/architecture" or "os/architecture/variant" (e.g. "linux/amd64")""".format(spec))
    normalized = normalize_platform(parts[0], parts[1], parts[2] if len(parts) == 3 else "")
    return struct(
        spec = spec,
        os = normalized.os,
        architecture = normalized.architecture,
        variant = normalized.variant,
        any_variant = len(parts) == 2,
    )

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
            (spec.any_variant or candidate.variant == spec.variant))
    ]

def format_platform(platform):
    """Format an OCI platform object for error messages.

    Args:
        platform: The "platform" object of an index entry (dict, may be empty).

    Returns:
        A string of the form "os/architecture[/variant]", or "<no platform>" if the entry
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

def select_index_children(children, specs):
    """Decide what to do with every child descriptor of an image index.

    The whole index is triaged before anything is downloaded, so a spec that matches
    nothing is reported without having spent a single request on the specs that did match.

    Args:
        children: The "manifests" list of an index blob.
        specs: List of structs as returned by parse_platform_spec. An empty list selects
            every child, which is what an unfiltered pull does.

    Returns:
        struct with
            selected: descriptors whose manifest and config are to be downloaded.
            omitted: digests of children that are deliberately not downloaded, either
                because a spec rejected them or because they are not image manifests.
            nested: digests of selected children that are indexes themselves.
            unmatched: specs, as written, that no child matched.
            available: the formatted platform of every child, deduplicated.
    """
    selected = []
    omitted = []
    nested = []
    available = []
    matched = [False] * len(specs)
    for child in children:
        platform = child.get("platform", {})
        formatted = format_platform(platform)
        if formatted not in available:
            available.append(formatted)
        if len(specs) > 0:
            matching = matching_spec_indices(platform, specs)
            if len(matching) == 0:
                omitted.append(child["digest"])
                continue
            for index in matching:
                matched[index] = True
        child_kind = kind(child.get("mediaType"))
        if child_kind == "index":
            nested.append(child["digest"])
        elif child_kind == "manifest":
            selected.append(child)
        else:
            # Not an image manifest at all (an OCI artifact, say). These were never
            # downloaded, so image_import has to skip them too.
            omitted.append(child["digest"])
    return struct(
        selected = selected,
        omitted = omitted,
        nested = nested,
        unmatched = [spec.spec for (index, spec) in enumerate(specs) if not matched[index]],
        available = available,
    )

def index_platform_groups(children, reference):
    """Group the children of an image index by the Bazel platform they can be selected for.

    Children that no Bazel platform can match are left out: attestation manifests (which
    buildkit publishes with the platform `unknown/unknown`), entries without a platform, and
    platforms rules_img has no constraint values for. They are part of the index, but never
    the base image of a build.

    Args:
        children: The "manifests" list of an index blob.
        reference: The image the index belongs to, for error messages.

    Returns:
        Dict of "goos_goarch" to the digests of the children declaring that os/architecture.
        An os/architecture with more than one child has one entry per variant.
    """
    groups = {}
    for child in children:
        digest = child.get("digest")
        if not digest:
            fail("child manifest of {} has no digest".format(reference))
        if kind(child.get("mediaType")) == "index":
            # Matches the behavior of the pull repository rule.
            fail("image index referenced another index ({}). Nested indexes are not supported.".format(digest))
        platform = child.get("platform", {})
        os = platform.get("os", "")
        architecture = platform.get("architecture", "")
        if not os or not architecture:
            continue
        normalized = normalize_platform(os, architecture, platform.get("variant", ""))
        if not has_constraint_setting(normalized.os, normalized.architecture):
            continue
        groups.setdefault("{}_{}".format(normalized.os, normalized.architecture), []).append(digest)
    return groups
