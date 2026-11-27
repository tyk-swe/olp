# Production deployment

The bundled Helm chart runs one immutable OLP image in gateway, control, worker, and migration modes. This guide covers the production topology, installation inputs, and verification steps. Day-two monitoring, recovery, version handling, and incident procedures belong in the [Operations runbook](operations.md).

## Prerequisites and secrets

The chart schema and example renders are validated against Kubernetes 1.27+ via `scripts/check-helm.sh`. Use a supported cluster release, PostgreSQL 18, and durable Valkey 9.1. Pin an approved OCI image digest and never deploy a mutable development tag. Create the required Secrets before installing, adjusting names and keys through `config`, `migration`, or `tracing` as needed.

| Purpose | Default Secret and key |
| --- | --- |
| PostgreSQL runtime URL | `olp-postgresql` / `url` (`config.databaseSecretName`, `config.databaseSecretKey`) |
| PostgreSQL migration URL (optional) | Falls back to `config.databaseSecretName` and `config.databaseSecretKey`. Set `migration.databaseSecretName` and `migration.databaseSecretKey` for a dedicated migration owner, which also requires `migration.runtimeRole` |
| PostgreSQL read-replica URL (optional) | None / `url`. Set `config.databaseReadSecretName` and `config.databaseReadSecretKey` |
| Coordination Valkey URL | `olp-valkey` / `url` (`config.valkeySecretName`, `config.valkeySecretKey`) |
| Response-cache Valkey URL (optional) | None / `url`. Set `config.cacheValkeySecretName` and `config.cacheValkeySecretKey` for a separate evicting Valkey instance |
| Master keyring | `olp-master-key` / `key` (`config.masterKeySecretName`, `config.masterKeySecretKey`) |
| Authentication HMAC key | `olp-auth-hmac-key` / `key` (`config.authHmacKeySecretName`, `config.authHmacKeySecretKey`) |
| OTLP exporter headers (optional) | None / `headers`. Set `tracing.headersSecretName` and `tracing.headersSecretKey` |

