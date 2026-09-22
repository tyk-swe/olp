# Operations runbook

Availability, monitoring, recovery, upgrade, incident, and key-rotation
procedures for production OpenLLMProxy. Keep this runbook with the deployed
release; deployment topology is in [`deployment.md`](deployment.md).

For the provider-pool and routing-policy schema upgrade, follow the
[coordinated 3.x procedure](provider-routing.md#coordinated-3x-upgrade). Drain
older gateways before migrating; mixed binaries cannot enforce the same policy
and release format.

## Strict route cutover

A route slug is a published contract identity. To move a legacy or transformed
route to strict, fetch its current ETag and create a review draft with
`POST /api/v3/routes/{route_id}/migration-draft`, `If-Match`, an idempotency key,
and a body such as `{"slug":"new-route","fidelity":{"mode":"strict"}}`. The
destination slug must never have been published, including as a retired route.
The response is an editable draft that copies the current revision, targets,
content policy and routing policy. Creation and plan inspection perform no
inference. Resolve any strict policy or provider-profile incompatibility,
validate the draft, then activate it. Reusing the original slug for this change
returns `route_fidelity_migration_required` before publication; configuration
plan/apply report the same boundary.

Provision an API key or update its allowed routes for the new slug, verify the
new gateway serves the strict route, and deliberately change clients to that
slug. Existing clients continue on the original non-strict route until their
cutover. Older live gateways reject the new release and retain their last
supported snapshot; they cannot dispatch a slug absent from that snapshot.
Drain them before retiring the old route. A newly started old binary refuses
the forward schema and is not a rollback mechanism. Rollback uses a compatible
binary and the retained legacy route, while keeping current key and credential
revocations. A database restore needs its own reviewed cutover plan that
reapplies those revocations; never restore deleted credentials as a shortcut.

The database also rejects an old writer that omits strictness from a new
revision or release. Migration 0027 refuses a pre-existing strict slug with
non-strict publication history because its identity cannot be proven safe.

## Objectives and monitoring

Measure availability and added latency at the client-facing listener. Define
latency objectives for representative unary and streaming workloads, and include
accounting ingestion and recovery when measuring capacity. OLP on-call owns
gateway availability and request-metadata completeness; provider owners own
credentials, quotas, and model availability.

Scrape each in-cluster `*-observability` Service on port 9090 every 15 seconds
and probe `/health/live` and `/health/ready`. The public listener returns 404
for these paths. Readiness snapshots refresh every five seconds and expensive
rollups every fifteen. Page when readiness is absent for five minutes, events
are dropped/abandoned, persistence is unavailable, hard-limited keys cannot
reach Valkey, worker checkpoints are stale, or cost reconciliation cannot
acquire leadership. Warn when request-metadata backlog exceeds its threshold for
ten minutes. The bundled Prometheus rules and per-component ServiceMonitors
provide starting alerts; keep control and gateway alerts separate.

### Distributed tracing

Enable tracing through the
[configuration reference](configuration.md#runtime-variables). In production,
start with a low locally rooted sampling ratio such as `0.01`; raise it for a
bounded investigation with known collector capacity. Keep `1.0` for local
development or short incident windows. Disable inbound trace acceptance where a
trust boundary must start new traces; upstream propagation is independent.

Supply the full OTLP/HTTP endpoint, including `/v1/traces`. Mount exporter
headers as a JSON secret file, using mode `0600` locally or Helm's
`tracing.headersSecretName` and `tracing.headersSecretKey`. Keep credentials out
of values files, environment variables, tickets, and traces. See
[deployment tracing](deployment.md#observability-and-capacity) for collector
network access and secret mounts.

Watch `olp_trace_export_dropped_total`. Export is bounded and asynchronous, so
an unavailable collector does not extend provider latency; sustained drops mean
the collector, network, sampling ratio, or queue budget needs attention. Request
and attempt spans contain only the documented allowlist. Prompt and response
content, tool payloads, raw headers, credentials, and raw provider errors are
prohibited even during incident debugging.

For local exploration, start the development-only Jaeger all-in-one overlay:

```console
docker compose -f deploy/compose.yaml -f deploy/compose.tracing.yaml up -d
```

Open `http://127.0.0.1:16686`. The overlay samples every local trace, exposes
only the UI port, and keeps traces in ephemeral memory. Stop it with the same
two `-f` arguments followed by `down`.

### Replicated worker health

Workers expose the private observability listener for `/health/live`,
`/health/ready`, and `/metrics`; they have no public management or inference
listener. Fleet health comes from PostgreSQL checkpoints, independent of which
replica last performed a task. With Valkey configured, `worker` and `all` run:

- **Media reconciliation:** claims durable video jobs, polls through pinned
  historical credentials, checks current credential revocation before upstream
  calls, records completion/deletion and finishes accounting. Claims survive
  restart and hand off between workers.
- **Request metadata consumer:** uses its own Valkey connection for blocking
  reads. It replays its pending entries before reclaiming idle deliveries,
  persists each event once, then acknowledges and deletes it. Unsupported wire
  versions remain pending. Malformed/invalid payloads and missing deliveries
  become explicit gaps before drainage. An acknowledgement is not an fsync
  guarantee.
- **Gateway epoch detection:** records an unclean gateway exit as a completeness
  gap after two confirming passes.
- **Maintenance:** every 60 seconds, uses a detached PostgreSQL session and
  advisory lock to roll up usage before purging facts, preserving spend
  reconstruction. It expires requests, receipts, audit, gaps, epochs, sessions,
  invitations, replays, and OIDC flows under stored `retention.*` settings.
  Closing the session after each pass releases the lock.
- **Cost reconciliation:** every 60 seconds, repairs current UTC spend windows
  from durable facts using a detached leadership session held between passes.
  Followers record skips; failure, cancellation, or the 120-second pass deadline
  closes the leader session. See [spend recovery](spend-budget-recovery.md).
- **Budget alert delivery:** every 60 seconds under a transaction advisory
  lock, evaluates enabled budget alert rules against the exact accrued window
  totals and posts each crossed threshold once per rule and window to its
  notification destination. Failed deliveries retry on later passes up to five
  attempts with `2^(attempts-1)`-minute backoff. See
  [budget notifications](#budget-threshold-notifications).

The first three tasks become stale after 20 seconds without a successful
checkpoint; maintenance, cost reconciliation, and budget alert delivery after
180 seconds. A skipped follower pass does not establish leader success.
`asynchronous_plane: healthy` requires current expected tasks, a
healthy/backlogged consumer checkpoint, and zero request-metadata pending and
lag counts. A current fleet with pending work is `backlogged`; missing task
evidence is `unknown`. This is metadata drainage, not a claim that every
upstream video job has finished.

Runtime publication is synchronous inside the activation transaction under the
installation row lock (`internal/runtime/publish.go`). The retained readiness
field `runtime_outbox` is `not_configured`; there is no outbox worker, takeover,
or backlog to drain. Monitor runtime-generation convergence and independent
key-authority freshness instead.

Pending metadata is reclaimable after 30 seconds and scanned every five seconds;
investigate if recovery has not begun within 35 seconds. Run three workers
across failure domains in production. Use these content-free signals:

- `olp_request_metadata_consumer_pending_events`, lag, and oldest-pending age;
- reclaimed/recovered and persistence-duplicate counters;
- `olp_worker_task_healthy{task=...}` and
  `olp_worker_task_runs_total{task=...,outcome=...}`.

Worker counters are additive PostgreSQL totals shared by replicas. When summary
reads fail, `olp_async_worker_observability_available` is zero; missing series
are not resets. Reclaims and duplicates show recovery, not necessarily an
incident. Inspect advisory-lock sessions if maintenance or cost repair stalls.
The consumer retries delivery failures internally; its outer process launcher
has no restart supervisor. If it exits (currently only startup misconfiguration
returns an error), correct the cause and restart the process.

`GET /api/v3/auth/capabilities` reports `limits_enforced` and
`retention_enforced` from configured Valkey, not live worker health. Configuring
Valkey without running a worker still reports these flags as true. Use task
checkpoints to confirm retention is running. Without Valkey, `all` starts only
media reconciliation: epoch detection and maintenance also remain stopped,
although readiness still expects their checkpoints and can stay degraded.
Production accounting and retention require Valkey and a worker or `all`
process.

### Spend-budget reconciliation

PostgreSQL is the spend authority; Valkey holds the admission snapshots for keys
and budget groups. The consumer applies cumulative totals, and the worker
reconciles them every minute without lowering valid counters. See
[spend recovery](spend-budget-recovery.md) for initialization, malformed-state
repair, window boundaries, and leader ownership.

Monitor `olp_worker_task_healthy{task="cost_reconciliation"}` and
`olp_worker_task_runs_total{task="cost_reconciliation",outcome=...}`. Rejections
use `olp_key_budget_rejections_total{window="daily|monthly"}`; there is no
per-key Prometheus spend gauge.

1. If reported spend exceeds a limit while requests continue, inspect the
   consumer, PostgreSQL, Valkey, and reconciliation checkpoint. Revoke the
   affected key when continued admission risks overspend.
2. After Valkey loss or malformed state, keep budgeted traffic stopped until
   a successful reconciliation checkpoint and authoritative accrued values
   confirm recovery. Restore the worker; never lower or delete a valid counter.
3. Only if reconciliation cannot run, remove an exact malformed cost hash while
   traffic is stopped. Never use wildcards or delete rate/concurrency keys.
4. Review `unpriced_attempts`. Missing usage or prices means budgets cannot
   account for all spend; repair coverage and retain provider-side quotas.

Key and group budgets always fail closed during Valkey outages, even with
`limits.valkey_unavailable=fail_open`. Do not remove a budget or insert
synthetic zero spend to bypass initialization.

### Budget threshold notifications

`GET/POST /api/v3/notifications/destinations` and
`GET/PATCH /api/v3/notifications/destinations/{id}` manage webhook endpoints;
`GET/POST /api/v3/notifications/rules` and
`GET/PATCH /api/v3/notifications/rules/{id}` manage alert rules, and
`GET /api/v3/notifications/deliveries` lists delivery metadata only.
Installation-wide destinations and rules require settings permission;
project-scoped ones require project-manager access, and a rule's subject (an API
key or budget group) and destination must belong to the same project.

A destination may carry a signing secret: it is write-only, stored encrypted in
the keyring, and never returned by any read. When configured, deliveries sign
the exact request body with HMAC-SHA256 in `X-OLP-Signature: sha256=<hex>`. The
webhook payload is metadata only — the `budget.threshold` event, rule, subject,
window, threshold, accrued, limit, and currency — and never contains prompts,
outputs, or attribution labels. Destination URLs pass the egress policy at
creation and again on every delivery dial; a five-second timeout applies and
responses are drained bounded. Delivery failures persist only a safe category
(`timeout`, `network`, `http_4xx`, `http_5xx`, `invalid_destination`), never
response bodies or raw error text.

The delivery worker runs only where Valkey-backed shared state exists.
`GET /api/v3/auth/capabilities` reports `notifications_active`; when it is
false, destinations and rules still save but nothing is delivered — monitor
`olp_worker_task_healthy{task="budget_alert_delivery"}` and the
`budget_alert_deliveries` status counters for live health.

## Accounting delivery and shutdown

Every request an API key owns produces one content-free metadata event;
playground traffic has no key and is not accounted for. Inference processes
buffer up to 8192 events and write them to the installation stream; the buffer
never blocks a request, and an overflow is counted as loss rather than paid for
in latency. Events carry identifiers, timing, token counts, and per-attempt
evidence only — never prompts, outputs, tool data, or headers.

Management processes serve the results: the usage summary, breakdown, time
series, and completeness endpoints under `/api/v3/usage/`, request listing and
detail under `/api/v3/requests`, pricing revisions under
`/api/v3/pricing/revisions`, and gateway epochs and their acknowledgement under
`/api/v3/request-metadata/gateway-epochs`. Reports mark a partial boundary
bucket as approximate and report what they excluded, and carry gap evidence and
consumer health so incompleteness stays visible after aggregation. Usage
endpoints accept `attribution_key` with an optional `attribution_value` to
restrict rows to labelled usage, and `dimension=attribution` groups the
breakdown by one key's values (the key filter is required and rows without it
are omitted). Request list and detail expose each request's stored labels.
Project-scoped readers see only their own projects' rows in every report.

Pricing can also come from managed sources rather than hand-entered revisions.
`GET/POST /api/v3/pricing/sources` and `GET/PATCH /api/v3/pricing/sources/{id}`
register an external price document; `POST /api/v3/pricing/sources/{id}/refresh`
fetches it through the egress policy (bounded size, JSON schema, redirect
validation), stores an immutable SHA-256-keyed snapshot, and returns a diff
against the latest published revision without publishing anything.
`GET /api/v3/pricing/sources/{id}/snapshots` lists retained snapshots and
`POST /api/v3/pricing/source-snapshots/{id}/publish` mints a new immutable
pricing revision from one snapshot, optionally merged with scoped per-entry
overrides. A source is advisory: negotiated rates need publish-time overrides,
because the published revision — not the raw source document — is what
accounting prices against. Revisions record their source name and snapshot for
provenance, and all entries share the installation's single pricing currency.

Shutdown stops the listeners and drains their handlers first, then closes
metadata intake and gives the writer a bounded opportunity to flush the buffer.
Only afterwards are delivery and worker contexts cancelled. An expired flush
budget records undelivered events as loss; a forced HTTP shutdown leaves the
gateway epoch open for detection because handlers may still emit metadata. A
clean drain closes the epoch against what was actually delivered. HTTP,
metadata, delivery, workers and trace flushing share `OLP_SHUTDOWN_TIMEOUT` (30
seconds by default). Forced closure records uncertainty instead of extending the
deployment termination budget.

## Shared state in Valkey

Every key is prefixed with the installation namespace
`olp:go:v1:<installation>:`, so installations sharing one Valkey service never
read, acknowledge, or reconcile one another's state.

| Key | Contents |
| --- | --- |
| `<prefix>limits:{<lookup>}:rate` | Request and token windows for one lookup. |
| `<prefix>limits:{<lookup>}:concurrency:v2` | Concurrency leases for one lookup. |
| `<prefix>limits:{<cost owner>}:cost:day` and `:cost:month` | Current UTC spend windows for an API-key or budget-group UUID. |
| `<prefix>limits:provider-cooldown:<scope>` | Credential-version and slot cooldowns. |
| `<prefix>request-metadata` | The request metadata stream, read by consumer group `olp:persistence`. |

A lookup is the key's lookup identifier, `pc_<provider uuid>` for a connection,
or `ps_<slot uuid>` for a credential slot; the braces are the cluster hash tag,
so one key's dimensions stay on one slot. Cost keys are tagged by the API key or
budget-group UUID, so key rotation preserves spend and group members share one
balance.

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
   record the direct peer address and a coarse user-agent family. Authentication admission resolves trusted proxy headers,
   while audit retains the direct peer. The full user-agent is never stored, and
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
may continue serving its last complete generation while a new snapshot is being
compiled, but it must not accept a partially indexed generation. Check the audit
stream for activation, route-permission, credential, and key changes before
attributing a provider error to the rollout; `occurred_after` and
`occurred_before` bound that page to the rollout window. Keep provider probe
failures separate from gateway admission failures so an upstream outage does not
hide a local capacity regression.

Never put prompts, outputs, raw headers, credentials, sessions, proxy-key
secrets, or master keys in tickets or diagnostic bundles.

### OpenAI-compatible capability certification

Follow the [provider lifecycle](provider-routing.md#provider-lifecycle): review
exact tuples, run bounded server certification, and activate only supported,
certified capabilities. Re-certify after transport or semantic changes;
credential rotation requires fresh validation. Keep probe failures separate from
gateway admission failures when investigating an incident.

## Backup and restore

For a production recovery point:

1. Stop new inference admission and control writes, leave workers running,
   and wait for admitted work, pending acknowledgements, and Stream lag to
   drain. Keep traffic fenced until the backup finishes.
2. On an encrypted volume with PostgreSQL 18 client, `jq`, and GNU `sha256sum`,
   run `scripts/backup.sh` with `OLP_DATABASE_URL` and
   `OLP_BACKUP_TRAFFIC_QUIESCED=true`.

The script requires a zero, at-most-30-second-old durable checkpoint and an
explicit quiescence assertion. It exports one PostgreSQL snapshot and creates an
`olp-go-v1` manifest containing the checksum, installation identity, migration
count, and runtime generation. Only the `olp_go` schema is backed up.

The dump contains password hashes, session and API-key digests, and encrypted
provider/OIDC credentials. Keep master-key rings and authentication HMAC files
in the secret manager and back them up separately. Retain historical master-key
versions while records still reference them.

Run `scripts/restore.sh BACKUP` using the dump path printed by the backup
script, with `OLP_RESTORE_DATABASE_URL` identifying an empty isolated database.
Set `OLP_RESTORE_VALKEY_ISOLATED=true`, point `OLP_VALKEY_URL` at a separate
empty Valkey service, and mount the original master and auth key files. The
restore role needs CREATEDB: the command first restores to a disposable staging
database, verifies the manifest/checksum/history/identity, applies supported Go
migrations, and authenticates every encrypted record with `olp doctor`. Only
then does one transaction recheck and populate the empty destination. Failure
removes staging and leaves the destination unchanged. Set `OLP_MAINTENANCE_BIN`
to the qualified binary when using a nondefault path. Start the restored
installation with its original keys and a fresh Valkey service. A restored
installation retains its namespace; run it as a replacement, or isolate its
Valkey service from the source installation.

## Installation and upgrades

Go does not upgrade Rust 2.x or Rust 3.x storage. Back up the existing
installation with its own version, provision independent Go storage and secrets,
and verify providers, routes, permissions, SDK requests, usage, and recovery
before redirecting traffic. Rust schemas are refused before any Go objects are
created.

For subsequent 3.x releases, review forward-only migrations, rehearse against an
isolated 3.0 backup, quiesce new inference and mutations, and drain accounting.
Take the final snapshot while workers still supply a fresh checkpoint, then stop
workers for migration. Run `olp migrate` once and roll out all process modes
before resuming admission. Verify readiness, generation convergence, backlog,
usage completeness, provider probes, and latency. Restore the saved database and
keys into a replacement installation if rollback requires an older schema.

Delivery and replay evidence is retained for seven days plus five minutes of
clock-skew grace. A late entry is recorded as uncertain completeness, never
silently counted twice. Size PostgreSQL for up to `sustained_requests_per_second
* 604800` receipt rows. During a delivery incident, restore/reconcile the Stream
within seven days; do not extend the window by suspending maintenance.

Migrations are forward-only; never edit migration history or checksums.

## Database deadlines and privileges

Go pool connections set a ten-second statement deadline, ten-second lock wait
and fifteen-second idle-transaction deadline. Commands additionally obey the
startup/dependency deadlines in the configuration reference. Backup should use a
dedicated read role with access to migration history and all backed-up tables
rather than sharing the runtime login. Large maintenance/export operations
should use their own role and explicitly chosen deadlines, not an unlimited
interactive account. PostgreSQL classifies statement cancellation as `57014` and
lock expiry as `55P03`; a timeout does not imply a committed mutation.

Use separate migration-owner and runtime logins as described in
[database roles](access.md#deployment-and-database-roles). After migration,
grant runtime access with `olp migrate --runtime-role olp_runtime` or run
`scripts/grant-runtime-database-role.sql` as the owner with
`psql -v runtime_role=olp_runtime`. The runtime role receives feature-table DML,
including installation-row updates, and read-only migration history. Reapply
grants after migrations; never give it migration-owner membership or CREATE
privileges. Production Helm needs both runtime and migration URL Secrets.

## Metric aggregation and incidents

Database-derived request counts, provider samples, worker counters and metadata
summaries are shared installation state exported by several replicas. Choose one
current exporter or use `max` across replicas of the **same installation**; do
not sum duplicated global values. Add a stable installation label at scrape time
if Prometheus monitors more than one installation. Rolling five/fifteen minute
values are gauges, not monotonic request counters. Never sum or average
precomputed p95/p99 values into a fleet percentile. Provider quantitative series
use stable provider IDs; names/status remain on the descriptive health series.
No sampled attempts means no success/latency series, not measured 100% success.

`olp_provider_metrics_complete=0` means collection failed or exceeded its
10,000-provider cap. Check `olp_provider_metrics_emitted` and
`olp_observability_metrics_snapshot_fresh`; a refresh exceeding four seconds
retains the last successful snapshot and exposes its age. Do not interpret stale
data as current health. Investigate database latency before increasing
cardinality. Process-local admission and trace-drop counters may be summed
across replicas; retain each counter's reset semantics when using `rate`.

Compare `olp_runtime_desired_generation` with `olp_runtime_generation` on each
gateway. A gap identifies a pending/rejected observed generation. Check
`olp_api_key_authority_age_seconds` separately: provider activation failure must
not hide successful authority refresh. At 60 seconds, new key-authenticated
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
Treat a suspected disaster interval as incomplete, stop budgeted traffic at the
edge, and reconcile provider billing before declaring accounting complete. For a
tighter RPO, qualify synchronous persistence and the actual replica/failover
policy on the deployment's storage; changing fsync alone is not a fleet
guarantee.

Logical backup requires externally stopping new inference and control writes,
waiting for admitted work and accounting to drain, then retaining that fence
until export completes. The environment flag is an operator assertion, not a
server admission fence. Verify the load balancer/ingress configuration and all
replicas; a fresh zero backlog alone does not prove quiescence. Export has
bounded database waits and a dump timeout (`OLP_BACKUP_TIMEOUT_SECONDS`, default
600). New backups publish their dump and manifest together by atomic directory
rename; hidden partial directories are incomplete. Manifests record migration
versions/checksums and the script checkout's application version. Supply
`OLP_BACKUP_APP_VERSION` and `OLP_BACKUP_IMAGE_DIGEST` for the producing
deployment when it differs from that checkout. Restore verifies included
migration hashes against the local migration files before writing the empty
destination.

Store backups with authenticated encryption, independent off-site retention and
audited access. Keep master-key recovery material in a separate recovery process
with tested key holders. Checksums detect corruption, not malicious replacement
of both a dump and its manifest. Rehearse restoration from the actual off-site
store, not just a local copy.

For PostgreSQL point-in-time recovery, use the managed service's tested PITR
procedure or
[continuous WAL archiving](https://www.postgresql.org/docs/18/continuous-archiving.html)
with base backups, a restore target and verified WAL continuity. Record the
service's measured RPO/RTO; neither this repository nor a logical dump promises
one. Fence traffic first, restore to an isolated replacement, preserve the
installation identity/keyring, and use isolated Valkey. Reconcile external
provider/media outcomes and the database/queue time skew before reopening
budgeted traffic. Never let a rehearsal consume the source's streams or leases.
`make integration` performs a logical restore, authenticates, decrypts a
retained provider credential by making a request, and verifies historical
accounting was preserved and new accounting added. It does not simulate storage
power loss or certify a managed service's PITR implementation.
