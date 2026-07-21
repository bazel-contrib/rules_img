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
[Buildbarn](#setting-up-the-gateway-on-buildbarn) below.

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

1. A **Bazel credential helper**, when `IMG_CREDENTIAL_HELPER` is set (see
   [Credential Helpers](credential-helpers.md)).
2. The **Docker config** (honors `DOCKER_CONFIG`; `REGISTRY_AUTH_FILE` is used
   inside build actions).
3. **Google** — `google.Keychain` (Application Default Credentials / workload identity).
4. **Amazon ECR** — the ambient AWS configuration.

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
ever reach the real registry.

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

The gateway is read-only and denies everything by default, so configure what it
may do:

| Flag | Default | Purpose |
|---|---|---|
| `--allowed-registry <host>` | (required) | Exact upstream host to allow (repeatable) |
| `--allowed-registry-regex <re>` | (required) | Anchored regex of allowed hosts (repeatable) |
| `--allow-blob-read` | `true` | `GET`/`HEAD` on `/v2/.../blobs` (pull) |
| `--allow-blob-write` | `false` | Blob uploads (needed for push) |
| `--allow-manifest-read` | `true` | `GET`/`HEAD` on `/v2/.../manifests` |
| `--allow-manifest-write` | `false` | Manifest/tag writes (needed for a full push) |
| `--default-registry <host>` | — | Upstream to use when the request omits the host header |
| `--unix-socket <path>` | — | Listen on a UNIX socket (else `--address`/`--port`) |
| `--credential-helper <path>` | — | Bazel credential helper for upstream auth |

At least one `--allowed-registry` / `--allowed-registry-regex` is required. For
push at build time add `--allow-blob-write` (and `--allow-manifest-write` for
`blobs_and_manifests`). The upstream credentials live **on the gateway**, not in
the actions.

> **Security:** the gateway is unauthenticated to its clients (any process that
> can reach the socket/port may use it within the configured policy and
> allow-list). Keep it on `localhost` or a UNIX socket and treat the allow-list +
> `--allow-*` flags as the guardrails. It speaks plaintext HTTP/1.1.

## Setting up the gateway on Buildbarn

> This is one concrete deployment; adapt it to your setup and read the raw
> manifests before editing — see [buildbarn/bb-deployments].

In [bb-deployments] each worker runs `bb_worker` and `bb_runner` as two containers
in one Pod that share the build directory over an `emptyDir` volume (named
`worker`, mounted at `/worker` in both). `bb_worker` already talks to `bb_runner`
over a UNIX socket on that volume (`unix:///worker/runner`), which is exactly the
mechanism we reuse: run the gateway as a **sidecar container in the same Pod**,
listening on another socket on the shared volume.

1. Add the sidecar to the worker Pod (`kubernetes/worker-*.yaml`), mounting the
   existing `worker` volume and listening on a socket on it:

   ```yaml
   - name: oci-distribution-gateway
     image: <your oci-distribution-gateway image>
     args:
       - --unix-socket=/worker/oci-gateway.sock
       - --allowed-registry=ghcr.io
       - --allow-blob-write         # push
       - --allow-manifest-write     # only for blobs_and_manifests
     volumeMounts:
       - name: worker
         mountPath: /worker
   ```

   Reuse the existing `worker` volume rather than adding a new one. Consider a
   Kubernetes native sidecar (an `initContainer` with `restartPolicy: Always`) so
   the gateway is up before actions run.

2. Point Bazel at that socket. This is a **client-side** setting: rules_img bakes
   the value into each action's environment, so you configure it at the Bazel
   invocation, **not** in the `bb_worker`/`bb_runner` config:

   ```bash
   common --@rules_img//img/settings:registry_pull_gateway=unix:/worker/oci-gateway.sock
   common --@rules_img//img/settings:registry_push_gateway=unix:/worker/oci-gateway.sock
   ```

   The path here must be identical to the sidecar's `--unix-socket`.

3. Give the sidecar the upstream credentials it needs (a Docker config, a cloud
   keychain, or its own `--credential-helper`). The gateway restricts which
   registries and operations are allowed through the `--allowed-registry(-regex)`
   and `--allow-*` flags, and authenticates upstream using the **same mechanisms
   the `img` tool uses** (see [How rules_img resolves credentials](#how-rules_img-resolves-credentials)).
   The actions themselves stay credential-free.

### Caveats

- This only helps for actions that execute **on the worker** (remote execution).
  If an action runs locally, a `/worker/...` socket path won't exist.
- Sharing the `worker` volume does not by itself guarantee the socket is visible
  **inside the action's sandbox**: `bb_runner` may confine the action to its input
  root. Make sure the socket path resolves to something the action can
  `connect()` to, and that the socket (and every parent directory) is reachable by
  the action's user — the bb-deployments runner runs as uid `65534`, so the socket
  typically must be group/other-connectable and its parent dirs traversable.
  Verify against your `runner-*.jsonnet`.
- The gateway is not part of bb-deployments — build and ship its image yourself.
  Image tags/digests in bb-deployments drift over time.

[buildbarn/bb-deployments]: https://github.com/buildbarn/bb-deployments
[bb-deployments]: https://github.com/buildbarn/bb-deployments