Use the [secret-file formats](configuration.md#file-based-secrets) for the master-key ring and authentication key. The chart generates a one-time setup token Secret for control pods and keeps it across upgrades. Set `config.bootstrapTokenSecretName` and `config.bootstrapTokenSecretKey` to supply your own Secret instead. Keep all secret values out of values files and shell history. The chart schema validates configured names and keys.

### Database roles

Use an empty, separate database for each installation. OLP owns schema `olp` and its checksum history, and the installation UUID survives repeated and concurrent migrations. Provision a migration owner and a separate, existing runtime login with neither superuser nor ownership privileges. Using the migration connection, run this command.

```sh
olp migrate --runtime-role olp_runtime
```

The `olp migrate --runtime-role` flag (or the `OLP_RUNTIME_ROLE` environment variable) grants `USAGE` on schema `olp`, `SELECT, INSERT, UPDATE, DELETE` on all tables in schema `olp`, and `USAGE, SELECT` on all sequences in schema `olp`, then revokes `INSERT, UPDATE, DELETE` on `olp.migrations` so the runtime role keeps read-only access to migration history. It neither creates login roles, grants DDL privileges, nor modifies `PUBLIC` schema grants.

For initial database hardening, run `scripts/grant-runtime-database-role.sql` as the owner with `psql -v runtime_role=olp_runtime`. In addition to applying the same runtime schema, table, and sequence grants, that SQL script revokes `CREATE` on schema `public` from `PUBLIC`, revokes all privileges on schema `olp` from `PUBLIC`, and revokes all privileges on `olp.migrations` from the runtime role before granting only `SELECT`. Run the SQL script when provisioning the database, then let `olp migrate --runtime-role` reapply table and sequence grants after each migration. Never give the runtime role migration-owner membership or `CREATE` privileges.

Supply the runtime connection to application processes. Serving and worker processes require an initialized installation, and startup checks the complete, unchanged migration history without migrating implicitly. Production Helm deployments configure separate runtime and migration URL Secret references and set `migration.runtimeRole` so every pre-install and pre-upgrade hook passes `OLP_RUNTIME_ROLE` and reapplies runtime grants. Using different keys in the same Secret also requires `migration.runtimeRole`. See [database deadlines](operations.md#database-deadlines-and-privileges) for query limits and backup roles.

### Regional read replicas

Set `OLP_DATABASE_READ_URL` (or its mounted-file form `OLP_DATABASE_READ_URL_FILE`) on regional gateways and workers to offload runtime release and key or credential authority reads to a physical PostgreSQL replica. The Helm chart accepts `config.databaseReadSecretName` and `config.databaseReadSecretKey`, and every enabled workload component mounts that connection when configured. Keep `OLP_DATABASE_URL` pointed at the single writable primary. Management mutations, accounting, workload-key registration, grant refresh requests, and historical secret reads always use the primary. Both connections must address the same installation and schema version.

Each authority poll samples the primary WAL position and the replica replay position before opening its read snapshot. A read inherits the local timestamp of the newest primary position that the replica has reached. Replica delay and time since the last successful poll together consume the 60-second authority deadline. A reachable replica cannot keep an unreplayed revocation fresh, so API-key and provider-credential admissions refuse once the deadline passes. A healthy idle replica stays fresh without clock synchronization or database heartbeat writes. Both the primary and the replica must remain reachable for new freshness proofs. No replica query runs on the inference admission path.

Every gateway or worker configured with a read URL opens a separate read pool bounded by the same `OLP_DATABASE_MAX_CONNECTIONS` setting. Budget those connections against replica capacity in addition to the primary connection budget. Control processes use only the primary and do not open the optional read pool.

Read replicas do not create independent installations. Monitor replication, authority age, and readiness, apply migrations to the primary, and wait for replay before rolling out a new binary. The integration harness runs a physical standby, pauses replay after key revocation, verifies deadline refusal, and then resumes replay to confirm the revoked key remains refused.

### Shared Valkey and workers

The PostgreSQL installation UUID supplies the Valkey namespace, so independent installations can share a logical database without key collisions. A restored database retains its identity and serves as a replacement rather than a clone. Use a fresh Valkey service for restore rehearsals and never let a source and replacement share streams or leases. An independently writable clone is unsupported.

The chart defaults to one worker for a small footprint. Production deployments should run three replicas with a PodDisruptionBudget and failure-domain spreading. `values.production.yaml` requires each component to spread across nodes and prefers spreading across zones, so a cluster without zone labels still schedules every replica. Workers consume work concurrently while PostgreSQL advisory locks serialize maintenance and cost reconciliation, and Valkey consumer groups reclaim metadata ownership. Runtime releases publish transactionally with their activating mutation rather than through a worker outbox. The worker Deployment uses the `Recreate` strategy so two worker versions never run at once. OLP supports no mixed-version deployment during 0.x. See [installation and versions](operations.md#installation-and-versions).

## Release artifacts

The release workflow publishes the multi-architecture image to `ghcr.io/tyk-swe/olp` and the Helm chart to `oci://ghcr.io/tyk-swe/charts/openllmproxy`. Select a published 0.x version and pin its image digest for production. When testing this source tree before publication, build `deploy/Dockerfile` and package `deploy/helm` locally. Publication depends on the tagged commit passing checks, dependency policy, and service integration, followed by the exact candidate index passing packaged Chromium journeys on native amd64 and arm64 runners. Release tags promote without rebuilding that index. Buildx SBOM provenance and GitHub build attestations are attached to the digest, so verify provenance before promotion and deployment.

## Edge routing

Route the shared origin as follows while preserving prefixes, streaming responses, and client disconnects.

| Prefix | Service |
| --- | --- |
| `/v1`, `/v1beta`, `/native`, `/anthropic`, `/gemini`, `/bedrock`, `/ws`, `/code`, `/mcp`, `/a2a` | gateway |
| `/api`, `/scim`, `/`, and console deep links | control |

Example Helm values for same-origin Ingress routing are shown below.

```yaml
config:
  publicOrigin: https://olp.example.com
  localLoginEnabled: false
  trustedProxyCidrs: 10.0.0.0/8
ingress:
  enabled: true
  className: nginx
  host: olp.example.com
  tls:
    enabled: true
    secretName: olp-tls
```

`config.publicOrigin` and `ingress.host` must identify the same trusted origin. Production deployments use OIDC and set `config.localLoginEnabled` to `false` after verifying the OIDC login path, while local login remains available for bootstrap and small installations. [Local MFA](../use/access.md#local-multi-factor-authentication) supports TOTP, WebAuthn, and one-use recovery codes, and installation owners can require it with `auth.mfa_required`. Use an HTTPS DNS public origin for security keys and keep recovery codes outside the browser. For Gateway API or a service mesh, leave chart Ingress disabled and reproduce the exact routing table above. Disable response buffering for SSE and avoid lowering request-size or idle-timeout bounds. Enable WebSocket upgrades for `/v1/realtime`, the Gemini Live paths under `/gemini/ws` and `/ws`, and native duplex paths under `/native/` when used.

The checked-in chart Ingress routes exactly these inference prefixes to `gateway` and routes `/api` and `/` (covering `/scim` and console deep links) to `control`. `internal/domain/surface` also reserves these public prefixes, including `/scim/` on the control listener, so unknown API requests never fall through to the console SPA. Vite does not proxy WebSockets, so point local realtime and Gemini Live clients at the Go listener directly.

## Observability and capacity

`OLP_OBSERVABILITY_LISTEN_ADDR` exposes only `/health/live`, `/health/ready`, and `/metrics` on the pod network. The chart creates internal `*-observability` ClusterIP Services on port 9090, and the public Ingress exposes no health or metrics route. [Network policy](#network-policy) restricts port 9090 to the installation's Prometheus topology. For container health checks where `curl` is unavailable, run `olp health-probe`, which sends an HTTP GET to `/health/ready` on `OLP_OBSERVABILITY_LISTEN_ADDR` (default `127.0.0.1:9090`, normalizing wildcard addresses to loopback) with a 3-second timeout and no redirects. Chart pods also run `olp internal-pre-stop --seconds <preStopDelaySeconds>` (default 10 seconds) in their `preStop` hook so edge proxies can stop routing traffic before SIGTERM begins draining.

The binary's `OLP_HTTP_MAX_CONNECTIONS` default is 1,024. The chart raises the gateway per-pod TCP cap to 16,384 and keeps the control cap at 1,024. In-flight work admission is bounded separately from TCP connections. Gateway and control inference pools default to 256 concurrent requests (`gateway.httpMaxInFlightInferenceRequests`) and management pools default to 32 (`gateway.httpMaxInFlightManagementRequests`, `control.httpMaxInFlightManagementRequests`). Each permit lasts through streaming completion or client cancellation.

By default, `gateway.httpAdmissionQueueDepth` (`OLP_HTTP_ADMISSION_QUEUE_DEPTH`) is `0`, so a full inference pool immediately returns HTTP 503 with `Retry-After: 1` without queueing. Setting `gateway.httpAdmissionQueueDepth` above zero (up to 100,000) enables opt-in priority admission queueing for inference traffic. Excess inference requests reserve a queue slot and wait up to `gateway.httpAdmissionQueueTimeout` (`OLP_HTTP_ADMISSION_QUEUE_TIMEOUT`, default `2s`, allowed from `1ms` to `1m`, never past the route deadline). Released permits pass directly to waiting requests using smooth weighted round-robin across priority classes (`critical`, `high`, `normal`, and `low`). Requests arriving when the queue is full or timing out while waiting receive HTTP 503 with `Retry-After: 1`. Management pools never queue.

Mirrored shadow traffic runs in a separate pool controlled by `gateway.httpMaxInFlightShadowRequests` (`OLP_HTTP_MAX_IN_FLIGHT_SHADOW_REQUESTS`, default `16`, allowed from 1 to 100,000). Excess shadow mirrors are dropped immediately and never queued or counted against primary inference permits.

The Helm chart validates related body-size, spool, and drain limits at template time and rejects invalid combinations before deployment. Rendering fails if `config.httpMaxInlineMediaTotalBytes` exceeds `config.httpMaxJsonBodyBytes`, if `config.httpMaxInlineMediaItemBytes` exceeds `config.httpMaxInlineMediaTotalBytes`, if `config.httpMaxMediaBodyBytes` exceeds half of `mediaSpool.capacityBytes`, if `config.providerMaxEventBytes` exceeds `config.providerMaxResponseBytes`, or if `config.httpConnectionDrainTimeoutSeconds` is not less than `terminationGracePeriodSeconds` minus `preStopDelaySeconds`.

### Capacity qualification

The production Helm example starts at 32 in-flight inference requests per gateway. At a 16 MiB response cap, that limit allows up to 512 MiB of buffered response bytes before parser expansion, copies, request bodies, streams, and other runtime overhead. The default evaluation chart's 256-request limit can allow up to 4 GiB of buffered response bytes alone, which is unsuitable for simultaneous maximum-size buffered responses under a 2 GiB memory limit. Measure maximum-size unary, streaming, and media traffic alongside slow readers, mass disconnects, accounting ingestion, and worker recovery before raising concurrency.

Helm's media spool uses a disk-backed `emptyDir` volume. The production gateway requests 2 GiB and limits 3 GiB of ephemeral storage for a 1 GiB application spool inside a 2 GiB volume. Compose uses tmpfs, which consumes memory, and its production overlay allocates 4 GiB for the process and spool. Monitor reserved bytes, filesystem free space, evictions, and worker recovery. These reservations do not constitute measured memory or disk-reliability guarantees, and the node-local spool never replaces durable provider job references. Keep analytics queries in control processes and metadata ingestion in workers, with database CPU and I/O headroom for both.

The pre-merge test tiers do not establish a production SLO, live-provider certification, invoice accuracy, or disaster-recovery RPO. Validate capacity and failure behavior on the target deployment. Use the [testing guide](../../tests/README.md) for test scope, [performance](../develop/performance.md) for measurement status, and [backup and restore](operations.md#backup-and-restore) for recovery requirements.

### Provider connection capacity

Over HTTP/1.1, every in-flight request holds a connection to its provider host, bounded by the transport pool, while HTTP/2 multiplexes requests over shared connections. A provider with neither `options.network` nor a profile uses the gateway's shared transport with no per-host total cap. A provider with any `options.network` field or a profile receives a dedicated pool per credential slot capped by default at 64 connections per host, 16 idle per host, and 128 idle connections overall. Requests beyond that cap wait inside the transport pool without emitting an admission error or opening a circuit. Size `max_conns_per_host` and `max_idle_conns_per_host` (each up to 4,096) for the concurrent requests one credential slot must carry, or distribute traffic across additional slots or providers. Historical benchmark scenario S2 held about 1,300 concurrent streams at full rate, which a default pool would queue. See [performance](../develop/performance.md) and the pool settings in the [provider guide](../use/provider-profiles.md#secure-connection-options).

### Tracing

Tracing is disabled by default. To export request and provider-attempt spans, configure the full OTLP/HTTP traces endpoint and an optional Secret containing a JSON object of exporter headers.

```yaml
tracing:
  endpoint: https://collector.example.com/v1/traces
  headersSecretName: olp-otlp-headers
  headersSecretKey: headers
  sampleRatio: 0.05
  propagateUpstream: true
  acceptInbound: true
```

The chart mounts the selected key at `/run/secrets/otlp-headers/headers` and configures both gateway and control pods. `propagateUpstream` and `acceptInbound` set `OLP_TRACE_PROPAGATE_UPSTREAM` and `OLP_TRACE_ACCEPT_INBOUND` and apply only when `endpoint` is set. Worker and migration pods do not receive tracing configuration. Keep the Secret value out of Helm values and use TLS for production collectors. The tracing exporter sends no OpenTelemetry metrics or logs. Because the collector is an operator-controlled endpoint that may be private or in-cluster, it is exempt from OLP's public-HTTPS provider egress policy. That exemption does not widen `config.providerEgressAllowCidrs` or `config.providerEgressAllowHttpHosts`, and provider endpoint checks remain unchanged.

## Network policy

Setting `networkPolicy.enabled` to `true` renders one NetworkPolicy per enabled component. Rules target container ports 8080 for the public listener and 9090 for observability rather than `gateway.service.port`, so changing a Service port does not change what the policy admits. The chart refuses to render without at least one edge peer because an empty peer list would silently deny all gateway traffic.

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

`edge.namespaceLabels` selects namespaces permitted to reach port 8080, and `edge.cidrs` adds raw peers for an edge load balancer, node range, or kubelet probe CIDRs when required by the CNI. The `prometheus` block is independent of `monitoring.*`, which only creates the ServiceMonitor object, so leaving both `prometheus` maps empty denies every scrape of port 9090. Workers expose only private health and metrics on port 9090, reachable by the Prometheus peer. Migration pods bind no listener and deny all ingress.

Egress defaults to allow-all because provider endpoints can be arbitrary public HTTPS hosts and the chart never sees PostgreSQL or Valkey addresses directly (the Secret values carry opaque connection URLs). A restrictive default would therefore break installation before an operator provides real network CIDRs. Harden egress once those addresses are known.

```yaml
networkPolicy:
  egress:
    restricted: true
    dns:
      cidrs: [10.96.0.10/32] # Replace with your cluster or node-local DNS resolver.
    postgresql:
      cidrs: [10.10.0.0/16]
    valkey:
      cidrs: [10.11.0.0/16]
    cacheValkey:
      cidrs: [10.12.0.0/16] # Required when config.cacheValkeySecretName is set (default port 6379).
```

When `networkPolicy.egress.restricted` is `true`, the chart requires `postgresql.cidrs`, `valkey.cidrs`, `dns.cidrs` (when `dns.enabled` is `true`), and `cacheValkey.cidrs` whenever `config.cacheValkeySecretName` is set. Set `dns.cidrs` to the resolver addresses reachable from pods, including node-local DNS when deployed, because the chart assumes no resolver address. Restricted egress allows TCP and UDP port 53 to the DNS peers, TCP traffic to PostgreSQL (default port 5432), coordination Valkey (default port 6379), and response-cache Valkey (default port 6379) on their configured ports, plus `providers.cidrs` on port 443. Narrow `providers.cidrs` from `0.0.0.0/0` only when every configured provider endpoint resolves inside a known range. `config.providerEgressAllowCidrs` continues to enforce OLP's application-level public-host rule independently of the CNI.

Tracing values do not widen NetworkPolicy egress. When restricted egress does not already admit the collector address and port, such as an in-cluster OTLP/HTTP receiver on port 4318, create a separate NetworkPolicy selecting gateway and control pods before enabling tracing. Never add the collector CIDR to the provider allowlist because collector and provider egress stay on separate trust boundaries.

## Install and verify

Pull the release chart, verify that the release workflow built it, and render the exact configuration before applying it. `production-values.yaml` should pin `image.digest` as described in the [production example](#production-example-and-connection-budget).

```console
VERSION=0.2.0
helm pull oci://ghcr.io/tyk-swe/charts/openllmproxy --version "$VERSION"
gh attestation verify "openllmproxy-$VERSION.tgz" --repo tyk-swe/olp \
  --signer-workflow tyk-swe/olp/.github/workflows/release.yml
helm lint --strict "openllmproxy-$VERSION.tgz" --values production-values.yaml
helm template olp "openllmproxy-$VERSION.tgz" --namespace olp \
  --values production-values.yaml
```

### First install

Install the verified archive with the approved values. Omit `--wait` and flags that imply it, such as `--atomic` or `--rollback-on-failure`. Helm still waits for the pre-install migration Job to succeed before creating the workloads. Set `--timeout` longer than `migration.activeDeadlineSeconds`, which defaults to 35 minutes.

```console
helm install olp "openllmproxy-$VERSION.tgz" \
  --namespace olp --create-namespace \
  --values production-values.yaml --timeout 40m
```

A fresh gateway stays unready until it loads a published runtime generation, so waiting for it during install would delay the Helm notes needed to create the first owner. The rollout commands below assume the default Deployment names for release `olp` in namespace `olp`. Wait for the control Deployment first.

```console
kubectl --namespace olp rollout status deployment/olp-openllmproxy-control --timeout=5m
```

Run the `olp setup-token` command from the Helm notes and enter the token on the first-owner setup page. Sign in, then [connect and activate a provider](../getting-started.md#connect-and-activate-a-provider) and [publish a route](../getting-started.md#publish-a-route) before waiting for the gateway.

## Readiness checks

After publishing the first route, wait for the gateway and worker Deployments.

```console
kubectl --namespace olp rollout status deployment/olp-openllmproxy-gateway --timeout=5m
kubectl --namespace olp rollout status deployment/olp-openllmproxy-worker --timeout=5m
```

Before issuing a proxy key or sending traffic, confirm a successful migration Job, ready pods, runtime-generation convergence, and healthy observability targets. With Valkey-backed workers, confirm every `olp_worker_task_healthy` task series is healthy and request-metadata pending and lag counts are zero. See [worker health](operations.md#replicated-worker-health) and continue with the monitoring and recovery checks in [`operations.md`](operations.md).

## Upgrade an existing release

Follow the [upgrade preparation in the operations runbook](operations.md#installation-and-versions). When the installation already has a published runtime generation, include `--wait` so Helm waits for the updated workloads. Verify and render the new archive as shown above, then run the upgrade with a timeout longer than the migration Job's deadline.

```console
helm upgrade olp "openllmproxy-$VERSION.tgz" \
  --namespace olp \
  --values production-values.yaml --timeout 40m --wait
```

Complete the [readiness checks](#readiness-checks) before resuming traffic.

## Production example and connection budget

Start from `deploy/helm/values.production.yaml`, configure a real public origin, TLS Secret, trusted proxy ranges, and network-policy namespace selectors, and pass `--set-string image.digest="$QUALIFIED_IMAGE_DIGEST"`. That variable must hold the qualified `sha256:...` digest rather than an example hash. Verify the image attestation and render the chart from the checkout.

```sh
gh attestation verify "oci://ghcr.io/tyk-swe/olp@$QUALIFIED_IMAGE_DIGEST" \
  --repo tyk-swe/olp --signer-workflow tyk-swe/olp/.github/workflows/release.yml
helm template olp deploy/helm --namespace olp -f deploy/helm/values.production.yaml \
  --set-string image.digest="$QUALIFIED_IMAGE_DIGEST"
```

To install from the checkout, follow the [first-install steps](#first-install) using `deploy/helm` as the chart path and the same values and image digest you rendered.

The production example runs three gateways, two control replicas, and three workers with 10 pooled connections per process, totaling 80 pooled connections. Reserve two extra connections per worker because cost reconciliation detaches a session and holds its advisory lock between passes while maintenance detaches another for each pass and closes it afterward, and neither detached session counts against pool capacity. Reserving both per replica conservatively covers leaders, contenders, and overlapping passes for 86 planned connections. Accounting for the chart's one-pod surge on each HTTP deployment plus a ten-connection migration Job brings the rollout peak to 116 application connections. Reserve at least 20 more connections for monitoring, administrators, closing or orphaned sessions, and recovery, rounding the minimum planned database `max_connections` budget up to 140. Avoid running a second full fleet concurrently inside that budget. Workers use `Recreate`, and the database, Valkey service, ingress, storage, and DNS still require independent redundancy. Verify topology and failure behavior on the target cluster because a clean Helm render does not qualify node-failure resilience.

For a single-host production Compose installation, set a real HTTPS `OLP_PUBLIC_ORIGIN` and a qualified `OLP_IMAGE` digest in `.env`, then start the stack.

```sh
docker compose --env-file .env -f deploy/compose.yaml -f deploy/compose.production.yaml up -d
```

The first start generates the database password and application secrets into named volumes, as described in [Getting Started](../getting-started.md#install-olp). Back up `olp-secrets` and `olp-database-secret` alongside the database because losing them loses access to encrypted credentials and the database login. The production overlay raises memory limits to accommodate tmpfs spooling but remains a single-host deployment rather than a high-availability profile.

### Upgrading an older Compose installation

Installations created before Compose generated its own secrets stored them in `deploy/secrets` and read `POSTGRES_PASSWORD` from `.env`. Copy the existing files into the named volumes before starting with the updated Compose file so `init-secrets` does not create fresh keys that cannot decrypt existing data.

```sh
docker compose -f deploy/compose.yaml create init-secrets
docker run --rm -v "$PWD/deploy/secrets:/old:ro" \
  -v openllmproxy_olp-secrets:/run/olp/secrets \
  -v openllmproxy_olp-database-secret:/run/olp/database \
  -e POSTGRES_PASSWORD="<the old POSTGRES_PASSWORD>" \
  busybox sh -ec '
    cp /old/olp_master_key /old/olp_auth_hmac_key /run/olp/secrets/
    printf %s "$POSTGRES_PASSWORD" > /run/olp/database/postgres_password
    chown 65532:65532 /run/olp/secrets/* /run/olp/database/postgres_password
    chmod 0400 /run/olp/secrets/*
    chmod 0444 /run/olp/database/postgres_password'
docker compose -f deploy/compose.yaml up -d
```

The initializer preserves the copied files and generates the setup token, an application password file, and `pgpass` for PostgreSQL clients.

The container image attaches SPDX attestations for the runtime plus Go and console build stages. Inspect them with `docker buildx imagetools inspect IMAGE@DIGEST --format '{{json .SBOM}}'`. Candidate qualification and the weekly release scan evaluate both the image and those inventories, treating missing inventory as a failure. Check downloaded chart archives with `gh attestation verify CHART.tgz --repo tyk-swe/olp` before installing.

## Regional fleets

Deploy one control plane and PostgreSQL primary alongside regional gateways, workers, and Valkey services, adding local read replicas where needed. Configure a shared `OLP_REGION` for every gateway and worker sharing a regional Valkey, keeping region names distinct across stores. Installation UUID, authentication HMAC key, and master-key ring are shared across regions, whereas independent installations require separate databases and keys.

RPM, TPM, concurrency, and route, end-user, and provider count limits are regional. Each region inherits full key defaults unless `regional_limits` overrides counts, for example `{"west":{"requests_per_minute":100},"east":{"max_concurrency":8}}`. Templates still impose ceilings and overrides cannot define currency limits. Adding or losing regions does not automatically partition limits.

Durable spend is global. One leader **per region** reconciles global facts every minute into local snapshots and holds. Enforcement is eventual across regions rather than synchronous at admission time. Overshoot exposure includes all cost admitted against stale snapshots rather than only unfinished requests, so two regions admitting $0.60 each against a $1 budget can reach $1.20 before reconciliation. That envelope assumes bounded estimates and timely accounting, and outages widen it. Rely on conservative estimates, count and concurrency caps, and recovery monitoring rather than treating regional spend controls as an invoice guarantee.

Regional readiness and checkpoints cannot be satisfied by another region, while PostgreSQL-only tasks and control summaries remain shared. Hard routing constraints and priority take precedence over provider or model region locality, and an unknown region applies no locality preference.

A former process test qualified count overrides, local concurrency and leaders, and reconciliation refusal with two Valkeys and one database provisioned with a physical standby. That lane was removed, so validate regional changes against staging services before production rollout.
