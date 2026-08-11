"""Prebuilt lockfile schema, and the fetch behind each of its download modes.

A prebuilt lockfile is a JSON list with one entry per platform. Entries are read
by both the `prebuilt_img_tool` module extension (bzlmod) and
`img_register_prebuilt_toolchains` (WORKSPACE), which is why the schema and the
fetch live here instead of in either of them.

The `download_mode` field of an entry selects how the binary is fetched.

`"url"` is the default, and the only mode understood by rules_img releases that
predate this file. It fetches the binary from a plain file server, e.g. a GitHub
release asset: every `url_templates` entry is expanded with `{version}`, `{os}`,
`{cpu}`, `{dot}` and `{extension}`.

```json
{"version": "v0.3.19", "integrity": "sha256-Wp...", "os": "linux", "cpu": "amd64"}
```

`"oci"` fetches the binary as a blob from a container registry - the ORAS idea,
where the file *is* the blob - which is how prerelease builds are published:

```json
{
  "download_mode": "oci",
  "registry": "ghcr.io",
  "repository": "bazel-contrib/rules_img/img",
  "sha256": "fd7025e15908a4960ecfd647b55b67f06670c09c471bb8f9072fc39735d7f336",
  "os": "linux",
  "cpu": "amd64"
}
```

Blobs are content addressed, so an `"oci"` entry needs no version: the digest in
`https://<registry>/v2/<repository>/blobs/sha256:<hex>` is the identity of the
file. State that digest as either `sha256` (hex, with or without a leading
`sha256:`) or `integrity` (SRI: `sha256-` followed by base64) - both carry the
same sha256, and an entry may carry both, in which case they have to agree.

Registry access is anonymous: no Docker config, credential helper or keychain is
consulted. `GET /v2/` decides whether a token is needed at all, and when it is, a
pull-scoped token is fetched from the registry's token endpoint. That endpoint
defaults to the one the distribution spec suggests, `https://<registry>/token`.

A registry whose `WWW-Authenticate` challenge points elsewhere is described by
recording the challenge in the entry - the tool that writes the lockfile is the
one that gets to read that header, since Bazel's downloader does not expose
response headers:

```json
"auth_challenge": {"realm": "https://auth.docker.io/token", "service": "registry.docker.io"}
```

A recorded challenge also states that the registry requires a token, so the
`GET /v2/` probe is skipped - worth recording even when the challenge matches the
default, because Bazel logs a warning for the 401 the probe runs into.

The scope is always `repository:<repository>:pull` and is deliberately not
configurable: the challenge a registry returns for `GET /v2/` advertises a
placeholder repository (ghcr.io answers with `repository:user/image:pull`), so
honoring a recorded scope would hand out a token for the wrong repository.
"""

MODE_URL = "url"
MODE_OCI = "oci"

_DEFAULT_URL_TEMPLATE = "https://github.com/bazel-contrib/rules_img/releases/download/{version}/img_{os}_{cpu}{dot}{extension}"

# Fields a lockfile entry may set. Doubles as the attribute schema of the
# repository rule and module extension tag that mirror an entry, so that the two
# cannot drift apart.
LOCKFILE_ATTRS = {
    "download_mode": attr.string(
        default = MODE_URL,
        values = [MODE_URL, MODE_OCI],
        doc = "How to fetch the binary: from a file server (`url`), or as a blob from a container registry (`oci`).",
    ),
    "version": attr.string(
        doc = "Version the binary was published under. Required in `url` mode, where it is substituted into `url_templates`; informational in `oci` mode.",
    ),
    "integrity": attr.string(
        doc = "Subresource Integrity of the binary, e.g. `sha256-Wp...`. Must be a sha256 in `oci` mode, where it doubles as the blob digest.",
    ),
    "sha256": attr.string(
        doc = "Sha256 of the binary in hex, with or without a leading `sha256:`. Alternative to `integrity`, or a cross-check of it.",
    ),
    "os": attr.string(
        values = ["darwin", "linux", "windows"],
        doc = "GOOS the binary is built for.",
    ),
    "cpu": attr.string(
        values = ["amd64", "arm64", "s390x"],
        doc = "GOARCH the binary is built for.",
    ),
    "url_templates": attr.string_list(
        default = [_DEFAULT_URL_TEMPLATE],
        doc = "`url` mode: templates to expand into download URLs, tried in order.",
    ),
    "registry": attr.string(
        doc = "`oci` mode: registry to fetch the blob from, e.g. `ghcr.io`.",
    ),
    "repository": attr.string(
        doc = "`oci` mode: repository the blob belongs to, e.g. `bazel-contrib/rules_img/img`.",
    ),
    "auth_challenge": attr.string_dict(
        doc = "`oci` mode: `realm` and `service` of the registry's token endpoint, as recorded from its `WWW-Authenticate` challenge. Setting it also states that the registry requires a token, which skips the `GET /v2/` probe.",
    ),
}

