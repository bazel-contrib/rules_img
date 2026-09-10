"""Shared platform preference and lazy image selection."""

def platform_vector(os, architecture, variant):
    """Generate an ordered vector of compatible platforms (best to worst).

    Based on containerd's platformVector logic:
    https://github.com/containerd/platforms/blob/2e51fd9435bd985e1753954b24f4b0453f4e4767/compare.go#L64

    Args:
        os: Operating system
        architecture: CPU architecture
        variant: Platform variant (may be empty)

    Returns:
        List of platform dicts in preference order (best match first)
    """
    base_platform = {
        "os": os,
        "architecture": architecture,
        "variant": variant,
    }
    vector = [base_platform]

    # AMD64: Parse variant as integer and create fallback chain
    if architecture == "amd64" and variant != "":
        # Try to parse variant like "v3" -> 3
        if variant.startswith("v"):
            variant_num_str = variant[1:]  # Remove "v" prefix
            if variant_num_str.isdigit():
                amd64_version = int(variant_num_str)
                if amd64_version > 1:
                    # Add fallback variants: v3 -> v2, v1
                    for v in range(amd64_version - 1, 0, -1):
                        vector.append({
                            "os": os,
                            "architecture": architecture,
                            "variant": "v" + str(v),
                        })

        # Add base amd64 (no variant) as final fallback
        vector.append({
            "os": os,
            "architecture": architecture,
            "variant": "",
        })

        # ARM 32-bit: Parse variant as integer and create fallback chain
    elif architecture == "arm" and variant != "":
        if variant.startswith("v"):
            variant_num_str = variant[1:]
            if variant_num_str.isdigit():
                arm_version = int(variant_num_str)
                if arm_version > 5:
                    # Add fallback variants: v7 -> v6, v5
                    for v in range(arm_version - 1, 4, -1):
                        vector.append({
                            "os": os,
                            "architecture": architecture,
                            "variant": "v" + str(v),
                        })

        # ARM64: Complex fallback with v8.x and v9.x support
    elif architecture == "arm64":
        # ARM64 variant defaults to v8 (already normalized by TargetPlatformInfo)
        effective_variant = variant if variant != "" else "v8"

        # Simplified arm64 variant support
        # Full implementation would need arm64variantToVersion map from containerd
        # For now, support basic v8 and v9 variants
        if effective_variant == "v8" or effective_variant.startswith("v8."):
            # v8.x can fall back to lower v8.y versions
            if effective_variant.startswith("v8."):
                # Parse v8.5 -> major=8, minor=5
                parts = effective_variant[1:].split(".")  # "8.5" -> ["8", "5"]
                if len(parts) == 2 and parts[0].isdigit() and parts[1].isdigit():
                    minor = int(parts[1])

                    # Add fallback from v8.5 -> v8.4 -> ... -> v8.0 -> v8
                    for m in range(minor - 1, -1, -1):
                        if m == 0:
                            vector.append({
                                "os": os,
                                "architecture": architecture,
                                "variant": "v8",
                            })
                        else:
                            vector.append({
                                "os": os,
                                "architecture": architecture,
                                "variant": "v8." + str(m),
                            })
        elif effective_variant == "v9" or effective_variant.startswith("v9."):
            # v9.x can fall back to lower v9.y, then to v8.x
            if effective_variant.startswith("v9."):
                parts = effective_variant[1:].split(".")
                if len(parts) == 2 and parts[0].isdigit() and parts[1].isdigit():
                    minor = int(parts[1])

                    # Add v9 fallbacks
                    for m in range(minor - 1, -1, -1):
                        if m == 0:
                            vector.append({
                                "os": os,
                                "architecture": architecture,
                                "variant": "v9",
                            })
                        else:
                            vector.append({
                                "os": os,
                                "architecture": architecture,
                                "variant": "v9." + str(m),
                            })

            # v9.x falls back to v8.5+ (per containerd mapping)
            # Simplified: just fall back to v8
            vector.append({
                "os": os,
                "architecture": architecture,
                "variant": "v8",
            })

    return vector

def select_descriptor(descriptors, os, architecture, variant):
    """Return the first descriptor preferred by the requested target platform.

    Args:
        descriptors: Ordered index descriptors.
        os: Target operating system.
        architecture: Target CPU architecture.
        variant: Target CPU variant, or the empty string.

    Returns:
        The matching descriptor position, or None.
    """
    if architecture == "arm64" and not variant:
        variant = "v8"
    for wanted in platform_vector(os, architecture, variant):
        for i, descriptor in enumerate(descriptors):
            platform = descriptor.get("platform", {})
            candidate_variant = platform.get("variant", "")
            if platform.get("architecture") == "arm64" and not candidate_variant:
                candidate_variant = "v8"
            if (platform.get("os") == wanted["os"] and
                platform.get("architecture") == wanted["architecture"] and
                candidate_variant == wanted["variant"]):
                return i
    return None

# Keep these mappings aligned with target_os_cpu. Using individual constraint values
# makes every variant branch disjoint, including the empty/default variant.
_OS = {name: name for name in ["android", "freebsd", "ios", "linux", "netbsd", "openbsd", "windows"]}
_OS.update({"macos": "darwin", "wasi": "wasip1"})
_CPU = {
    "aarch64": "arm64",
    "arm64": "arm64",
    "armv7": "arm",
    "mips64": "mips64",
    "ppc64le": "ppc64le",
    "riscv64": "riscv64",
    "s390x": "s390x",
    "wasm32": "wasm",
    "x86_32": "386",
    "x86_64": "amd64",
}
_VARIANTS = {str(Label("//img/constraints:empty_variant")): ""}
_VARIANTS.update({str(Label("//img/constraints/amd64:v" + str(i))): "v" + str(i) for i in range(1, 5)})
_VARIANTS.update({str(Label("//img/constraints/arm:v" + str(i))): "v" + str(i) for i in range(5, 9)})
_VARIANTS.update({str(Label("//img/constraints/arm64:v8." + str(i))): "v8." + str(i) for i in range(1, 10)})
_VARIANTS.update({str(Label("//img/constraints/arm64:v9")): "v9"})
_VARIANTS.update({str(Label("//img/constraints/arm64:v9." + str(i))): "v9." + str(i) for i in range(1, 8)})

def selection_configs(descriptors):
    """Return (constraint values, descriptor position) for every supported match.

    Args:
        descriptors: Ordered index descriptors.

    Returns:
        A list of disjoint constraint combinations and selected positions.
    """
    result = []
    available = {(d.get("platform", {}).get("os"), d.get("platform", {}).get("architecture")): True for d in descriptors}
    for os_constraint, os in _OS.items():
        for cpu_constraint, cpu in _CPU.items():
            if (os, cpu) not in available:
                continue
            for variant_constraint, variant in _VARIANTS.items():
                selected = select_descriptor(descriptors, os, cpu, variant)
                if selected != None:
                    result.append(([
                        str(Label("@platforms//os:" + os_constraint)),
                        str(Label("@platforms//cpu:" + cpu_constraint)),
                        variant_constraint,
                    ], selected))
    return result
