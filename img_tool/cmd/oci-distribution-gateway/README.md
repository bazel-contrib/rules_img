# oci-distribution-gateway

A container registry gateway that only forwards: clients speak the OCI
distribution protocol to it anonymously and name the upstream registry they want
in the `X-rules_img-Original-Host` header, and the gateway authenticates to that
registry itself and authorizes every request against a policy file. Build actions
therefore need no registry credentials of their own.

See [Authenticating Build Actions](../../../docs/authenticating-build-actions.md#3-oci-distribution-gateway)
for how build actions are pointed at a gateway. **This README documents running it
as a service**: its modes, flags, policy file, client authentication, deployment,
and metrics. One feature has a document of its own — the
[blob existence cache](blob-existence-cache.md), which is also where replicating it
between the instances of a serving deployment is described.

## Container image

A pre-built multi-platform (linux/amd64, linux/arm64) image is published to
GitHub Container Registry on every commit to main and on every version tag:

```
ghcr.io/bazel-contrib/rules_img/oci-distribution-gateway:latest
ghcr.io/bazel-contrib/rules_img/oci-distribution-gateway:<version>
```

## Modes

```bash
oci-distribution-gateway serve   [OPTIONS]   # talk to registries (the default)
oci-distribution-gateway forward [OPTIONS]   # relay to another gateway
oci-distribution-gateway         [OPTIONS]   # no verb: serving mode, unchanged
```

`serve` is the mode described above. `forward` holds no registry credentials and
no policy at all: it relays the very same protocol to a serving gateway named by
`--peer`, over one multiplexed HTTP/2 connection, adding only its own peer
credential. That is how a build farm keeps its registry credentials in one shared
deployment instead of in every worker pod — see
[Two-hop deployment](#two-hop-deployment).

A bare invocation with no verb means serving mode, so deployments that predate the
subcommands keep working unchanged.

Both modes answer `GET /healthz` with `200 ok` **before** any authentication, so a
Kubernetes readiness probe works against a listener that requires a credential. It
is deliberately unauthenticated and reveals nothing else. A serving gateway that is
[seeding its cache from a peer](blob-existence-cache.md#warming-up-a-new-replica) answers `503 warming up`
until it is done, or until its warm-up budget runs out.

## Flags

### `serve`

| Flag | Default | Purpose |
|---|---|---|
| `--policy-file <path>` | (required) | JSON/YAML policy of per-repository allow/deny rules (see [Policy file](#policy-file)) |
| `--dangerously-allow-all` | `false` | Allow every request to every upstream, ignoring the policy. **Dangerous**; only for trusted, isolated environments |
| `--validate-policy` | `false` | Load and validate `--policy-file`, then exit without serving |
| `--default-registry <host>` | — | Upstream to use when the request omits the host header (still policy-checked) |
| `--credential-helper <path>` | — | Bazel credential helper for upstream auth |
| `--deny-private-upstreams` | `false` | Refuse upstreams that resolve to a loopback, link-local, or private address (see [Restricting which upstreams are reachable](#restricting-which-upstreams-are-reachable)) |
| `--blob-existence-cache-ttl <dur>` | `6h` | How long a blob the registry confirmed is assumed to still be there (see [Blob existence cache](blob-existence-cache.md)). `0` disables the cache |
| `--blob-existence-cache-max-memory <size>` | `64MiB` | Memory that cache may use, preallocated at startup, e.g. `256MiB`. `0` disables the cache |
| `--blob-existence-cache-peer <url>` | — | Another instance to replicate blob existence facts to, as `https://host:port`. Repeatable (see [Replicating the cache between instances](blob-existence-cache.md#replicating-the-cache-between-instances)) |
| `--blob-existence-cache-peer-service <s>` | — | Discover the peers from the Kubernetes EndpointSlices of this Service, as `[<namespace>/]<name>`. Follows scaling with no restart |
| `--blob-existence-cache-peer-server-name <n>` | — | Certificate name to verify in a peer. Needed with `--blob-existence-cache-peer-service`, which dials pod IPs |
| `--blob-existence-cache-peer-token-file <p>` | — | Bearer token presented to peers. Not needed when they accept this gateway's own certificate |
| `--allowed-cache-peer-id <id>` | any authenticated client | Identity permitted to write to this gateway's cache: a SPIFFE ID, a DNS name, or a `system:serviceaccount:<ns>:<name>`. Repeatable. **Set this** |
| `--blob-existence-cache-replication-batch-size <n>` | `256` | Facts per replication message |
| `--blob-existence-cache-warmup-timeout <dur>` | `10s` | How long a starting instance seeds its cache from a peer before reporting itself healthy. `0` serves at once |
| `--blob-existence-cache-warmup-entries <n>` | `20000` | How many of a peer's hottest entries it asks for. `0` disables seeding |
| `--dangerously-allow-plaintext-cache-peer` | `false` | Replicate over plaintext HTTP |
| `--tls-cert-file`, `--tls-key-file` | — | Serve TLS, which also enables HTTP/2 via ALPN. Hot-reloaded |
| `--client-ca-file <path>` | — | CAs whose client certificates are accepted (enables mTLS). Requires `--tls-cert-file`. Hot-reloaded |
| `--allowed-client-id <id>` | any cert from the CA | Permitted client identity: a SPIFFE ID or a DNS name (a single leading `*.` wildcard allowed). Repeatable |
| `--client-token-file <path>` | — | File of accepted bearer tokens, one per line. Repeatable. Hot-reloaded |
| `--client-serviceaccount-audience <s>` | — | Accept Kubernetes projected ServiceAccount tokens for this audience, validated with TokenReview |
| `--allowed-serviceaccount <s>` | any for the audience | `system:serviceaccount:<ns>:<name>`. Repeatable |
| `--dangerously-allow-plaintext-h2c` | `false` | Accept prior-knowledge h2c on the plaintext listener |
| `--dangerously-allow-unauthenticated-clients` | `false` | Serve a network-reachable address with no client authentication |
| `--peer-address <host>`, `--peer-port <n>` | `--address`, — | A [second listener](#a-second-listener-for-peers) carrying only cache replication between instances, so peers are authenticated differently from clients. `--peer-port` enables it |
| `--peer-tls-cert-file`, `--peer-tls-key-file` | `--tls-cert-file`/`--tls-key-file` | TLS keypair of the peer listener. Hot-reloaded |
| `--peer-client-ca-file <path>` | `--client-ca-file` | CAs whose client certificates the peer listener accepts. This is what lets instances speak mTLS while clients do not. Hot-reloaded |
| `--dangerously-allow-unauthenticated-peer-listener` | `false` | Run the peer listener with no client authentication |

### `forward`

Notably absent: `--policy-file`, `--default-registry` and `--credential-helper`. A
forwarder authorizes nothing and holds no registry credentials — it is a pure
pass-through, so a sidecar can never refuse something the (possibly newer) serving
gateway would have allowed.

| Flag | Default | Purpose |
|---|---|---|
| `--peer <url>` | (required) | `https://host[:port]` (HTTP/2 over TLS) or `http://host[:port]` (plaintext h2c) |
| `--peer-ca-file <path>` | system roots | CAs used to verify the peer's certificate |
| `--peer-server-name <name>` | — | Certificate name to verify, when dialing a pod IP directly |
| `--peer-cert-file`, `--peer-key-file` | — | Client certificate for mTLS to the peer. Hot-reloaded; pooled connections are recycled so a rotated certificate takes effect |
| `--peer-token-file <path>` | — | Bearer token presented to the peer. Re-read per request (10 s cache) so a projected ServiceAccount token keeps working |
| `--forwarder-id <s>` | hostname | Identifies this forwarder in the peer's decision log |
| `--dangerously-allow-plaintext-peer` | `false` | Permit an `http://` peer |
| `--dangerously-allow-anonymous-peer` | `false` | Relay with no credential of our own (a service mesh authenticates the hop) |
| `--dangerously-skip-peer-verification` | `false` | Do not verify the peer's certificate |

### Both modes

| Flag | Default | Purpose |
|---|---|---|
| `--unix-socket <path>` | — | Listen on a UNIX socket (else `--address`/`--port`) |
| `--unix-socket-mode <octal>` | (as created) | `chmod` the socket after binding, e.g. `0660` |
| `--address <host>`, `--port <n>` | `localhost`, `0` | TCP alternative to `--unix-socket` |
| `--shutdown-timeout <dur>` | `30s` | How long in-flight transfers may take to finish after a shutdown signal |
| `--metrics-*` | off | See [Metrics](#metrics) |

Reloadable material — the policy file, TLS keypairs, CA bundles and token files —
is re-read when it changes on disk and on `SIGHUP`. A failed reload keeps what is
already in force, so a bad edit never widens access or takes the gateway down.

> **Security:** by default the gateway is unauthenticated to its clients, so any
> process that can reach the socket or port may use it within the configured
> policy. Keep it on a UNIX socket or `localhost` and treat the policy file as the
> guardrail, or configure [client authentication](#client-authentication) and serve
> TLS. The gateway **refuses to start** on a network-reachable address with no
> client authentication configured, unless you pass
> `--dangerously-allow-unauthenticated-clients`.
>
> A UNIX socket is **not** an authentication boundary either: it is created with
> the process umask, and the sidecar recipes below deliberately need it connectable
> by the action's user. Anything that can `connect()` to it can use the gateway
> within the policy.

## Policy file

The policy file lets you set different read/write rules per repository path and
scope access to parts of a registry (allow `docker.acme.corp/team-a/**` while
forbidding `docker.acme.corp/secret`).
It is a JSON or YAML file following a simple schema.

```json
{
  "version": 1,
  "defaultAction": "deny",
  "rules": [
    {
      "description": "explicitly forbid the secret repo (before any broader allow)",
      "action": "deny",
      "registry": "docker.acme.corp",
      "repository": "secret",
      "operations": ["blob:read", "blob:write", "manifest:read", "manifest:write"]
    },
    {
      "description": "docker.acme.corp/team-a/** is fully writable (push)",
      "action": "allow",
      "registry": "docker.acme.corp",
      "repository": "team-a/**",
      "operations": ["blob:read", "blob:write", "manifest:read", "manifest:write"]
    },
    {
      "description": "everything else under docker.acme.corp is read-only (pull)",
      "action": "allow",
      "registry": "docker.acme.corp",
      "repository": "**",
      "operations": ["blob:read", "manifest:read"]
    },
    {
      "description": "Docker Hub official images: pull only",
      "action": "allow",
      "registry": "index.docker.io",
      "repository": "library/**",
      "operations": ["blob:read", "manifest:read"]
    }
  ]
}
```

**Evaluation.** Rules are evaluated top-to-bottom and the **first match wins**;
a request that matches no rule falls back to `defaultAction` (default `deny`, so
the gateway fails closed). Because the first match wins, put narrow `deny` rules
*before* broader `allow` rules — otherwise the broad allow shadows them. The
winning rule is recorded in the gateway's decision log.

**Matching** is against the *resolved* upstream, not the raw request:

- `registry` matches the resolved host: an exact host (`docker.acme.corp`,
  `index.docker.io`), a single leading `*.` wildcard matching one or more leading
  labels (`*.docker.io` matches `index.docker.io` and `a.b.docker.io` but **not**
  bare `docker.io`), or `*` for any host. `docker.io` resolves to
  `index.docker.io`, and a bare host like `myregistry` resolves to Docker Hub, so
  write the resolved host.
- `repository` is a glob over the resolved repository path: `*` matches within one
  path segment, `**` matches across segments (including zero, so `team-a/**` also
  matches exactly `team-a`), and `?` matches a single non-`/` character. On Docker
  Hub a single-segment name is normalized with the `library/` prefix
  (`docker.io/ubuntu` → `library/ubuntu`), so match it as `library/**`.
- `operations` lists the operations the rule speaks to; each is one of
  `blob:read`, `blob:write`, `manifest:read`, `manifest:write`, or `*` for all
  four. Tag listings and referrers count as `manifest:read`. A `HEAD` is allowed
  when either the read or the write of that kind is permitted. A cross-repo blob
  mount additionally requires `blob:read` on the source repository.

A malformed or unreadable file — at startup or on reload — is a hard error:
the gateway refuses to start, or (on reload) keeps the previous policy. Validate
a file in CI without starting the gateway with `--validate-policy --policy-file
<path>`.

**Reloading.** Send the gateway process a `SIGHUP` to re-read the policy file
without restarting or dropping connections. A reload that fails to parse or
validate is logged and the previous policy is kept.

### Restricting which upstreams are reachable

The upstream registry is named by the client in the `X-rules_img-Original-Host`
header, and go-containerregistry resolves a private, `.local` or loopback host to a
**plaintext** endpoint. So a policy whose `registry` pattern is `*` — or
`--dangerously-allow-all` — lets a client use the gateway to reach any address the
*gateway* can reach, which is not the same set the client can reach. The gateway
warns about such a policy at startup.

`--deny-private-upstreams` refuses those addresses at dial time, against the
address actually resolved, so DNS names and registry redirects are covered by the
same check. It is off by default because a legitimate in-cluster registry behind a
ClusterIP *is* a private address. For a gateway shared between workloads, turn it
on and add an egress NetworkPolicy naming your registries.

## Blob existence cache

A serving gateway memoizes one fact — **this blob is in this repository**, which is
the answer `200` to `HEAD /v2/<name>/blobs/<digest>` — so that a build farm's "is
this layer already pushed?" probes cost one upstream round trip instead of
thousands. It is on by default, remembering a blob for six hours within 64 MiB of
preallocated memory, and the replicas of a serving deployment can replicate what
they learn to each other so that a fleet pays for one probe per blob rather than one
per replica.

It has a document of its own: **[Blob existence cache](blob-existence-cache.md)** —
what fills and empties it, how to size the TTL and the memory, and how to set up
replication between replicas, including peer discovery, the warm-up of a joining
replica, and who is allowed to write to a gateway's cache.

## Client authentication

Needed whenever the gateway listens on an address other processes can reach — most
importantly for the [two-hop topology](#two-hop-deployment), where a shared
credential-holding deployment is reachable from every namespace in a cluster.

**Any one configured method authenticates a request** (they are OR'd), so you can
migrate between them with no downtime. Pick from:

- **mTLS** — the strongest option and nearly free once you serve TLS. Set
  `--client-ca-file` and, in practice always, `--allowed-client-id`: without an
  allow-list, *any* workload holding a certificate from that CA can use the
  gateway, which with a shared cluster CA is effectively cluster-wide access. Use a
  dedicated issuer. cert-manager with DNS SANs is the documented default; SPIFFE
  URI SANs work too (materialise the SVID to files with `spiffe-helper` or the
  SPIFFE CSI driver).
- **A shared bearer token** — simplest, and the right choice outside Kubernetes.
  Several tokens are valid at once, which is the rotation story: publish the new
  one, roll the clients, drop the old one. It carries no identity, so a single
  compromise means rotating for everyone.
- **A projected Kubernetes ServiceAccount token** — the only option with real
  **revocation**: the API server rejects a token whose pod or ServiceAccount is
  gone, so deleting a compromised worker pod invalidates its credential
  immediately. It also needs no PKI. Project a token with an audience **dedicated
  to the gateway** and bind `system:auth-delegator` to the gateway's
  ServiceAccount.

> **Never reuse the default ServiceAccount token** at
> `/var/run/secrets/kubernetes.io/serviceaccount/token`: it is issued for the API
> server's audience, every pod in the cluster has one, and a gateway that accepted
> it would authenticate the whole cluster. The gateway rejects any token whose
> TokenReview comes back without your audience, which is exactly this case.

Client certificate identities are re-checked on **every request**, not just per
connection, so removing one from `--allowed-client-id` takes effect at once even on
an established connection. There is no CRL or OCSP, so that allow-list *is* the
revocation mechanism for certificates; use short-lived leaves.

### A second listener for peers

A listener either asks its clients for a certificate or it does not. So a
deployment that wants **anonymous plaintext for its build clients and mTLS between
its own instances** cannot express that on one socket. `--peer-port` gives the
instance-to-instance traffic — the [blob existence cache
replication](blob-existence-cache.md#replicating-the-cache-between-instances) —
a listener of its own, with its own TLS material and its own client
authentication:

```bash
oci-distribution-gateway serve --policy-file /etc/img/policy.yaml \
  --address 0.0.0.0 --port 8080 \
  --dangerously-allow-unauthenticated-clients \
  --peer-port 8443 \
  --peer-tls-cert-file /tls/tls.crt --peer-tls-key-file /tls/tls.key \
  --peer-client-ca-file /tls/ca.crt \
  --allowed-cache-peer-id oci-distribution-gateway.img-gateway.svc \
  --blob-existence-cache-peer-service oci-distribution-gateway
```

Enabling it **moves** the endpoints rather than duplicating them: from then on the
client listener answers `/_rules_img/cache/` with `404`, and the peer listener is
the only place they exist. That is the point of the split, not a side effect —
inserting a cache entry is a write into this instance's view of what is upstream,
and an anonymous client that could claim a blob exists would make push clients skip
an upload they still owe.

The peer listener is a closed surface: the two replication endpoints and
`/healthz`, and `404` for everything else. None of the registry protocol is
reachable there, so a peer credential cannot be spent on an upstream registry.
Probe it on its own port — `/healthz` answers before authentication there too, and
each listener reports the readiness of its own socket.

What it inherits, so the common cases stay short:

| Flag | Falls back to |
|---|---|
| `--peer-address` | `--address` |
| `--peer-tls-cert-file` / `--peer-tls-key-file` | `--tls-cert-file` / `--tls-key-file` |
| `--peer-client-ca-file` | `--client-ca-file` |
| the peer identity allow-list | `--allowed-cache-peer-id` (the peers are this listener's only clients, so who may connect and who may write to the cache are the same question) |

`--peer-port` is never inherited and never defaulted: it is the port a
**discovered** peer is dialled on, so every instance of a deployment has to serve
it on the same one. Replication follows the peer listener in every other respect
too — its TLS decides whether the hop is `https`, so a gateway that is plaintext to
its clients and TLS to its peers replicates over TLS, with no
`--dangerously-allow-plaintext-cache-peer` needed.

With this in play the Service needs both ports, and the peer one should be
reachable only from the gateway's own namespace:

```yaml
ports:
  - {name: gateway, port: 8080, targetPort: gateway}
  - {name: peer,    port: 8443, targetPort: peer}
```

## Two-hop deployment

A gateway per worker pod means the upstream registry credentials in every worker
pod. To keep them in **one** shared, auditable deployment instead, run the gateway
in both modes:

```text
build action (img)
  ── unix socket ──────────────▶ oci-distribution-gateway forward   (sidecar per worker pod, NO credentials)
  ── HTTP/2, authenticated ────▶ oci-distribution-gateway serve     (shared Deployment + Service, credentials + policy)
  ── authenticated HTTPS ──────▶ real registry
```

The second hop carries **exactly the same protocol** as the first, over one
multiplexed HTTP/2 connection: many concurrent blob transfers share a single TCP
connection, and the serving gateway needs no configuration beyond client
authentication. Nothing changes on the Bazel side — build actions still point at a
UNIX socket, exactly as in the single-hop setup.

### Trust model

- **The serving gateway is the security boundary.** It holds the credentials and
  the only authoritative policy.
- A build action **shares its worker pod's network namespace**, so it can dial the
  serving Service directly, and a NetworkPolicy cannot separate the two (same pod
  IP). The only boundary is that the peer credential is mounted into the **sidecar
  container alone**. So: mount it only there, with `defaultMode: 0400`, never onto
  the volume the action can read, and never through an environment variable. Leave
  `shareProcessNamespace` at its `false` default.
- **One serving Deployment per trust domain.** The policy is matched on registry,
  repository and operation — not on who is asking — so every client of a given
  deployment shares its entire grant. Two worker pools that must not read each
  other's blobs need two deployments, with their own credentials, policy and
  accepted identities.
- Ship **NetworkPolicies** with the deployment: ingress from the worker namespace
  only, egress to your registries only. The serving gateway is now the
  credential-holding blast-radius centre.

### Operational notes

- **Metrics stay off in sidecars** by default, and should: thousands of worker pods
  each pushing OTLP every 60 s is a real cost for data the serving tier already
  reports. The two modes report **disjoint** instrument sets so nothing is counted
  twice; see [Two hops](#two-hops) for what a forwarder reports and how to verify
  the hop from it.
- **Rolling updates are safe.** Shutdown sends a graceful HTTP/2 GOAWAY and the
  connection stays open until in-flight streams finish, so multi-gigabyte transfers
  survive and the sidecar re-dials. Set `--shutdown-timeout` above your longest
  blob transfer, give the pod a larger `terminationGracePeriodSeconds`, and add a
  `preStop` sleep so the Service drops the endpoint before shutdown begins. A
  request dispatched in the moment a GOAWAY arrives can still fail with a 502; the
  `img` client retries it.
- **Load balancing** is an ordinary ClusterIP Service. Each sidecar dials
  independently, so a fleet of them spreads evenly across replicas by itself. What
  a single sidecar does *not* do is rebalance while it stays busy: its connection
  is dropped after 90 s idle (and by the server's 5-minute idle timeout), which
  bursty build traffic hits constantly. Watch for imbalance with the PromQL under
  [Example queries](#example-queries).
- **The hop is designed for in-cluster round-trip times.** HTTP/2 caps a
  connection's flow-control window just below 4 MiB, so aggregate upload
  throughput per connection is about `window / RTT`: ample at 0.2 ms, and roughly
  80 MB/s at a cross-region 50 ms.
- **Kubernetes credential rotation gotchas:** a Secret mounted with `subPath`
  **never** updates, an `immutable: true` Secret never changes, and a projected
  ServiceAccount token must be re-read (kubelet rotates it at 80 % of a lifetime
  whose floor is 10 minutes) — which the forwarder does.
- A failed reload keeps the previous material and increments
  `oci_gateway_material_reloads_total{oci_result="failure"}` — alert on that, since
  serving a stale certificate is otherwise invisible until it expires.

### Sizing a shared serving deployment

The HTTP/2 flow control windows a serving gateway advertises are credits rather
than preallocations, but a stalled upstream registry realises them, so budget
worst-case receive buffers as
`MaxConcurrentStreams (64) × per-stream window (2 MiB) × connections`, i.e. about
128 MiB per fully-loaded peer connection. Set the pod's memory request and limit
with that in mind.

Add `--blob-existence-cache-max-memory` (64 MiB by default) on top: unlike the flow
control windows, that one is allocated up front and held for the life of the
process, so it is a flat addition to the request rather than a worst case.

### A serving deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: oci-distribution-gateway
  namespace: img-gateway
spec:
  replicas: 3
  template:
    spec:
      serviceAccountName: oci-distribution-gateway
      terminationGracePeriodSeconds: 1800   # > --shutdown-timeout
      containers:
        - name: gateway
          image: ghcr.io/bazel-contrib/rules_img/oci-distribution-gateway:latest
          args:
            - serve
            - --address=0.0.0.0
            - --port=8443
            - --policy-file=/etc/img/policy.yaml
            - --tls-cert-file=/tls/tls.crt
            - --tls-key-file=/tls/tls.key
            - --client-ca-file=/tls/ca.crt
            - --allowed-client-id=spiffe://cluster.local/ns/buildbarn/sa/worker
            # Share the blob existence cache with the other replicas, so a
            # first-seen blob costs the fleet one upstream probe rather than three.
            - --blob-existence-cache-peer-service=oci-distribution-gateway
            - --blob-existence-cache-peer-server-name=oci-distribution-gateway.img-gateway.svc
            - --allowed-cache-peer-id=spiffe://cluster.local/ns/img-gateway/sa/oci-distribution-gateway
            - --shutdown-timeout=25m
            - --deny-private-upstreams
            - --metrics-exporter=prometheus
          ports:
            - {name: gateway, containerPort: 8443}
            - {name: metrics, containerPort: 9464}
          readinessProbe:
            # 503 while it seeds its cache from a peer, so it joins the Service warm.
            httpGet: {path: /healthz, port: gateway, scheme: HTTPS}
          lifecycle:
            preStop:
              exec: {command: ["sleep", "10"]}   # let the Service drop us first
          volumeMounts:
            - {name: tls, mountPath: /tls, readOnly: true}
            - {name: policy, mountPath: /etc/img, readOnly: true}
      volumes:
        # cert-manager writes tls.crt, tls.key and ca.crt here and rotates them.
        - {name: tls, secret: {secretName: oci-gateway-tls, defaultMode: 0400}}
        - {name: policy, configMap: {name: rules-img-gateway-policy}}
---
apiVersion: v1
kind: Service
metadata:
  name: oci-distribution-gateway
  namespace: img-gateway
spec:
  selector: {app: oci-distribution-gateway}
  ports:
    - {name: gateway, port: 8443, targetPort: gateway}
```

Give the gateway's ServiceAccount the upstream registry credentials it needs, the
same way a single-hop gateway gets them. If you use the ServiceAccount-token
method, also bind the built-in delegator role so it may call TokenReview:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: oci-gateway-auth-delegator}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: system:auth-delegator}
subjects:
  - {kind: ServiceAccount, name: oci-distribution-gateway, namespace: img-gateway}
```

The `--blob-existence-cache-peer-service` flag above needs its own (namespaced)
Role for EndpointSlices — see [Discovering peers in
Kubernetes](blob-existence-cache.md#discovering-peers-in-kubernetes).

The matching sidecar is in [Buildbarn](#buildbarn) or
[BuildBuddy self-hosted executors](#buildbuddy-self-hosted-executors) below.

## Deploying as a sidecar

Under remote execution the gateway runs alongside the worker, listening on a UNIX
socket the build actions can reach. rules_img does not currently publish a gateway
container image: build and publish one containing `oci-distribution-gateway`, then
replace the image placeholders below, preferably with an immutable digest.

### Buildbarn

> This is one concrete deployment; adapt it to your setup and read the raw
> manifests before editing — see [buildbarn/bb-deployments].

In [bb-deployments] each worker runs `bb_worker` and `bb_runner` as two containers
in one Pod that share the build directory over an `emptyDir` volume (named
`worker`, mounted at `/worker` in both). `bb_worker` already talks to `bb_runner`
over a UNIX socket on that volume (`unix:///worker/runner`), which is exactly the
mechanism we reuse: run the gateway as a **sidecar container in the same Pod**,
listening on another socket on the shared volume.

1. Add the sidecar to the worker Pod (`kubernetes/worker-*.yaml`), mounting the
   existing `worker` volume and listening on a socket on it. Mount the policy
   file too (for example from a `ConfigMap`):

   ```yaml
   - name: oci-distribution-gateway
     image: ghcr.io/bazel-contrib/rules_img/oci-distribution-gateway:latest
     args:
       - --unix-socket=/worker/oci-gateway.sock
       - --policy-file=/etc/img/gateway-policy.json
     volumeMounts:
       - name: worker
         mountPath: /worker
       - name: gateway-policy
         mountPath: /etc/img
   ```

   This gives every worker Pod its own gateway, and therefore its own copy of the
   upstream registry credentials. To keep them in one place instead, run the
   sidecar in **forwarding mode** and point it at a shared serving deployment (see
   [Two-hop deployment](#two-hop-deployment)). This sidecar has no policy and no
   registry credentials at all:

   ```yaml
   - name: oci-distribution-gateway
     image: ghcr.io/bazel-contrib/rules_img/oci-distribution-gateway:latest
     args:
       - forward
       - --unix-socket=/worker/oci-gateway.sock
       - --peer=https://oci-distribution-gateway.img-gateway.svc:8443
       - --peer-ca-file=/tls/ca.crt
       - --peer-cert-file=/tls/tls.crt
       - --peer-key-file=/tls/tls.key
     volumeMounts:
       - name: worker
         mountPath: /worker
       # Mounted into THIS container only: a build action shares the Pod's network
       # namespace and could otherwise use the credential directly.
       - name: gateway-peer-tls
         mountPath: /tls
         readOnly: true
   ```

   ```yaml
   volumes:
     - name: gateway-peer-tls
       secret:
         secretName: oci-gateway-client-tls   # written and rotated by cert-manager
         defaultMode: 0400
   ```

   Reuse the existing `worker` volume rather than adding a new one. Consider a
   Kubernetes native sidecar (an `initContainer` with `restartPolicy: Always`) so
   the gateway is up before actions run. Editing the mounted policy and sending
   the gateway a `SIGHUP` reloads it without a restart.

2. Point Bazel at that socket. This is a **client-side** setting: rules_img bakes
   the value into each action's environment, so you configure it at the Bazel
   invocation, **not** in the `bb_worker`/`bb_runner` config:

   ```bash
   common --@rules_img//img/settings:registry_pull_gateway=unix:/worker/oci-gateway.sock
   common --@rules_img//img/settings:registry_push_gateway=unix:/worker/oci-gateway.sock
   ```

   The path here must be identical to the sidecar's `--unix-socket`.

3. Give the sidecar (or, in the two-hop setup, the serving deployment) the upstream
   credentials it needs: a Docker config, a cloud keychain, or its own
   `--credential-helper`. The gateway restricts which registries and operations are
   allowed through its `--policy-file`, and authenticates upstream using the **same
   mechanisms the `img` tool uses** (see
   [How rules_img resolves credentials](../../../docs/authenticating-build-actions.md#how-rules_img-resolves-credentials)).
   The actions themselves stay credential-free.

#### Caveats

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
- The socket has to be connectable by the action's user, which means it is
  connectable by **anything** in the Pod that can reach the path. Hop 1 is
  unauthenticated by design. With a forwarding sidecar that is the reason its peer
  credential must live only in the sidecar container's filesystem, never on the
  shared `worker` volume.

### BuildBuddy self-hosted executors

Run one gateway as a **sidecar container in each BuildBuddy executor Pod**. The
gateway is not a separate Pod or DaemonSet in this setup: the
`extraContainers` value adds it to the executor Pod template, so each executor
replica gets its own gateway.

```text
BuildBuddy executor Pod
├── oci-distribution-gateway sidecar
└── buildbuddy-executor
    └── child OCI action container
```

The sidecar listens on a UNIX socket in a private `emptyDir`. The
[BuildBuddy executor Helm chart] shares that socket with the executor and, with
the chart's default OCI isolation, the executor bind-mounts it into child action
containers. This requires configuration at three layers:

1. An `emptyDir` shared by the gateway sidecar and executor Pod containers.
2. An `extraVolumeMount` that makes the directory visible to the executor.
3. An `executor.oci.mounts` entry that bind-mounts the directory into every
   action container.

First create a `ConfigMap` containing the gateway policy in the executor's
Kubernetes namespace:

```bash
kubectl --namespace=buildbuddy create configmap rules-img-gateway-policy \
  --from-file=policy.yaml=/path/to/gateway-policy.yaml
```

Then add these values to the `buildbuddy-executor` chart:

```yaml
extraVolumes:
  - name: rules-img-gateway-socket
    emptyDir: {}
  - name: rules-img-gateway-policy
    configMap:
      name: rules-img-gateway-policy
extraVolumeMounts:
  - name: rules-img-gateway-socket
    mountPath: /run/rules-img-gateway
extraContainers:
  - name: oci-distribution-gateway
    image: ghcr.io/bazel-contrib/rules_img/oci-distribution-gateway:latest
    args:
      - --unix-socket=/run/rules-img-gateway/gateway.sock
      - --policy-file=/etc/rules-img-gateway/policy.yaml
    volumeMounts:
      - name: rules-img-gateway-socket
        mountPath: /run/rules-img-gateway
      - name: rules-img-gateway-policy
        mountPath: /etc/rules-img-gateway
        readOnly: true
config:
  executor:
    oci:
      mounts:
        - type: bind
          source: /run/rules-img-gateway
          destination: /run/rules-img-gateway
          options:
            - bind
            - ro
```

Point Bazel at the same socket path:

```bash
common --@rules_img//img/settings:registry_gateway=unix:/run/rules-img-gateway/gateway.sock
```

Give only the gateway sidecar the upstream credentials it needs, using its
environment or additional secret volume mounts. The actions connect to the
gateway anonymously and do not need registry credentials.

To keep those credentials out of every executor Pod, replace the `args` above with
a forwarding sidecar and run one shared serving deployment instead (see
[Two-hop deployment](#two-hop-deployment)):

```yaml
    args:
      - forward
      - --unix-socket=/run/rules-img-gateway/gateway.sock
      - --peer=https://oci-distribution-gateway.img-gateway.svc:8443
      - --peer-token-file=/var/run/rules-img-gateway/token
```

Mount that token into the gateway container only, not into the shared
`emptyDir`: the executor bind-mounts that directory into every action container.

This example assumes the chart's default OCI isolation. Other isolation types
need their own mechanism for exposing the socket inside action containers.
`executor.oci.mounts` applies to every OCI action on the executor, so use a
restrictive gateway policy and consider a dedicated executor pool. Also ensure
the socket permissions allow the action user to connect. `extraContainers`
start concurrently with the executor; if startup ordering is important, use a
Kubernetes restartable init sidecar with a startup probe through the chart's
`extraInitContainers` value. A separate Deployment or DaemonSet would require
different network or volume plumbing and would expose the unauthenticated
gateway beyond this private per-Pod socket.

## Metrics

The gateway reports traffic, blob transfers, and errors as
[OpenTelemetry](https://opentelemetry.io) metrics. Metrics are **off until an
exporter is configured**, and configuration follows the standard OpenTelemetry
environment variables, so the tooling a cluster already runs works unchanged:

```bash
# Push to an OpenTelemetry collector (what the OpenTelemetry Operator injects).
# With OTEL_EXPORTER_OTLP_ENDPOINT set, OTLP is enabled automatically.
export OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318
oci-distribution-gateway serve --policy-file /etc/img/policy.json --port 8080

# Or expose a scrape endpoint for Prometheus on :9464/metrics.
oci-distribution-gateway serve --policy-file /etc/img/policy.json --port 8080 \
  --metrics-exporter prometheus

# Or print them to stderr while debugging.
oci-distribution-gateway serve --policy-file /etc/img/policy.json --port 8080 \
  --metrics-exporter console
```

| Flag | Default | Purpose |
|---|---|---|
| `--metrics-exporter <list>` | — | Exporters to enable, comma-separated: `otlp`, `prometheus`, `console`, `none`. Overrides `OTEL_METRICS_EXPORTER`; defaults to `otlp` when an OTLP endpoint is configured, else off |
| `--metrics-otlp-protocol <p>` | `http/protobuf` | `grpc` (collector port 4317) or `http/protobuf` (port 4318). Defaults to `OTEL_EXPORTER_OTLP_METRICS_PROTOCOL`, then `OTEL_EXPORTER_OTLP_PROTOCOL` |
| `--metrics-otlp-endpoint <url>` | — | OTLP endpoint URL to push to; `http://` is plaintext, `https://` uses TLS, and a path is used as given. **Repeatable** — see [Pushing to several collectors](#pushing-to-several-collectors). Defaults to `IMG_METRICS_OTLP_ENDPOINTS`, then to `OTEL_EXPORTER_OTLP_[METRICS_]ENDPOINT` |
| `--metrics-address <addr>` | `:9464` | Where the `prometheus` exporter serves `/metrics` |

`OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_METRIC_EXPORT_INTERVAL`,
and the remaining `OTEL_EXPORTER_OTLP_*` variables (headers, TLS material,
compression, timeouts) behave as specified. `service.name` defaults to
`oci-distribution-gateway` when serving and `oci-distribution-gateway-forward`
when forwarding, and `service.instance.id` to the hostname — the pod name in
Kubernetes — so **several gateway replicas keep their series apart**, and a
sidecar fleet stays distinguishable from the deployment it forwards to.

> **Security:** `--metrics-address` binds all interfaces by default so a
> Kubernetes scraper can reach it. The endpoint exposes upstream registry
> hostnames and traffic volumes (never credentials or repository names); keep it
> on a trusted network.

### Pushing to several collectors

`--metrics-otlp-endpoint` may be repeated. Each endpoint gets its own exporter
and its own periodic reader on the one meter provider, and the same metrics are
pushed to all of them:

```bash
oci-distribution-gateway serve --policy-file /etc/img/policy.json --port 8080 \
  --metrics-otlp-protocol grpc \
  --metrics-otlp-endpoint http://collector-0.collectors:4317 \
  --metrics-otlp-endpoint http://collector-1.collectors:4317
```

This exists for a collector deployed as a set of replicas where only one is
active at a time — a leader-elected pair, with the follower accepting and
discarding what it receives. A client that resolves such a service to a single
address has a 1-in-N chance of talking to the follower, in which case its metrics
are silently dropped until the next election; pushing to every replica is the fix.

> **Only point this at collectors of which at most one forwards the data.** If
> every endpoint forwards to the same backend, the backend sees each measurement
> N times and every counter is multiplied by N — unless it deduplicates. Behind a
> load balancer, use one endpoint. Note the cost, too: N endpoints means N times
> the serialization and export work per interval.

Repeated identical endpoints collapse, so a duplicated argument does not double
the work, and the endpoints are independent: one unreachable collector does not
stop the others from exporting, and its failures are logged through the usual
error handler. `IMG_METRICS_OTLP_ENDPOINTS` takes the same list, comma-separated,
for deployments that template environment variables rather than arguments; the
flag wins when both are set. This is intentionally not spelled `OTEL_*`: the
specification defines its endpoint variables as single-valued.

An endpoint that is not an absolute `http`/`https` URL is a startup error naming
the value — including when metrics are switched off. A typo that silently stops
the export is the failure this whole option exists to prevent, so it is never
just a log line.

### Instruments

OTLP reports the dotted names; the Prometheus exporter's translation is shown
next to them. **The two modes report deliberately disjoint sets**, so that summing
any `oci_gateway_*` series across a whole fleet is correct without filtering — see
[Two hops](#two-hops).

Reported by **both** modes:

| Instrument | Prometheus | Kind | What it measures |
|---|---|---|---|
| `http.server.request.duration` | `http_server_request_duration_seconds` | histogram (s) | Every request the gateway serves. Its `_count` series is the request rate |
| `http.server.active_requests` | `http_server_active_requests` | up-down counter | Requests in flight (blob transfers can be long-lived) |
| `oci.gateway.material.reloads` | `oci_gateway_material_reloads_total` | counter | Reloads of the TLS keypair, CA bundle and token files, by `oci.gateway.material` and `oci.result` |

Reported by **`serve`** only — this is the tier that actually talks to a registry,
so it is the one that counts registry traffic:

| Instrument | Prometheus | Kind | What it measures |
|---|---|---|---|
| `oci.gateway.io` | `oci_gateway_io_bytes_total` | counter (By) | Bandwidth: bytes received from and sent to clients, by `network.io.direction` |
| `oci.gateway.blob.downloads` | `oci_gateway_blob_downloads_total` | counter | Blobs downloaded to completion |
| `oci.gateway.blob.download.size` | `oci_gateway_blob_download_size_bytes` | histogram (By) | Size distribution of those blobs |
| `oci.gateway.blob.uploads` | `oci_gateway_blob_uploads_total` | counter | Blobs stored upstream, by `oci.blob.upload.kind` |
| `oci.gateway.blob.upload.size` | `oci_gateway_blob_upload_size_bytes` | histogram (By) | Size distribution of uploaded blobs |
| `oci.gateway.existence_checks` | `oci_gateway_existence_checks_total` | counter | `HEAD` probes by `oci.result` (`hit`/`miss`/`error`). A probe answered from the [blob existence cache](blob-existence-cache.md) counts as a `hit` here too, so this number means "how often was the content already there" whatever the cache is doing |
| `oci.gateway.blob_existence_cache.lookups` | `oci_gateway_blob_existence_cache_lookups_total` | counter | Cacheable blob probes by whether the cache answered them (`oci.result`) |
| `oci.gateway.blob_existence_cache.entries` | `oci_gateway_blob_existence_cache_entries` | gauge | Blobs the cache holds. Over `..._capacity`, how full it is |
| `oci.gateway.blob_existence_cache.capacity` | `oci_gateway_blob_existence_cache_capacity` | gauge | Blobs it has room for. Fixed: the memory is preallocated |
| `oci.gateway.blob_existence_cache.evictions` | `oci_gateway_blob_existence_cache_evictions_total` | counter | Entries dropped, by `oci.gateway.cache.eviction.reason` (`capacity`/`expired`/`deleted`) |
| `oci.gateway.blob_existence_cache.replication.events` | `oci_gateway_blob_existence_cache_replication_events_total` | counter | Facts replicated between instances, by `oci.gateway.cache.event` (`insert`/`delete`/`warmup`) and `network.io.direction` |
| `oci.gateway.blob_existence_cache.replication.batches` | `oci_gateway_blob_existence_cache_replication_batches_total` | counter | Replication messages, by `network.io.direction` and `oci.result`. A `failure` means a peer did not get the facts in it |
| `oci.gateway.blob_existence_cache.replication.dropped` | `oci_gateway_blob_existence_cache_replication_dropped_total` | counter | Facts never sent because the queue was full: replication shedding load to keep requests fast |
| `oci.gateway.blob_existence_cache.replication.peers` | `oci_gateway_blob_existence_cache_replication_peers` | gauge | Instances this one replicates to. Zero, with several replicas, is a discovery problem |
| `oci.gateway.errors` | `oci_gateway_errors_total` | counter | Failures by `error.type` and registry |
| `oci.gateway.upstream.duration` | `oci_gateway_upstream_duration_seconds` | histogram (s) | Time until the registry returned response headers |
| `oci.gateway.upstream.auth_handshakes` | `oci_gateway_upstream_auth_handshakes_total` | counter | Ping + token exchanges (cached per repository and scope) |
| `oci.gateway.policy.decisions` | `oci_gateway_policy_decisions_total` | counter | Authorization decisions by `oci.policy.decision` |
| `oci.gateway.policy.reloads` | `oci_gateway_policy_reloads_total` | counter | `SIGHUP` reloads by `oci.result`; a `failure` means the old policy is still in force |
| `oci.gateway.policy.rules` | `oci_gateway_policy_rules` | gauge | Rules in the policy this instance loaded |

Reported by **`forward`** only — the hop, which is the part no other tier can see:

| Instrument | Prometheus | Kind | What it measures |
|---|---|---|---|
| `oci.gateway.forward.peer.duration` | `oci_gateway_forward_peer_duration_seconds` | histogram (s) | Time until the peer gateway returned response headers. Its `_count` series is the relayed request rate, and its `network.protocol.version` attribute is how you confirm HTTP/2 is in use |
| `oci.gateway.forward.peer.connections` | `oci_gateway_forward_peer_connections_total` | counter | Connections opened to the peer. Relayed requests divided by this is requests per connection — the number that says multiplexing works |
| `oci.gateway.forward.errors` | `oci_gateway_forward_errors_total` | counter | Failures relaying to the peer, by `error.type` |

> **Alert on `oci_gateway_material_reloads_total{oci_result="failure"}`.** A failed
> reload deliberately keeps the previous certificate or token in force, which is
> the safe behaviour but also what makes a persistently broken file invisible —
> until the certificate expires and every client fails at once.

### Attributes

`oci.registry` (the resolved upstream host) is on nearly every measurement,
alongside `oci.operation` (`blob.read`, `blob.head`, `blob.write`, `blob.upload`,
`manifest.read`, `manifest.head`, `manifest.write`, `tags.list`,
`referrers.read`, `version.check`, `cache.events`, `cache.donate`, `unknown`).
Requests also carry the semantic-convention `http.request.method`, `url.scheme`,
`http.response.status_code`, `http.route` (templated, e.g.
`/v2/{name}/blobs/{digest}`), and `error.type` when they failed.

A forwarder's hop instruments additionally carry `oci.gateway.peer` (the peer's
host, constant for the process, so it costs no cardinality) and
`network.protocol.version` (`1.1`, `2`, or `unknown`).

The blob existence cache's occupancy instruments carry no registry at all — the
memory is one pool shared by every upstream — and its evictions carry only
`oci.gateway.cache.eviction.reason`. Its replication instruments carry no registry
either: one message carries facts about several.

`error.type` is a fixed set, grouped by who is at fault:

- **network** — `connection_refused`, `connection_reset`, `network_unreachable`,
  `timeout`, `dns`, `tls`, `tls_certificate`, `network`, `transfer_aborted`,
  `client_canceled`
- **upstream auth** — `upstream_auth` (credential resolution, ping, or token
  exchange), `upstream_unauthorized` (401)
- **permission denied** — `policy_denied`, `registry_denied`, `mount_denied`
  (this gateway's policy) and `upstream_forbidden` (403 from the registry)
- **other upstream** — `upstream_server_error` (5xx), `upstream_client_error`,
  `upstream_rate_limited` (429)
- **rejected request** — `missing_host`, `invalid_registry`,
  `invalid_repository`, `unsupported_endpoint`, `malformed_query`,
  `redirect_refused`, `too_many_redirects`, `bad_upstream_request`,
  `private_upstream` (`--deny-private-upstreams` refused the resolved address),
  `cache_self_replication` (a replication request arrived from this very instance,
  which means it is in its own peer list)
- **client authentication**, reported by a serving gateway that rejected a client —
  `peer_unauthenticated` (no credential), `peer_bad_credential` (rejected),
  `peer_identity_denied` (verified but not in the allow-list), `peer_auth_failed`
  (could not be validated, e.g. the Kubernetes API server was unreachable; fails
  closed)
- **peer**, reported by a forwarding gateway about *its* peer —
  `peer_unauthorized` (the peer rejected our credential), `peer_forbidden` (the
  peer rejected our identity). These are deliberately distinct from the
  `upstream_*` family: they mean the gateway-to-gateway credential is wrong, not
  that a registry rejected the gateway's registry credential

Every error also names the registry it happened with, in the metric attribute and
in the log line.


### Example queries

PromQL; the same aggregations apply to any OTLP backend.

```promql
# Requests per second, by upstream registry and operation.
sum by (oci_registry, oci_operation) (rate(http_server_request_duration_seconds_count[5m]))

# Bandwidth up and down, in bytes per second.
sum by (network_io_direction) (rate(oci_gateway_io_bytes_total[5m]))

# Blobs (and bytes) pushed and pulled per second, per registry.
sum by (oci_registry) (rate(oci_gateway_blob_uploads_total[5m]))
sum by (oci_registry) (rate(oci_gateway_blob_upload_size_bytes_sum[5m]))
sum by (oci_registry) (rate(oci_gateway_blob_downloads_total[5m]))

# Median downloaded blob size.
histogram_quantile(0.5, sum by (le) (rate(oci_gateway_blob_download_size_bytes_bucket[1h])))

# HEAD hit rate: how often a push found the content already upstream.
  sum(rate(oci_gateway_existence_checks_total{oci_result="hit"}[5m]))
/ sum(rate(oci_gateway_existence_checks_total[5m]))

# How much of that the gateway answered itself, without asking a registry — the
# round trips the blob existence cache saved your build farm.
  sum(rate(oci_gateway_blob_existence_cache_lookups_total{oci_result="hit"}[5m]))
/ sum(rate(oci_gateway_blob_existence_cache_lookups_total[5m]))

# Is that cache big enough? A capacity eviction rate above zero means the memory
# bound, not the TTL, is deciding how long blobs are remembered.
sum(rate(oci_gateway_blob_existence_cache_evictions_total{oci_gateway_cache_eviction_reason="capacity"}[5m]))
sum(oci_gateway_blob_existence_cache_entries) / sum(oci_gateway_blob_existence_cache_capacity)

# Is the cache actually shared across the replicas? Every instance should see the
# same number of peers as it has siblings, and receive roughly what the others send.
min(oci_gateway_blob_existence_cache_replication_peers)
sum(rate(oci_gateway_blob_existence_cache_replication_events_total{network_io_direction="receive"}[5m]))
sum(rate(oci_gateway_blob_existence_cache_replication_events_total{network_io_direction="transmit"}[5m]))

# Is replication failing or shedding load? Neither breaks a request; both cost hit rate.
sum(rate(oci_gateway_blob_existence_cache_replication_batches_total{oci_result="failure"}[5m]))
sum(rate(oci_gateway_blob_existence_cache_replication_dropped_total[5m]))

# Errors per second by type and registry.
sum by (error_type, oci_registry) (rate(oci_gateway_errors_total[5m]))

# Requests denied by policy.
sum by (oci_registry, oci_operation) (rate(oci_gateway_policy_decisions_total{oci_policy_decision="deny"}[5m]))

# 95th percentile serving latency, and requests in flight across the fleet.
histogram_quantile(0.95, sum by (le) (rate(http_server_request_duration_seconds_bucket[5m])))
sum(http_server_active_requests)

# Is one replica of a shared serving deployment carrying the load? Each forwarding
# sidecar pins one HTTP/2 connection to one replica for its life, so a small number
# of busy sidecars can skew badly. 1.0 is perfectly even.
    max by (service_instance_id) (rate(http_server_request_duration_seconds_count[5m]))
  / avg by (service_name)        (rate(http_server_request_duration_seconds_count[5m]))

# A failed reload keeps the previous certificate or token, so this is the only
# signal that one is going stale. Alert on it.
sum by (oci_gateway_material) (rate(oci_gateway_material_reloads_total{oci_result="failure"}[15m]))

# Is a serving gateway rejecting clients? (Reported by the server.)
sum by (error_type) (rate(oci_gateway_errors_total{error_type=~"peer_.*"}[5m]))
```

### Two hops

A forwarding gateway relays the very traffic the serving gateway then reports in
full. If it re-exported the registry-shaped instruments, every fleet-wide
`sum(rate(oci_gateway_blob_uploads_total[5m]))` would be **twice** the real number,
and you would have to remember a `service_name` filter on every panel forever. So
it does not export them at all: blobs, bytes, existence checks, policy decisions
and upstream latency are reported once, by the tier that talks to the registry.

What a forwarder exports instead is the hop, which nothing else can see, under
`oci.gateway.forward.*` names that cannot be added to the serving tier's series
even by accident. Both roles also export the semantic-convention
`http.server.*` pair — every HTTP service in a cluster does, so those are always
read with a `service_name` filter anyway — and their own
`oci_gateway_material_reloads_total`, because both have certificates to rotate and
you want to alert on either.

`service.name` distinguishes them: `oci-distribution-gateway` when serving,
`oci-distribution-gateway-forward` when forwarding (both overridden by
`OTEL_SERVICE_NAME`).

Three queries answer "is the second hop working?":

```promql
# 1. Is the hop up, and how fast? Its _count is the relayed request rate.
sum by (oci_gateway_peer) (rate(oci_gateway_forward_peer_duration_seconds_count[5m]))
histogram_quantile(0.95, sum by (le) (rate(oci_gateway_forward_peer_duration_seconds_bucket[5m])))

# 2. Is it actually multiplexed? Requests per connection — should be far above 1.
# A ratio near 1 means HTTP/2 was lost and every request is dialling again.
  sum(rate(oci_gateway_forward_peer_duration_seconds_count[5m]))
/ sum(rate(oci_gateway_forward_peer_connections_total[5m]))

# ...and the direct answer, which should be entirely version "2":
sum by (network_protocol_version) (rate(oci_gateway_forward_peer_duration_seconds_count[5m]))

# 3. Is the hop failing, and whose fault is it?
#    peer_unauthorized / peer_forbidden mean OUR peer credential is wrong.
#    connection_refused / tls_certificate mean we cannot reach or trust the peer.
sum by (error_type) (rate(oci_gateway_forward_errors_total[5m]))
```

Cross-checking the two tiers is then a meaningful comparison rather than a
tautology: relayed requests (from the forwarders) should track requests served
(from the serving gateway), and a persistent gap means requests are being lost on
the hop.

```promql
  sum(rate(oci_gateway_forward_peer_duration_seconds_count[5m]))
- sum(rate(http_server_request_duration_seconds_count{service_name="oci-distribution-gateway"}[5m]))
```

> **Leave `--metrics-exporter` unset in sidecars.** Thousands of worker pods each
> pushing OTLP every 60 s is a real bill, and the serving tier already reports the
> registry-facing half in full. Enable it on a handful of pods when you need to see
> the hop itself, or scrape a few with Prometheus.


### Running several gateway instances

Every instrument is additive, so a fleet is monitored by summing over instances,
as the queries above do — and because the two modes report disjoint sets (see
[Two hops](#two-hops)), a sum over `oci_gateway_*` never counts the same traffic
twice even with forwarders exporting. Nothing is derived from state that only one replica
holds — except `oci_gateway_policy_rules`, which is per instance on purpose:
`count by (oci_gateway_policy_rules)` (or graphing it per `service_instance_id`)
shows when a replica is still serving an older policy.

An upload whose chunks were spread across replicas by a load balancer is still
counted once, by the replica that handles the committing request, but its size is
reported as `oci.blob.upload.kind="unknown"` because no single instance saw all of
the bytes; the `oci_gateway_io_bytes_total` bandwidth is exact either way.

The blob existence cache is per replica, and its two occupancy gauges are written
to be summed: `sum(..._entries) / sum(..._capacity)` is how full the fleet's caches
are. A first-seen blob costs up to one upstream probe per replica — unless the
replicas [replicate the cache to each
other](blob-existence-cache.md#replicating-the-cache-between-instances), which is what turns that back
into one probe for the fleet. The replication counters are additive too, and the
`transmit` side is naturally larger than the `receive` side by roughly the number of
peers: one fact is sent to each of them.

### What is deliberately *not* measured

Repository paths are never used as attributes: on a build farm they are
unbounded, and they would multiply time series in the backend (the audit trail
for who accessed what is the decision log). For the same reason the number of
distinct `oci.registry` values one process reports is capped at 128, after which
further hosts are reported as `_other`.

A `404` is not an error — clients probe for content that does not exist as a
matter of course, and those show up as existence-check misses. A partial (`206`)
blob response counts toward bandwidth but not as a completed download, and a
cross-repository mount counts as an upload with no size (it transfers no bytes).

A probe the [blob existence cache](blob-existence-cache.md) answered records no
`oci.gateway.upstream.duration`, because there was no upstream leg to time. That is
also how you tell the two apart on a dashboard: `existence_checks` counts the
question, `upstream.duration` counts the ones that reached a registry.

[buildbarn/bb-deployments]: https://github.com/buildbarn/bb-deployments
[bb-deployments]: https://github.com/buildbarn/bb-deployments
[BuildBuddy executor Helm chart]: https://github.com/buildbuddy-io/buildbuddy-helm/tree/master/charts/buildbuddy-executor
