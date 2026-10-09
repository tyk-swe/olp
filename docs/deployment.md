# Production deployment

The bundled Helm chart deploys one immutable image in gateway, control, worker,
and migration modes. This guide covers production topology;
[`operations.md`](operations.md) covers monitoring, recovery, versions, and
incidents.

## Prerequisites and secrets

The example renders are checked for Kubernetes 1.33–1.35. Use a supported
cluster release, PostgreSQL 18, and durable Valkey 9.1. Pin an approved OCI
image digest; do not deploy a mutable development tag. Create these Secrets
before installing (names and keys are configurable through `config`):

| Purpose | Default Secret/key |
| --- | --- |
| PostgreSQL URL | `olp-postgresql` / `url` |
| Valkey URL | `olp-valkey` / `url` |
| Master keyring | `olp-master-key` / `key` |
| Authentication HMAC key | `olp-auth-hmac-key` / `key` |
| OTLP exporter headers (optional) | none / `headers`; set the name with `tracing.headersSecretName` |

Use the [secret-file formats](configuration.md#file-based-secrets) for the
master-key ring and authentication key. A new installation also needs a 32-byte
base64 bootstrap-token Secret mounted only into control pods. Keep all secret
values out of values files and shell history; the chart schema validates
configured names and keys.

### Database roles

Use an empty, separate database for each installation. OLP owns schema `olp`
and its checksum history; the installation UUID survives repeated and
concurrent migrations. Provision a migration owner and a separate, existing
runtime login with neither superuser nor ownership privileges. Using the
migration connection, run:

```sh
olp migrate --runtime-role olp_runtime
```

The command grants schema usage and feature-table DML, including
installation-row updates, with read-only migration history. It neither creates
login roles nor grants DDL privileges. Alternatively, run
`scripts/grant-runtime-database-role.sql` as the owner with
`psql -v runtime_role=olp_runtime`. Reapply grants after migrations; never give
the runtime role migration-owner membership or CREATE privileges.

Supply the runtime connection to application processes. Serving and worker
processes require an initialized installation; startup checks the complete,
unchanged migration history and never migrates implicitly. Production Helm
needs separate runtime and migration URL Secrets and sets
`migration.runtimeRole`, so every pre-install and pre-upgrade hook reapplies the
runtime grants. See
[database deadlines](operations.md#database-deadlines-and-privileges) for query
limits and backup roles.

### Regional read replicas

Set `OLP_DATABASE_READ_URL` (or its mounted-file form) on regional gateways and
workers to offload runtime release and key/credential authority reads to a
physical PostgreSQL replica. The Helm chart accepts an optional
`config.databaseReadSecretName` and `config.databaseReadSecretKey`; all workload
components in that chart use the named connection. Set `OLP_DATABASE_URL` to
the single writable primary. Management mutations, accounting, workload-key
registration, grant refresh requests and historical secret reads use the
primary. Both connections must name the same installation and schema version.

Each authority poll samples the primary WAL position and the replica replay
position before opening its read snapshot. A read inherits the local timestamp
of the newest primary position that the replica has reached. Replica delay and
time since the last successful poll together consume the existing 60-second
authority deadline. A reachable replica cannot keep an unreplayed revocation
fresh; API-key and provider-credential admissions refuse once the deadline is
exceeded. A healthy idle replica stays fresh without clock synchronization or
database heartbeat writes. The primary and replica must both remain reachable
for new freshness proofs. No replica query runs on the inference admission
path.

Every gateway or worker with a read URL has a separate read pool, bounded by
the same `OLP_DATABASE_MAX_CONNECTIONS` setting. Budget those connections
against replica capacity in addition to the primary connection budget. Control
processes use only the primary and do not open the optional read pool.

Read replicas do not create independent installations. Monitor replication,
authority age and readiness, apply migrations to the primary, and wait for
replay before rolling out the new binary. The integration harness runs a
physical standby, pauses replay after key revocation, verifies deadline
refusal, then resumes replay and verifies the revoked key remains refused.

### Shared Valkey and workers

The PostgreSQL installation UUID supplies the Valkey namespace, so independent
installations may share a logical database without key collisions. A restored
database retains its identity and is a replacement, not a clone; use a fresh
Valkey service for rehearsals; never let source and replacement share streams or
leases. A writable independent clone is unsupported.

The chart defaults to one worker for a small footprint. Production should use
three replicas, a PodDisruptionBudget, and failure-domain spreading. Workers
consume work concurrently; PostgreSQL advisory locks serialize maintenance and
cost reconciliation, and Valkey consumer groups reclaim metadata ownership.
Runtime releases publish transactionally with their activating mutation, not
through a worker outbox. The worker Deployment uses `Recreate`, so two worker
versions never run at once. OLP supports no mixed-version deployment during 0.x;
see [installation and versions](operations.md#installation-and-versions).

## Release artifacts

The release workflow publishes the multi-architecture image to
`ghcr.io/tyk-swe/olp` and the chart to
`oci://ghcr.io/tyk-swe/charts/openllmproxy`. Select a published 0.x version and
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
| --- | --- |
| `/v1`, `/v1beta`, `/native`, `/anthropic`, `/gemini`, `/bedrock`, `/ws` | gateway |
| `/api`, `/`, and console deep links | control |

Example values:

```yaml
image:
  repository: ghcr.io/tyk-swe/olp
  tag: "0.1.0"
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

`config.publicOrigin` and `ingress.host` must identify the same trusted origin.
Production uses OIDC and sets `config.localLoginEnabled: false` after the OIDC
login path has been verified. Local login remains available for bootstrap and
small installations. [Local MFA](access.md#local-multi-factor-authentication)
supports TOTP, WebAuthn and one-use recovery codes; installation owners can
require it with `auth.mfa_required`. Use an HTTPS DNS public origin for security
keys and keep recovery codes outside the browser. For Gateway API or a mesh, leave chart Ingress disabled and
reproduce the same routing table. Disable buffering for SSE and do not lower
request-size or idle-timeout bounds. Enable WebSocket upgrades for
`/v1/realtime` and the Gemini Live paths under `/gemini/ws` and `/ws` when they
are used.

The checked-in chart Ingress routes exactly these prefixes, which
`internal/surface` also reserves from the console and admits from the
inference pool. Vite does not proxy WebSockets; use the Go listener directly
for local realtime and Gemini Live clients.

## Observability and capacity

`OLP_OBSERVABILITY_LISTEN_ADDR` exposes only `/health/live`, `/health/ready`,
and `/metrics` on the pod network. The chart creates internal `*-observability`
ClusterIP Services on port 9090; the public Ingress has no health or metrics
route. [Network policy](#network-policy) below closes the port to everything
except the installation's Prometheus topology.

The binary's `OLP_HTTP_MAX_CONNECTIONS` default remains 1,024. The chart raises
the gateway per-pod TCP cap to 16,384 and keeps the control cap at 1,024.
In-flight work is separate: gateway/control inference pools default to 256 and
management pools to 32. Each permit lasts through streaming completion or
cancellation; a full pool returns HTTP 503 with `Retry-After: 1` instead of
queueing.

### Capacity qualification

The production Helm example starts at 32 in-flight inference requests per
gateway. At a 16 MiB response cap this permits 512 MiB of response bytes before
parser expansion, copies, request bodies, streams and other memory. The default
evaluation chart's 256-request limit can permit 4 GiB of response bytes alone;
it is unsuitable for simultaneous maximum-size buffered responses under a 2 GiB
memory limit. Measure maximum-size unary, streaming and media traffic, slow
readers, mass disconnects, accounting ingestion and worker recovery before
increasing concurrency.

Helm's media spool is disk-backed `emptyDir`. The production gateway requests
2 GiB and limits 3 GiB of ephemeral storage for a 1 GiB application spool inside
a 2 GiB volume. Compose uses tmpfs, which consumes memory; its production
overlay allows 4 GiB for the process and spool. Monitor reserved bytes,
filesystem free space, evictions and worker recovery. These reservations are
not measured memory or disk-reliability guarantees, and the node-local spool
does not replace durable provider job references. Keep analytics in control
processes and ingestion in workers, with database CPU/I/O headroom for both.

Successful mock suites qualify their tested behavior, not a production SLO,
live-provider certification, invoice accuracy, or disaster-recovery RPO.
Validate capacity and failure behavior on the actual deployment; use the
[testing guide](../tests/README.md) for qualification scope,
[performance](performance.md) for how added latency and CPU per request are
measured, and [backup and restore](operations.md#backup-and-restore) for
recovery requirements.

### Provider connection capacity

Over HTTP/1.1, every request in flight holds a connection to its provider's
host, which a pool bounds; HTTP/2 shares a connection among many. A provider with no
`options.network` and no profile uses the gateway's shared transport, which caps
none. A provider with any `options.network` field, or a profile, has a pool of its
own for each credential slot, capped by default at 64 connections to the host, 16
idle and 128 idle in all, and a request past the cap waits in the pool without an
error or a rejection. Size `max_conns_per_host` and `max_idle_conns_per_host`
(each at most 4,096) for the concurrent requests one credential slot of a provider
should carry, or spread them over more slots or providers; S2 of the
[benchmark](performance.md) holds about 1,300 streams at its full rate, which a
default pool would queue. The pool settings are in the
[provider guide](provider-profiles.md#secure-connection-options).

### Tracing

Tracing is disabled by default. To export request and provider-attempt spans,
set the full OTLP/HTTP traces endpoint and an optional Secret containing a JSON
object of exporter headers:

```yaml
tracing:
  endpoint: https://collector.example.com/v1/traces
  headersSecretName: olp-otlp-headers
  headersSecretKey: headers
  sampleRatio: 0.05
  propagateUpstream: true
  acceptInbound: true
```

The chart mounts the selected key at `/run/secrets/otlp-headers/headers` and
configures both gateway and control pods. `propagateUpstream` and
`acceptInbound` set `OLP_TRACE_PROPAGATE_UPSTREAM` and
`OLP_TRACE_ACCEPT_INBOUND`; they apply only when `endpoint` is set. Worker and
migration pods do not receive tracing configuration. Keep the Secret value out
of Helm values and use TLS for production collectors. The tracing exporter sends
no OpenTelemetry metrics or logs. The collector is an operator-controlled
endpoint and may be private or in-cluster; unlike provider endpoints, it is not
subject to OLP's public-HTTPS provider egress policy. This explicit exception
does not widen `config.providerEgressAllowCidrs` or
`config.providerEgressAllowHttpHosts`, and provider endpoint checks are
unchanged.

## Network policy

`networkPolicy.enabled: true` renders one NetworkPolicy per enabled component.
Rules target the container ports — 8080 for the public listener and 9090 for
observability — not `gateway.service.port`, so changing a Service port does not
change what the policy admits. The chart refuses to render without at least one
edge peer, because an empty peer list would silently deny all traffic to the
gateway.

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
leaving both `prometheus` maps empty denies every scrape of 9090. Workers expose
only private health/metrics on 9090; the Prometheus peer may reach that port.
Migration pods have no listener and deny ingress.

Egress defaults to allow-all. Provider endpoints are arbitrary public HTTPS
hosts, and the chart never sees the PostgreSQL or Valkey addresses —
`config.databaseSecretName` and `config.valkeySecretName` hold opaque connection
URLs — so a restrictive default would break every installation. Harden it once
those addresses are known:

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
  --set-string image.tag=0.1.0 \
  --set ingress.enabled=true --set ingress.className=nginx \
  --set ingress.host=olp.example.com \
  --set-string config.trustedProxyCidrs=10.0.0.0/8 \
  --set config.publicOrigin=https://olp.example.com
```

Install with approved values and at least a 20-minute timeout:

```console
helm upgrade --install olp \
  oci://ghcr.io/tyk-swe/charts/openllmproxy --version 0.1.0 \
  --namespace olp --create-namespace \
  --set-string image.tag=0.1.0 \
  --values production-values.yaml --timeout 20m --wait
```

## Readiness checks

Before issuing a proxy key or sending traffic, require a successful migration
Job, ready pods, runtime-generation convergence, and healthy observability
targets. With Valkey-backed workers, require all seven `olp_worker_task_healthy`
task series and zero request-metadata pending/lag; see
[worker health](operations.md#replicated-worker-health). `runtime_outbox` is
`not_configured`, not a drainage requirement. Continue with the monitoring and
recovery checks in [`operations.md`](operations.md).

## Production example and connection budget

Start with `deploy/helm/values.production.yaml`, set real public origin, TLS
Secret, trusted proxy ranges and network-policy namespace selectors, and pass
`--set-string image.digest="$QUALIFIED_IMAGE_DIGEST"`. That variable must
contain the qualified `sha256:...` value, not an example hash. Before
installing, verify:

```sh
gh attestation verify "oci://ghcr.io/tyk-swe/olp@$QUALIFIED_IMAGE_DIGEST" \
  --repo tyk-swe/olp --signer-workflow tyk-swe/olp/.github/workflows/release.yml
helm upgrade --install olp deploy/helm -f deploy/helm/values.production.yaml \
  --set-string image.digest="$QUALIFIED_IMAGE_DIGEST"
```

The example runs three gateways, two control replicas and three workers with 10
pooled connections per process: 80 pooled connections. Reserve two additional
connections per worker: cost reconciliation detaches a session and holds its
advisory lock between passes; maintenance detaches another for each pass and
closes it afterward. Both cease counting against pool capacity. Reserving both
for every replica conservatively covers leaders, contenders, and overlapping
passes: 86 planned connections. With the chart's one-pod surge for each HTTP
deployment and a ten-connection migration job, budget 116 application
connections during rollout. Reserve at least 20 more for monitoring,
administrators, closing/orphaned sessions and recovery (round the minimum
planned database limit up to 140). Do not run a second full fleet concurrently
within that budget. Workers use Recreate, and the database/Valkey service,
ingress, storage and DNS still need independent redundancy. Verify topology and
failure behavior on the actual cluster; a successful Helm render is not a
node-failure qualification.

For a single-host production Compose installation, run
`scripts/prepare-compose-production.sh`, then supply
`--env-file deploy/secrets/production.env -f deploy/compose.yaml -f deploy/compose.production.yaml`
to Compose, together with the documented bootstrap overlay for first setup. Set
a real HTTPS public origin, qualified image digest and non-root UID/GID. The
command creates a database password once and constructs its encoded URL;
PostgreSQL reads the raw value from a mounted secret. For an externally chosen
password, place it in `deploy/secrets/olp_database_password` with mode 0600
before running the command. Existing database passwords must not be regenerated
without an explicit database rotation. The overlay increases memory to include
tmpfs spooling but remains a single-host deployment, not an HA profile.

The image includes SPDX attestations for the runtime plus Go and console build
stages. Inspect them with
`docker buildx imagetools inspect IMAGE@DIGEST --format '{{json .SBOM}}'`.
Candidate qualification and the weekly release scan evaluate both the image and
those inventories; missing inventory is a failure. Downloaded chart archives can
be checked with `gh attestation verify CHART.tgz --repo tyk-swe/olp` before
installation.
