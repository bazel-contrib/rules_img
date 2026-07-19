# rules_img_signer_cosign

A [rules_img](https://github.com/bazel-contrib/rules_img) signer plugin that
produces Sigstore (cosign) signatures using `sigstore-go`. It implements the
`sign-oci-artifact` subprocess protocol: it reads the subject OCI descriptor on
stdin and writes an OCI image layout tar — a Sigstore **bundle** (media type
`application/vnd.dev.sigstore.bundle.v0.3+json`) — on stdout. `img deploy` then
pushes that artifact as an OCI 1.1 referrer of the signed image.

The plugin **never talks to a container registry** (that is `img deploy`'s job);
it may reach Fulcio, Rekor, and an RFC3161 timestamp authority, which are signing
infrastructure. Its command-line flags, defaults, descriptions, and environment
variables mirror the real [`cosign sign`](https://github.com/sigstore/cosign)
CLI wherever they apply to a registry-less signer (see
[cosign parity](#cosign-parity) for what is and isn't supported and why).

This is an **independent Bazel module**.

## Usage

```python
bazel_dep(name = "rules_img_signer_cosign", version = "<version>")
```

```python
load("@rules_img//img:signing.bzl", "signing_config")

signing_config(
    name = "cosign",
    tool = "@rules_img_signer_cosign",  # short form -> the plugin binary
    # Keyless by default; needs an OIDC token at deploy time. Or pass --key.
    args = ["--tlog-upload=true"],
)
```

`@rules_img_signer_cosign` resolves to a prebuilt binary for released versions
(zero configuration), or a source-built `go_binary` otherwise. The `args` list
holds the plugin flags documented below; `img deploy` invokes the plugin as
`<tool> sign-oci-artifact <args...>` and passes the ambient process environment
through to it (so `$COSIGN_PASSWORD`, `$SIGSTORE_ID_TOKEN`, `$SIGSTORE_*`, etc.
reach the plugin).

### Common configurations

Keyless (ephemeral key certified by Fulcio, logged in Rekor) — the default. The
OIDC token is supplied at deploy time via `$SIGSTORE_ID_TOKEN` (e.g. minted by
CI) rather than an interactive browser flow:

```python
signing_config(
    name = "keyless",
    tool = "@rules_img_signer_cosign",
    # $SIGSTORE_ID_TOKEN is read from the environment at deploy time.
)
```

Key-based with a local cosign key (offline, no transparency log):

```python
signing_config(
    name = "key",
    tool = "@rules_img_signer_cosign",
    args = [
        "--key=/keys/cosign.key",   # encrypted key -> set $COSIGN_PASSWORD
        "--tlog-upload=false",
    ],
)
```

Key-based, with a timestamp authority and signed annotations:

```python
signing_config(
    name = "timestamped",
    tool = "@rules_img_signer_cosign",
    args = [
        "--key=/keys/cosign.key",
        "--timestamp-server-url=https://freetsa.org/tsr",
        "-a=build-system=bazel",
        "-a=env=prod",
    ],
)
```

### Signing a release

With a `signing_config` referencing this plugin, enable signing on the push at
deploy time. For the keyless flow, supply an OIDC identity token first — in CI
you fetch one from the platform's OIDC provider, and the plugin reads it from
`--identity-token` or `$SIGSTORE_ID_TOKEN`:

```bash
# e.g. in GitHub Actions with `id-token: write` permission
export SIGSTORE_ID_TOKEN="$(curl -sSL -H "Authorization: bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
  "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=sigstore" | jq -r .value)"

bazel run //path/to:push \
  --@rules_img//img/settings:sign=enabled \
  --@rules_img//img/settings:sign_setting=//path/to:keyless
```

### Verifying

`rules_img` stores the signature as an **OCI 1.1 referrer**, not as cosign's
legacy `sha256-<digest>.sig` tag. Stock `cosign verify` discovers signatures via
that tag scheme by default, so it will not find a referrer-attached signature;
referrer-based discovery on `cosign verify` has evolved across cosign versions
and may require a recent cosign and/or extra flags. Check the
[cosign documentation](https://docs.sigstore.dev/) for how *your* version
discovers OCI 1.1 referrers, then verify by digest, e.g.:

```bash
cosign verify \
  --certificate-identity-regexp '.*' \
  --certificate-oidc-issuer-regexp '.*' \
  ghcr.io/myorg/myapp@sha256:...
```

Tighten the identity and issuer matchers to the values you actually trust.
Alternatively, list the attached signatures directly through the registry's
Referrers API (e.g. `oras discover`) and validate the Sigstore bundle yourself.

---

## Manual

### NAME

`cosign sign-oci-artifact` — produce a Sigstore signature bundle for an OCI
subject descriptor.

### SYNOPSIS

```
cosign sign-oci-artifact [keyless]
    [--identity-token TOKEN|FILE] [--fulcio-url URL] [--signing-algorithm ALG]
    [--rekor-url URL] [--tlog-upload[=true|false]]
    [--timestamp-server-url URL]
    [-a KEY=VALUE ...]
    [--record-creation-timestamp]

cosign sign-oci-artifact --key FILE|KMS-URI
    [--certificate FILE] [--certificate-chain FILE]
    [--rekor-url URL] [--tlog-upload[=true|false]]
    [--timestamp-server-url URL]
    [-a KEY=VALUE ...]
    [--record-creation-timestamp]
```

The subject descriptor JSON is read from stdin; the signature bundle OCI layout
tar is written to stdout. Diagnostics go to stderr.

### DESCRIPTION

Two signing modes are selected by the presence of `--key`:

- **Keyless (default).** An ephemeral key is generated, certified by Fulcio using
  an OIDC identity token, and the signature is (by default) uploaded to the Rekor
  transparency log. The bundle embeds the Fulcio certificate. Requires an OIDC
  token via `--identity-token` or `$SIGSTORE_ID_TOKEN`; the plugin does **not**
  run an interactive OAuth/device flow.
- **Key-based (`--key`).** Signs with a local private key or a cloud KMS key.
  Encrypted cosign keys (`ENCRYPTED SIGSTORE PRIVATE KEY`, as produced by
  `cosign generate-key-pair`) are decrypted with `$COSIGN_PASSWORD`. A KMS URI
  (`awskms://`, `gcpkms://`, `azurekms://`, `hashivault://`) signs with a key
  held in the provider; any other `scheme://` reference is delegated to a
  `sigstore-kms-<scheme>` plugin binary on `PATH`. The bundle carries the bare
  public key, or a caller-supplied certificate when
  `--certificate`/`--certificate-chain` is given.

In both modes the signed content is a DSSE-wrapped in-toto Statement whose
subject is the signed image's manifest digest, with predicate type
`https://sigstore.dev/cosign/sign/v1`. This is exactly what `cosign sign
--new-bundle-format` produces, so a released `cosign verify` accepts it (cosign
has no verification path for a bare message-signature in a bundle).

### OPTIONS

```
--key string
        Path to a PEM-encoded private key file (ECDSA, RSA, or ED25519), or a
        KMS URI (awskms://, gcpkms://, azurekms://, hashivault://) (or
        $RULES_IMG_COSIGN_KEY). Encrypted cosign/sigstore private keys are
        decrypted with $COSIGN_PASSWORD. If unset, sign keyless via
        Fulcio/OIDC.

--identity-token string
        identity token to use for certificate from fulcio. the token or a
        path to a file containing the token is accepted (or $SIGSTORE_ID_TOKEN).

--fulcio-url string
        address of sigstore PKI server (or $SIGSTORE_FULCIO_URL). Used for
        keyless signing. (default "https://fulcio.sigstore.dev")

--signing-algorithm string
        signing algorithm for the ephemeral keyless key: one of ecdsa-p256,
        ecdsa-p384, ecdsa-p521, rsa-2048, rsa-3072, rsa-4096, ed25519.
        (default "ecdsa-p256"; ed25519 may be rejected by Fulcio)

--rekor-url string
        address of rekor transparency log server (or $SIGSTORE_REKOR_URL).
        (default "https://rekor.sigstore.dev")

--tlog-upload
        whether to upload the signature to the Rekor transparency log.
        (default true)

--timestamp-server-url string
        URL of an RFC3161 timestamp authority. When set, a signed timestamp
        is obtained and embedded in the bundle.

--certificate string
        path to the X.509 certificate in PEM format to include in the OCI
        signature (used with --key).

--certificate-chain string
        path to a list of CA X.509 certificates in PEM format used to build
        the certificate chain for the signing certificate, ordered from the
        intermediate CA that issued the signing certificate towards (but not
        including) the root. Included in the OCI signature (used with --key).

-a, --annotations key=value
        extra key=value pairs to sign (repeatable; recorded as annotations on
        the signed in-toto statement subject, matching cosign's -a).

--record-creation-timestamp
        set the org.opencontainers.image.created annotation on the signature
        artifact to the signing time. Off by default for reproducible
        signatures; honors $SOURCE_DATE_EPOCH. (default false)

-h, --help
        print usage and exit.
```

### ENVIRONMENT

```
SIGSTORE_ID_TOKEN
        OIDC token used to authenticate to Fulcio. Fallback for
        --identity-token.

RULES_IMG_COSIGN_KEY
        Signing key reference (PEM file path or KMS URI). Fallback for --key.

COSIGN_PASSWORD
        Password used to decrypt an encrypted --key PEM. cosign prompts on
        stdin; this plugin is non-interactive and treats an unset value as
        empty.

SIGSTORE_FULCIO_URL
        Fulcio base URL. Used when --fulcio-url is not passed explicitly.

SIGSTORE_REKOR_URL
        Rekor base URL. Used when --rekor-url is not passed explicitly.

SOURCE_DATE_EPOCH
        Seconds since the Unix epoch used as the creation time when
        --record-creation-timestamp is set (reproducible-builds.org).
```

Precedence for the Fulcio/Rekor URLs is: explicit flag > environment variable >
built-in default.

When `--key` is a KMS URI, the provider authenticates using its own standard
credential environment (e.g. `AWS_*` / instance role for `awskms://`,
`GOOGLE_APPLICATION_CREDENTIALS` for `gcpkms://`, `AZURE_*` for `azurekms://`,
`VAULT_ADDR`/`VAULT_TOKEN` for `hashivault://`). The plugin inherits the ambient
environment from `img deploy`, so those variables reach the provider unchanged.

### EXIT STATUS

`0` on success. Non-zero with a `cosign-plugin:` diagnostic on stderr if flag
parsing, key loading, or signing fails.

### EXAMPLES

Keyless with a CI-minted token:

```
SIGSTORE_ID_TOKEN=$(mint-oidc-token) \
  img deploy ... # signing_config passes no extra args
```

Offline key-based signing, reproducible:

```
args = ["--key=cosign.key", "--tlog-upload=false"]
# $COSIGN_PASSWORD set in the environment
```

Key-based with your own certificate and an RFC3161 timestamp:

```
args = [
    "--key=signing.key",
    "--certificate=signing.crt",
    "--certificate-chain=ca-chain.pem",
    "--timestamp-server-url=https://freetsa.org/tsr",
]
```

Cloud KMS key (credentials come from the provider's standard environment):

```
args = ["--key=awskms:///alias/my-signing-key"]
# or gcpkms://projects/P/locations/L/keyRings/R/cryptoKeys/K/cryptoKeyVersions/V
```

### NOTES

- The bundle is always the new-format Sigstore bundle v0.3 (equivalent to cosign
  `--new-bundle-format`); there is no legacy signature layout. Its content is a
  DSSE envelope over an in-toto Statement — technically an *attestation* — which
  is the only new-bundle shape a released `cosign verify` can verify. (cosign has
  no verification path for a bare message-signature bundle.)
- The signed Statement records only the subject **digest** (plus any
  `--annotations`); no image reference is included, because the plugin only
  receives the subject descriptor.
- Reproducibility caveat: uploading to Rekor (`--tlog-upload`) or embedding a TSA
  timestamp records observer timestamps, so those signatures are not bit-for-bit
  reproducible even with `--record-creation-timestamp` unset.

### VERIFYING

`rules_img` attaches the bundle as an **OCI 1.1 referrer** of the image. Verify
by digest with a recent cosign (v3.x), which discovers the referrer and accepts
the DSSE/in-toto bundle with its default flags:

```
cosign verify --key cosign.pub ghcr.io/you/image@sha256:<image-digest>
```

Verifying by tag (`:latest`) works too when the registry exposes referrers.

---

## cosign parity

Flag names, defaults, and descriptions track `cosign sign` for the features that
make sense in a registry-less, non-interactive signer. A few `cosign sign` flags
are deprecated on cosign `main` in favor of a TUF-provided `signing-config`
(`--fulcio-url`, `--rekor-url`) or were historically renamed (`--tlog-upload` vs
`--upload`, `--timestamp-server-url` vs `--rfc3161-timestamp`). This plugin keeps
the **stable, familiar cosign v2 names** and does not depend on TUF/signing-config.

`--key` accepts the same references cosign does — a PEM file or a cloud KMS URI
(`awskms://`, `gcpkms://`, `azurekms://`, `hashivault://`); other `scheme://`
references fall through to a `sigstore-kms-<scheme>` plugin binary. The only
cosign `--key` form not supported is `k8s://` (Kubernetes secret), which would
require a Kubernetes client and cluster access.

Deliberately **not** supported, with the reason:

| cosign feature | why it is out of scope |
|---|---|
| `--upload`, `--output-*`, `--bundle`, `--registry-*`, `--allow-*-registry`, `--k8s-keychain`, `--registry-referrers-mode` | The plugin never contacts a registry. `img deploy` owns all registry I/O, auth, and referrers writing — the bundle is simply this plugin's stdout. |
| `--recursive` / `-r` | Multi-arch fan-out is decided by `img deploy`, which invokes the signer once per subject descriptor. |
| `--oidc-issuer`, `--oidc-client-id`, `--oidc-client-secret-file`, `--oidc-redirect-url`, `--oidc-provider`, `--fulcio-auth-flow` | The plugin runs headless; it cannot open a browser or run a device flow. Supply a pre-minted token via `--identity-token`/`$SIGSTORE_ID_TOKEN` (equivalent to cosign's `token` flow). Ambient token minting would pull in the entire GitHub/GitLab/SPIFFE dependency surface. |
| `--sk`, `--slot` (hardware/PIV/PKCS#11) | sigstore-go has no PIV/PKCS#11 support; it needs cgo (hostile to hermetic Go builds) and a physically-present token + PIN, which a non-interactive `img deploy` subprocess cannot provide. |
| `k8s://` references in `--key` | Reading a key from a Kubernetes secret needs a Kubernetes client and cluster credentials; use a KMS URI or a file instead. |
| `--signing-config`, `--use-signing-config`, `--trusted-root`, `TUF_*` | Service URLs and trust roots are configured explicitly via `--fulcio-url`/`--rekor-url` instead of a TUF client, avoiding that dependency/network surface. |
| `--yes` / `-y`, `--insecure-skip-verify` | The plugin is always non-interactive (nothing to confirm), and SCT verification is no longer performed during signing. |
| `--timestamp-client-cert`/`-key`/`-cacert`, `--timestamp-server-name` (TSA mTLS) | Deferred; add only if a target TSA requires mutual TLS. |

---

## Building from source (git_override / local path)

`sigstore-go` pulls proto-heavy transitive dependencies whose `.proto` sources
do not build cleanly under gazelle's proto generation. Because `gazelle_override`
is a **root-module-only** tag, a downstream *source* build (e.g. via
`git_override`) requires adding the following to your **root** `MODULE.bazel`:

```python
go_deps = use_extension("@gazelle//:extensions.bzl", "go_deps")
go_deps.gazelle_override(
    directives = ["gazelle:go_generate_proto false"],
    path = "github.com/sigstore/rekor-tiles/v2",
)
go_deps.gazelle_override(
    directives = ["gazelle:go_generate_proto false"],
    path = "github.com/google/certificate-transparency-go",
)
```

Released versions ship a prebuilt binary and need none of this.
