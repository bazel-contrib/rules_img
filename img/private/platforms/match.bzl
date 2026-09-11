"""Matching image manifests against a target platform.

One implementation, used from two places that must agree:

* `select_base` narrows an `ImageIndexInfo` to the manifest for the target platform, in the
  analysis phase, over providers.
* The images module extension generates a routing repository that makes the same choice in the
  loading phase, over the descriptors of an index, so only the selected manifest's repository is
  ever fetched.

If those two disagree, `image()` hands `select_base` a manifest it would not have picked itself,
and `select_base` accepts it without a second check. `match_test.bzl` pins the equivalence.

Based on containerd's platform matching:
https://github.com/containerd/platforms/blob/2e51fd9435bd985e1753954b24f4b0453f4e4767/compare.go#L64
"""

def normalized_variant(architecture, variant):
    """Apply the defaults that make two platform triples comparable.

    Args:
        architecture: CPU architecture (as GOARCH).
        variant: Platform variant, possibly empty.

    Returns:
        The variant to compare with. Idempotent: `//img/private/config:target_os_cpu` and
        `image_import` already normalize what they report.
    """

    # ARM64 defaults to v8.
    # See: https://github.com/containerd/platforms/blob/2e51fd9435bd985e1753954b24f4b0453f4e4767/platforms.go#L290
    if architecture == "arm64" and variant == "":
        return "v8"
    return variant

def platform_vector(os, architecture, variant):
    """Generate an ordered vector of compatible platforms (best to worst).

    Args:
        os: Operating system
        architecture: CPU architecture
        variant: Platform variant (may be empty), already normalized

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

def _matches(wanted, os, architecture, variant):
    """Report whether a candidate platform is exactly the wanted one."""
    return (wanted["os"] == os and
            wanted["architecture"] == architecture and
            wanted["variant"] == normalized_variant(architecture, variant))

def match_manifest(manifests, os, architecture, variant):
    """Pick the manifest that best matches a target platform.

    Args:
        manifests: List of `ImageManifestInfo` to choose from.
        os: Wanted operating system (as GOOS).
        architecture: Wanted CPU architecture (as GOARCH).
        variant: Wanted platform variant (may be empty).

    Returns:
        The best matching `ImageManifestInfo`, or None if none matches.
    """
    for wanted in platform_vector(os, architecture, normalized_variant(architecture, variant)):
        for manifest in manifests:
            if _matches(wanted, manifest.os, manifest.architecture, manifest.variant):
                return manifest
    return None

def match_platform_index(platforms, os, architecture, variant):
    """Pick the position of the platform that best matches a target platform.

    Ties go to the first entry, matching `match_manifest`, so an index that lists the same
    platform twice resolves to the same occurrence in both phases.

    Args:
        platforms: Ordered list of OCI platform dicts. An entry may be None, for an index
            descriptor that declares no platform and whose manifest config could not be read.
        os: Wanted operating system (as GOOS).
        architecture: Wanted CPU architecture (as GOARCH).
        variant: Wanted platform variant (may be empty).

    Returns:
        The matching position, or None if none matches.
    """
    for wanted in platform_vector(os, architecture, normalized_variant(architecture, variant)):
        for (position, platform) in enumerate(platforms):
            if not platform:
                continue
            if _matches(
                wanted,
                platform.get("os", ""),
                platform.get("architecture", ""),
                platform.get("variant", ""),
            ):
                return position
    return None

def no_match_message(os, architecture, variant):
    """Error message for a target platform that no manifest matches.

    Args:
        os: Wanted operating system (as GOOS).
        architecture: Wanted CPU architecture (as GOARCH).
        variant: Wanted platform variant (may be empty).

    Returns:
        A human readable error message.
    """
    variant_msg = ""
    if variant != "":
        variant_msg = " variant={}".format(variant)
    return "no matching base image found for os={} architecture={}{}".format(
        os,
        architecture,
        variant_msg,
    )
