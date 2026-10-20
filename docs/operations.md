# Operations runbook

Availability, monitoring, recovery, version, incident, and key-rotation
procedures for production OpenLLMProxy. Keep this runbook with the deployed
release; deployment topology is in [`deployment.md`](deployment.md).

## Commitment accounting recovery

Connection disablement stops traffic while retaining published commitments.
Check the existing FOCUS dataset view for purchased, allocated and unused totals,
provisional state and incomplete coverage. Pending edits apply at UTC period
boundaries; removing a draft amount does not cancel the current obligation.

Migration 0073 recovers supported history from published runtime snapshots and
retained facts/hourly evidence. The FOCUS worker catches up in bounded batches;
incomplete historical or partial export coverage is reported explicitly. Keep
`commitment_schedules`, `commitment_periods` and `commitment_usage` in the normal
whole-schema backup. Allocation context remains available for the lifetime of retained hourly
reporting, including periods outside the FOCUS export window. Restore preserves the originating installation history;
configuration promotion carries desired settings, and the destination establishes
its own obligations when it publishes. Do not copy period rows during promotion.

Late facts restate affected days under the existing delivery lease and retry
rules. Read the manifest last, deduplicate by day and revision, and replace the
previous dataset revision. Never move an out-of-window purchase forward to make
a partial export appear complete. See [FinOps](finops.md#commitments).

## Objectives and monitoring

Measure public-listener availability/added latency and accounting/recovery
capacity. Gateway operators own availability and metadata completeness; provider
owners own credentials, quotas and upstream model availability.

Scrape private port 9090 every 15 seconds; public health/metrics paths return 404.
Readiness refreshes every five seconds, expensive rollups every fifteen. Page on
five minutes without readiness, metadata loss/persistence failure, unavailable
hard limits or stale worker/cost checkpoints. Warn on ten-minute backlog.
Bundled Prometheus rules/ServiceMonitors are starting points, not an SLO guarantee.

### Distributed tracing

Use [tracing configuration](configuration.md#runtime-variables) with a full
OTLP/HTTP `/v1/traces` endpoint and private JSON header file. Start production
sampling low (for example `0.01`); inbound trust and upstream propagation are
independent. Observe `olp_trace_export_dropped_total`: bounded async export never
extends provider latency. Only allowlisted content-free attributes are permitted,
including reviewed OTel `gen_ai.*` schema 1.41.0 metadata.

Development-only Jaeger:

```console
docker compose -f deploy/compose.yaml -f deploy/compose.tracing.yaml up -d
```

Open `http://127.0.0.1:16686`; memory-only local traces use full sampling. Stop
with the same files and `down`. Never add bodies/tools/raw headers/errors/secrets
to production traces or incident bundles.

### Export sinks, payload capture, and business metrics

[Export sinks](export-sinks.md) deliver durable content-free request/attempt/usage/
guardrail/audit events at least once. Receivers deduplicate immutable event IDs;
object storage uses stable `stream/YYYY-MM-DD/event_id.jsonl` keys. Two-minute
claims commit before network calls; retries back off one minute–one hour and
20-second rotating passes prevent sink starvation. Retention expiry creates
`olp.export_gaps`/`olp_export_gap_total`, not proof the remote never received data.
Monitor pending, lag and delivery metrics. Remote outages do not make the task
unhealthy; failed durable claim/outcome/checkpoint writes do.

Destination changes require explicit replacement/removal of write-only credentials.
GCS/Azure tokens stay on their respective public storage HTTPS/443 hosts; provider
egress exceptions cannot widen these bindings.

FOCUS datasets and metering deliveries add `olp_focus_dataset_deliveries_total`,
`olp_focus_pending_datasets`, `olp_billing_imports_total` and
`olp_billing_capture_rate`; see [finops](finops.md) for object layout,
restatement and the reconciliation report.

A FOCUS day stuck with a `last_error_code` stays in `focus_datasets` with a
future `next_attempt_at`; the worker retries it automatically (same revision
keys, manifest last). Clear the cause (endpoint, credential, egress) rather
than editing the row — `current` false means a dirty day awaiting restatement.
A billing import's `last_error_code` follows the shared categories:
`credential` means the sealed admin key is absent or invalid for the source —
replace it; `status`/`response` are upstream answers, `endpoint` a missing
provider or egress refusal. Imported costs are purged at
`retention.usage_days`. Stripe meter posts and the OpenAI/Anthropic admin cost
APIs run under the provider egress policy; restricted-egress deployments
listing specific hosts need `api.stripe.com`, `api.openai.com` and
`api.anthropic.com` reachable or the calls report `status`/`network`.

Owner-enabled payload capture is best-effort and memory-only. Policies select
sink, input/output/tool calls, deterministic sample ratio and scope selectors.
Limits: 1 MiB/record, 256 collectors, 64 queued records, 4 MiB/project/minute;
violations drop the whole record. Input follows content policy; output reflects
already-delivered frames. Optional local redactors pin immutable revisions on
creation or explicit refresh, inspect complete fields before enqueue and fail
closed for capture under a 100 ms total budget. Ordinary edits preserve pins.
Empty redactor lists capture unredacted content; see [capture operations](payload-capture.md).
Captured content never enters PostgreSQL/Valkey/logs/spans. `payload_captured`
means queued, not delivered. Send/failed/stale-policy audit is metadata-only and
best-effort; queue/cache drops are counters only. Monitor `olp_capture_*_total`.

Business metrics include tokens, approximate cost, TTFT/output rate. Optional
project/key/end-user series labels are bounded by `OLP_METRICS_SERIES_CAP`;
overflow is counted. Durable accounting remains authoritative and caller-paid
exempt spend still contributes observable cost.

### Replicated worker health

Worker mode has only private observability. Production needs Valkey plus worker
or `all`; configured capability flags do not establish live worker health.

| Task | Schedule/authority |
| --- | --- |
| Media reconciliation | Durable claims; historical pins and live revocation; also without Valkey |
| Plugin grant refresh | Five-second scan; per-grant advisory lock; record answered refresh during shutdown; also without Valkey |
| Active probes | 15-second scan; fleet claim per configured interval/model; low-priority accounted system work |
| Metadata consumer | Replay/reclaim, commit once then acknowledge/delete; dedicated blocking Valkey connection |
| Export delivery | PostgreSQL leases, independent of Valkey |
| Epoch detection | Two confirming passes before unclean-exit gap |
| Maintenance | 60-second advisory-lock pass: roll up before purge; bounded resource expiry/savepoints |
| Cost reconciliation | 60-second region-scoped leader pass |
| Notifications | 60-second claims, durable attempt evidence, up to five retries with exponential minute backoff |
| FOCUS dataset export | 60-second pass; PostgreSQL leases, no Valkey; at most four due days per sink |
| Billing import | 60-second scan; each due import runs hourly after success, five-minute geometric backoff on failure |
| Batch settlement | 5-second durable claim scan for submission reconciliation, polling and result settlement |

Media/grant/consumer/epoch/batch settlement checkpoints stale at 20 seconds; maintenance/cost/
notification/focus-export/billing-import checkpoints at 180. Follower skips do not prove leader success.
Healthy async status requires expected current tasks and zero pending/lag;
backlogged is not lost, and missing evidence is unknown. Unsupported integer
metadata versions remain pending; invalid supported formats create gaps.

Metadata reclaim is eligible after 30 seconds, scanned every five; investigate
if recovery has not begun by 35 seconds. Run three workers across failure domains.
Watch pending/lag/oldest age, reclaim/duplicate counts and `olp_worker_task_*`.
Counters are shared durable totals; unavailable summary reads are not resets.
Restart a consumer after startup misconfiguration; there is no outer supervisor.

Runtime publication is synchronous under the installation lock. `runtime_outbox`
is `not_configured`, not a missing worker. Monitor release convergence and
independent authority freshness. Without Valkey, media/grants/exports run, but
accounting/retention are not production-ready and readiness can remain degraded.

### Spend-budget reconciliation

PostgreSQL facts are spend authority; Valkey holds exact-decimal admission
snapshots across the [budget hierarchy](access.md#installation-and-project-budgets).
Accounting/reconciliation alone initialize balances. Unknown/malformed/wrong-window
state returns 503; known exhaustion returns 429. New budgets normally wait for
next minute's pass. Never insert zero, remove policy or reset a valid counter to
bypass initialization. Supply caps skip targets while unknown/exhausted.

Accounting installs every owner's spend on both bases — `accrued`/
`unpriced_attempts` from `estimated_cost`, `charged`/`uncharged_attempts` from
`charge` — so a `cost_basis` change takes effect on the next admission and
reconciliation installs both bases for every owner it reconciles.

Each region has one dedicated PostgreSQL leader session, never returned while
locked. Failure/cancellation/120-second pass deadline closes it; followers cannot
claim leader success. Regional stores use the same durable facts, not global
instantaneous monetary reservations; see [regional fleets](deployment.md#regional-fleets).
Malformed hashes can be rebuilt from matching authoritative windows; valid
inflated totals need reviewed repair. Future/stale snapshots cannot initialize
another period. Day/week/month windows use the configured calendar.

1. Inspect consumer, database, Valkey, clocks and leader checkpoint on persistent
   503s or unexplained spend. Revoke affected keys if continued admission is unsafe.
2. After Valkey loss, fence budgeted traffic until checkpoint and accrued values
   confirm reconstruction. Do not lower/delete valid counters or rate leases.
3. Only during stopped traffic and failed reconciliation may an exact malformed
   cost hash be removed; never use wildcard deletion.
4. Review missing prices/usage and retained pending native work. Keep upstream
   quotas: local [reservations](gateway.md#cost-reservation) are not invoice caps.

Holds can produce 429 below displayed spend and lapse at deadline plus five
minutes, retired in bounded pages. Damaged advisory holds cannot block accounting.
Deleting `cost:pending`/`cost:expiry` loses in-flight protection, not accrued spend.
No maximum ingestion lag or monetary overshoot has been qualified.

### Notifications

Manage destinations/rules/delivery metadata under `/api/v1/notifications`.
Destination types: webhook, Slack, Teams, Discord, PagerDuty and SMTP email.
Secrets are sealed/write-only; URL changes require explicit replacement/removal.
Webhook timeout is five seconds, typed channels fifteen; PagerDuty needs 202.
Email requires SMTPS/STARTTLS. Egress is checked at creation and every dial.

Rules subscribe to immutable events: budget thresholds/exhaustion, key expiry,
grant lapse, circuits/error rate/credentials, latency, retirement, runtime failure,
stale workers and spend reports. [OpenAPI](../openapi/management.json) owns event
schemas and defaults. Project events require matching project subject/destination
and manager authority; installation events require settings authority. Missing
fresh measurement is UNKNOWN, not recovery. Thresholds fire once per calendar
window; key reminders deduplicate reason/date and cancel superseded pending sends;
retired unused grants do not emit lapse.

HMAC-SHA256 webhook signatures cover exact bodies in
`X-OLP-Signature: sha256=<hex>`. Payloads are metadata-only; failures retain safe
categories, never response bodies/URLs/secrets. Disabled rules/destinations pause
unsent delivery. Grant-lapse evidence identifies the affected version/slots and
observed principal, never token material or refresh error.

Notifications need Valkey only for limiter-dependent signals. Capability
`notifications_active` means configured, not healthy; check notification task
checkpoints and delivery status counters.

## Accounting delivery and shutdown

Every caller/playground/probe terminal event is content-free. Each producing
process has an 8192-event nonblocking buffer feeding the installation's Valkey
stream. Overflow checkpoints local loss so other replicas can refuse incomplete
cost admission. Never store prompts/outputs/tools/raw headers/cookies/uploads;
provider names, route slugs and labels must also remain secret-free.

Supported wire format is JSON `event`, `version: 1`. Unsupported integer versions
stay in stream/pending for compatible consumers; malformed supported records become
durable gaps. PostgreSQL commit precedes acknowledge/delete; replay does not
repeat charges. Attributable attempts use key project, then provider project,
then route project for aggregate monetary ownership, including retries/rollups.

Retained Response/Interaction mappings own immutable original financial events
and pending subject projections. Newly enabled budgets cannot hide unresolved
spend. Native completion, durable attachment and HTTP delivery are separate;
usage survives projection/write/flush failure. Null unfinished HTTP metadata is
not success. A detached two-second finalizer patches only receipt-qualified
original observations without changing identities/prices/usage. Investigate
`deferred generation HTTP outcome not recorded` warnings. Delete/expiry saves
partial usage/uncertainty first; unsupported templates remain, with bounded
maintenance/savepoints. See [retained accounting](gateway.md#retained-generation-accounting).

Retained identity includes caller, immutable revisions, selected route target,
slot and credential version. Migration 0062 backfills targets only where historical
evidence proves a unique match; null/ambiguous pins refuse dispatch. Never repair
using mutable current configuration. Retired budget groups retain policy-free
historical UUIDs for late settlement; same-name replacements are distinct.
Audio duration is recorded to microseconds; invalid duration is missing billing
evidence, not a fabricated zero.

Management exposes usage summary/breakdown/timeseries/completeness, request
history, pricing revisions and metadata epochs. Project readers see only their
scope. Partial boundary buckets are approximate with exclusion/gap evidence.
Attribution filters and end-user digest breakdowns preserve raw-ID privacy.
Estimate error compares only attempts with both estimated/reported input;
model family/provenance breakdowns distinguish unestimated and historical unknown.
Committed connections report zero marginal cost; `allocated_commitment`
apportions each commitment amount across the period's list-cost weight —
see [pricing](pricing.md#commitments).

Pricing entries and every component's exact billing semantics are covered in
[pricing](pricing.md). Managed pricing sources refresh bounded, egress-checked immutable snapshots and
return diffs; only explicit publication affects accounting. Negotiated rates use
publish-time overrides. `catalog` sources verify detached signatures, refuse
catalog rollback and map supported model aliases. Bundled refresh is offline;
remote catalog URL must serve directly with `.sig` and allowed egress. Plugin
prices require provider scope (`provider_id`), otherwise remain unpriced.
See [catalog](catalog.md) and the management contract for endpoints.

Shutdown and failed-startup cleanup share one `OLP_SHUTDOWN_TIMEOUT` deadline
(default 30 seconds): drain requests/shadows/probes while authority remains,
close/flush metadata, then stop delivery/workers/dependencies/traces. Deadline
expiry forcibly unblocks resources and logs incomplete joins; it never adds an
unbounded wait. Only verified drain closes the producer epoch. Forced producers
leave it open; undelivered buffered events become explicit loss.

Receipts/replay evidence last seven days plus five-minute skew grace. Size receipt
storage for `sustained_requests_per_second * 604800`; recover queues within this
window, not by suspending retention. Late events remain uncertain, never silently
double-counted.

### Queue durability

Acknowledgment is not fsync. Compose AOF `everysec` can lose about one second on
power/storage failure; replication adds risk. Surviving gaps/epochs expose known
loss but cannot enumerate facts lost with their evidence. Fence budgeted traffic
during suspected loss and reconcile provider billing. Qualify actual persistence/
replication storage for tighter RPO; no fleet guarantee follows from fsync alone.

## Shared state in Valkey

Every key is prefixed with the installation namespace
`olp:<installation>:`, so installations sharing one Valkey service never
read, acknowledge, or reconcile one another's state.

| Key | Contents |
| --- | --- |
| `<prefix>limits:{<lookup>}:rate` | Request and token windows for one lookup. |
| `<prefix>limits:{<lookup>}:concurrency` | Concurrency leases for one lookup. |
| `<prefix>limits:{<cost owner>}:cost:day`, `:cost:week` and `:cost:month` | Current calendar spend windows for caller and supply owners; supply caps use day/month. Custom calendar hashes also contain start/end instants. |
| `<prefix>limits:{<cost owner>}:charge:day`, `:charge:week` and `:charge:month` | The same windows under the consumer-charge basis, installed for every owner so a budget can switch `cost_basis` without state migration. |
| `<prefix>limits:{<cost owner>}:cost:pending` and `:cost:expiry` | Cost reserved by requests in flight against that owner: a hash of each request's amount and their total, and the set of when each lapses. Advisory; absent when nothing is in flight. |
| `<prefix>limits:{<cost owner>}:charge:pending` and `:charge:expiry` | The same in-flight state for charge-basis reservations. |
| `<prefix>limits:provider-cooldown:<scope>` | Credential-version and slot cooldowns. |
| `<prefix>limits:health:circuits` | Each provider whose circuit the fleet holds open, until when. Gateways publish transitions and read it every two seconds. |
| `<prefix>limits:health:probes` and `:health:probe:<provider>/<model>` | Each provider's latest active probe result, and the claim that keeps one probe per model and interval. |
| `<prefix>request-metadata` | The request metadata stream, read by consumer group `olp:persistence`. |

A lookup is the key's lookup identifier, `pc_<provider uuid>` for a connection,
`ps_<slot uuid>` for a credential slot, or `rt_<route uuid>` for a route's
spend cap; the braces are the cluster hash tag,
so one key's dimensions stay on one slot. Cost keys are tagged by the API key or
budget-group UUID, so key rotation preserves spend and group members share one
balance.

The gateway runs its Lua scripts by SHA-1 digest and sends a script's source only
to a Valkey that answers it holds none, which happens once after a restart, a
failover or `SCRIPT FLUSH`, and is cached from then on. Flushing the script cache
is therefore safe; it costs one slower call per script.

## Routine checks

1. Confirm readiness, release convergence and independently fresh key authority.
2. Check PostgreSQL replication/WAL/disk/backup age and Valkey latency/memory/AOF.
3. Review completeness/pricing before cost exports; missing usage is not zero.
4. Review bounded `/api/v1/audit` pages for auth/role/key/credential/publication
   changes. Direct-peer/coarse-UA metadata is not trusted forwarded identity.
5. Revoke/rotate keys when offboarding: disabling a user does not revoke API keys.
6. Compare spool used/capacity metrics and readiness. The chart budgets 1 GiB in a
   2 GiB volume, separate from the 64 MiB general `/tmp` mount.
7. Watch independent management/inference admission pools (default 32/256).

Separate upstream/probe failures from local admission or rollout regressions.
Never include bodies/raw headers/credentials/session/master secrets in tickets.

### OpenAI-compatible capability certification

[Review, certify and activate](provider-routing.md#provider-lifecycle) exact model/
operation tuples. Re-certify transport/semantic changes and validate rotation;
capability claims alone never qualify a provider.

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

Budget-zone changes are scheduled, not immediate resets. See
[budget time zones](access.md#budget-time-zone) for the effective-boundary display,
DST behavior and retained-cost precision. If a newly active period returns 503,
check the existing cost-reconciliation worker before retrying; never seed an
unknown balance with zero. The calendar tables are backed up with PostgreSQL and
are required to interpret delayed and retained usage correctly.

### Local MFA recovery

Before enforcing local MFA, enroll at least one working factor and store the
one-use recovery codes separately. Security keys need the configured HTTPS DNS
public origin (localhost is the development exception). Keep the relying-party
hostname stable during a deployment migration; use TOTP or a recovery code if a
key cannot operate at the current origin. A recovered factor still requires the
account password for a new local session.

An ordinary offline password reset keeps MFA enabled. Only use
`olp account reset-password EMAIL PASSWORD_FILE --reset-mfa [flags]` for an
explicit operator-approved factor reset: it invalidates factors, codes and
sessions, records `user.mfa_recover`, and preserves the required-MFA policy.
The next password sign-in bootstraps a new factor when policy requires it.
See [local MFA and account recovery](access.md#local-multi-factor-authentication)
for the proof protocol, storage inventory and recovery-file permissions.

SAML operators should register the console's stable public origin and current SP
signing certificate with the IdP, preserve the sealed-key inventory during backup,
and keep an independent owner recovery method. A signing-key rotation invalidates
pending flows and requires distributing the new public certificate. Do not solve a
callback failure by loosening session SameSite policy or allowing unsigned
assertions. See [SAML console sign-in](access.md#saml-console-sign-in) for supported
bindings, expiry, promotion and recovery semantics.

## Guardrail operations

Guardrail activation publishes a complete immutable release. If activation fails,
inspect the definition's timeout, project references, route fidelity and required
plugin or credential availability. A failed release does not replace the active
snapshot. `guardrails.stream_window_bytes` is changed through installation
settings and publishes a new release.

Monitor `olp_guardrail_decisions_total` and `olp_guardrail_duration_seconds` by
phase, action and outcome. `failed_open` means evaluation could not complete and
the configured policy allowed continuation; `failed_closed` means continuation
was refused. External circuit breakers are independent per definition revision.
Do not enable provider or webhook body logging to diagnose guardrail failures.
Use the content-free request/attempt evidence and explicit sandbox evaluations.

External adapters are tested against local contract fakes. Live-account vendor
qualification and M1 reference-hardware performance qualification must be recorded
separately before declaring the M7 exit checklist complete.

## Response cache

Response caching is opt-in per route and uses `OLP_CACHE_VALKEY_URL` (or its
`_FILE` form), a separate evicting Valkey deployment. The optional Compose cache
profile uses a 256 MiB limit and no persistence; Valkey Search is required for
semantic matching. A cache failure produces `bypass` and leaves coordination,
admission and accounting on their original Valkey. Monitor
`olp_cache_requests_total`, `olp_cache_bytes_stored_total` and
`olp_cache_errors_total`. Route/project purges invalidate a namespace without
scanning content. [Configuration, controls and accounting](caching.md).

## Batch settlement recovery

The `batch_settlement` worker claims due work on its 5-second pass. Normal
transitions are `submitting → active → settling → settled`; ambiguous creates
remain `submission_unknown` until provider-specific reconciliation proves one
job. Conflicting evidence closes as `conflict`; missing or incomplete result
evidence closes as `uncertain`, never as zero cost. Monitor its health task,
pending count, oldest age and checkpoint freshness.

- For `submission_unknown`, inspect the stored submission cursor and the
  provider control plane. Vertex matches the preallocated submission label;
  Bedrock reissues the original body with the same client request token. Do not
  submit a second job manually.
- For `conflict`, preserve the metadata and compare the provider identifier and
  reconciliation evidence. Escalate rather than attaching a guessed job.
- For `uncertain`, inspect `last_error_class`, result gaps and configured
  storage scope. Restore read access or provider output evidence and use the
  reviewed recovery process; never rewrite a missing result as zero usage.
- Retain batch settlement rows and result facts under the configured usage
  retention. Do not delete individual rows to unblock a worker.

Allow provider egress to the Gemini upload host `generativelanguage.googleapis.com`,
`{location}-aiplatform.googleapis.com`, `storage.googleapis.com`,
`bedrock.{region}.amazonaws.com` and
`{bucket}.s3.{region}.amazonaws.com` (or `s3.{region}.amazonaws.com` for dotted
bucket names). Keep configured storage objects available
to the reader identity until settlement closes.
