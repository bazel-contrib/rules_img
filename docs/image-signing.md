# Image Signing

`rules_img` can sign the container images it pushes, so that consumers can
cryptographically verify **who** produced an image and that its bytes have
**not been tampered with** since.

This guide explains what image signing is, how `rules_img` implements it through
pluggable external signers, how to choose between the two signer plugins that
ship with `rules_img` (Notation and cosign), and how to set each of them up. It
finishes with a short guide to writing your own signer plugin.

For the reference documentation of the `signing_config` rule, see
[`signing_config`](signing.md#signing_config).

## What is image signing?

A container image is addressed by the digest of its manifest — a SHA-256 hash of
the bytes that describe the image. A **signature** is a statement, made with a
private key, that binds an identity to that digest. Anyone with the matching
public key (or trust anchor) can later **verify** the signature and learn two
things:

- **Integrity** — the manifest digest that was signed is exactly the one they
  are about to run. If a single byte of the image changed, the digest changes,
  and the signature no longer matches.
- **Provenance** — the signature was produced by a party they trust (a specific
  build system, a specific corporate certificate authority, a specific
  identity).

Signatures are the foundation of a secure software supply chain: admission
controllers such as [Kyverno][kyverno], [Sigstore Policy Controller][policy-controller],
or [Ratify][ratify] can be configured to only admit images that carry a valid
signature from a trusted signer, blocking unsigned or tampered images before
they ever run.

A signature does not live *inside* the image. Instead it is stored next to the
image in the same registry repository, linked back to the image it describes.
`rules_img` stores signatures as **[OCI 1.1 referrers][oci-referrers]**: separate
manifests whose `subject` field points at the signed image. Verifiers discover
them through the registry's Referrers API by asking "what refers to this
digest?", so no extra tag or naming convention is required.

## How signing works in `rules_img`

Signing happens as the last step of `img deploy` — the tool behind
[`image_push`](push.md#image_push), [`image_push_spec`](push.md#image_push_spec),
and [`multi_deploy`](multi_deploy.md#multi_deploy). After an image has been
pushed, `img` signs the requested descriptors and attaches the resulting
signatures to the same repository as referrers.

### The core tool carries no cryptography

The `img` binary itself contains **no signing keys, no certificates, and no
signature crypto at all**. Everything that touches key material lives in an
external **signer plugin**, which `img` runs as a subprocess. This keeps the
core tool small and lets each organization pick (or build) a signer that matches
its own trust model without bloating `img` with every signing ecosystem's
dependencies.

The contract between `img` and a plugin is a tiny subprocess protocol:

1. `img deploy` runs the plugin as `<tool> sign-oci-artifact [args...]`.
2. It writes the JSON **descriptor** of the artifact to sign (media type, digest,
   size) to the plugin's **stdin**.
3. The plugin produces a signature and writes an **OCI image layout tar** — the
   signature artifact, with its `subject` field already pointing at the signed
   descriptor — to its **stdout**.
4. `img` reads that artifact and **pushes it to the registry as a referrer** of
   the signed image.

The plugin never talks to the registry (it may contact its own signing
infrastructure, such as a KMS or a transparency log). Registry authentication,
blob uploads, and referrer bookkeeping are entirely `img`'s job. Because the
plugin only ever sees a digest and returns an artifact, the same plugin works
regardless of which push strategy produced the image.

```
  image_push / multi_deploy (bazel run)
                │
                ▼
        ┌───────────────┐   subject descriptor (JSON, stdin)   ┌───────────────┐
        │   img deploy  │ ───────────────────────────────────▶ │ signer plugin │
        │  (no crypto)  │                                      │  (has keys)   │
        │               │◀───────────────────────────────────  │               │
        └───────┬───────┘   OCI layout tar (signature, stdout) └───────────────┘
                │
                ▼
        push signature to the registry as an OCI referrer of the image
```

Because the keys live in the plugin's process, **secrets are supplied by the
environment `bazel run` executes in — never by Bazel.** A `signing_config` may
set non-secret flags and environment variables, but private keys, HSM/KMS
credentials, and OIDC tokens are read from files, environment variables, or
hardware at deploy time, so they never enter the build graph or a remote cache.

### Configuring signing

Signing is described by the [`signing_config`](signing.md#signing_config) rule
and turned on with a flag or a per-target attribute.

```python
load("@rules_img//img:signing.bzl", "signing_config")

signing_config(
    name = "release_signer",
    tool = "@rules_img_signer_notation",   # a signer plugin (see below)
    args = ["--key", "release.key", "--certificate-chain", "chain.pem"],
    # Which descriptors to sign; "roots" (the pushed image) is the default.
    targets = ["roots"],
)
```

A `signing_config` selects a plugin in one of two ways, exactly one of which
must be set:

- **`tool`** — a Bazel executable (usually a signer plugin module such as
  `@rules_img_signer_notation`) that is shipped in the push binary's runfiles.
  Fully hermetic: the exact plugin is pinned by your `MODULE.bazel`.
- **`tool_command`** — the name or path of a host-installed command, resolved on
  `$PATH` at deploy time. Useful for a company-internal signer you distribute
  separately.

Two knobs control *whether* and *how* an image is signed:

| Knob | Scope | Values |
| --- | --- | --- |
| [`//img/settings:sign`](../img/settings/BUILD.bazel) | global flag | `disabled` (default), `enabled`, `best_effort` |
| `sign` attribute | per `image_push` / `image_push_spec` | `auto` (default), `enabled`, `best_effort`, `disabled` |
| [`//img/settings:sign_setting`](../img/settings/BUILD.bazel) | global flag | a `signing_config` label |
| `sign_setting` attribute | per `image_push` / `image_push_spec` | a `signing_config` label |

The per-target `sign` attribute defaults to `auto`, which defers to the global
`--@rules_img//img/settings:sign` flag. So the common pattern is to leave the
targets alone and flip signing on for a release build:

```bash
bazel run //path/to:push \
  --@rules_img//img/settings:sign=enabled \
  --@rules_img//img/settings:sign_setting=//path/to:release_signer
```

- **`enabled`** signs, and a signing failure (or a missing `sign_setting`) fails
  the deploy.
- **`best_effort`** signs when it can; failures are warnings and do not fail the
  deploy.
- **`disabled`** never signs.

A target's own `sign_setting` attribute overrides the global
`//img/settings:sign_setting`, so different images can be signed by different
plugins in a single `multi_deploy`.

The `targets` attribute of `signing_config` selects **which descriptors** get a
signature: `roots` (the pushed image or index — the default), `child_manifests`
(each per-platform child of an index), and `referrers` (referrer artifacts such
as SBOMs).

### Deploy-time overrides

Because signing runs inside `img deploy`, a few things can be overridden at
`bazel run` time without rebuilding, by passing flags after `--`:

| Flag | Effect |
| --- | --- |
| `--sign_targets=roots,child_manifests,referrers` (or `all`) | Override which descriptors are signed. |
| `--sign_force` | Sign every pushed image with the default signer, even targets not configured to sign at build time. |
| `--default_sign_setting=<path\|sha256:...>` | Provide the default signer at deploy time. |
| `--sign_setting_file=<path>` | Ingest an extra signer config file. |

```bash
# Sign the index root and every per-platform child manifest:
bazel run //path/to:push -- --sign_targets=roots,child_manifests
```

## Choosing a plugin: Notation vs. cosign

`rules_img` ships two signer plugins as **independent Bazel modules**, each
versioned separately from `rules_img` and each pulling in only its own signing
ecosystem's dependencies:

- **[`@rules_img_signer_notation`](../modules/rules_img_signer_notation)** —
  produces [Notary Project][notary] (Notation) signatures.
- **[`@rules_img_signer_cosign`](../modules/rules_img_signer_cosign)** —
  produces [Sigstore][sigstore] (cosign) signatures.

Both attach their signatures as OCI referrers, both work with any of the push
strategies, and both keep key material out of Bazel. They differ mainly in
**trust model**.

| | **cosign** (Sigstore) | **Notation** (Notary Project) |
| --- | --- | --- |
| Primary focus | Keyless signing & public transparency | Enterprise / company-internal PKI |
| Identity anchor | Short-lived certificate from **Fulcio**, bound to an **OIDC** identity | Your own **X.509 certificate chain** from your CA |
| Key management | None by default (ephemeral keys); optional local key or KMS | You own the keys (PEM files, HSM, or KMS) |
| Transparency | Records signatures in the public **Rekor** transparency log by default | No transparency log; signatures are self-contained |
| Verification is by… | *Identity* (e.g. "signed by CI workflow X in repo Y") | *Trust store + trust policy* (which CAs / identities you trust) |
| External dependencies | Fulcio + Rekor (public "sigstore" instances, or run your own) | None at signing time |
| Signature format | Sigstore bundle (`application/vnd.dev.sigstore.bundle.v0.3+json`) | COSE or JWS envelope (`application/vnd.cncf.notary.signature`) |
| Best fit | Open-source projects, CI-driven public supply chains | Regulated / air-gapped / private environments with existing PKI |

### The tradeoffs in more detail

**cosign leans into keyless signing and public transparency.** Its headline
feature is signing *without managing a private key*: the plugin mints an
ephemeral key, asks Fulcio to issue a short-lived (\~10-minute) code-signing
certificate that binds that key to an OIDC identity (a GitHub Actions workflow,
a Google/GitHub/Microsoft account, …), signs, and — by default — records the
signature in the public Rekor transparency log. There is nothing to rotate,
store, or leak. Verifiers check *who* signed rather than *which key* signed
("accept anything signed by the `release.yml` workflow in `myorg/myrepo`"), and
the transparency log makes signatures publicly auditable and lets verification
succeed after the certificate itself has expired.

The cost of that model is its dependencies and its publicity. Keyless signing
needs a reachable Fulcio and an OIDC identity provider at deploy time, and the
public transparency log means the signed digest and signer identity become
**public information** — which can leak the existence of internal images or
repositories. Organizations that want the Sigstore model without the exposure
can run their own private Fulcio/Rekor deployment, or fall back to key-based
cosign signing (`--key`) with `--tlog-upload=false`.

**Notation leans into company-internal PKI and self-containment.** It expects
you to bring your **own** X.509 trust anchors and (typically long-lived) signing
certificates — usually issued by your own certificate authority, the same PKI
many enterprises already run for TLS and code signing (self-signed roots also
work for smaller setups). Signatures are self-contained COSE or JWS envelopes
with no transparency log and no external service in the signing path, which
makes it a natural fit for **air-gapped, regulated, or otherwise private**
environments. Verification is governed by a *trust store* (the certificates you
trust) and a *trust policy* (which identities and registries those certificates
may vouch for), giving security teams centralized, auditable control.

The cost of that model is operational: you own key generation, storage
(typically an HSM or KMS), rotation, and revocation (CRL/OCSP), and — in the
typical enterprise setup — you operate the certificate authority that issues
them. Nothing is published to a public log, so there is no built-in public
auditability — that is a feature in a private setting and a gap in a public one.

**Rule of thumb:** if you publish open-source images from CI and want the
lowest-friction, publicly auditable signatures, reach for **cosign** (keyless).
If you sign internal images inside an organization that already has a PKI and
compliance requirements, reach for **Notation**.

## Setting up a signer

Each signer plugin ships with its own in-depth setup guide — installation,
`signing_config` recipes for keyless and key-based signing, deploy-time secret
handling, and how to verify the signatures it produces:

- **cosign (Sigstore)** —
  [`rules_img_signer_cosign` README](../modules/rules_img_signer_cosign/README.md)
- **Notation (Notary Project)** —
  [`rules_img_signer_notation` README](../modules/rules_img_signer_notation/README.md)

Both are wired into a build the same way: reference the plugin from a
[`signing_config`](signing.md#signing_config) via `tool` (or `tool_command`) and
enable signing on the push as described in [Configuring signing](#configuring-signing).

## Writing your own signer plugin

Because `img` and the plugins are decoupled by a subprocess protocol, you can
write a signer for any signing scheme — a corporate signing service, a
different transparency log, an internal envelope format — without changing
`rules_img`. The two bundled plugins are just reference implementations of the
same contract.

A plugin is any executable that:

1. Accepts the subcommand `sign-oci-artifact` as its first argument, followed by
   any flags your `signing_config` passes in `args`.
2. Reads a single JSON [OCI descriptor][oci-descriptor] from **stdin** — the
   `mediaType`, `digest`, and `size` of the artifact to sign.
3. Writes an **OCI image layout tar** to **stdout** containing the signature
   artifact, whose manifest sets the `subject` field to the descriptor it read.
4. Exits `0` on success, non-zero (with a diagnostic on stderr) on failure.
5. Never contacts the container registry — that is `img`'s job. The plugin may
   reach its *own* signing infrastructure (KMS, HSM, transparency log).

In Go, the bundled plugins share a small helper package that implements the
stdin/stdout framing and the OCI-layout writing for you; you only implement the
signing itself:

```go
// OCIArtifactSigner is the single seam between the plugin runner and your logic.
type OCIArtifactSigner interface {
    // Sign returns a v1.Image (the signature artifact) whose subject is `subject`.
    Sign(ctx context.Context, subject v1.Descriptor) (v1.Image, error)
}
```

Wire it up with the plugin's `Dispatch` helper (see
[`cmd/notation/notation.go`](../modules/rules_img_signer_notation/cmd/notation/notation.go)
and [`cmd/cosign/cosign.go`](../modules/rules_img_signer_cosign/cmd/cosign/cosign.go)
for complete, working examples), build it as a `go_binary` (or any executable),
and reference it from a `signing_config` via `tool` (a Bazel target) or
`tool_command` (a host command). Plugins do not need to be written in Go —
anything that speaks the stdin/stdout protocol above works.

[kyverno]: https://kyverno.io/
[policy-controller]: https://docs.sigstore.dev/policy-controller/overview/
[ratify]: https://ratify.dev/
[oci-referrers]: https://github.com/opencontainers/distribution-spec/blob/main/spec.md#listing-referrers
[oci-descriptor]: https://github.com/opencontainers/image-spec/blob/main/descriptor.md
[notary]: https://notaryproject.dev/
[sigstore]: https://www.sigstore.dev/