# Fields that only mean something in "oci" mode. Set in an entry that stays in
# "url" mode, they are a sign of a lockfile that forgot to switch mode.
_OCI_ONLY_FIELDS = ["registry", "repository", "auth_challenge"]

_BLOB_URL = "https://{registry}/v2/{repository}/blobs/sha256:{sha256}"
_PING_URL = "https://{registry}/v2/"
_TOKEN_URL = "{realm}?scope=repository:{repository}:pull&service={service}"

# Scratch files for the two requests that precede a blob download. Both are
# deleted again, so their names only need to stay clear of the repository
# contents.
_PING_OUTPUT = "v2-ping.json"
_TOKEN_OUTPUT = "token.json"

_HEX_DIGITS = "0123456789abcdef"
_BASE64_DIGITS = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

def describe_entry(entry):
    """Name an entry the way an error message should refer to it.

    Args:
        entry: Lockfile entry (dict).

    Returns:
        Human readable description of the entry.
    """
    return "prebuilt lockfile entry for {}/{}".format(entry.get("os", "unknown"), entry.get("cpu", "unknown"))

def _hex_from_base64(encoded):
    """Decode base64 into lowercase hex, or None if it is not valid base64."""
    accumulator = 0
    bits = 0
    digits = []
    for character in encoded.elems():
        if character == "=":
            break
        value = _BASE64_DIGITS.find(character)
        if value < 0:
            return None
        accumulator = accumulator * 64 + value
        bits += 6
        if bits >= 8:
            # Emit the most significant byte that is now complete. Bytes emitted
            # earlier stay in the accumulator, above the `bits` that are still
            # pending, and are masked off here.
            bits -= 8
            byte = (accumulator >> bits) & 0xFF
            digits.append(_HEX_DIGITS[byte // 16] + _HEX_DIGITS[byte % 16])
    return "".join(digits)

def _is_sha256_hex(value):
    if len(value) != 64:
        return False
    for character in value.elems():
        if _HEX_DIGITS.find(character) < 0:
            return False
    return True

def sha256_of(entry):
    """Sha256 of the binary an entry describes, in lowercase hex.

    Reads it from `sha256`, from `integrity`, or from both - in which case the
    two have to agree.

    Args:
        entry: Lockfile entry (dict).

    Returns:
        The sha256 as 64 lowercase hex characters, or the empty string if the
        entry states neither `sha256` nor `integrity`.
    """
    from_sha256 = entry.get("sha256", "").strip().removeprefix("sha256:").lower()
    if from_sha256 and not _is_sha256_hex(from_sha256):
        fail("{}: field 'sha256' is not a sha256 in hex: {}".format(describe_entry(entry), entry.get("sha256")))

    integrity = entry.get("integrity", "").strip()
    if not integrity:
        return from_sha256

    algorithm, dash, encoded = integrity.partition("-")
    if not dash:
        fail("{}: field 'integrity' is not a Subresource Integrity value: {}".format(describe_entry(entry), integrity))
    if algorithm != "sha256":
        # Other algorithms are fine for Bazel to verify a download with, they
        # just cannot name a blob in a registry.
        return from_sha256

    # Strip the optional `?options` part of an SRI value before decoding.
    encoded = encoded.partition("?")[0]
    from_integrity = _hex_from_base64(encoded)
    if from_integrity == None or not _is_sha256_hex(from_integrity):
        fail("{}: field 'integrity' does not encode a sha256: {}".format(describe_entry(entry), integrity))
    if from_sha256 and from_sha256 != from_integrity:
        fail("""{description}: fields 'sha256' and 'integrity' disagree:
    sha256:    {from_sha256}
    integrity: {from_integrity} (decoded from {integrity})""".format(
            description = describe_entry(entry),
            from_sha256 = from_sha256,
            from_integrity = from_integrity,
            integrity = integrity,
        ))
    return from_integrity

def blob_url(*, registry, repository, sha256):
    """URL a blob is served under by a registry implementing the distribution spec.

    Args:
        registry: Registry to fetch from, e.g. `ghcr.io`.
        repository: Repository the blob belongs to.
        sha256: Digest of the blob, as 64 lowercase hex characters.

    Returns:
        The blob URL.
    """
    return _BLOB_URL.format(registry = registry, repository = repository, sha256 = sha256)

def token_url(*, registry, repository, auth_challenge = {}):
    """URL that hands out a pull-scoped token for a repository.

    Args:
        registry: Registry to authenticate against, e.g. `ghcr.io`.
        repository: Repository the token should grant pull access to.
        auth_challenge: Recorded `WWW-Authenticate` parameters (`realm` and
            `service`) of the registry, defaulting to what the distribution spec
            suggests.

    Returns:
        The token endpoint URL, with scope and service query parameters.
    """
    realm = auth_challenge.get("realm", "")
    if not realm:
        realm = "https://{}/token".format(registry)
    elif realm.find("://") < 0:
        realm = "https://" + realm
    return _TOKEN_URL.format(
        realm = realm,
        repository = repository,
        service = auth_challenge.get("service", "") or registry,
    )

def binary_urls(entry):
    """URLs the binary of a `url` mode entry can be downloaded from.

    Args:
        entry: Lockfile entry (dict).

    Returns:
        List of URLs, to be tried in order.
    """
    version = entry.get("version", "")
    if not version:
        fail("{}: field 'version' is required in '{}' download mode".format(describe_entry(entry), MODE_URL))
    extension = "exe" if entry.get("os", "") == "windows" else ""
    templates = entry.get("url_templates", None) or [_DEFAULT_URL_TEMPLATE]
    return [template.format(
        version = version,
        os = entry.get("os", ""),
        cpu = entry.get("cpu", ""),
        dot = "." if extension else "",
        extension = extension,
    ) for template in templates]

def _validated_mode(entry):
    unknown = sorted([field for field in entry if field not in LOCKFILE_ATTRS])
    if unknown:
        fail("{}: unknown field(s) {}. Known fields: {}".format(
            describe_entry(entry),
            ", ".join(unknown),
            ", ".join(sorted(LOCKFILE_ATTRS)),
        ))
    mode = entry.get("download_mode", "") or MODE_URL
    if mode not in [MODE_URL, MODE_OCI]:
        fail("{}: unknown download_mode {}, expected one of {}, {}".format(describe_entry(entry), repr(mode), repr(MODE_URL), repr(MODE_OCI)))
    if mode == MODE_URL:
        misplaced = sorted([field for field in _OCI_ONLY_FIELDS if entry.get(field, None)])
        if misplaced:
            fail("""{description}: field(s) {misplaced} only apply to the '{oci}' download mode, but this entry uses '{url}'.
    Set "download_mode": "{oci}" to fetch the binary as a registry blob.""".format(
                description = describe_entry(entry),
                misplaced = ", ".join(misplaced),
                oci = MODE_OCI,
                url = MODE_URL,
            ))
    return mode

def _anonymous_token(rctx, *, registry, repository, auth_challenge):
    """Acquire an anonymous pull token, or None if the registry does not want one."""
    if not auth_challenge:
        # Nothing recorded about this registry, so ask it whether it wants a
        # token at all. Bazel logs a warning for the 401 that a registry
        # requiring one answers with; recording the challenge skips this probe.
        ping = rctx.download(
            url = [_PING_URL.format(registry = registry)],
            output = _PING_OUTPUT,
            allow_fail = True,
        )
        rctx.delete(_PING_OUTPUT)
        if ping.success:
            # The registry serves its API unauthenticated.
            return None

    url = token_url(registry = registry, repository = repository, auth_challenge = auth_challenge)
    exchange = rctx.download(
        url = [url],
        output = _TOKEN_OUTPUT,
        allow_fail = True,
    )
    if not exchange.success:
        rctx.delete(_TOKEN_OUTPUT)
        fail("""{registry} requires authentication and the anonymous token exchange with {url} failed.
    Only anonymous pulls are supported here: a registry that needs credentials cannot serve a prebuilt lockfile entry.
    If the registry's token endpoint is not {url}, record its WWW-Authenticate challenge in the lockfile entry:
        "auth_challenge": {{"realm": "...", "service": "..."}}""".format(registry = registry, url = url))
    body = rctx.read(_TOKEN_OUTPUT)
    rctx.delete(_TOKEN_OUTPUT)
    response = json.decode(body)

    token = response.get("token", "") or response.get("access_token", "")
    if not token:
        fail("{}: token exchange with {} returned neither 'token' nor 'access_token'".format(registry, url))
    return token

def _fetch_oci_blob(rctx, entry, *, output, executable):
    registry = entry.get("registry", "")
    repository = entry.get("repository", "")
    if not registry:
        fail("{}: field 'registry' is required in '{}' download mode".format(describe_entry(entry), MODE_OCI))
    if not repository:
        fail("{}: field 'repository' is required in '{}' download mode".format(describe_entry(entry), MODE_OCI))
    sha256 = sha256_of(entry)
    if not sha256:
        fail("""{}: '{}' download mode requires the digest of the blob to fetch.
    Set either "sha256" (hex) or "integrity" (sha256 as Subresource Integrity).""".format(describe_entry(entry), MODE_OCI))

    headers = {}
    token = _anonymous_token(
        rctx,
        registry = registry,
        repository = repository,
        auth_challenge = entry.get("auth_challenge", {}),
    )
    if token:
        headers["Authorization"] = "Bearer " + token

    rctx.download(
        url = [blob_url(registry = registry, repository = repository, sha256 = sha256)],
        output = output,
        executable = executable,
        sha256 = sha256,
        headers = headers,
    )

def _fetch_url(rctx, entry, *, output, executable):
    # Resolved for its cross-check of `sha256` against `integrity`, even when it
    # is the SRI value that ends up being handed to Bazel.
    sha256 = sha256_of(entry)

    kwargs = {}
    if entry.get("integrity", ""):
        # Hand the SRI value over verbatim, so that entries remain free to use a
        # digest algorithm that cannot name a registry blob.
        kwargs["integrity"] = entry["integrity"]
    elif sha256:
        kwargs["sha256"] = sha256
    else:
        fail("""{}: no digest to verify the download with.
    Set either "integrity" (Subresource Integrity) or "sha256" (hex).""".format(describe_entry(entry)))

    rctx.download(
        url = binary_urls(entry),
        output = output,
        executable = executable,
        **kwargs
    )

def fetch_tool(rctx, entry, *, output, executable = True):
    """Fetch the binary a lockfile entry describes into the repository.

    Args:
        rctx: Repository context.
        entry: Lockfile entry (dict), or the equivalent built from the
            attributes of `LOCKFILE_ATTRS`.
        output: Path to download the binary to, relative to the repository root.
        executable: Whether to set the executable bit on the downloaded file.
    """
    mode = _validated_mode(entry)
    if mode == MODE_OCI:
        _fetch_oci_blob(rctx, entry, output = output, executable = executable)
    else:
        _fetch_url(rctx, entry, output = output, executable = executable)
