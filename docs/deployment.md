# Production deployment

The bundled Helm chart deploys one immutable image in gateway, control,
worker, and migration modes. This guide covers production topology;
[`operations.md`](operations.md) covers monitoring, recovery, upgrades, and
incidents.

## Prerequisites and secrets

The example renders are checked for Kubernetes 1.33–1.35. Use a supported
cluster release, PostgreSQL 18, and durable Valkey 9.1. Pin an approved
OCI image digest; do not deploy a mutable development tag. Create these
Secrets before installing (names and keys are configurable through `config`):

| Purpose | Default Secret/key |
|---|---|
| PostgreSQL URL | `olp-postgresql` / `url` |
| Valkey URL | `olp-valkey` / `url` |
| Master keyring | `olp-master-key` / `key` |
| Authentication HMAC key | `olp-auth-hmac-key` / `key` |
| OTLP exporter headers (optional) | none / `headers`; set the name with `tracing.headersSecretName` |

Provision fresh 3.0 PostgreSQL storage and a JSON master-key ring. Existing 2.x
storage cannot be upgraded in place.
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
during schema changes.

## Release artifacts

The release workflow publishes the multi-architecture image to
`ghcr.io/tyk-swe/olp` and the chart to
`oci://ghcr.io/tyk-swe/charts/openllmproxy`. Select a published 3.x version and
pin its image digest for production. When testing this source tree before
publication, build `deploy/Dockerfile` and package `deploy/helm` locally.
Publication depends on the tagged commit passing check, dependency policy and
service integration, then the exact candidate index passing packaged Chromium
journeys on native amd64 and arm64 runners. Release tags are promoted without
rebuilding that index. Buildx SBOM/provenance and GitHub build attestations are
attached to the digest; verify provenance before promotion and deployment.

## Edge routing

Route the shared origin as follows, preserving prefixes, streaming, and client
disconnects:

| Prefix | Service |
|---|---|
| `/v1`, `/v1beta`, `/anthropic`, `/gemini` | gateway |
| `/api`, `/`, and console deep links | control |

Example values:

```yaml
image:
  repository: ghcr.io/tyk-swe/olp
  tag: "3.0.0"
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
deployment requests it. For Gateway API or a mesh, leave
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

Size deployments with representative unary and streaming workloads, including
accounting ingestion and worker recovery. CPU, memory, upstream latency, and
stream duration determine the concurrency a replica can sustain.

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
  --set-string image.tag=3.0.0 \
  --set ingress.enabled=true --set ingress.className=nginx \
  --set ingress.host=olp.example.com \
  --set-string config.trustedProxyCidrs=10.0.0.0/8 \
  --set config.publicOrigin=https://olp.example.com
```

Install with approved values and at least a 20-minute timeout:

```console
helm upgrade --install olp \
  oci://ghcr.io/tyk-swe/charts/openllmproxy --version 3.0.0 \
  --namespace olp --create-namespace \
  --set-string image.tag=3.0.0 \
  --values production-values.yaml --timeout 20m --wait
```

## Readiness checks

Before issuing a proxy key or sending traffic, require a successful migration
Job, ready pods, runtime-generation convergence, and healthy observability
targets. With replicated workers also require all five
`olp_worker_task_healthy` series, zero request-metadata pending/lag, and zero
runtime-outbox pending/claimed rows. Continue with the monitoring and recovery
checks in [`operations.md`](operations.md).


## Production example and connection budget

Start with `deploy/helm/values.production.yaml`, set real public origin, TLS
Secret, trusted proxy ranges and network-policy namespace selectors, and pass
`--set-string image.digest="$QUALIFIED_IMAGE_DIGEST"`. That variable must contain
the qualified `sha256:...` value, not an example hash. Before installing, verify:

```sh
gh attestation verify "oci://ghcr.io/tyk-swe/olp@$QUALIFIED_IMAGE_DIGEST" \
  --repo tyk-swe/olp --signer-workflow tyk-swe/olp/.github/workflows/release.yml
helm upgrade --install olp deploy/helm -f deploy/helm/values.production.yaml \
  --set-string image.digest="$QUALIFIED_IMAGE_DIGEST"
```

The example runs three gateways, two control replicas and three workers with
10 pooled connections per process: 80 pooled connections. Reserve two additional
connections per worker for detached outbox/cost leadership sessions, which no
longer count against pool capacity: 86 planned connections. With the chart's
one-pod surge for each HTTP deployment and a ten-connection migration job, budget
116 application connections during rollout. Reserve at least 20 more for
monitoring, administrators, closing/orphaned sessions and recovery (round the
minimum planned database limit up to 140).
Do not run a second full fleet concurrently within that budget. Workers use
Recreate, and the database/Valkey service, ingress, storage and DNS still need
independent redundancy. Verify topology and failure behavior on the actual
cluster; a successful Helm render is not a node-failure qualification.

For a single-host production Compose installation, run
`scripts/prepare-compose-production.sh`, then supply
`--env-file deploy/secrets/production.env -f deploy/compose.yaml -f deploy/compose.production.yaml`
to Compose, together with the documented bootstrap overlay for first setup.
Set a real HTTPS public origin, qualified image digest and non-root UID/GID.
The command creates a database password once and constructs its encoded URL;
PostgreSQL reads the raw value from a mounted secret. For an externally chosen
password, place it in `deploy/secrets/olp_database_password` with mode 0600 before
running the command. Existing database passwords must not be regenerated without
an explicit database rotation. The overlay increases memory to include tmpfs
spooling but remains a single-host deployment, not an HA profile.

The operational assumptions and evidence are in
[production-guarantees.md](production-guarantees.md).

The image includes SPDX attestations for the runtime plus Rust and console build
stages. Inspect them with `docker buildx imagetools inspect IMAGE@DIGEST --format
'{{json .SBOM}}'`. Candidate qualification and the weekly release scan evaluate
both the image and those inventories; missing inventory is a failure. Downloaded
chart archives can be checked with `gh attestation verify CHART.tgz --repo
tyk-swe/olp` before installation.
