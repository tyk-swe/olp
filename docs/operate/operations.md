# Operations runbook

This runbook covers production availability, monitoring, recovery, version changes, incidents, and key rotation. Keep the runbook version matched with the deployed release because recovery details can depend on the storage and runtime contract in that release. Installation topology and first deployment belong in [Deployment](deployment.md).

## Commitment accounting recovery

Disabling a provider connection stops traffic while retaining published commitments. Inspect the FOCUS dataset view for purchased, allocated, and unused totals, provisional state, and incomplete coverage. Pending edits apply at UTC period boundaries, and removing a draft amount does not cancel the active obligation.

The FOCUS worker builds history from published runtime snapshots and retained facts or hourly evidence, catching up in bounded batches and reporting incomplete historical or partial export coverage explicitly. Keep `commitment_schedules`, `commitment_periods`, and `commitment_usage` in the normal whole-schema backup. Allocation context stays available for the lifetime of retained hourly reporting, including periods outside the FOCUS export window. Restoring a backup preserves the originating installation history, whereas configuration promotion carries desired settings so a destination establishes its own obligations upon publication. Never copy period rows during promotion.

Late facts restate affected days under the standard delivery lease and retry rules. Consumers should read the manifest last, deduplicate by day and revision, and replace the prior dataset revision. Never shift an out-of-window purchase forward to make a partial export appear complete. See [FinOps](finops.md#commitments).

## Objectives and monitoring

Measure public-listener availability, added gateway latency, and accounting and recovery capacity. Gateway operators own listener availability and metadata completeness, while provider owners own credentials, quotas, and upstream model availability.

Scrape private port 9090 every 15 seconds because public health and metrics paths return HTTP 404. Readiness refreshes every five seconds and expensive operational rollups refresh every fifteen seconds. Page on five minutes without readiness, metadata loss or persistence failure, unavailable hard limits, or stale worker and cost checkpoints, and warn on ten-minute backlogs. Bundled Prometheus rules and ServiceMonitors are starting points rather than an SLO guarantee.

For container health checks without `curl`, run `olp health-probe`. It reads `OLP_OBSERVABILITY_LISTEN_ADDR` (default `127.0.0.1:9090`, mapping wildcard bind addresses to loopback), issues `GET /health/ready` with a 3-second timeout and redirects disabled, and exits non-zero on any status other than 200. For Kubernetes pod draining before SIGTERM, use `olp internal-pre-stop --seconds N` (default 10 seconds, maximum 3600 seconds), which waits for `N` seconds or exits promptly on cancellation so load balancers can stop routing traffic before the shared `OLP_SHUTDOWN_TIMEOUT` budget starts.

### Export and billing incidents

Use [Observability](observability.md) for sink, tracing, capture, and notification configuration. Monitor export pending counts, lag, gap counts, and worker task checkpoints. An unreachable remote destination leaves durable work queued, while failed claim, outcome, or checkpoint writes mark the delivery task unhealthy.

A FOCUS day recording `last_error_code` stays in `focus_datasets` with a future `next_attempt_at`. Repair the endpoint, credentials, or egress rules and let the worker retry the same revision with the manifest written last, without manually editing database rows. `current: false` marks a dirty day awaiting restatement.

For billing-import errors, `credential` indicates a missing, invalid, or unreadable sealed billing credential or an upstream authentication or authorization failure. `status` and `response` indicate upstream or export failures, and `endpoint` indicates a missing provider or egress refusal. Imported costs expire at `retention.usage_days`. Restricted egress deployments must allow `api.stripe.com`, `api.openai.com`, and `api.anthropic.com` for the corresponding meter or import APIs, while cloud export and authentication hosts depend on the selected source and credential. See [FinOps](finops.md#billing-imports).

Payload capture failures drop records without affecting inference. Review `olp_capture_dropped_total`, `olp_capture_failed_total`, and `olp_capture_redaction_failed_total`, bearing in mind that a queued capture does not confirm delivery. Notifications require healthy task checkpoints and receiver evidence rather than a configured capability flag alone.

### Replicated worker health

Worker mode exposes only the private observability listener. Production deployments require Valkey plus `worker` or `all` processes, and configured capability flags never prove live worker health. OLP tracks 16 checkpointed tasks in `olp.worker_task_health` (`internal/platform/workerhealth/health.go`). Three regional tasks (`request_metadata_consumer`, `cost_reconciliation`, and `health_probes`) also checkpoint per region in `olp.regional_worker_task_health` when `OLP_REGION` is set.

| Task label | Stale threshold | Schedule and authority |
| --- | --- | --- |
| `request_metadata_consumer` | 20s | Regional. Dedicated blocking Valkey connection that replays, reclaims, commits once to PostgreSQL, and then acknowledges and deletes entries. |
| `request_metadata_gateway_epoch_detection` | 20s | Two confirming passes before recording an unclean-exit completeness gap. Also runs without Valkey. |
| `media_reconciliation` | 20s | Durable claims reconciling accepted media jobs with historical pins and live revocation. Also runs without Valkey. |
| `grant_refresh` | 20s | Five-second scan with per-grant advisory locks that records answered refreshes during shutdown. Also runs without Valkey. |
| `export_delivery` | 30s | Five-second rotating pass (20-second budget) over PostgreSQL leases for `/api/v1/observability/sinks`. Independent of Valkey. |
| `code_allowance_read` | 60s | 15-second scan with per-account advisory locks reading up to 50 due coding-subscription allowance accounts per pass (5-minute freshness window). Independent of Valkey. |
| `batch_settlement` | 180s | Five-second durable claim scan for batch submission reconciliation, polling, and result settlement. Also runs without Valkey. |
| `retained_settlement` | 180s | Five-second wake claiming up to 16 items with four concurrent polls, two-minute leases, 30-second heartbeats, and 60-second observations with backoff. Also runs without Valkey. |
| `maintenance` | 180s | 60-second advisory-lock pass that rolls up usage before purging and bounds resource expiry with savepoints. |
| `cost_reconciliation` | 180s | Regional. 60-second region-scoped leader pass publishing authoritative budget snapshots to Valkey. |
| `execution_debits` | 180s | 60-second pass (40-second budget) reconciling prepared and in-flight execution debits, recovering orphaned MCP tool dispatches, and draining up to 256 expired call identities. Also runs without Valkey. |
| `notification_delivery` | 180s | 60-second claims with durable attempt evidence and up to five retries using exponential minute backoff. |
| `managed_export_delivery` | 180s | Five-second idle ticker (drains backlogs continuously) expiring rows past seven days in batches of 1,000 and claiming up to 16 `/api/v1/sinks` deliveries per pass with 1-minute leases and 5-second HTTP timeouts. Independent of Valkey. |
| `health_probes` | 180s | Regional. 15-second scan using fleet claims per configured interval and model for low-priority accounted probes. |
| `billing_import` | 180s | 60-second scan running hourly imports after success (Google backfill eligible after one minute) with five-minute geometric backoff on failures. Independent of Valkey. |
| `focus_export` | 180s | 60-second pass over PostgreSQL leases exporting at most four due days per sink. Independent of Valkey. |

Checkpoints turn stale after 20 seconds for `request_metadata_consumer`, `request_metadata_gateway_epoch_detection`, `media_reconciliation`, and `grant_refresh`, after 30 seconds for `export_delivery`, after 60 seconds for `code_allowance_read`, and after 180 seconds for the remaining ten tasks (`batch_settlement`, `retained_settlement`, `maintenance`, `cost_reconciliation`, `execution_debits`, `notification_delivery`, `managed_export_delivery`, `health_probes`, `billing_import`, and `focus_export`). Follower skips never prove leader success. Healthy asynchronous status requires every expected task to be current alongside zero pending and lag counts. A backlog is delayed rather than lost, while missing evidence is unknown. Unsupported integer metadata versions remain pending, and invalid supported formats record explicit gaps.

Metadata reclaim becomes eligible after 30 seconds and is scanned every five seconds, so investigate if recovery has not started by 35 seconds. Run three workers across failure domains. Monitor pending count, lag, oldest pending age, reclaim and duplicate counters, and the `olp_worker_task_*` series. Recovery counters are shared durable totals, and unavailable summary reads never indicate counter resets. Restart a stopped consumer after fixing startup misconfiguration because OLP includes no outer process supervisor.

Runtime publication commits synchronously under the installation lock, so there is no runtime outbox to monitor. Track release convergence via `GET /api/v1/runtime-generations` (cursor-paginated immutable release history with sequence number, SHA-256 digest, creator, and timestamp) and independent authority freshness. Without Valkey, PostgreSQL-only tasks such as media reconciliation, grant refresh, managed exports, observability exports, FOCUS exports, billing imports, and code allowance reads continue running, but accounting and retention are not production-ready and readiness can stay degraded.

### Spend-budget reconciliation

PostgreSQL facts are the spend authority, and Valkey holds exact-decimal admission snapshots across the [budget hierarchy](../use/access.md#installation-and-project-budgets). Only accounting and reconciliation initialize balances. Unknown, malformed, or wrong-window state returns HTTP 503, while known exhaustion returns HTTP 429. Newly configured budgets normally wait for the next minute's pass. Never seed zero, drop a policy, or delete a valid counter to bypass initialization. Supply caps skip targets while their window is unknown or exhausted.

Accounting installs every owner's spend on both cost bases, deriving `accrued` and `unpriced_attempts` from `estimated_cost` and `charged` and `uncharged_attempts` from `charge`. Changing `cost_basis` therefore takes effect on the next admission without migrating state. Reconciliation likewise populates both bases for every reconciled owner.

Each region maintains one dedicated PostgreSQL leader session that stays detached while holding its advisory lock. Any failure, cancellation, or 120-second pass deadline closes the session, and followers never report leader success. Regional Valkey stores consume the same durable facts rather than sharing instantaneous monetary holds across regions, as described in [regional fleets](deployment.md#regional-fleets). Malformed hashes can be rebuilt from matching authoritative windows, whereas valid inflated totals need reviewed repair. Future or stale snapshots cannot initialize a different period, and day, week, and month windows follow the configured budget calendar.

1. Inspect the consumer, database, Valkey, clocks, and leader checkpoint on persistent 503 responses or unexplained spend, and revoke affected keys if continued admission is unsafe.
2. After Valkey loss, fence budgeted traffic until the checkpoint and accrued values confirm reconstruction, without lowering or deleting valid counters or rate leases.
3. Only when traffic is stopped and reconciliation fails may an operator remove an exact malformed cost hash, never using wildcard deletion.
4. Review missing prices, incomplete usage, and pending retained native work, and keep upstream provider quotas because local [reservations](../develop/architecture.md#cost-reservation) are not invoice caps.

In-flight holds can return HTTP 429 below displayed spend and lapse at deadline plus five minutes, retired in bounded pages. Damaged advisory holds cannot block accounting. Removing `cost:pending` or `cost:expiry` drops in-flight protection without altering accrued spend. No maximum ingestion lag or monetary overshoot envelope has been qualified.

## Accounting delivery and shutdown

Every caller, playground, and probe terminal event is content-free. Each producing process holds an 8,192-event nonblocking buffer feeding the installation's Valkey stream. Buffer overflow checkpoints local loss so other replicas can refuse incomplete cost admission. Never store prompts, outputs, tools, raw headers, cookies, or uploads, and keep provider names, route slugs, and labels free of secrets.

The supported wire format is JSON `event` at `version: 1`. Unsupported integer versions stay pending in the stream for compatible consumers, while malformed supported records create durable gaps. PostgreSQL commits before acknowledging and deleting stream entries so replays never double-charge. Attributable attempts assign aggregate monetary ownership to the key project, then the provider project, and finally the route project, including retries and rollups.

Retained Response and Interaction mappings own immutable original financial events and pending subject projections so newly enabled budgets cannot hide unresolved spend. Native completion, durable attachment, and HTTP delivery stay decoupled so usage survives projection, write, or flush failures. Null unfinished HTTP metadata does not prove success. A detached two-second finalizer patches only receipt-qualified original observations without altering identities, prices, or usage. Investigate `deferred generation HTTP outcome not recorded` warnings. Deletion and expiry save partial usage or uncertainty first, while unsupported templates stay visible with bounded maintenance and savepoints. See [retained accounting](../develop/architecture.md#retained-generation-accounting).

Retained identity pins the caller, immutable revisions, selected route target, credential slot, and credential version. Null or ambiguous pins refuse dispatch, and operators must never repair records using mutable current configuration. Retired budget groups keep policy-free historical UUIDs for late settlement while same-name replacements get distinct UUIDs. Audio duration is stored to microseconds, and an invalid duration records missing billing evidence rather than a fabricated zero.

Management endpoints expose usage summaries, breakdowns, timeseries, completeness, request history, pricing revisions, runtime generations (`GET /api/v1/runtime-generations`), and gateway metadata epochs (`GET /api/v1/request-metadata/gateway-epochs`, filterable by `state` set to `open`, `gracefully_closed`, `unresolved`, or `acknowledged`, plus `POST /api/v1/request-metadata/gateway-epochs/{process_epoch}/acknowledge` to record an audited `request_metadata.gateway_epoch_acknowledge` event for an unresolved epoch). Project readers see only their authorized scope. Partial boundary buckets are approximate and surface exclusion or gap evidence. Attribution filters and end-user digest breakdowns protect raw caller identifiers. Estimate error compares only attempts having both estimated and reported input tokens, and model family or provenance breakdowns distinguish unestimated and historical unknown rows. Committed connections record zero marginal cost while `allocated_commitment` apportions each commitment across the period's list-cost weight. See [Pricing](pricing.md#commitments).

Pricing entries and component billing semantics are detailed in [pricing](pricing.md). Managed pricing sources fetch bounded, egress-checked immutable snapshots and emit diffs, while only explicit publication changes accounting. Negotiated rates use publish-time overrides. `catalog` sources verify detached signatures, block catalog rollback, and map supported model aliases. Bundled refresh works offline, whereas a remote catalog URL must serve directly alongside `.sig` within allowed egress. Plugin prices require provider scope (`provider_id`) and remain unpriced otherwise. See [catalog](../develop/catalog.md) and the management OpenAPI contract for endpoints.

Shutdown and failed-startup cleanup share a single `OLP_SHUTDOWN_TIMEOUT` deadline (default 30 seconds) to drain requests, shadows, and probes while authority remains available, close and flush metadata, and stop delivery, workers, dependencies, and traces. Deadline expiry forcibly unblocks resources and logs incomplete joins rather than waiting indefinitely. Only verified drain closes the producer epoch, while forced exits leave the epoch open so undelivered buffered events surface as explicit loss.

Receipts and replay evidence remain for seven days plus a five-minute skew grace. Size receipt storage for `sustained_requests_per_second * 604800` and recover backlogged queues within that window instead of suspending retention. Late events remain marked uncertain rather than silently double-counted.

### Queue durability

Stream acknowledgment does not equal fsync. Compose AOF `everysec` can lose roughly one second of data on power or storage failure, and asynchronous replication adds further exposure. Surviving gaps and open epochs report known loss without enumerating facts lost alongside their evidence. Fence budgeted traffic during suspected queue loss and reconcile against provider invoices. Qualify persistence and replication storage directly for tighter RPO targets because fsync alone does not establish a fleet-wide guarantee.

## Shared state in Valkey

Every key carries the installation namespace prefix `olp:<installation>:`, isolating independent installations that share a Valkey service.

| Key | Contents |
| --- | --- |
| `<prefix>limits:{<lookup>}:rate` | Request and token windows for one lookup. |
| `<prefix>limits:{<lookup>}:concurrency` | Concurrency leases for one lookup. |
| `<prefix>limits:{<lookup>}:members` | Generation-stamped shared-quota membership set and ceiling synced by gateways for `sg_` shared quota lookups. |
| `<prefix>limits:{<cost owner>}:cost:day`, `:cost:week`, `:cost:month` | Current calendar spend windows for caller and supply owners (supply caps use day and month). Custom calendar hashes also record start and end instants. |
| `<prefix>limits:{<cost owner>}:charge:day`, `:charge:week`, `:charge:month` | Equivalent windows on the consumer-charge basis, maintained for every owner so changing `cost_basis` needs no state migration. |
| `<prefix>limits:{<cost owner>}:cost:pending`, `:cost:expiry` | Advisory in-flight cost reservations per owner, holding each request's reserved amount, the total, and expiry timestamps. Absent when idle. |
| `<prefix>limits:{<cost owner>}:charge:pending`, `:charge:expiry` | Equivalent in-flight reservation state for charge-basis budgets. |
| `<prefix>limits:provider-cooldown:<scope>` | Credential-version and credential-slot cooldown markers. |
| `<prefix>limits:health:circuits` | Providers whose circuit the fleet holds open and their expiry timestamps, refreshed by gateways every two seconds. |
| `<prefix>limits:health:probes`, `:health:probe:<provider>/<model>` | Latest active probe result per provider and the coordination claim keeping one probe per model and interval. |
| `<prefix>request-metadata` | Request metadata stream read by consumer group `olp:persistence`. |

Lookup identifiers use cluster hash tags inside braces so one subject's rate, concurrency, and membership keys map to the same Valkey cluster slot (`internal/limits/owners.go`). Lookups use the API key's lookup ID for key limits, `pc_<provider uuid>` for a provider connection, `ps_<slot uuid>` for a credential slot, `rt_<route uuid>` for a route spend cap, `bg_<budget group uuid>` for a budget group, `ab_<aggregate owner uuid>` for an aggregate budget (organization, project, key-route, or attribution), `eu_<end-user account uuid>` for an end-user budget, `sg_<shared quota group uuid>` for a shared quota group, and `tr_<16-byte SHA-256 hex of key id>` for per-key inbound trace ingestion rate limits. Cost keys are tagged by the owner UUID so key rotation preserves accrued spend and group members share one balance.

Gateways invoke Lua scripts by SHA-1 digest and upload script source only when Valkey reports a cache miss after a restart, failover, or `SCRIPT FLUSH`. Flushing the script cache is therefore safe and adds only one reload round-trip per script.

### Request metadata stream loss

If the request metadata stream is lost before persistence consumes it, such as after a Valkey failure without persistence, usage reports cannot recover those events. Record the loss as an explicit gap so reports and `olp_request_metadata_consumer_healthy` stop treating the missing entries as pending work. Run `scripts/record-request-metadata-stream-loss.sh` with an incident identifier, the lost event count, `exact` or `lower-bound` for count certainty, and whole-second UTC start and end timestamps. The script requires `OLP_DATABASE_URL` and `OLP_CONFIRM_REQUEST_METADATA_STREAM_LOSS=record-explicit-gap`. It inserts one content-free gap row and an audit entry, and rerunning it with the same incident identifier is idempotent.

## Routine checks

1. Confirm readiness, runtime generation convergence (`GET /api/v1/runtime-generations`), and independently fresh key authority.
2. Check PostgreSQL replication lag, WAL growth, disk usage, and backup age alongside Valkey latency, memory, and AOF health.
3. Review completeness, unresolved gateway epochs (`GET /api/v1/request-metadata/gateway-epochs?state=unresolved`), and pricing coverage before exporting cost totals because missing usage is never zero.
4. Review bounded `/api/v1/audit` pages for authentication, role, key, credential, and publication changes, keeping in mind that direct-peer and coarse user-agent metadata are not trusted forwarded client identity.
5. Revoke or rotate API keys during offboarding because disabling a user account does not revoke issued API keys.
6. Compare media spool used and capacity metrics against readiness. The Helm chart budgets a 1 GiB spool inside a 2 GiB volume, separate from the 64 MiB memory-backed `/tmp` mount.
7. Monitor independent management and inference admission pools (defaults 32 and 256) and admission queue metrics when queueing is enabled.

Separate upstream or probe failures from local admission or rollout regressions. Never attach request bodies, raw headers, credentials, session cookies, or master secrets to incident tickets.

### OpenAI-compatible capability certification

[Review, certify, and activate](../use/provider-routing.md#provider-lifecycle) exact model and operation tuples. Re-certify after transport or semantic changes and validate credential rotation because capability claims alone never qualify a provider.

## Backup and restore

### Create a backup

1. Stop new inference admission and control writes, keep workers running, and wait for admitted work, pending acknowledgements, and stream lag to drain completely. Keep traffic fenced until the backup completes.
2. On an encrypted volume with the PostgreSQL 18 client, `jq`, and GNU `sha256sum`, run `scripts/backup.sh` with `OLP_DATABASE_URL` and `OLP_BACKUP_TRAFFIC_QUIESCED=true`.

The script requires a zero-backlog durable checkpoint no older than 30 seconds. `OLP_BACKUP_TRAFFIC_QUIESCED=true` is an operator assertion rather than a server admission fence, so verify load balancers, ingress rules, and all replicas independently because a fresh zero backlog alone does not prove quiescence.

The backup exports one PostgreSQL snapshot of schema `olp`. Its manifest records format and schema markers (`olp`), the dump SHA-256 checksum, installation identity, migration count, migration versions and checksums, runtime generation, and the checkout's application version. Set `OLP_BACKUP_APP_VERSION` and `OLP_BACKUP_IMAGE_DIGEST` when the producing deployment differs from the script checkout. Export applies bounded database waits and a dump timeout (`OLP_BACKUP_TIMEOUT_SECONDS`, default 600). Dump and manifest publish together via atomic directory rename, and hidden partial directories indicate incomplete backups.

The dump holds password hashes, session and API-key digests, and encrypted provider and OIDC credentials. Store it with authenticated encryption, independent off-site retention, and audited access. Back up master-key rings and authentication HMAC files separately in a secret manager with tested key custodians, retaining older master-key versions as long as records or backups need them. Checksums detect corruption rather than coordinated tampering of both dump and manifest. Rehearse restores directly from the off-site store.

### Restore a replacement

Run `scripts/restore.sh BACKUP` using the dump path printed by `scripts/backup.sh`, with `OLP_RESTORE_DATABASE_URL` identifying an empty isolated database. The script requires PostgreSQL 18 client utilities (`psql`, `createdb`, `dropdb`, `pg_restore`, and `pg_dump`), `jq`, `sha256sum`, `python3`, and `openssl`. Put the database name in the URL path, such as `postgresql://host/replacement`. A `dbname` query parameter is rejected before database access because it would override the disposable staging database path, while other connection options remain supported. Set `OLP_RESTORE_VALKEY_ISOLATED=true`, point `OLP_VALKEY_URL` at a separate empty Valkey service, and mount the original master-key and authentication HMAC files. `scripts/restore.sh` defaults `OLP_MAINTENANCE_BIN` to `$repo_dir/.local/bin/olp`, so set `OLP_MAINTENANCE_BIN` to the qualified binary path whenever the binary lives elsewhere.

Use the migration owner for the restore connection. It requires `CREATE` on the replacement database and `CREATEDB` for the temporary staging database, and it owns the restored objects. The script restores into staging, verifies the manifest, checksum, migration history, installation identity, and local migration file hashes, runs the binary's migrations, and authenticates every encrypted record with `olp doctor`. Only after all checks succeed does one transaction recheck and populate the empty destination. Any failure drops staging and leaves the destination untouched.

If you use a separate runtime login, keep the replacement stopped after restore because `pg_restore --no-privileges` omits grants and verification as the owner in staging does not test the runtime login. Ensure the restricted login exists on the destination server and can connect to the replacement database. Set the shell variable `runtime_role` to its name, for example `olp_runtime`. Using the same migration-owner URL for the replacement rather than the source or staging database, reapply the [runtime grants](deployment.md#database-roles).

```sh
OLP_DATABASE_URL="${OLP_RESTORE_DATABASE_URL:?}" OLP_DATABASE_URL_FILE= \
  "${OLP_MAINTENANCE_BIN:-olp}" migrate --runtime-role "${runtime_role:?}"
```

This grants schema usage, table and sequence access, and read-only migration history. Never grant ownership, `CREATE` privileges, or migration-owner membership to the runtime login. Once the grant command succeeds, configure all application processes to use the runtime login's connection to the replacement database.

Start the replacement with its original keys and isolated Valkey service, then check readiness before reopening traffic. Recovery preserves the installation UUID, Valkey namespace, and historical references, so never allow a rehearsal to share streams or leases with the source installation. An independently writable database clone is unsupported. For an independent environment, provision a fresh installation and apply nonsecret state via [configuration promotion](configuration.md#configuration-promotion-artifacts).

### Recovery qualification

For PostgreSQL point-in-time recovery, follow the managed service's tested PITR procedure or [continuous WAL archiving](https://www.postgresql.org/docs/18/continuous-archiving.html) with base backups, a restore target, and verified WAL continuity. Record the service's measured RPO and RTO because neither this repository nor a logical dump promises one. Fence traffic first, restore into an isolated replacement, preserve the installation identity and keyring, and attach an isolated Valkey service. Reconcile external provider and media outcomes and database or queue time skew before reopening budgeted traffic. See [queue durability](#queue-durability).

Former process suites performed a logical restore, authenticated, decrypted a retained provider credential by making a request, and verified that historical accounting remained intact while new accounting appended. Those suites were removed, and they never simulated storage power loss or certified a managed PITR implementation. Rehearse restores against an isolated replacement before relying on them.

## Master-key rotation and recovery

1. Keep every master-key version still in use, add a higher version number, and set it as `active_version` in the private ring file. Take a [drained database backup](#backup-and-restore) and store its required key material separately.
2. Stop management writers for a controlled maintenance window, mount the updated ring, and run `olp master-key reencrypt` with the same database and authentication HMAC key.
3. Rerun `olp master-key reencrypt` if interrupted. It commits batches of 100 records while older authenticated records stay readable through retained keys, and stale processes cannot write ciphertext under a retired active version. Keep each version's key material unchanged because every run authenticates existing destination-version records before committing batches.
4. Run `olp master-key status` and `olp doctor` to authenticate stored ciphertext and emit only installation, version, and count metadata. Remove an older key only after `olp master-key verify-retirement VERSION` succeeds and backup retention allows retirement. Run `olp master-key reencrypt --dry-run` to verify records without modifying them.
5. Restart every process that decrypts credentials with the updated ring. Existing sessions and API keys preserve their HMAC identity, encrypted replays return their original responses, and rotation logs a metadata-only audit event.

## Installation and versions

Each installation begins from an empty PostgreSQL database and uses its own Valkey namespace. During 0.x, OLP makes no compatibility, upgrade, mixed-version, or rollback guarantees because any release may alter the management API, configuration, or storage schema and may require a fresh installation.

Run the same version across every process mode. To upgrade an existing database to a newer release, take a [drained backup](#backup-and-restore), stop every process, run `olp migrate` once with the new binary, and start all process modes on that version. With Helm, scale the gateway, control, and worker Deployments to zero before [upgrading the release](deployment.md#upgrade-an-existing-release) so the pre-upgrade hook runs the migration Job before new pods start. Verify readiness, generation convergence, backlog drain, usage completeness, provider probes, and latency before resuming traffic. Rollback is unsupported because binaries refuse newer schemas and migrations never run in reverse. Never edit migration history or checksums. The migration runner validates recorded checksums, rejects gaps, and refuses databases created before the `0001_schema.sql` baseline, which require a fresh installation. Failed migrations roll back atomically alongside their history rows.

## Database deadlines and privileges

Pool connections enforce a 10-second statement timeout, a 10-second lock wait timeout, and a 15-second idle-in-transaction timeout, while commands also obey the startup and dependency timeouts from the configuration reference. Backups should use a dedicated read role with access to migration history and backed-up tables rather than reusing the runtime login. Large maintenance or export tasks should run under their own role with explicit deadlines rather than an unlimited interactive account. PostgreSQL reports statement cancellations as `57014` and lock timeouts as `55P03`, and a timeout never proves whether a mutation committed.

Provision separate migration-owner and runtime logins and reapply runtime grants after migrations as described in [database roles](deployment.md#database-roles).

## Metric aggregation and incidents

Database-derived request counts, provider samples, worker counters, and metadata summaries represent shared installation state exported by every replica. Pick one current exporter or aggregate with `max` across replicas of the **same installation**, never summing duplicated global gauges or counters. Attach a stable installation label at scrape time when Prometheus monitors multiple installations. Rolling five-minute and fifteen-minute series are gauges rather than monotonic request counters. Never sum or average precomputed p95 or p99 quantiles into a fleet percentile. Quantitative provider series key on stable provider IDs while names and health status live on `olp_provider_health`. Zero sampled attempts emit no success or latency series rather than claiming 100 percent success.

`olp_provider_metrics_complete=0` signals a query failure or truncation past the 10,000-provider cap. Inspect `olp_provider_metrics_emitted` and `olp_observability_metrics_snapshot_fresh`. When a refresh takes longer than four seconds, OLP retains the previous snapshot and exports its age, so never read stale snapshots as live health. Investigate database latency before raising cardinality. Process-local admission, queue, shadow, trace-drop, business, and Go runtime counters (the standard Prometheus Go collector `go_*` series, including `go_memstats_mallocs_total` and `go_memstats_alloc_bytes_total`) may be summed across replicas while honoring counter reset semantics in `rate`.

Compare `olp_runtime_desired_generation` against `olp_runtime_generation` on every gateway to spot pending or rejected generations. Monitor `olp_api_key_authority_age_seconds` independently so a provider activation failure never masks fresh key authority. At 60 seconds, new key-authenticated requests fail closed. Restore database reachability and confirm that authority age drops and the desired generation converges before restoring traffic.

For metadata or worker alerts, inspect database and Valkey reachability, consumer heartbeat age, and pending or lag counts first. Preserve the queue, restart any failed worker, wait through the 30-second reclaim and five-second scan window, and verify progress and cost attribution. For provider alerts, distinguish zero samples, failed probes, provider rate limits, and open circuits. Group Alertmanager notifications by installation and incident cause, inhibit secondary backlog alerts during a confirmed dependency outage, and test alert routing directly because repository tests cannot verify delivery to an on-call responder.

Success SLIs must state their denominator explicitly. `olp_request_success_ratio_5m` measures persisted user requests having no terminal error and a 2xx or 3xx status, whereas provider success ratios measure individual attempts so a retried request can produce both a failed attempt and a successful request. First byte marks a latency milestone rather than completion. A stream failing after headers counts as a terminal failure, and client cancellations should be tracked separately with a clear inclusion policy. Local refusals and missing usage need distinct operational indicators. Cost totals are estimates derived from recorded priced usage and must always be read alongside unpriced, incomplete, pending, and loss indicators.

Budget time-zone changes take effect at future period boundaries rather than resetting active spend immediately. See [budget time zones](../use/access.md#budget-time-zone) for boundary display, DST handling, and retained-cost precision. If a newly active period returns HTTP 503, inspect the `cost_reconciliation` worker before retrying and never seed an unknown balance with zero. Keep the budget calendar tables in PostgreSQL backups because delayed and retained usage depend on them.

### Local MFA recovery

Before enforcing local MFA, enroll at least one working factor and store one-use recovery codes outside the browser. Security keys require the configured HTTPS DNS public origin (localhost is the development exception). Keep the relying-party hostname stable across deployment migrations, using TOTP or a recovery code if a key cannot operate on the current origin. Recovering a factor still requires the account password for a new local session.

A standard offline password reset leaves MFA enabled. Run `olp account reset-password EMAIL PASSWORD_FILE --reset-mfa [flags]` only for an operator-approved factor reset. It invalidates enrolled factors, recovery codes, and active sessions, records `user.mfa_recover`, and keeps the required-MFA policy active so the next password sign-in prompts for fresh factor enrollment. See [local MFA and account recovery](../use/access.md#local-multi-factor-authentication) for proof protocol, storage details, and file permissions.

SAML operators should register the console's stable public origin and current SP signing certificate with the identity provider, preserve the sealed-key inventory in backups, and retain an independent owner recovery path. Rotating the signing key invalidates in-flight flows and requires updating the public certificate at the IdP. Never work around callback issues by weakening session `SameSite` settings or accepting unsigned assertions. See [SAML console sign-in](../use/access.md#saml-console-sign-in) for supported bindings, expiry, promotion, and recovery rules.

## Guardrail operations

Activating guardrails publishes a complete immutable runtime release. If activation fails, check the definition's timeout, project references, route fidelity, and required plugin or credential availability. A failed release leaves the active snapshot unchanged. Changing `guardrails.stream_window_bytes` through installation settings publishes a new release immediately.

Monitor `olp_guardrail_decisions_total` and `olp_guardrail_duration_seconds` by route, phase, action, and outcome. `failed_open` means evaluation could not finish and policy permitted continuation, while `failed_closed` means continuation was blocked. External circuit breakers track each definition revision independently. Never turn on provider or webhook body logging to debug guardrail failures, and instead use content-free request and attempt evidence alongside explicit sandbox evaluations.

External adapters are exercised against local contract fakes. Record live-account vendor qualification and reference-hardware performance qualification separately before signing off a guardrail release checklist.

## Managed Gemini resources

Configure the `gemini-agents` profile at revision `1` with a Gemini API-key credential and a `/v1beta` endpoint, certify `agent_resource/gemini/unary`, and publish a route for that capability. Inference keys must allow both the route and provider state. Resource-control routes refuse configured execution prices and rate cards, and running an agent stays outside this API.

The provider-resources console handles create, list, inspect, and delete operations using an inference key kept in page memory. `GET /api/v1/provider-resource-contracts` publishes implemented capability metadata to the generated management SDK, CLI, and MCP tools. The same console exposes declared fine-tuning, vector-store, conversation, container, eval, skill, and context-cache operations including typed nested parent IDs.

Creates require `X-OLP-Route` and a stable `Idempotency-Key`, returning `X-OLP-Resource-ID` for both successful and ambiguous creates. Recovery compares the original caller payload before selecting a target, so republishing the route or losing capacity never loses the recovery ID. Equivalent JSON whitespace and key ordering preserve identity, while changed request content produces an idempotency conflict. Retrying an already submitted identity returns HTTP 409 without dispatching a second create. Inspect the local resource ID instead of changing the idempotency key to retry an ambiguous submission. A `submission_unknown` resource needs provider-side inspection and cannot be adopted by guessing an upstream ID, redispatched automatically, or cleared by retention. A crash prior to dispatch retains its fence as well.

For multipart skill and skill-version creates, reordering distinct metadata fields preserves the submission identity. File order, file content, and the order of repeated values for the same field still matter. For uploads first submitted before metadata order was normalized, retry with the original multipart part order.

Listing returns only resources created by the calling inference key using local pagination tokens, tombstoning and skipping provider-deleted agents without blocking later pages. Reads and deletes enforce the original route's admission limits against the resource's original provider, target, and credential version. Revoked or missing credentials return `provider_resource_credential_unavailable` and replacement credentials never inherit access. Foreign IDs return HTTP 404, provider errors are sanitized, and native environment references or unbound nested IDs are refused. Resource metadata stores no agent definitions, prompts, tool content, or provider results.

Before release, run service-backed retained-agent ownership, route-limit, replay-recovery, and deletion scenarios alongside live qualification against the pinned Gemini contract because local implementation checks do not establish those results.

## Retained work and evidence

Paid retained creations, uploads, and expiry mutations commit their owner, immutable connection, credential, and pricing pins, accounting templates, and submission fence prior to dispatch. Unknown acceptance never authorizes another dispatch. Recovery adopts only an authenticated provider result correlated to the original intent, and an upstream ID alone confers no authority. Containers and Gemini caches expose no qualified native create-correlation field, so unknown acceptance remains pending rather than being guessed. A durable intent proven never dispatched may close without inventing usage.

Unbounded retained paid work requires an uncapped caller and uncapped applicable supply owners. Delayed training and eval jobs always refuse applicable TPM caps because a monetary hold cannot enforce a token rate. Storage-only liabilities block monetary admission, while unresolved token-consuming work blocks token admission as well, including owners added after acceptance. Zero-cost housekeeping stays permitted under late caps. Credential revocation, polling errors, and missing usage preserve liability. Unsettled retained resources keep their original refreshable grant reference through deletion and expiry, while explicit credential revocation still wins.

The provider-resources inventory displays settlement state, pinned models, expiry, and missing evidence. Call `POST /api/v1/provider-resources/{id}/reconcile` with `configure` permission and project-change authority to schedule a poll, passing an optional `upstream_id` strictly as a correlation-verification hint.

An installation settings administrator with project-change authority can submit `POST /api/v1/provider-resources/{id}/usage-evidence` with `idempotency_key`, a lowercase SHA-256 `evidence_digest`, UTC `start` and `end`, applicable exact quantities, and `terminal`. Choose an originally pinned `model` whenever multiple models exist. Evaluation evidence must provide explicit input, output, cached-input, and invocation counts. Interval boundaries use microsecond precision and raw quantities remain exact. Ownership, price revisions, and credentials come from the original acceptance and record `operator_verified` provenance. Never apportion an aggregate invoice onto a single resource or infer storage or session charges from snapshots or elapsed age.

Automatic and operator evidence share one atomic coverage ledger. Exact duplicates never double-charge, overlapping or conflicting intervals return HTTP 409, and omitted applicable quantities remain unknown. Terminal evidence closes liability only after independent remote-termination verification and complete coverage across every billed model and component because a missing asynchronous job alone cannot prove execution stopped. Deleting a vector store preserves underlying files, and native history, training examples, cache contents, and eval samples remain on the provider side. Keep retained settlements, mutations, coverage, evidence, and minimal tombstones in whole-schema backups independently of request-history retention.

Project-owned [cloud retrieval bindings](../use/retrieval.md) are revisioned operator-managed references to Bedrock Knowledge Bases, Vertex RAG corpora, or Azure AI Search indexes. Grant specific project API keys explicitly and configure backend field mappings. Bindings promote with configuration, and queries enforce their current grant alongside the certified route, target, and credential. Index creation and synchronization remain outside this API, and unsupported filters or ranking options fail before dispatch.

OpenAI availability limits remain provider qualification concerns. The [announced deprecations](https://developers.openai.com/api/docs/deprecations) restrict new fine-tuning customers from May 7, 2026, further restrict inactive customers from July 2, 2026, and end new jobs for active customers on January 6, 2027, while Evals become read-only on October 31, 2026 and shut down on November 30, 2026. The frozen endpoint scope remains implemented without calendar-based gateway switches. Because the locked OpenAI SDK removed Evals methods, frozen REST fixtures support local qualification, while service, SDK-service, browser, and live qualification stay separate release gates.

## Response cache

Response caching is opt-in per route and uses `OLP_CACHE_VALKEY_URL` (or `OLP_CACHE_VALKEY_URL_FILE`), pointing to a separate evicting Valkey deployment. The optional Compose cache profile sets a 256 MiB memory cap with persistence disabled, and Valkey Search is required for semantic matching. Cache failures return `bypass` and keep coordination, admission, and accounting unaffected on the primary Valkey. Monitor `olp_cache_requests_total`, `olp_cache_bytes_stored_total`, and `olp_cache_errors_total`. Route and project purges invalidate namespaces without scanning stored entries. See [response caching](../use/caching.md) for configuration, controls, and accounting.

## Batch settlement recovery

The `batch_settlement` worker claims due work on its 5-second pass. Normal transitions progress from `submitting` to `active`, then `settling`, and finally `settled`. Ambiguous creates remain `submission_unknown` until provider-specific reconciliation proves a single job. Conflicting evidence closes as `conflict`, while missing or incomplete result evidence closes as `uncertain` rather than zero cost. Monitor the `batch_settlement` health task, pending count, oldest age, and checkpoint freshness.

For `submission_unknown`, inspect the stored submission cursor and the provider control plane instead of submitting a second job manually. Vertex matches the preallocated submission label, and Bedrock reissues the original request body with the same client request token. For `conflict`, preserve all metadata, compare the provider identifier against reconciliation evidence, and escalate instead of attaching a guessed job. Unmapped `conflict` and `uncertain` submission intents retain duplicate-submit protection through their normal retention cutoff and stay visible in the resource inventory.

Vertex checkpoints record completed-shard bytes so the result limit holds across shards and worker restarts. A legacy checkpoint lacking that proof transitions to `conflict` for operator review rather than guessing remaining allowance. For `uncertain`, inspect `last_error_class`, result gaps, and configured storage scope, restore read access or output evidence, and run reviewed recovery without rewriting missing results as zero usage. Retain batch settlement rows and result facts under `retention.usage_days` and never delete individual rows to unblock a worker.

Allow provider egress to the Gemini upload host `generativelanguage.googleapis.com`, `{location}-aiplatform.googleapis.com`, `storage.googleapis.com`, `bedrock.{region}.amazonaws.com`, and `{bucket}.s3.{region}.amazonaws.com` (or `s3.{region}.amazonaws.com` for dotted bucket names). Keep configured storage objects readable by the configured reader identity until settlement completes.
