# Operations runbook

Availability, monitoring, recovery, version, incident, and key-rotation
procedures for production OpenLLMProxy. Keep this runbook with the deployed
release; deployment topology is in [`deployment.md`](deployment.md).

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
- **Grant refresh:** every five seconds, refreshes the
  [grants](plugins.md#grant-refresh) that are due through their plugins, each
  under its own advisory lock so a rotating refresh token is spent once. A
  refresh that fails permanently [lapses](plugins.md#lapsed-grants) the grant,
  and a grant that no provider configuration uses any more is
  [retired](plugins.md#grant-refresh) instead of refreshed. A refresh the
  upstream answered is recorded even when the worker is shutting down.
- **Request metadata consumer:** uses its own Valkey connection for blocking
  reads. It replays its pending entries before reclaiming idle deliveries,
  persists each event once, then acknowledges and deletes it. Events without
  the current wire version, malformed or invalid payloads, and missing
  deliveries become explicit gaps before drainage. An acknowledgement is not an
  fsync guarantee. A consumer persists one event at a time, about 150 a second on
  a development machine; a request rate above what the workers ingest leaves
  events queued in the stream, late and not lost, until more workers or a lull
  catches up, which `olp_request_metadata_consumer_lag_events` shows (see
  [performance](performance.md)).
- **Gateway epoch detection:** records an unclean gateway exit as a completeness
  gap after two confirming passes.
- **Maintenance:** every 60 seconds, uses a detached PostgreSQL session and
  advisory lock to roll up usage before purging facts, preserving spend
  reconstruction. It expires requests, receipts, audit, gaps, epochs, sessions,
  invitations, replays, and OIDC flows under stored `retention.*` settings.
  Closing the session after each pass releases the lock.
- **Cost reconciliation:** every 60 seconds, repairs current UTC spend windows
  from durable facts. See [spend-budget reconciliation](#spend-budget-reconciliation)
  for initialization, leadership and recovery.
- **Notification delivery:** every 60 seconds under a transaction advisory
  lock, evaluates enabled budget threshold rules against the exact accrued
  window totals and claims each crossed threshold once per rule and window, then
  posts every pending delivery, including [grant lapses](#notifications), to
  its rule's notification destination. Each attempt is recorded before it is
  made, so replicas never repeat one. Failed deliveries retry on later passes up
  to five attempts with `2^(attempts-1)`-minute backoff. See
  [notifications](#notifications).

Media reconciliation, grant refresh, the consumer and epoch detection become
stale after 20 seconds without a successful checkpoint; maintenance, cost
reconciliation, and notification delivery after 180 seconds. Media
reconciliation and grant refresh also run without Valkey. A skipped follower
pass does not establish leader success.
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

`GET /api/v1/auth/capabilities` reports `limits_enforced` and
`retention_enforced` from configured Valkey, not live worker health. Configuring
Valkey without running a worker still reports these flags as true. Use task
checkpoints to confirm retention is running. Without Valkey, `all` starts only
media reconciliation and grant refresh: epoch detection and maintenance also
remain stopped, although readiness still expects their checkpoints and can stay
degraded. Production accounting and retention require Valkey and a worker or
`all` process.

### Spend-budget reconciliation

PostgreSQL is the spend authority; Valkey holds admission snapshots for each
configured key and budget-group daily/monthly window. The terminal accounting
consumer and the reconciliation worker publish cumulative PostgreSQL totals.
Only these paths initialize cost hashes; admission never creates a zero counter.

New keys, expired UTC windows, missing hashes and malformed or wrong-window
state return `503 distributed_limits_unavailable` until an authoritative
snapshot arrives. With a healthy worker, initialization normally waits for the
next minute's reconciliation pass. Known exhausted windows return HTTP 429
`budget_exhausted`. Cost budgets always fail closed, including during Valkey
outages with `limits.valkey_unavailable=fail_open`; rate/concurrency-only keys
keep their configured outage behavior. Never remove a budget or insert
synthetic zero spend to bypass initialization.

Reconciliation replaces malformed hashes (including non-integer `unpriced`
fields or non-hash values) from matching authoritative snapshots, without
lowering the other window's valid counter. Stale or future snapshots cannot
initialize a different current UTC window. Daily aggregation uses
`[daily_start, daily_end)`, so accepted clock-skewed events for tomorrow do not
exhaust today. Inflated but otherwise valid counters require reviewed data
repair; never reset them to zero or bypass migration-history/checksum checks.

One dedicated PostgreSQL session holds the reconciliation advisory lock between
minute ticks and performs monthly reconstruction on that same session. Followers
record skipped passes. Error, shutdown, cancellation or the 120-second pass
deadline closes the leader session, releasing leadership; a connection holding
that session lock is never returned to the pool. Repeated timeouts require
addressing scan/application load before enabling budgeted traffic. A follower's
skip is not evidence of successful reconciliation.

Monitor `olp_worker_task_healthy{task="cost_reconciliation"}` and
`olp_worker_task_runs_total{task="cost_reconciliation",outcome=...}`. Rejections
use `olp_key_budget_rejections_total{window="daily|monthly"}`, which counts both a
budget that is exhausted and one whose room cannot hold the request's estimated
cost; there is no per-key Prometheus spend gauge.

1. For persistent initialization 503s or spend exceeding a limit while requests
   continue, inspect the consumer, PostgreSQL, Valkey, their clocks, and the
   reconciliation checkpoint. Revoke the affected key when continued admission
   risks overspend.
2. After Valkey loss or malformed state, keep budgeted traffic stopped until
   a successful reconciliation checkpoint and authoritative accrued values
   confirm recovery. Restore the worker; never lower or delete a valid counter.
3. Only if reconciliation cannot run, remove an exact malformed cost hash while
   traffic is stopped. Never use wildcards or delete rate/concurrency keys.
4. Review `unpriced_attempts`. Missing usage or prices means budgets cannot
   account for all spend; repair coverage and retain provider-side quotas.

Admission measures these thresholds with exact decimal arithmetic and UTC
boundaries against accrued spend plus the estimated cost of requests in flight
([cost reservation](gateway.md#cost-reservation)), which is not a reserved
invoice cap: unpriced attempts accrue no money, operations whose cost is unknown
beforehand reserve nothing, and spend can pass a limit by what the estimates
under-counted. A reservation that accounting never removes lapses at the route
deadline plus five minutes, and until it does it counts beside the accrued spend,
so a stalled consumer shows as extra `429 budget_exhausted` before it shows as
spend. Each reservation or settlement retires at most a bounded page of lapsed
reservations, so a large backlog is worked through over the calls that follow
and keeps counting until then; the message of a refusal says whether the budget
is exhausted or the request's estimate does not fit beside what is spent and in
flight. A current-window hash does not prove attribution is current; no maximum
lag or monetary overshoot is measured. The keys that hold reservations, `cost:pending`
and `cost:expiry`, are advisory and derived, so deleting them while traffic is
stopped loses only the protection for requests in flight, and a reservation that
finds one of the two without the other discards both; the accrued `cost:day`
and `cost:month` hashes remain the authority and are never lowered or deleted
for this. During a rolling upgrade a replica still running the previous release
does not remove reservations made by a newer one, which then lapse on their own.
See the [limits](../tests/integration/limits_test.go),
[reservation](../tests/integration/limits_reservation_test.go) and
[fleet recovery](../tests/integration/fleet_recovery_test.go) tests for evidence.

### Notifications

`GET/POST /api/v1/notifications/destinations` and
`GET/PATCH /api/v1/notifications/destinations/{id}` manage webhook endpoints;
`GET/POST /api/v1/notifications/rules` and
`GET/PATCH /api/v1/notifications/rules/{id}` manage rules, and
`GET /api/v1/notifications/deliveries` lists delivery metadata only. A rule
subscribes a destination to one `event`, which never changes:

- `budget.threshold`: an API key's or budget group's accrued spend reached the
  rule's `threshold_percent` of its limit in the current UTC `day` or `month`
  window. A rule fires once per window. Installation-wide destinations and rules
  require settings permission; project-scoped ones require project-manager
  access, and a rule's subject and destination must belong to the same project.
- `provider.grant.lapsed`: a provider plugin's [grant lapsed](plugins.md#lapsed-grants).
  Provider events concern the whole installation: their rules take no subject,
  window or threshold, are installation-wide with an installation-wide
  destination, and require settings permission; a destination is subscribed to
  them once. The worker that records a lapse enqueues, in the same transaction,
  exactly one delivery for each enabled rule whose destination is enabled. A
  retired grant, which nothing used, is not a lapse to notify.

A destination may carry a signing secret: it is write-only, stored encrypted in
the keyring, and never returned by any read. When configured, deliveries sign
the exact request body with HMAC-SHA256 in `X-OLP-Signature: sha256=<hex>`.
Destination URLs pass the egress policy at creation and again on every delivery
dial; a five-second timeout applies and responses are drained bounded. Delivery
failures persist only a safe category (`timeout`, `network`, `http_4xx`,
`http_5xx`, `invalid_destination`), never response bodies or raw error text. Deliveries
for a disabled rule or destination wait, unsent, until both are enabled again.

Webhook payloads are metadata only, and the management contract documents both
under `webhooks`. A `budget.threshold` payload names the rule, subject, window,
threshold, accrued, limit, and currency, never prompts, outputs, or attribution
labels. A `provider.grant.lapsed` payload reports the lapse as it was when the
grant lapsed:

```json
{
  "event": "provider.grant.lapsed",
  "rule_id": "0199…",
  "rule_name": "Lapsed grants",
  "provider_id": "0199…",
  "provider_name": "Team account",
  "credential_version_id": "0199…",
  "credential_version": 3,
  "credential_slots": [{ "id": "0199…", "name": "default" }],
  "observed_principal": "operator@example.com",
  "lapsed_at": "2026-09-27T12:00:00.000000+00:00"
}
```

`credential_slots` are the provider's slots bound to the credential version in
its draft or active revision, usually one; re-enroll their grant. The payload
never carries secret material: no access or refresh token, grant facts, or the
refresh failure that lapsed the grant.

The delivery worker runs only where Valkey-backed shared state exists.
`GET /api/v1/auth/capabilities` reports `notifications_active`; when it is
false, destinations and rules still save but nothing is delivered — monitor
`olp_worker_task_healthy{task="notification_delivery"}` and the
`notification_deliveries` status counters for live health.

## Accounting delivery and shutdown

Every request an API key owns produces one content-free metadata event;
playground traffic has no key and is not accounted for. Inference processes
buffer up to 8192 events and write them to the installation stream; the buffer
never blocks a request, and an overflow is counted as loss rather than paid for
in latency. Events carry identifiers, timing, token counts, and per-attempt
evidence only — never prompts, outputs, tool data, headers, credentials,
cookies, or uploads. Provider names, route slugs and labels are metadata; keep
secrets out of them.
[Provider-retained content](compatibility.md#files-batches-realtime-and-provider-retained-state)
is governed separately from diagnostics.

The stream carries JSON in one `event` field with `version: 1`. Missing or
different versions, malformed payloads and permanently invalid records become
`malformed_stream_event` gaps instead of being interpreted as another format.
See [metadata tests](../internal/usage/) for the persistence contract.

Management processes serve the results: the usage summary, breakdown, time
series, and completeness endpoints under `/api/v1/usage/`, request listing and
detail under `/api/v1/requests`, pricing revisions under
`/api/v1/pricing/revisions`, and gateway epochs and their acknowledgement under
`/api/v1/request-metadata/gateway-epochs`. Reports mark a partial boundary
bucket as approximate and report what they excluded, and carry gap evidence and
consumer health so incompleteness stays visible after aggregation. Usage
endpoints accept `attribution_key` with an optional `attribution_value` to
restrict rows to labelled usage, and `dimension=attribution` groups the
breakdown by one key's values (the key filter is required and rows without it
are omitted). Request list and detail expose each request's stored labels.
Project-scoped readers see only their own projects' rows in every report.

Each attempt also records the admission estimate of its input, how it was
produced (`tokenizer`, `calibrated` or `heuristic`) and the tokenizer family it
was counted for; [how the gateway estimates](gateway.md#how-the-prompt-is-estimated)
describes the three methods. Reports total `estimated_input_tokens` beside
`reported_input_tokens` over only the attempts that had both an estimate and
reported input usage, so the two compare directly: the estimation error is
estimated minus reported over reported, positive when admission over-estimated.
Routes show it through the route breakdown, and `dimension=model_family` and
`dimension=estimate_provenance` group it by family and by method. An attempt
that was never estimated (a stored-response call, a realtime session, a job
poll, an upload or a video creation) has no provenance and appears under `none`,
and it still has its model's family, so it is in the family's row without
adding to its error; `unknown` is only the attempts recorded before families
were. The console usage page shows the signed error in its totals and in the
breakdown table.

Pricing can also come from managed sources rather than hand-entered revisions.
`GET/POST /api/v1/pricing/sources` and `GET/PATCH /api/v1/pricing/sources/{id}`
register an external price document; `POST /api/v1/pricing/sources/{id}/refresh`
fetches it through the egress policy (bounded size, JSON schema, redirect
validation), stores an immutable SHA-256-keyed snapshot, and returns a diff
against the latest published revision without publishing anything.
`GET /api/v1/pricing/sources/{id}/snapshots` lists retained snapshots and
`POST /api/v1/pricing/source-snapshots/{id}/publish` mints a new immutable
pricing revision from one snapshot, optionally merged with scoped per-entry
overrides. A source is advisory: negotiated rates need publish-time overrides,
because the published revision — not the raw source document — is what
accounting prices against. Revisions record their source name and snapshot for
provenance, and all entries share the installation's single pricing currency.
No list price applies to a [plugin provider](plugins.md#providers-from-plugin-profiles):
its attempts stay unpriced until a revision carries a price scoped to that
provider, and a `plugin` price must name its `provider_id`.

Shutdown stops the listeners and drains their handlers first, then closes
metadata intake and gives the writer a bounded opportunity to flush the buffer.
Only afterwards are delivery and worker contexts cancelled. An expired flush
budget records undelivered events as loss; a forced HTTP shutdown leaves the
gateway epoch open for detection because handlers may still emit metadata. A
clean drain closes the epoch against what was actually delivered. HTTP,
metadata, delivery, workers and trace flushing share `OLP_SHUTDOWN_TIMEOUT` (30
seconds by default). Forced closure records uncertainty instead of extending the
deployment termination budget.

Delivery and replay evidence is retained for seven days plus five minutes of
clock-skew grace. A late entry is recorded as uncertain completeness, never
silently counted twice. Size PostgreSQL for up to
`sustained_requests_per_second * 604800` receipt rows. During a delivery
incident, restore/reconcile the Stream within seven days; do not extend the
window by suspending maintenance.

### Queue durability

A Valkey acknowledgement is not an fsync guarantee. Compose AOF `everysec` may
lose roughly one second of recently acknowledged metadata on power/storage
loss; replication and failover can add loss. Surviving epochs, gaps and pending
counts expose known incompleteness, but cannot reconstruct lost facts from
counters or enumerate facts lost with their evidence.

Treat a suspected disaster interval as incomplete, stop budgeted traffic at the
edge, and reconcile provider billing before declaring accounting complete. For a
tighter RPO, qualify synchronous persistence and the actual replica/failover
policy on the deployment's storage; changing fsync alone is not a fleet
guarantee. See [Valkey persistence](https://valkey.io/topics/persistence/) and
the [replica and recovery suites](../tests/integration/).

## Shared state in Valkey

Every key is prefixed with the installation namespace
`olp:<installation>:`, so installations sharing one Valkey service never
read, acknowledge, or reconcile one another's state.

| Key | Contents |
| --- | --- |
| `<prefix>limits:{<lookup>}:rate` | Request and token windows for one lookup. |
| `<prefix>limits:{<lookup>}:concurrency` | Concurrency leases for one lookup. |
| `<prefix>limits:{<cost owner>}:cost:day` and `:cost:month` | Current UTC spend windows for an API-key or budget-group UUID. |
| `<prefix>limits:{<cost owner>}:cost:pending` and `:cost:expiry` | Cost reserved by requests in flight against that owner: a hash of each request's amount and their total, and the set of when each lapses. Advisory; absent when nothing is in flight. |
| `<prefix>limits:provider-cooldown:<scope>` | Credential-version and slot cooldowns. |
| `<prefix>request-metadata` | The request metadata stream, read by consumer group `olp:persistence`. |

A lookup is the key's lookup identifier, `pc_<provider uuid>` for a connection,
or `ps_<slot uuid>` for a credential slot; the braces are the cluster hash tag,
so one key's dimensions stay on one slot. Cost keys are tagged by the API key or
budget-group UUID, so key rotation preserves spend and group members share one
balance.

The gateway runs its Lua scripts by SHA-1 digest and sends a script's source only
to a Valkey that answers it holds none, which happens once after a restart, a
failover or `SCRIPT FLUSH`, and is cached from then on. Flushing the script cache
is therefore safe; it costs one slower call per script.

## Routine checks

1. Confirm pod readiness and one nonzero runtime generation across gateways.
2. Check PostgreSQL replication, WAL archiving, disk headroom, and backup age;
   check Valkey latency, memory and AOF durability (it holds metadata awaiting
   database ingestion).
3. Review usage completeness and pricing coverage before exporting costs.
   Missing upstream usage is incomplete and unpriced, never zero.
4. Review provider health, authentication, role/key changes, credential
   rotations, and route activations in the audit stream. `GET /api/v1/audit`
   narrows a page by `action`, `resource_type`, `resource_id`,
   `actor_user_id`, `outcome`, `occurred_after`, and `occurred_before`, so
   each category can be reviewed on its own. Session-driven actions also
   record the direct peer address and a coarse user-agent family. Authentication
   admission resolves trusted proxy headers, while audit retains the direct
   peer. The full user-agent is never stored, and background maintenance and
   reconciliation events leave both empty.
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

### Create a backup

1. Stop new inference admission and control writes, leave workers running,
   and wait for admitted work, pending acknowledgements, and Stream lag to
   drain. Keep traffic fenced until the backup finishes.
2. On an encrypted volume with PostgreSQL 18 client, `jq`, and GNU `sha256sum`,
   run `scripts/backup.sh` with `OLP_DATABASE_URL` and
   `OLP_BACKUP_TRAFFIC_QUIESCED=true`.

The script requires a zero, at-most-30-second-old durable checkpoint. The
quiescence flag is an operator assertion, not a server admission fence: verify
the load balancer/ingress configuration and all replicas. A fresh zero backlog
alone does not prove quiescence.

The script exports one PostgreSQL snapshot of the `olp` schema. Its manifest
records format/schema markers `olp`, dump checksum, installation identity,
migration count and versions/checksums, runtime generation, and the script
checkout's application version. Set `OLP_BACKUP_APP_VERSION` and
`OLP_BACKUP_IMAGE_DIGEST` for the producing deployment when it differs from that
checkout. Export has bounded database waits and a dump timeout
(`OLP_BACKUP_TIMEOUT_SECONDS`, default 600). Dump and manifest publish together
by atomic directory rename; hidden partial directories are incomplete.

The dump contains password hashes, session/API-key digests and encrypted
provider/OIDC credentials. Store it with authenticated encryption, independent
off-site retention and audited access. Back up master-key rings and
authentication HMAC files separately through a secret manager and tested key
holders; retain historical key versions while records or backups need them.
Checksums detect corruption, not malicious replacement of both dump and
manifest. Rehearse restoration from the actual off-site store.

### Restore a replacement

Run `scripts/restore.sh BACKUP` using the dump path printed by the backup
script, with `OLP_RESTORE_DATABASE_URL` identifying an empty isolated database.
Set `OLP_RESTORE_VALKEY_ISOLATED=true`, point `OLP_VALKEY_URL` at a separate
empty Valkey service, and mount the original master and auth key files. Set
`OLP_MAINTENANCE_BIN` to the qualified binary when using a nondefault path.

The restore role needs CREATEDB. The command restores to a disposable staging
database, verifies the manifest/checksum/history/identity and included migration
hashes against local files, applies the binary's migrations, and authenticates
every encrypted record with `olp doctor`. Only then does one transaction recheck
and populate the empty destination. Failure removes staging and leaves the
destination unchanged.

Start the replacement with its original keys and isolated Valkey service.
Recovery preserves the installation UUID, namespace and historical references;
never let a rehearsal consume the source's streams or leases. An independently
writable database clone is unsupported. For independent use, create a fresh
installation and recreate nonsecret configuration through
[configuration promotion](configuration.md#configuration-promotion-artifacts).

### Recovery qualification

For PostgreSQL point-in-time recovery, use the managed service's tested PITR
procedure or
[continuous WAL archiving](https://www.postgresql.org/docs/18/continuous-archiving.html)
with base backups, a restore target and verified WAL continuity. Record the
service's measured RPO/RTO; neither this repository nor a logical dump promises
one. Fence traffic first, restore to an isolated replacement, preserve the
installation identity/keyring, and use isolated Valkey. Reconcile external
provider/media outcomes and database/queue time skew before reopening budgeted
traffic; see [queue durability](#queue-durability).

`make integration` performs a logical restore, authenticates, decrypts a retained
provider credential by making a request, and verifies historical accounting was
preserved and new accounting added. It does not simulate storage power loss or
certify a managed service's PITR implementation. See the
[recovery journey](../console/tests/journeys/recovery.spec.ts) and
[process suites](../tests/integration/).

## Master-key rotation and recovery

1. Retain every key version still in use, add a higher version, and select it as
   `active_version` in the private ring file. Take a
   [drained database backup](#backup-and-restore) and retain its
   required key material separately.
2. Stop management writers for a controlled maintenance window, mount the new
   ring, and run `olp master-key reencrypt` using the same database and auth key.
3. Rerun the command after an interruption. It commits batches of 100 records;
   authenticated old records remain readable with the retained keys. A stale
   process cannot write ciphertext using the retired active version. Keep the
   same key material for each version: every run authenticates existing
   destination-version records before committing any batches.
4. Run `olp master-key status` and `olp doctor`. They authenticate stored
   ciphertext and emit only installation/version/count metadata. Remove an old
   key only after `olp master-key verify-retirement VERSION` succeeds and backup
   retention permits removal. `olp master-key reencrypt --dry-run` authenticates
   records without changing them.
5. Restart all processes that decrypt credentials with the new ring. Existing
   sessions and API credentials retain their HMAC identity; encrypted replays retain
   their original response. Rotation records a metadata-only audit event.

## Installation and versions

Each installation starts from an empty PostgreSQL database and uses its own
Valkey namespace. During 0.x, OLP makes no compatibility, upgrade,
mixed-version or rollback promises: any release may change the management API,
configuration and storage, and a release may require a fresh installation.

Run one version across every process mode. To run a new release against an
existing database, take a [drained backup](#backup-and-restore), stop every
process, run `olp migrate` once with the new binary, and start all process
modes on that version. With Helm, scale the gateway, control and worker
Deployments to zero before `helm upgrade`; its pre-upgrade hook runs the
migration Job before the new pods start. Verify readiness, generation
convergence, backlog, usage completeness, provider probes, and latency before
resuming admission. There is no rollback: a binary refuses a database whose
schema is newer than its own, and migrations never run in reverse. Never edit
migration history or checksums. The runner requires a sequential history prefix
with matching checksums and rolls back failed migration transactions;
[integration tests](../tests/integration/) interrupt DDL to verify recovery.

## Database deadlines and privileges

Pool connections set a ten-second statement deadline, ten-second lock wait
and fifteen-second idle-transaction deadline. Commands additionally obey the
startup/dependency deadlines in the configuration reference. Backup should use a
dedicated read role with access to migration history and all backed-up tables
rather than sharing the runtime login. Large maintenance/export operations
should use their own role and explicitly chosen deadlines, not an unlimited
interactive account. PostgreSQL classifies statement cancellation as `57014` and
lock expiry as `55P03`; a timeout does not imply a committed mutation.

Provision separate migration-owner and runtime logins, and reapply runtime
grants after migrations, following [database roles](deployment.md#database-roles).

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
cardinality. Process-local admission, trace-drop and Go allocation counters
(`go_memstats_mallocs_total` and `go_memstats_alloc_bytes_total`, read live from
the runtime) may be summed across replicas; retain each counter's reset
semantics when using `rate`.

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
