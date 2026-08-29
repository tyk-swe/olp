# Operations runbook

Availability, monitoring, recovery, upgrade, incident, and key-rotation
procedures for production OpenLLMProxy. Keep this runbook with the deployed
release; deployment topology is in [`deployment.md`](deployment.md).

## Objectives and monitoring

Measure SLOs at the client-facing listener: 99.9% successful availability
(excluding upstream failures), at most 15 ms p95 and 30 ms p99 added latency.
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

## Routine checks

1. Confirm pod readiness and one nonzero runtime generation across gateways.
2. Check PostgreSQL replication, WAL archiving, disk headroom, and backup age;
   check Valkey latency/memory (Valkey is runtime state, not backup authority).
3. Review usage completeness and pricing coverage before exporting costs.
   Missing upstream usage is incomplete and unpriced, never zero.
4. Review provider health, authentication, role/key changes, credential
   rotations, and route activations in the audit stream. `GET /api/v1/audit`
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

1. Stop new inference admission, leave workers running, and wait for zero
   pending acknowledgements and zero Stream lag.
2. On an encrypted volume with PostgreSQL 18 client, `jq`, and GNU `sha256sum`,
   run `scripts/backup.sh` with `OLP_BACKUP_TRAFFIC_QUIESCED=true`.

The script requires a zero, at-most-30-second-old durable checkpoint. Without
the quiescence assertion, the manifest marks
`request_metadata_stream_drained: false` and is not a production recovery
point. Current schemas produce v2 manifests; supported legacy producers use
v1. `scripts/backup-manifest.sh validate BACKUP [v1|v2]` enforces versioned
fields, checksums, and drain/checkpoint consistency. Legacy v1 manifests remain
restorable; `convert-v2-to-v1` is only a compatibility fixture path.

The dump contains password hashes, session/proxy-key digests, and encrypted
provider/OIDC credentials. Back up mounted master-key and HMAC files
separately in the secret manager. Losing a historical master key makes its
records unrecoverable. A restored dump preserves `installation_identity` and
its Valkey namespace: restore as a replacement, or use a fresh Valkey database;
never run source and restore against one keyspace.

At least weekly, run `scripts/restore-rehearsal.sh` in isolation. It requires a
valid checksum/manifest, refuses the production URL, requires `--replace` for
a nonempty destination, and verifies migration count and runtime-generation
ordinal. Start control and gateway with fresh Valkey, then test setup, login,
runtime loading, and a mock-provider request. Do not reuse production OIDC
redirects or provider credentials.

## Upgrade

### Naming migration prerequisites

Keep the existing HMAC bytes; replacing them invalidates API-key and bootstrap
digests. For Compose, rename
`deploy/secrets/olp_key_hash_key` to `deploy/secrets/olp_auth_hmac_key` and
update `OLP_PUBLIC_ORIGIN` if the host port changed. For Helm, rename
`config.keyHashSecretName/key` to `config.authHmacKeySecretName/key` and
monitoring values from `usage*` to `requestMetadata*`. Copy the Secret without
exposing or regenerating its data, verify both hashes, and keep the old Secret
until candidate health and the rollback decision point pass.

### Procedure

1. Verify the immutable image digest plus required signature, provenance, SBOM,
   and vulnerability evidence.
2. Run `scripts/upgrade-rehearsal.sh` against an isolated restored database
   and fresh Valkey. It restores/migrates twice and rejects incomplete or
   non-idempotent results. For N-1 rehearsal set
   `OLP_REHEARSAL_EXPECTED_NEW_MIGRATIONS`; CI derives its fixture from
   `release-metadata.env`, whose baseline is advanced only after a release.
3. Enter maintenance: stop edge inference admission and every control
   mutation, drain old inference workloads, and verify no active requests or
   media reconciliation writers. Leave the old worker until Stream pending and
   lag are durably zero, then scale it down and confirm they remain zero.
   This stop is mandatory for the legacy global
   `olp:v2:request-metadata` transition. Candidate `olp migrate` atomically
   renames it to the installation namespace, preserves group/pending state, and
   aborts if both old and namespaced Streams exist. Do not let N-1 workloads
   write the legacy key after migration.
4. With admission and workers stopped, take the final PostgreSQL backup with
   `OLP_BACKUP_TRAFFIC_QUIESCED=true` and snapshot mounted keys.
5. Run the Helm upgrade with at least a 20-minute timeout. Keep management and
   admission frozen until migration succeeds and every workload uses the
   candidate digest; preserve `maxUnavailable: 0`, the 10-second pre-stop, and
   five-minute termination grace. Database guards reject N-1 publications,
   non-additive usage rollups, and OIDC completions. A migration that retires
   schema only relaxes it during the release that stops using it, because the
   N-1 binary still names those columns and tables in its own writes; the drops
   themselves ship one release later, once no N-1 replica can be running.
