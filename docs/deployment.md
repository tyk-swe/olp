# Production deployment

The bundled Helm chart deploys one immutable image in gateway, control,
worker, and migration modes. This guide covers production topology;
[`operations.md`](operations.md) covers monitoring, recovery, upgrades, and
incidents.

## Prerequisites and secrets

Use Kubernetes 1.27+, PostgreSQL 18, and durable Valkey 9.1. Pin an approved
OCI image digest; do not deploy a mutable development tag. Create these
Secrets before installing (names and keys are configurable through `config`):

| Purpose | Default Secret/key |
|---|---|
| PostgreSQL URL | `olp-postgresql` / `url` |
| Valkey URL | `olp-valkey` / `url` |
| Master keyring | `olp-master-key` / `key` |
| Authentication HMAC key | `olp-auth-hmac-key` / `key` |
| OTLP exporter headers (optional) | none / `headers`; set the name with `tracing.headersSecretName` |

Installations using `olp-key-hash-key` must copy the exact bytes to the new
HMAC Secret before upgrading; follow
[`operations.md#naming-migration-prerequisites`](operations.md#naming-migration-prerequisites).
New installations also need a 32-byte base64 bootstrap-token Secret mounted
only into control pods. Keep all secret values out of values files and shell
history; the chart schema validates configured names and keys.

### Shared Valkey and workers

The PostgreSQL installation UUID supplies the Valkey namespace, so independent
installations may share a logical database without key collisions. A restored
database retains its identity and is a replacement, not a clone; use a fresh
Valkey database for rehearsals and never run source and restore together.

The chart defaults to one worker for a small footprint. Production should use
three replicas, a PodDisruptionBudget, and failure-domain spreading. Workers
consume work concurrently; PostgreSQL advisory locking serializes runtime
outbox publication and Valkey consumer groups reclaim metadata ownership. The
worker Deployment uses `Recreate`: mixed-version workers are not supported
through the namespace transition.

## Release artifacts and verification

Published releases use two public GHCR packages: `ghcr.io/tyk-swe/olp` for
the multi-architecture image and `ghcr.io/tyk-swe/charts/openllmproxy` for the
Helm chart. Image tags `2.3.0`, `2.3`, and `latest` identify the same index at
publication. The versioned tag supports the Compose quick start and `latest`
is a convenience alias; production installations must pin the index digest.
The chart is selected independently with `--version 2.3.0`.

GitHub creates the first version of each GHCR package as private. On the first
release, a maintainer must open the package settings for both `olp` and
`charts/openllmproxy`, change their visibility to **Public**, and rerun the
failed release jobs. The workflow creates no GitHub Release until fresh,
unauthenticated runners pull the image, render the chart, and verify both
signatures. Publishing with the repository `GITHUB_TOKEN` and the OCI source
label links the packages to this repository, but does not make them public.

The `v2.3.0` image index and chart are published at the immutable digests used
below. Resolve the versioned references independently before each upgrade and
confirm they still match the approved release:

```console
docker buildx imagetools inspect ghcr.io/tyk-swe/olp:2.3.0
helm pull oci://ghcr.io/tyk-swe/charts/openllmproxy --version 2.3.0
```

Both commands report a `Digest:`. Confirm each one matches the pinned value
below, then verify the exact OCI artifacts with cosign:

```console
cosign verify \
  --certificate-identity 'https://github.com/tyk-swe/olp/.github/workflows/release.yml@refs/tags/v2.3.0' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  'ghcr.io/tyk-swe/olp@sha256:51a19182a05e0f5cae582203f99d5335a56b7a90cd363a5cad889d1b04b653ae'

cosign verify \
  --certificate-identity 'https://github.com/tyk-swe/olp/.github/workflows/release.yml@refs/tags/v2.3.0' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  'ghcr.io/tyk-swe/charts/openllmproxy@sha256:02be3a6af3fd88bb667d42cdbbdff42155e3b5de226057623723a89a56702f83'
```

The chart digest is the OCI manifest digest reported by `helm push`, not the
SHA-256 checksum of the downloaded `.tgz` release asset.

## Edge routing

Route the shared origin as follows, preserving prefixes, streaming, and client
disconnects:

| Prefix | Service |
|---|---|
| `/v1`, `/openai`, `/anthropic`, `/gemini` | gateway |
| `/api`, `/`, and console deep links | control |

Example values:

```yaml
image:
  repository: ghcr.io/tyk-swe/olp
  digest: sha256:51a19182a05e0f5cae582203f99d5335a56b7a90cd363a5cad889d1b04b653ae
config:
  publicOrigin: https://olp.example.com
  localLoginEnabled: false
  trustedProxyCidrs: 10.0.0.0/8
  bootstrapTokenSecretName: olp-bootstrap-token
  bootstrapTokenSecretKey: token
ingress:
  enabled: true
  className: nginx
  host: olp.example.com
  tls:
    enabled: true
    secretName: olp-tls
```

`config.publicOrigin` and `ingress.host` must identify the same trusted
origin. Production uses OIDC and sets `config.localLoginEnabled: false` after
the OIDC login path has been verified. Local login remains available for
bootstrap and small installations; MFA for that surface is deferred until a
deployment requests it, as recorded in
[`roadmap/deferred.md`](roadmap/deferred.md). For Gateway API or a mesh, leave
chart Ingress disabled and reproduce the same routing table.
Disable buffering for SSE and do not lower request-size or idle-timeout
bounds.

## Observability and capacity

`OLP_OBSERVABILITY_LISTEN_ADDR` exposes only `/health/live`, `/health/ready`,
and `/metrics` on the pod network. The chart creates internal
`*-observability` ClusterIP Services on port 9090; the public Ingress has no
health or metrics route. [Network policy](#network-policy) below closes the
port to everything except the installation's Prometheus topology.

The binary's `OLP_HTTP_MAX_CONNECTIONS` default remains 1,024. The chart raises
the gateway per-pod TCP cap to 16,384 and keeps the control cap at 1,024.
In-flight work is separate: gateway/control inference pools default to 256 and
management pools to 32. Each permit lasts through streaming completion or
cancellation; a full pool returns HTTP 503 with `Retry-After: 1` instead of
queueing.

The release baseline on an 8-vCPU KVM Haswell guest exercised the 256-request
inference limit with a fixed 200 ms upstream at 1,044–1,047 chat requests/s,
without admission rejections. At concurrency 64 it sustained 1,051–1,089 fixed
50-token streams/s and 262–263 embedding requests/s; `/v1/models` at concurrency
256 sustained 20,490–21,150 requests/s. Gateway throughput varied by at most
3.6% across three runs. The `/v1/models` figure is an HTTP-path stress signal,
not a metadata-safe request-rate target; that run saturated request-metadata
ingestion. The 256 inference default therefore remains the measured saturation
point, while the standalone 1,024 connection cap preserves four
connection slots per inference permit and did not bind the scenarios. Treat
these as a starting point, not a pod-sizing guarantee: CPU, memory, provider
connections, and especially long-lived stream duration still determine safe
production capacity. Full results are in [`bench/README.md`](../bench/README.md).

Tracing is disabled by default. To export request and provider-attempt spans,
set the full OTLP/HTTP traces endpoint and an optional Secret containing a JSON
object of exporter headers:

```yaml
tracing:
  endpoint: https://collector.example.com/v1/traces
  headersSecretName: olp-otlp-headers
  headersSecretKey: headers
  sampleRatio: 0.05
```

The chart mounts the selected key at
`/run/secrets/otlp-headers/headers` and configures both gateway and control
pods. Worker and migration pods do not receive tracing configuration. Keep the
Secret value out of Helm values and use TLS for production collectors. The
tracing exporter sends no OpenTelemetry metrics or logs. The collector is an
operator-controlled endpoint and may be private or in-cluster; unlike provider
endpoints, it is not subject to OLP's public-HTTPS provider egress policy. This
explicit exception does not widen `config.providerEgressAllowCidrs` or
`config.providerEgressAllowHttpHosts`, and provider endpoint checks are
unchanged.

## Network policy

`networkPolicy.enabled: true` renders one NetworkPolicy per enabled
component. Rules target the container ports — 8080 for the public listener
and 9090 for observability — not `gateway.service.port`, so changing a
Service port does not change what the policy admits. The chart refuses to
render without at least one edge peer, because an empty peer list would
silently deny all traffic to the gateway.

```yaml
networkPolicy:
  enabled: true
  edge:
    namespaceLabels:
      kubernetes.io/metadata.name: ingress-nginx
    cidrs: []
  prometheus:
    namespaceLabels:
      kubernetes.io/metadata.name: monitoring
    podLabels:
      app.kubernetes.io/name: prometheus
```

`edge.namespaceLabels` selects the namespaces allowed to reach 8080;
`edge.cidrs` adds raw peers for an edge load balancer or node range, and some
CNIs need the kubelet probe CIDRs there as well. The `prometheus` block is
separate from `monitoring.*`, which only places the ServiceMonitor object:
leaving both `prometheus` maps empty denies every scrape of 9090. Worker and
migration pods have no listener, so they receive a default-deny ingress
policy and their egress rules only.

Egress defaults to allow-all. Provider endpoints are arbitrary public HTTPS
hosts, and the chart never sees the PostgreSQL or Valkey addresses —
`config.databaseSecretName` and `config.valkeySecretName` hold opaque
connection URLs — so a restrictive default would break every installation on
first upgrade. Harden it once those addresses are known:

```yaml
networkPolicy:
  egress:
    restricted: true
    postgresql:
      cidrs: [10.10.0.0/16]
    valkey:
      cidrs: [10.11.0.0/16]
```

`restricted: true` requires both `postgresql.cidrs` and `valkey.cidrs` and
replaces allow-all with DNS on 53, those two peers on their configured ports,
and `providers.cidrs` on 443. Narrow `providers.cidrs` from `0.0.0.0/0` only
when every configured provider endpoint resolves inside a known range;
`config.providerEgressAllowCidrs` continues to enforce the application-level
public-host rule independently of the CNI.

Tracing values do not widen NetworkPolicy egress. When restricted egress does
not already admit the collector address and port, particularly an in-cluster
OTLP/HTTP receiver on 4318, add a separate NetworkPolicy selecting gateway and
control pods before enabling tracing. Do not add the collector CIDR to the
provider allowlist: collector and provider egress remain separate trust
boundaries.

## Install and verify

Render the exact configuration before applying it:

```console
helm lint --strict deploy/helm
helm template olp deploy/helm --namespace olp \
  --set-string image.digest=sha256:51a19182a05e0f5cae582203f99d5335a56b7a90cd363a5cad889d1b04b653ae \
  --set ingress.enabled=true --set ingress.className=nginx \
  --set ingress.host=olp.example.com \
  --set-string config.trustedProxyCidrs=10.0.0.0/8 \
  --set config.publicOrigin=https://olp.example.com
```

Install with approved values and at least a 20-minute timeout:

```console
helm upgrade --install olp \
  oci://ghcr.io/tyk-swe/charts/openllmproxy --version 2.3.0 \
  --namespace olp --create-namespace \
  --set-string image.digest=sha256:51a19182a05e0f5cae582203f99d5335a56b7a90cd363a5cad889d1b04b653ae \
  --values production-values.yaml --timeout 20m --wait
```

## Readiness checks

Before issuing a proxy key or sending traffic, require a successful migration
Job, ready pods, runtime-generation convergence, and healthy observability
targets. With replicated workers also require all five
`olp_worker_task_healthy` series, zero request-metadata pending/lag, and zero
runtime-outbox pending/claimed rows. Continue with the monitoring and recovery
checks in [`operations.md`](operations.md).
