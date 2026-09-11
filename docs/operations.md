# Operations runbook

Availability, monitoring, recovery, upgrade, incident, and key-rotation
procedures for production OpenLLMProxy. Keep this runbook with the deployed
release; deployment topology is in [`deployment.md`](deployment.md).

For the provider-pool and routing-policy schema upgrade, follow the
[coordinated 3.x procedure](provider-routing.md#coordinated-3x-upgrade).
Drain older gateways before migrating; mixed binaries cannot enforce the same
policy and release format.

## Objectives and monitoring

Measure availability and added latency at the client-facing listener. Define
latency objectives for representative unary and streaming workloads, and
include accounting ingestion and recovery when measuring capacity.
OLP on-call owns gateway availability and request-metadata completeness;
provider owners own credentials, quotas, and model availability.

Scrape each in-cluster `*-observability` Service on port 9090 every 15 seconds
and probe `/health/live` and `/health/ready`. The public listener returns 404
for these paths. Readiness snapshots refresh every five seconds and expensive
rollups every fifteen. Page when readiness is absent for five minutes, events
are dropped/abandoned, persistence is unavailable, hard-limited keys cannot
reach Valkey, all asynchronous reporters are stale, or an outbox owner cannot
be taken over. Warn when request-metadata or runtime-outbox backlog exceeds
its threshold for ten minutes. The bundled Prometheus rules and per-component
ServiceMonitors provide starting alerts; keep control and gateway alerts
separate.

### Distributed tracing

Tracing is off until `OLP_OTLP_TRACES_ENDPOINT` is set. In production, start
with a low ratio such as `0.01` for locally rooted traces, then raise it only
for a bounded investigation or a traffic class whose collector budget is
known. `1.0` is appropriate for local development and short incident windows,
not as an unreviewed high-volume default. Valid inbound W3C context keeps its
upstream trace relationship when `OLP_TRACE_ACCEPT_INBOUND=true`; disable that
setting at a trust boundary that must start new traces. Upstream provider
propagation is independently controlled by `OLP_TRACE_PROPAGATE_UPSTREAM`.

Set the complete OTLP/HTTP traces URL, including `/v1/traces`. A self-managed
Tempo or Jaeger receiver commonly uses
`http://tempo:4318/v1/traces` or `http://jaeger:4318/v1/traces`. Honeycomb's US
endpoint is `https://api.honeycomb.io/v1/traces` (use the documented regional
endpoint where applicable) and requires an `x-honeycomb-team` value in the
JSON headers file; classic environments also require `x-honeycomb-dataset`.
Tempo multi-tenancy uses `x-scope-orgid` in the same file. Mount header files
from a secret manager with mode `0600` locally or through Helm's
`tracing.headersSecretName` and `tracing.headersSecretKey`; never put exporter
credentials in values files, environment variables, tickets, or traces.

Watch `olp_trace_export_dropped_total`. Export is bounded and asynchronous, so
an unavailable collector does not extend provider latency; sustained drops
mean the collector, network, sampling ratio, or queue budget needs attention.
Request and attempt spans contain only the documented allowlist. Prompt and
response content, tool payloads, raw headers, credentials, and raw provider
errors are prohibited even during incident debugging.

For local exploration, start the development-only Jaeger all-in-one overlay:

```console
docker compose -f deploy/compose.yaml -f deploy/compose.tracing.yaml up -d
```

Open `http://127.0.0.1:16686`. The overlay samples every local trace, exposes
only the UI port, and keeps traces in ephemeral memory. Stop it with the same
two `-f` arguments followed by `down`.

### Replicated worker health

`/health/ready` and `/metrics` read PostgreSQL-backed fleet summaries; worker
pods do not serve HTTP. `asynchronous_plane: healthy` means each fixed worker
task has a current checkpoint and both the request-metadata group and runtime
outbox are drained. It does not require one specific replica. Metadata, outbox,
and gateway-epoch checkpoints become stale after 20 seconds; maintenance
after 180 seconds. A released outbox session can be replaced during the
20-second handoff. Run three workers across failure domains in production.

Pending metadata is reclaimable after 30 seconds and scanned every five
seconds; investigate if recovery has not begun within 35 seconds. PostgreSQL
session loss releases outbox leadership. Use these content-free signals:

- `olp_request_metadata_consumer_pending_events`, lag, and oldest-pending age;
- reclaimed/recovered and persistence-duplicate counters;
- runtime-outbox pending/claimed/stale-owner and publication retry counters;
- `olp_worker_task_healthy{task=...}` and
  `olp_worker_task_runs_total{task=...,outcome=...}`.

Counters are additive PostgreSQL totals shared by replicas. If summaries cannot
be read, readiness reports `null` and
`olp_async_worker_observability_available` is zero; do not interpret missing
series as a reset. Reclaims and duplicates show recovery, not necessarily an
incident. Inspect the PostgreSQL advisory-lock session when failed takeover
counts rise.

### Spend-budget reconciliation

PostgreSQL usage facts are the spend authority; Valkey is the admission copy.
The terminal metadata transaction advances durable cumulative window totals,
and its consumer applies that absolute snapshot to Valkey. The
cost-reconciliation worker rebuilds and applies the same snapshots once per
minute. Reconciliation never lowers a valid counter, so it cannot erase or
double-count a terminal charge that races its query. Monitor
`olp_worker_task_healthy{task="cost_reconciliation"}` and
`olp_worker_task_runs_total{task="cost_reconciliation",outcome=...}`. Budget
rejections are counted only by window in
`olp_key_budget_rejections_total{window="daily|monthly"}`; there is deliberately
no per-key Prometheus spend gauge.
[`spend-budget-recovery.md`](spend-budget-recovery.md) explains how a budgeted
key initializes and recovers its Valkey snapshot.

If the API-key read shows accrued spend at or above a limit while requests are
still admitted, check the request-metadata consumer, PostgreSQL, Valkey, and the
cost-reconciliation checkpoint. Revoke the affected key until reconciliation
is healthy when continued admission risks overspend. For missing or valid but
lagging state, do not delete or lower a cost counter: restore the worker and let
the PostgreSQL snapshot repair it monotonically. After Valkey loss, wait for a
successful cost-reconciliation checkpoint before restoring traffic to
budgeted keys.

Malformed cost state fails admission closed and is replaced from the next
authoritative cumulative snapshot. Keep budgeted traffic stopped until a
successful reconciliation checkpoint and the API-key accrued values confirm
recovery. Only if reconciliation itself cannot run should an operator remove
an exact malformed cost hash while traffic is stopped; never use a wildcard or
delete the lookup-ID rate/concurrency keys. PostgreSQL remains the recovery
source.

Review `unpriced_attempts` with each budget. Unpriced attempts accrue 0 because
OLP never invents usage or cost, so a growing count means the configured budget
cannot bound all provider spend. Repair upstream usage reporting or pricing
coverage and keep provider-side quotas in place. Budgeted keys remain
fail-closed during a Valkey outage even when
`limits.valkey_unavailable=fail_open`; do not remove a budget to bypass that
safety boundary.

## Routine checks

1. Confirm pod readiness and one nonzero runtime generation across gateways.
2. Check PostgreSQL replication, WAL archiving, disk headroom, and backup age;
   check Valkey latency, memory and AOF durability (it holds metadata awaiting database ingestion).
3. Review usage completeness and pricing coverage before exporting costs.
   Missing upstream usage is incomplete and unpriced, never zero.
4. Review provider health, authentication, role/key changes, credential
   rotations, and route activations in the audit stream. `GET /api/v3/audit`
   narrows a page by `action`, `resource_type`, `resource_id`,
   `actor_user_id`, `outcome`, `occurred_after`, and `occurred_before`, so
   each category can be reviewed on its own. Session-driven actions also
   record the client source address, resolved through the same
   `OLP_TRUSTED_PROXY_CIDRS` rules the authentication boundary uses, and a
   coarse user-agent family; the full user-agent string is never stored, and
   background maintenance and reconciliation events leave both empty.
5. Offboarding requires rotating or revoking installation-scoped keys;
   deactivating a user alone does not revoke them.
6. Keep media-spool usage below `OLP_MEDIA_SPOOL_CAPACITY_BYTES`. Watch
   `olp_media_spool_used_bytes` against `olp_media_spool_capacity_bytes`; both
   are also on `/health/ready` as `media_spool_used_bytes` and
   `media_spool_capacity_bytes`. The chart budgets 1 GiB in a 2 GiB volume; do
   not use the 64 MiB general `/tmp` mount.
7. Watch `olp_http_admission_rejections_total{surface=...}` against admitted
   requests/capacity. Default independent pools are 256 inference and 32
   management requests; permits last through streaming or cancellation.

During a rollout, compare the active runtime-generation ordinal and provider
revision on every replica before comparing request latency. A healthy gateway
may continue serving its last complete generation while a new snapshot is
being compiled, but it must not accept a partially indexed generation. Check
the audit stream for activation, route-permission, credential, and key changes
before attributing a provider error to the rollout; `occurred_after` and
`occurred_before` bound that page to the rollout window. Keep provider probe
failures separate from gateway admission failures so an upstream outage does
not hide a local capacity regression.

Never put prompts, outputs, raw headers, credentials, sessions, proxy-key
secrets, or master keys in tickets or diagnostic bundles.

### OpenAI-compatible capability certification

Review at most 64 exact provider/model/operation tuples per compatible model,
then run **Server-certify capabilities**. The probe uses production codecs,
requests at most one generated token, and persists no prompt or response. Only
`succeeded` tuples become eligible; `partial` and `failed` remain declared.
Remove unsupported media, asynchronous, or cross-surface claims. Changing the
endpoint, region, project, deployment, or API version resets every tuple to
declared, so re-certify afterwards; renaming a provider, rotating its
credential, or re-reviewing an unchanged tuple set keeps the certification
(rotation still requires a fresh probe). Every enabled tuple needs a
certification timestamp before activation.

## Backup and restore

For a production recovery point:

1. Stop new inference admission and control writes, leave workers running,
   and wait for admitted work, pending acknowledgements, and Stream lag to
   drain. Keep traffic fenced until the backup finishes.
2. On an encrypted volume with PostgreSQL 18 client, `jq`, and GNU `sha256sum`,
   run `scripts/backup.sh` with `OLP_DATABASE_URL` and
   `OLP_BACKUP_TRAFFIC_QUIESCED=true`.

The script requires a zero, at-most-30-second-old durable checkpoint and an
explicit quiescence assertion. It exports one PostgreSQL snapshot and creates
an `olp3` manifest containing the checksum, installation identity, migration
count, and runtime generation. Only the `olp_v3` schema is backed up.

The dump contains password hashes, session and API-key digests, and encrypted
provider/OIDC credentials. Keep master-key rings and authentication HMAC files
in the secret manager and back them up separately. Retain historical master-key
versions while records still reference them.

Run `scripts/restore.sh BACKUP` using the dump path printed by the backup
script, with `OLP_RESTORE_DATABASE_URL` identifying an empty isolated database.
The command validates the 3.0 manifest and checksum,
restores in one transaction, and checks the restored identity, migrations, and
generation. Start the restored installation with its original keys and a fresh
Valkey service. A restored installation retains its namespace; run it as a
replacement, or isolate its Valkey service from the source installation.

## Installation and upgrades

3.0 does not upgrade 2.x. Back up the existing installation with its own
version, provision an independent 3.0 database and secrets, and verify providers,
routes, permissions, SDK requests, usage, and recovery before redirecting
traffic. Existing 2.x schemas are refused before any 3.0 objects are created.
There is no legacy Stream rename or historical migration replay.

For subsequent 3.x releases, review forward-only migrations, rehearse against
an isolated 3.0 backup, quiesce new inference and mutations, and drain accounting.
Take the final snapshot while workers still supply a fresh checkpoint, then
stop workers for migration. Run `olp migrate` once and roll out all process
modes before resuming admission. Verify readiness, generation convergence,
backlog, usage completeness, provider probes, and latency. Restore the saved
database and keys into a replacement installation if rollback requires an
older schema.

Delivery and replay evidence is retained for seven days plus five minutes of
clock-skew grace. A late entry is recorded as uncertain completeness, never
silently counted twice. Size PostgreSQL for up to
`sustained_requests_per_second * 604800` receipt rows. During a delivery
incident, restore/reconcile the Stream within seven days; do not extend the
window by suspending maintenance.

Migrations are forward-only. Never edit migration history or checksums. Restore
a verified backup into an empty replacement database when a release cannot be
rolled back safely.

## Database deadlines and privileges

Runtime connections set a 30-second statement deadline, five-second lock
wait and 60-second idle-transaction deadline when the role/session setting is
otherwise unlimited. Explicit nonzero deployment settings take precedence.
Migration connections close after use and have a separate five-minute
statement and ten-second lock budget. Backup requires a dedicated read role with access to migration history and all
backed-up tables; the restricted runtime login deliberately lacks that access.
Large maintenance/export operations should use their own role and explicitly chosen deadlines, not an unlimited
interactive account. PostgreSQL classifies statement cancellation as `57014`
and lock expiry as `55P03`; a timeout does not imply a committed mutation.

Provision a non-superuser, non-owner runtime login and a separate migration
owner. After migrations, run `scripts/grant-runtime-database-role.sql` as that
owner with psql's `-v runtime_role=olp_runtime`. The script grants table DML and
sequence usage, then removes migration-history access and installation-identity
writes. Reapply after each migration; do not give the runtime role membership
in the migration owner or schema/database CREATE privileges. Set both runtime
and migration URL Secrets before deploying the production Helm profile.

## Metric aggregation and incidents

Database-derived request counts, provider samples, worker counters and outbox
summaries are shared installation state exported by several replicas. Choose
one current exporter or use `max` across replicas of the **same installation**;
do not sum duplicated global values. Add a stable installation label at scrape
time if Prometheus monitors more than one installation. Rolling five/fifteen
minute values are gauges, not monotonic request counters. Never sum or average
precomputed p95/p99 values into a fleet percentile. Provider quantitative series
use stable provider IDs; names/status remain on the descriptive health series.
No sampled attempts means no success/latency series, not measured 100% success.

`olp_provider_metrics_complete=0` means collection failed or exceeded its
10,000-provider cap. Check `olp_provider_metrics_emitted` and
`olp_observability_metrics_snapshot_fresh`; a refresh exceeding four seconds
retains the last successful snapshot and exposes its age. Do not interpret
stale data as current health. Investigate database latency before increasing
cardinality. Process-local admission and trace-drop counters may be summed
across replicas; retain each counter's reset semantics when using `rate`.

Compare `olp_runtime_desired_generation` with `olp_runtime_generation` on each
gateway. A gap identifies a pending/rejected observed generation. Check
`olp_api_key_authority_age_seconds` separately: provider activation failure
must not hide successful authority refresh. At 60 seconds, new key-authenticated
requests are refused. Restore database reachability and confirm that authority
age falls and the desired generation converges before returning traffic.

For metadata/worker alerts, first check database and Valkey reachability,
consumer heartbeat and pending/lag counts. Preserve the queue; restart the
failed worker, allow the 30-second reclaim plus five-second scan window, and
confirm progress and cost attribution. For provider alerts, distinguish zero
samples, failed probes, provider rate limits and circuit failures. Group
notifications by installation and incident cause, inhibit secondary backlog
alerts during a confirmed dependency outage, and test routing/recovery in the
operator's Alertmanager configuration. Repository tests cannot prove that a
production notification reached an on-call responder.

Success SLIs must identify their denominator. `olp_request_success_ratio_5m`
counts persisted user requests with no terminal error and a 2xx/3xx status;
provider success ratios count attempts. A retried request can have a successful
request and a failed attempt. First byte is a latency milestone, not successful
completion. A stream failing after headers is a terminal failure; client
cancellations must be reported separately, with an explicit inclusion/exclusion
policy. Refusals and missing usage need distinct product/accounting indicators.
Cost totals are estimates from recorded priced usage; always read their
unpriced, incomplete, pending and loss coverage alongside the total.

## Recovery assurance

Compose AOF `everysec` may lose roughly one second of recently acknowledged
metadata on power/storage loss. Surviving epoch/gap records report known
uncertainty, but cannot enumerate facts that disappeared with their evidence.
Treat a suspected disaster interval as incomplete, stop budgeted traffic at
the edge, and reconcile provider billing before declaring accounting complete.
For a tighter RPO, qualify synchronous persistence and the actual replica/failover
policy on the deployment's storage; changing fsync alone is not a fleet guarantee.

Logical backup requires externally stopping new inference and control writes,
waiting for admitted work and accounting to drain, then retaining that fence
until export completes. The environment flag is an operator assertion, not a
server admission fence. Verify the load balancer/ingress configuration and all
replicas; a fresh zero backlog alone does not prove quiescence. Export has
bounded database waits and a dump timeout (`OLP_BACKUP_TIMEOUT_SECONDS`, default
600). New backups publish their dump and manifest together by atomic directory
rename; hidden partial directories are incomplete. Manifests record migration
versions/checksums and the script checkout's application version. Supply
`OLP_BACKUP_APP_VERSION` and `OLP_BACKUP_IMAGE_DIGEST` for the producing deployment
when it differs from that checkout. Restore verifies included migration hashes
against the local migration files before writing the empty destination.

Store backups with authenticated encryption, independent off-site retention
and audited access. Keep master-key recovery material in a separate recovery
process with tested key holders. Checksums detect corruption, not malicious
replacement of both a dump and its manifest. Rehearse restoration from the
actual off-site store, not just a local copy.

For PostgreSQL point-in-time recovery, use the managed service's tested PITR
procedure or [continuous WAL archiving](https://www.postgresql.org/docs/18/continuous-archiving.html)
with base backups, a restore target and verified WAL continuity. Record the
service's measured RPO/RTO; neither this repository nor a logical dump promises
one. Fence traffic first, restore to an isolated replacement, preserve the
installation identity/keyring, and use isolated Valkey. Reconcile external
provider/media outcomes and the database/queue time skew before reopening
budgeted traffic. Never let a rehearsal consume the source's streams or leases.
`make integration` performs a logical restore, authenticates, decrypts a retained
provider credential by making a request, and verifies historical accounting was
preserved and new accounting added. It does not simulate storage power loss or
certify a managed service's PITR implementation.
