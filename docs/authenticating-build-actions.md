# Authenticating Build Actions

Most of rules_img is hermetic, but a few operations talk to a container registry
from inside a **build action**:

- **Lazily pulled base-image layers** — layer blobs are fetched by a build action
  (mnemonic `DownloadBlob`) instead of during repository fetching.
- **Push at build time** — [`push_at_build_time`](push-strategies.md#push-at-build-time)
  uploads image blobs (all layers and the config) and, optionally, the manifest(s)
  as build actions.

Both need to reach the registry and authenticate. This page explains how.

## Build actions need network access

Registry traffic from a build action needs **network access**, which makes the
action non-hermetic. Some sandboxing setups deliberately block network access
from actions (or run them where the registry isn't reachable); build-time
pull/push will not work in those environments. Under remote execution the
connection is opened by the **executor**, not by the machine running Bazel — see
[option 3](#3-oci-distribution-gateway) below.

## What access is required

The registry permissions differ per operation:

- **Lazy pulls are read-only.** The `DownloadBlob` action only issues `GET`/`HEAD`
  on `/v2/<repo>/blobs/...` and writes the blob to a local file — read access is
  enough. (It also intentionally clears `IMG_CREDENTIAL_HELPER` so a host-local
  helper path is never baked into a potentially-remote action.)
- **Push at build time needs write access**, and the scope depends on
  `push_at_build_time_content`:
  - `blobs` — writes only to `/v2/<repo>/blobs/` (blob uploads: every layer and
    the config). A credential that may upload blobs but may not read them or write
    manifests is sufficient (see the multi-tenant note in [Push Strategies](push-strategies.md#blobs-at-build-time-manifest-afterwards-blobs)).
  - `blobs_and_manifests` — additionally writes `/v2/<repo>/manifests/<ref>` (the
    config, manifest, and tags), so it also needs manifest write access.

## How rules_img resolves credentials

rules_img (the `img` tool, its build actions, and the gateway) resolves registry
credentials with one keychain, tried in order:

1. A **Bazel credential helper**, when
   `IMG_CREDENTIAL_HELPER_OCI_REGISTRY` or its `IMG_CREDENTIAL_HELPER` fallback
   is set (see [Credential Helpers](credential-helpers.md)).
2. **Host-scoped environment credentials**, when `IMG_REGISTRY_AUTH_HOST` and
   either `IMG_REGISTRY_AUTH_USERNAME` plus `IMG_REGISTRY_AUTH_PASSWORD`, or
   `IMG_REGISTRY_AUTH_BEARER_TOKEN`, are set.
3. An **inline Docker config**, when `IMG_DOCKER_CONFIG_INLINE` holds the JSON
   contents of a `config.json`. Resolved just like the Docker config below, but
   from memory. Intended for secret-injection mechanisms only — see
   [Option 4](#4-inline-docker-config-from-an-injected-environment-variable) and
   the warning there.
4. The **Docker config** (honors `DOCKER_CONFIG`; `REGISTRY_AUTH_FILE` is used
   inside build actions).
5. **Google** — `google.Keychain` (Application Default Credentials / workload identity).
6. **Amazon ECR** — the ambient AWS configuration.

Whatever option you pick below, the credentials it provides are consumed through
this keychain.

## Options

### 1. Ship a config file into the action

Provide a Docker-style `config.json` in the environment the action runs in, and
make sure that environment sets the variable that points at it:

- For lazy pulls, set
  `--@rules_img//img/settings:docker_config_path=/path/to/config.json`; rules_img
  passes it to the `DownloadBlob` action as `REGISTRY_AUTH_FILE`.
- Otherwise ensure `DOCKER_CONFIG` (or `REGISTRY_AUTH_FILE`) is set in the
  environment the action executes in, pointing at a readable config.
- Under remote execution the file must exist **on the executor**, inside the
  action's environment (for example mounted into the worker/runner) — a path that
  only exists on the machine running Bazel is not enough.

### 2. Cloud workload identity (GCP / AWS)

When the action runs on GCP or AWS and targets that cloud's own registry
(Artifact Registry / GCR, or ECR), use **workload identity**. The Google and
Amazon keychains are built in and discover the ambient credentials automatically
(ADC / the metadata server on GCP; the instance or task role on AWS). No config
file is needed — just make sure the executor runs with the right identity.

### 3. OCI distribution gateway

Instead of handing every action registry credentials, route registry traffic
through the **OCI distribution gateway** (`oci-distribution-gateway`). Actions
connect to it anonymously; the gateway authenticates to the real upstream itself
(using the keychain above), enforces a per-operation policy, and restricts which
upstream registries may be reached.

![OCI distribution gateway](visuals/oci-distribution-gateway-light.svg#gh-light-mode-only)
![OCI distribution gateway](visuals/oci-distribution-gateway-dark.svg#gh-dark-mode-only)

Under remote execution the gateway runs alongside the worker (for example as a
sidecar sharing a UNIX socket). The build action's registry requests reach it
**unauthenticated over that socket**; the gateway decides which requests are
allowed and adds the upstream credentials, so only **authenticated** requests
ever reach the real registry. The credentials live on the gateway, not in the
actions.

Point the build actions at a gateway with the registry-gateway settings:

```bash
# Shared endpoint for both pull and push:
common --@rules_img//img/settings:registry_gateway=unix:/path/to/gw.sock

# Or split pull and push (these take precedence over registry_gateway):
common --@rules_img//img/settings:registry_pull_gateway=https://pull-gw.example.com
common --@rules_img//img/settings:registry_push_gateway=unix:/path/to/gw.sock
```

Endpoint forms: `http://host[:port]`, `https://host[:port]`, or `unix:<path>`.
Use a single `unix:` prefix followed by an absolute path (e.g.
`unix:/run/gw.sock`) — **not** `unix://` or `unix:///`.

That is the whole client side. Everything about running the gateway is in the
**[gateway service README]**:

| | |
|---|---|
| [Modes] | `serve` talks to registries; `forward` relays to another gateway |
| [Flags] | the full flag list for both modes |
| [Policy file] | the JSON/YAML schema, how rules are matched, and reloading |
| [Blob existence cache] | memoizing "is this layer already pushed?", and sharing that between replicas |
| [Client authentication] | mTLS, bearer tokens, or Kubernetes ServiceAccount tokens |
| [Two-hop deployment] | keep registry credentials in one shared deployment instead of in every worker pod |
| [Deploying as a sidecar] | Kubernetes recipes for Buildbarn and BuildBuddy self-hosted executors |
| [Metrics] | instruments, attributes, and example queries |

Two things worth knowing before you start:

- The gateway **denies everything until a `--policy-file` allows it** (or you pass
  `--dangerously-allow-all`). Validate a policy in CI with `--validate-policy`.
- It is **unauthenticated to its clients** unless you configure client
  authentication, so keep it on a UNIX socket or `localhost`. It refuses to start
  on a network-reachable address without client authentication configured.

[gateway service README]: ../img_tool/cmd/oci-distribution-gateway/README.md
[Modes]: ../img_tool/cmd/oci-distribution-gateway/README.md#modes
[Flags]: ../img_tool/cmd/oci-distribution-gateway/README.md#flags
[Policy file]: ../img_tool/cmd/oci-distribution-gateway/README.md#policy-file
[Blob existence cache]: ../img_tool/cmd/oci-distribution-gateway/blob-existence-cache.md
[Client authentication]: ../img_tool/cmd/oci-distribution-gateway/README.md#client-authentication
[Two-hop deployment]: ../img_tool/cmd/oci-distribution-gateway/README.md#two-hop-deployment
[Deploying as a sidecar]: ../img_tool/cmd/oci-distribution-gateway/README.md#deploying-as-a-sidecar
[Metrics]: ../img_tool/cmd/oci-distribution-gateway/README.md#metrics

### 4. Inline Docker config from an injected environment variable

Set `IMG_DOCKER_CONFIG_INLINE` to the **JSON contents** of a Docker
`config.json` (the same format as `~/.docker/config.json` — *not* a path to a
file). rules_img resolves it exactly like the Docker config file, but entirely
in memory, so it works on an executor that has no config file on disk. It is
tried after the host-scoped environment variables and before the on-disk Docker
config, so a per-invocation host-scoped credential can override a broader stored
config.

> **Not recommended for general use.** The value *is* the credential, so
> anything that records your Bazel command line or an action's declared
> environment will capture it. Never set it inline on the command line
> (`IMG_DOCKER_CONFIG_INLINE=… bazel build …`), through `--action_env`, or via a
> Bazel setting — it would leak into the build event stream (BES), action
> metadata, logs, and the remote cache. Use it **only** with a mechanism that
> injects the variable straight into the (remote) action's process, where the
> value never appears in the Bazel invocation itself.

Use this format when an executor-side secret store provides a complete Docker
config or when its additional authentication fields are required. On BuildBuddy
Cloud-managed executors, prefer the shorter host-scoped variables described
below when username/password or a bearer token is sufficient.

For example, BuildBuddy organization secrets are encrypted, organization-scoped
environment variables. Save the `config.json` contents once as a secret named
`IMG_DOCKER_CONFIG_INLINE`, then opt the target whose build actions push or pull
in to it with the `env-secrets` execution property:

```python
exec_properties = {"env-secrets": "IMG_DOCKER_CONFIG_INLINE"},
```

The executor decrypts the secret and sets it in the action's environment at run
time; the value is never baked into the action by Bazel and — unlike a plain
environment variable — is not stored unencrypted in the action cache. See
BuildBuddy's [Secrets documentation](https://www.buildbuddy.io/docs/secrets) for
defining secrets and for the `env-secrets` / `include-secrets` execution
properties. Other secret-injection systems that set an action's environment on
the executor work the same way.

Because the credentials flow through the shared keychain, no rules_img setting
is involved: both lazy base-image pulls (`DownloadBlob`) and push-at-build-time
actions pick the variable up automatically wherever the executor sets it.

## BuildBuddy: inject short-lived credentials

For short-lived registry credentials on BuildBuddy Cloud-managed executors,
this is the preferred authentication method. Use BuildBuddy's [short-lived
secrets] to inject registry credentials at execution time instead of operating
a gateway. The secret values are redacted from BuildBuddy's action-cache entries
and workflow logs, and because they are supplied in a remote execution header
rather than the Bazel-declared action environment, they do not affect the action
key.

Set the registry host and exactly one authentication mode:

| Authentication | Environment variables |
|---|---|
| Username and password | `IMG_REGISTRY_AUTH_HOST`, `IMG_REGISTRY_AUTH_USERNAME`, `IMG_REGISTRY_AUTH_PASSWORD` |
| Ready-to-send bearer token | `IMG_REGISTRY_AUTH_HOST`, `IMG_REGISTRY_AUTH_BEARER_TOKEN` |

`IMG_REGISTRY_AUTH_BEARER_TOKEN` is only for a registry-issued bearer token that
can be sent directly in an `Authorization: Bearer` header. Registry access
tokens or personal access tokens that require a username are passwords in the
username/password mode. For the configured host, incomplete or mixed
authentication variables are an error rather than falling through to another
keychain.

For example, inject a short-lived username and access token:

```bash
bazel build //path/to:image \
  --remote_exec_header="x-buildbuddy-platform.secret-env-overrides=\
IMG_REGISTRY_AUTH_HOST=${REGISTRY_HOST},\
IMG_REGISTRY_AUTH_USERNAME=${REGISTRY_USER},\
IMG_REGISTRY_AUTH_PASSWORD=${REGISTRY_TOKEN}"
```

`IMG_REGISTRY_AUTH_HOST` is a registry host such as `ghcr.io` or
`registry.example.com:5000`, without a URL scheme or repository path. rules_img
normalizes the Docker Hub alias `docker.io` to `index.docker.io` and only offers
the credentials to the matching host.

The plain `secret-env-overrides` format separates entries with commas. If a
value contains a comma or other characters that make this format unsuitable,
base64-encode each complete `KEY=VALUE` entry and use BuildBuddy's
`secret-env-overrides-base64` header instead, as described in [short-lived
secrets].

> **Scope and caching:** `--remote_exec_header` applies to every remotely
> executed action in the Bazel invocation, even though rules_img only sends the
> registry credentials to the configured host. Use a narrowly scoped invocation
> when possible and do not print secrets or write them to action outputs. A cache
> hit skips execution, so rotating the credential alone does not rerun
> `DownloadBlob` or `PushImage`.

The values are redacted by BuildBuddy, but shell expansion can still expose them
locally through process arguments, shell tracing, or CI command logs. Disable
shell tracing and use the secret-masking facilities of the environment launching
Bazel. Base64 encoding makes complex values transport-safe; it does not encrypt
them.

This mechanism requires BuildBuddy remote execution. BuildBuddy remote caching
alone and local execution cannot inject these environment variables; other
remote execution services need an equivalent execution-time environment
injection mechanism.

[short-lived secrets]: https://www.buildbuddy.io/docs/secrets#short-lived-secrets