6. Resume admission and OIDC initiation. For 30 minutes verify readiness,
   zero metadata backlog, generation convergence, usage completeness, provider
   probes, error rate, and added latency.

Delivery and replay evidence is retained for seven days plus five minutes of
clock-skew grace. A late entry is recorded as uncertain completeness, never
silently counted twice. Size PostgreSQL for up to
`sustained_requests_per_second * 604800` receipt rows. During a delivery
incident, restore/reconcile the Stream within seven days; do not extend the
window by suspending maintenance.

Migrations are forward-only. Once a migration beyond the released baseline in
`release-metadata.env` applies, an N-1 binary rollback is unsupported. Restore
the final pre-upgrade database and keys to a replacement cluster with fresh
Valkey, verify migration state, workloads, readiness, and generation, then
redirect traffic.

## Incident response

### All workers unavailable

Keep admission open only while the local spool has capacity and the business
explicitly accepts delayed accounting. Restore a same-version worker and
require zero metadata pending/lag, zero outbox pending/claimed rows, four
healthy task checkpoints, and reconciled usage. Near the seven-day window,
stop admission and preserve Valkey/AOF and PostgreSQL before repair.

### Dependency failure

- **PostgreSQL/control:** freeze mutations and keep healthy gateways on their
  last-known-good generation; restore PostgreSQL rather than restarting them.
- **Valkey:** hard-limited keys fail closed; unlimited keys may continue.
  Readiness reports `status: degraded` and `limits: unavailable`; restore
  Valkey and verify lease cleanup. To keep hard-limited keys serving during a
  prolonged outage, an owner can set `limits.valkey_unavailable` to
  `fail_open` in Settings (or `PUT /api/v1/settings/limits.valkey_unavailable`);
  gateways apply it within 15 seconds, admit those keys without RPM/TPM/
  concurrency enforcement, and count each admission in
  `olp_limits_fail_open_total`. Revert to `fail_closed` once Valkey is back. Valkey server time controls fixed UTC-minute
  RPM/TPM windows, `Retry-After`, and lease expiry.
- **Metadata persistence:** continue only with explicit acceptance of
  incomplete cost data. Preserve Stream state, suspend retention, record the
  interval, and reconcile request/attempt/usage/gap counts. Never report a gap
  as zero cost.

`olp_request_metadata_loss_reported_total{kind="events|dropped|abandoned"}`
counts the local-buffer loss this process has durably reported as gaps. It is
exact loss, not uncertainty, and every increment also appears as a warning in
the log with the gateway instance.

### Unclean gateway epochs

An unclean epoch is uncertain, not proof of loss. Compare its bounds with the
worker checkpoint, Stream, and request/attempt records, then use:

```text
GET  /api/v1/request-metadata/gateway-epochs?state=unresolved
POST /api/v1/request-metadata/gateway-epochs/{process_epoch}/acknowledge
```

Acknowledgement is idempotent, session/CSRF/Origin protected, requires
settings-management permission, and emits an audit event. It clears readiness
only; historical uncertain gaps remain incomplete. Retention never removes an
unacknowledged epoch.

### Unrecoverable Valkey Stream

Stop admission and the worker; do not attach an empty Valkey until the missing
interval is explicit. Derive count/timestamps from the checkpoint, AOF, and
monitoring, then record an idempotent content-free gap:

```console
export OLP_CONFIRM_REQUEST_METADATA_STREAM_LOSS=record-explicit-gap
scripts/record-request-metadata-stream-loss.sh incident-123 42 exact \
  2026-07-13T01:00:00Z 2026-07-13T01:02:00Z
```

Use `lower-bound` when exact count is impossible; cost exports remain
incomplete. Start replacement Valkey and worker only after recording the gap.

### Secret exposure

Revoke the proxy key or rotate the provider credential first, then activate a
new runtime generation. Rotate master keys only through the procedure below
and record containment in audit/incident records.

## Master-key rotation

`olp master-key` is database-administration only: no HTTP, migrations,
plaintext/envelope output, or raw credentials. It covers provider credentials,
OIDC secrets/flows, and encrypted idempotency replays. Back up the database and
key file first.

Use three stages: add the new key while the old version is active and restart
all replicas; set `active_version` to the new version and restart/verify;
re-encrypt and verify before removing the old key. Obtain values from the
secret manager, never shell history or tickets.

```console
olp master-key status --batch-size 100
olp master-key reencrypt --dry-run --batch-size 100
olp master-key reencrypt --batch-size 100
olp master-key status --batch-size 100
olp master-key verify-retirement --version 1 --batch-size 100
```

`status` and dry-run authenticate every envelope. Re-encryption commits
bounded batches and resumes from the stored key version after interruption.
Retire a version only after zero references and successful authentication;
restart every replica with the reduced keyring and run `status` again. On any
failure retain both keys and record only table/row identifiers, never
ciphertext, nonces, credentials, OIDC state, or replay bodies.
