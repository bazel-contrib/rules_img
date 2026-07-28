# oci-distribution-gateway

A container registry gateway that only forwards: clients speak the OCI
distribution protocol to it anonymously and name the upstream registry they want
in the `X-rules_img-Original-Host` header, and the gateway authenticates to that
registry itself and authorizes every request against a policy file.

See [Authenticating Build Actions](../../../docs/authenticating-build-actions.md#3-oci-distribution-gateway)
for how build actions are pointed at a gateway, the full flag list, and the
policy file format. This README documents running it as a service.

## Metrics

The gateway reports traffic, blob transfers, and errors as
[OpenTelemetry](https://opentelemetry.io) metrics. Metrics are **off until an
exporter is configured**, and configuration follows the standard OpenTelemetry
environment variables, so the tooling a cluster already runs works unchanged:

```bash
# Push to an OpenTelemetry collector (what the OpenTelemetry Operator injects).
# With OTEL_EXPORTER_OTLP_ENDPOINT set, OTLP is enabled automatically.
export OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318
oci-distribution-gateway --policy-file /etc/img/policy.json --port 8080

# Or expose a scrape endpoint for Prometheus on :9464/metrics.
oci-distribution-gateway --policy-file /etc/img/policy.json --port 8080 \
  --metrics-exporter prometheus

# Or print them to stderr while debugging.
oci-distribution-gateway --policy-file /etc/img/policy.json --port 8080 \
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
`oci-distribution-gateway` and `service.instance.id` to the hostname — the pod
name in Kubernetes — so **several gateway replicas keep their series apart**.

> **Security:** `--metrics-address` binds all interfaces by default so a
> Kubernetes scraper can reach it. The endpoint exposes upstream registry
> hostnames and traffic volumes (never credentials or repository names); keep it
> on a trusted network.

### Pushing to several collectors

`--metrics-otlp-endpoint` may be repeated. Each endpoint gets its own exporter
and its own periodic reader on the one meter provider, and the same metrics are
pushed to all of them:

```bash
oci-distribution-gateway --policy-file /etc/img/policy.json --port 8080 \
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
next to them.

| Instrument | Prometheus | Kind | What it measures |
|---|---|---|---|
| `http.server.request.duration` | `http_server_request_duration_seconds` | histogram (s) | Every request the gateway serves. Its `_count` series is the request rate |
| `http.server.active_requests` | `http_server_active_requests` | up-down counter | Requests in flight (blob transfers can be long-lived) |
| `oci.gateway.io` | `oci_gateway_io_bytes_total` | counter (By) | Bandwidth: bytes received from and sent to clients, by `network.io.direction` |
| `oci.gateway.blob.downloads` | `oci_gateway_blob_downloads_total` | counter | Blobs downloaded to completion |
| `oci.gateway.blob.download.size` | `oci_gateway_blob_download_size_bytes` | histogram (By) | Size distribution of those blobs |
| `oci.gateway.blob.uploads` | `oci_gateway_blob_uploads_total` | counter | Blobs stored upstream, by `oci.blob.upload.kind` |
| `oci.gateway.blob.upload.size` | `oci_gateway_blob_upload_size_bytes` | histogram (By) | Size distribution of uploaded blobs |
| `oci.gateway.existence_checks` | `oci_gateway_existence_checks_total` | counter | `HEAD` probes by `oci.result` (`hit`/`miss`/`error`) |
| `oci.gateway.errors` | `oci_gateway_errors_total` | counter | Failures by `error.type` and registry |
| `oci.gateway.upstream.duration` | `oci_gateway_upstream_duration_seconds` | histogram (s) | Time until the registry returned response headers |
| `oci.gateway.upstream.auth_handshakes` | `oci_gateway_upstream_auth_handshakes_total` | counter | Ping + token exchanges (cached per repository and scope) |
| `oci.gateway.policy.decisions` | `oci_gateway_policy_decisions_total` | counter | Authorization decisions by `oci.policy.decision` |
| `oci.gateway.policy.reloads` | `oci_gateway_policy_reloads_total` | counter | `SIGHUP` reloads by `oci.result`; a `failure` means the old policy is still in force |
| `oci.gateway.policy.rules` | `oci_gateway_policy_rules` | gauge | Rules in the policy this instance loaded |

### Attributes

`oci.registry` (the resolved upstream host) is on nearly every measurement,
alongside `oci.operation` (`blob.read`, `blob.head`, `blob.write`, `blob.upload`,
`manifest.read`, `manifest.head`, `manifest.write`, `tags.list`,
`referrers.read`, `version.check`, `unknown`). Requests also carry the
semantic-convention `http.request.method`, `url.scheme`,
`http.response.status_code`, `http.route` (templated, e.g.
`/v2/{name}/blobs/{digest}`), and `error.type` when they failed.

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
  `redirect_refused`, `too_many_redirects`, `bad_upstream_request`

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

# Errors per second by type and registry.
sum by (error_type, oci_registry) (rate(oci_gateway_errors_total[5m]))

# Requests denied by policy.
sum by (oci_registry, oci_operation) (rate(oci_gateway_policy_decisions_total{oci_policy_decision="deny"}[5m]))

# 95th percentile serving latency, and requests in flight across the fleet.
histogram_quantile(0.95, sum by (le) (rate(http_server_request_duration_seconds_bucket[5m])))
sum(http_server_active_requests)
```

### Running several gateway instances

Every instrument is additive, so a fleet is monitored by summing over instances,
as the queries above do. Nothing is derived from state that only one replica
holds — except `oci_gateway_policy_rules`, which is per instance on purpose:
`count by (oci_gateway_policy_rules)` (or graphing it per `service_instance_id`)
shows when a replica is still serving an older policy.

An upload whose chunks were spread across replicas by a load balancer is still
counted once, by the replica that handles the committing request, but its size is
reported as `oci.blob.upload.kind="unknown"` because no single instance saw all of
the bytes; the `oci_gateway_io_bytes_total` bandwidth is exact either way.

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
