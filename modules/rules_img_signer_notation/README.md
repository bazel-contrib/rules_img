# rules_img_signer_notation

A [rules_img](https://github.com/bazel-contrib/rules_img) signer plugin that
produces Notary Project (Notation) signatures with
[notation-core-go](https://github.com/notaryproject/notation-core-go). It
implements the `sign-oci-artifact` subprocess protocol used by `img deploy`: it
reads the subject descriptor from stdin, signs it with a local PEM private key
and X.509 certificate chain, and writes the signature as an OCI image layout tar
to stdout. **It never contacts a container registry** — `img deploy` pushes the
resulting signature as an OCI 1.1 referrer of the subject.

This is an **independent Bazel module**.

## Installation

```python
bazel_dep(name = "rules_img_signer_notation", version = "<version>")
```

`@rules_img_signer_notation` resolves to a prebuilt binary for released versions,
or a source-built `go_binary` for any other commit / local / `git_override`
build. Source builds of this module work with no extra configuration.

## Usage

```python
load("@rules_img//img:signing.bzl", "signing_config")

signing_config(
    name = "notary",
    tool = "@rules_img_signer_notation",  # short form -> the plugin binary
    args = [
        "--key", "/path/to/key.pem",
        "--certificate-chain", "/path/to/chain.pem",
        "--signature-format", "cose",
        "--expiry", "8760h",
        "--user-metadata", "buildId=42",
        "--timestamp-url", "http://timestamp.digicert.com",
        "--timestamp-root-cert", "/path/to/tsa-root.pem",
    ],
)
```

Key material can also be supplied through the environment instead of `args`, so
it never appears in Bazel command lines or the build graph:

```sh
export RULES_IMG_NOTATION_KEY=/path/to/key.pem
export RULES_IMG_NOTATION_CERTIFICATE_CHAIN=/path/to/chain.pem
```

With a `signing_config` referencing this plugin, sign a release push by enabling
signing at deploy time:

```bash
bazel run //path/to:push \
  --@rules_img//img/settings:sign=enabled \
  --@rules_img//img/settings:sign_setting=//path/to:notary
```

## Verifying

The plugin produces a standard Notary Project signature attached as a referrer,
so `notation verify` discovers it through the Referrers API once you have
configured a trust store (your CA certificates) and a trust policy:

```bash
notation verify ghcr.io/myorg/myapp@sha256:...
```

See the [Notary Project documentation](https://notaryproject.dev/) for setting
up trust stores and trust policies.

## Supported features

| Feature | Flag | Notes |
| --- | --- | --- |
| Local key-based signing | `--key`, `--certificate-chain` | PEM private key + X.509 chain (leaf first). RSA (PKCS#1/PKCS#8) and EC keys are supported. |
| Signature envelope format | `--signature-format` | `jws` (default) or `cose`, mapped to `notation-core-go`'s JWS / COSE envelopes. |
| Signature expiry | `--expiry` / `-e` | Embeds a "best by use" expiry in the signed attributes. `0` (default) means no expiry. |
| User metadata | `--user-metadata` / `-m` | Repeatable `{key}={value}` pairs added to the signed `targetArtifact.annotations`. |
| RFC 3161 timestamping | `--timestamp-url`, `--timestamp-root-cert` | Countersigns the signature with a trusted Timestamping Authority. Both flags are required together. |

Every signature uses the `notary.x509` signing scheme, sets the signing agent to
`rules_img-notation-plugin`, and records the certificate-chain thumbprint in the
required `io.cncf.notary.x509chain.thumbprint#S256` annotation — matching what a
`notation` verifier expects.

## Relationship to the `notation` CLI

Where a `notation sign` flag shapes the **signature envelope** and is meaningful
for a config-less, registry-less signer, this plugin reuses notation's exact
flag name, shorthand, default, and description:

| `notation sign` | This plugin | Status |
| --- | --- | --- |
| `--signature-format` (default `jws`) | `--signature-format` (default `jws`) | Mirrored verbatim. |
| `--expiry` / `-e` | `--expiry` / `-e` | Mirrored verbatim. |
| `--user-metadata` / `-m` | `--user-metadata` / `-m` | Mirrored verbatim. |
| `--timestamp-url` | `--timestamp-url` | Mirrored verbatim. |
| `--timestamp-root-cert` | `--timestamp-root-cert` | Mirrored verbatim. |
| `--key` / `-k` (a **named** key from notation's key list) | `--key` (a **PEM file path**) | Same name, different meaning — see NOTES. No `-k` shorthand, deliberately. |
| *(cert is bound to a named key in `signingkeys.json`)* | `--certificate-chain` | Plugin-specific; notation has no sign-time cert-path flag. |

The following `notation sign` flags are **intentionally omitted** because this
plugin never talks to a registry, keeps no notation config directory, and *is*
the signer rather than delegating to one:

- Registry auth: `--username` / `-u`, `--password` / `-p`, `--insecure-registry`
  (and the `NOTATION_USERNAME` / `NOTATION_PASSWORD` env vars).
- Referrers storage strategy: `--force-referrers-tag`, `--allow-referrers-api`
  (`img deploy` owns how the signature is pushed).
- Plugin/KMS delegation: `--plugin`, `--id`, `--plugin-config`.
- Experimental subject sourcing: `--oci-layout` (and `NOTATION_EXPERIMENTAL`).
- CLI logging: `--debug` / `-d`, `--verbose` / `-v`.
- Notation config/cache directories: `NOTATION_CONFIG`, `NOTATION_CACHE`,
  `NOTATION_LIBEXEC` — this plugin is config-less by design.

---

## Manual (man-page style)

### NAME

**notation** — rules_img signer plugin that produces Notary Project signatures.

### SYNOPSIS

**notation** `sign-oci-artifact` \[*options*\] < *subject-descriptor.json* > *signature-oci-layout.tar*

### DESCRIPTION

The plugin is invoked by `img deploy` (never directly by end users) with the
single subcommand `sign-oci-artifact`. It reads a JSON OCI descriptor of the
artifact to sign from **stdin**, creates a Notary Project signature envelope over
the `application/vnd.cncf.notary.payload.v1+json` payload, and writes an OCI
image layout **tar** to **stdout**. The output manifest has artifactType
`application/vnd.cncf.notary.signature`, a single envelope layer, an empty
config, and the subject descriptor set as its OCI 1.1 `subject`.

Signing happens locally with the private key and certificate chain named by the
options below; no network connection is made except an optional RFC 3161
timestamping request to the TSA given by `--timestamp-url`.

### OPTIONS

- **--key** *path*

  Path to a PEM-encoded private key (or `$RULES_IMG_NOTATION_KEY`). Required.
  RSA (PKCS#1 or PKCS#8) and EC private keys are accepted. Unlike
  `notation sign --key`, this is a filesystem path to key material, not a named
  key from notation's key list.

- **--certificate-chain** *path*

  Path to a PEM-encoded X.509 certificate chain, leaf certificate first (or
  `$RULES_IMG_NOTATION_CERTIFICATE_CHAIN`). Required. The chain populates the
  signature's certificate list and the
  `io.cncf.notary.x509chain.thumbprint#S256` annotation.

- **--signature-format** *jws|cose*

  signature envelope format, options: "jws", "cose". Default: `jws`.

- **--expiry**, **-e** *duration*

  optional expiry that provides a "best by use" time for the artifact. The
  duration is specified in minutes(m) and/or hours(h). For example: 12h, 30m,
  3h20m. Default: `0` (no expiry). Must be non-negative and a whole number of
  seconds.

- **--user-metadata**, **-m** *key=value*

  {key}={value} pairs that are added to the signature payload. May be repeated.
  Each pair is added to the signed `targetArtifact.annotations`. Keys using the
  reserved `io.cncf.notary` prefix, malformed pairs, and duplicate keys are
  rejected.

- **--timestamp-url** *url*

  RFC 3161 Timestamping Authority (TSA) server URL. Must be used together with
  `--timestamp-root-cert`.

- **--timestamp-root-cert** *path*

  filepath of timestamp authority root certificate. The PEM file supplies the
  trust anchor used to validate the TSA's timestamp response. Must be used
  together with `--timestamp-url`.

### ENVIRONMENT

- **RULES_IMG_NOTATION_KEY**

  Fallback for `--key` when the flag is not given.

- **RULES_IMG_NOTATION_CERTIFICATE_CHAIN**

  Fallback for `--certificate-chain` when the flag is not given.

- **NOTATION_KEY**, **NOTATION_CERTIFICATE_CHAIN**

  Deprecated fallbacks for the two variables above, still honored for backward
  compatibility. Prefer the `RULES_IMG_*` names: these are **not** real
  `notation` environment variables (the `notation` CLI has no env var for a PEM
  key/cert path), so the `RULES_IMG_*` prefix avoids implying otherwise.

In all cases a flag takes precedence over its environment variables, and the
`RULES_IMG_*` name takes precedence over the deprecated `NOTATION_*` name.

### EXAMPLES

Sign with a local key and the default JWS envelope:

```sh
notation sign-oci-artifact \
    --key key.pem --certificate-chain chain.pem \
    < subject.json > signature.tar
```

Sign with a COSE envelope, a one-year expiry, build metadata, and an RFC 3161
timestamp:

```sh
notation sign-oci-artifact \
    --key key.pem --certificate-chain chain.pem \
    --signature-format cose \
    --expiry 8760h \
    --user-metadata buildId=42 --user-metadata commit=abcdef \
    --timestamp-url http://timestamp.digicert.com \
    --timestamp-root-cert tsa-root.pem \
    < subject.json > signature.tar
```

### EXIT STATUS

Returns `0` on success. On any error (missing/invalid key or certificate,
malformed options, unreadable stdin descriptor, signing failure) it prints a
`notation-plugin: <error>` message to stderr and exits `1`.

### NOTES

- **`--key` is a path, not a named key.** The `notation` CLI resolves `--key`
  to an entry in its on-disk key list (`signingkeys.json`) and uses the `-k`
  shorthand. This plugin is intentionally config-less — Bazel and `img deploy`
  must not depend on a notation config directory — so `--key` is a direct path
  to PEM key material. The `-k` shorthand is deliberately **not** offered to
  avoid implying drop-in compatibility with `notation sign --key`.
- **Verification trust store is out of scope.** When timestamping, `notation`
  also requires the TSA root to be added to its trust store for later
  verification. This plugin only *produces* the signature; it uses
  `--timestamp-root-cert` to validate the TSA response and its certificate-chain
  revocation status at signing time (as `notation sign` does), but
  verification-side trust configuration is separate — set it up in your verifier.
  Because the TSA-chain revocation check queries CRL/OCSP endpoints, timestamped
  signing needs outbound network access to those endpoints in addition to the TSA
  itself.
- **Signing agent / scheme.** Signatures are produced under the `notary.x509`
  signing scheme with signing agent `rules_img-notation-plugin`. These are not
  configurable and are not exposed by `notation sign` either.

### SEE ALSO

- `notation sign` — <https://notaryproject.dev/docs/reference/cli/notation_sign/>
- Notary Project signature specification —
  <https://github.com/notaryproject/specifications/blob/main/specs/signature-specification.md>
- rules_img signing docs — `@rules_img//img:signing.bzl`
