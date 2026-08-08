# Operations runbook

Availability objectives, routine checks, backup and restore, upgrades,
incident response, and master-key rotation for production OpenLLMProxy.

## Objectives and monitoring

Measure at the client-facing listener. The target-load SLO is 99.9%
successful availability excluding upstream-provider failures, with at most
15 ms p95 and 30 ms p99 added latency. The OLP on-call owns availability and
request-metadata completeness; provider owners own upstream credentials,
quotas, and model availability.

Scrape each in-cluster `*-observability` Service on port 9090 every 15
seconds and probe `/health/live` and `/health/ready` there; the public
listener returns 404 for those paths by design. Readiness snapshots refresh
every five seconds and expensive metric rollups every fifteen, so page on
stale snapshot freshness as well as absent readiness. Page when readiness is
absent for five minutes, request-metadata events are dropped or abandoned,
request-metadata persistence is unavailable, or the distributed limiter is
unavailable while hard limits are configured; warn when a request-metadata
or runtime-outbox backlog stays above its configured threshold for ten
minutes. Page when every asynchronous-plane reporter is stale or when a live
replica cannot take over an outbox owner past its heartbeat window. The
bundled Prometheus rules implement these defaults, one ServiceMonitor per HTTP
component keeps a healthy gateway from hiding control failure, and the
Grafana dashboard is a starting point.
See [deployment.md](deployment.md) for edge routing and observability
exposure.

### Replicated worker health

`/health/ready` and `/metrics` read PostgreSQL-backed fleet summaries; worker
pods do not serve HTTP. `asynchronous_plane: healthy` means every fixed worker
responsibility has a current successful checkpoint and both the
request-metadata consumer group and runtime outbox are drained. It never
requires a particular replica or exactly one worker. Metadata consumption,
outbox publication, and gateway-epoch detection become stale after 20 seconds
without a successful checkpoint (four missed five-second heartbeats).
Maintenance becomes stale after 180 seconds (three missed one-minute passes).
A clean outbox release remains current during the 20-second handoff window, so
a replacement can acquire leadership without leaving a permanent incident.
Run three worker replicas in production and spread them across failure
domains. A failed metadata consumer's pending entry is eligible for reclaim
after 30 seconds and is scanned at least every five seconds, so investigate if
recovery has not begun within 35 seconds. PostgreSQL session loss releases
outbox leadership; another replica probes every five seconds. The 20-second
owner heartbeat is the stale-health and failed-takeover bound, not a reason to
wait before acquiring a released lock.

Use these bounded, content-free signals during diagnosis:

- `olp_request_metadata_consumer_pending_events`,
  `olp_request_metadata_consumer_lag_events`, and
  `olp_request_metadata_consumer_oldest_pending_age_seconds` show group
  backlog.
- `olp_request_metadata_events_reclaimed_total`,
  `olp_request_metadata_events_recovered_total`, and
  `olp_request_metadata_persistence_duplicates_total` show at-least-once
  recovery activity. They count outcomes, not unique event identities.
- `olp_runtime_outbox_pending_rows`, `olp_runtime_outbox_claimed_rows`,
  `olp_runtime_outbox_owner_stale`, and the publication attempt/retry counters
  distinguish backlog, an active side effect, abandoned ownership, and a
  harmless repeated hint.
- `olp_worker_task_healthy{task=...}` and
  `olp_worker_task_runs_total{task=...,outcome=...}` use only four fixed task
  values and three fixed outcomes. No worker, stream, event, route, key,
  provider, or installation identifier is exported.

The counters are PostgreSQL-side additive totals shared by every replica. If
one of the durable summaries cannot be read, readiness returns `null` for
these totals and Prometheus omits their series while setting
`olp_async_worker_observability_available` to zero; an outage therefore does
not appear as a counter reset. The `OLPAsyncWorkerObservabilityUnavailable`
alert covers that unavailable-summary path.

When the plane is current but not drained, inspect the oldest-entry ages and
dependency health before scaling workers. Reclaim and duplicate increments
alone are evidence that recovery worked, not a paging condition. A rising
failed-takeover counter is actionable: inspect the PostgreSQL session holding
the outbox advisory lock before terminating it through the database's normal
operational procedure.

## Routine checks

1. Confirm pod readiness and the same nonzero runtime generation across
   gateways.
2. Check PostgreSQL replication, WAL archiving, disk headroom, and backup
   age.
3. Check Valkey latency and memory. Valkey is runtime state, not the backup
   authority.
4. Review usage completeness and pricing coverage before exporting costs.
5. Review provider health, authentication failures, owner or role changes,
   credential rotations, and route activations in the audit stream.
6. When offboarding a user, rotate or revoke the installation-scoped keys
   attributed to them. Deactivating a user deliberately does not revoke them.
7. Keep media-spool usage below `OLP_MEDIA_SPOOL_CAPACITY_BYTES`. The chart
   provides a 1-GiB process budget in a 2-GiB volume; do not use the 64-MiB
   general `/tmp` mount.
8. Alert on sustained growth in
   `olp_http_admission_rejections_total{surface=...}` and compare
   `olp_http_admitted_requests` with `olp_http_admission_capacity`.
   `OLP_HTTP_MAX_IN_FLIGHT_INFERENCE_REQUESTS` (default 256) and
   `OLP_HTTP_MAX_IN_FLIGHT_MANAGEMENT_REQUESTS` (default 32) configure the
   independent process-wide pools, which reject immediately when full and
   retain permits through streaming completion or cancellation.

![Usage dashboard showing completeness, request volume, and cost](assets/screenshots/usage.png)

Never place prompts, outputs, raw headers, provider credentials, sessions,
proxy-key secrets, or master keys in tickets or diagnostic bundles.

### OpenAI-compatible capability certification

After discovery, review at most 16 exact tuples per compatible model and run
**Certify reviewed capabilities**. Certification sends only
`OLP capability probe`, requests at most one generated token, uses production
codecs, and persists neither prompt nor response. Only `succeeded` tuples
become eligible; `partial` and `failed` remain declared. Remove unsupported
media, asynchronous, or cross-surface claims, or use a separately qualified
native connector. Re-certify after endpoint, model, or credential changes,
and do not activate until every enabled tuple has a certification timestamp.

## Backup and restore

For a production recovery point:

1. Stop admission of new inference requests, leave the worker running, and
   wait for zero pending acknowledgements and zero Stream lag.
2. On an encrypted volume with a PostgreSQL 18 client, `jq`, and GNU
   `sha256sum`, run `scripts/backup.sh` with
   `OLP_BACKUP_TRAFFIC_QUIESCED=true`.

The script requires a durable worker checkpoint that is zero and at most 30
seconds old. Without the quiescence assertion, the manifest records
`request_metadata_stream_drained: false` and the dump is not a production
recovery point. The output is a mode-`0600` custom-format dump, checksum, and
manifest — v2 for current schemas, v1 for the supported legacy schema.
`scripts/backup-manifest.sh` is the executable contract:
`validate BACKUP [v1|v2]` enforces the exact versioned field set, both
recorded checksums, and drained/quiesced/checkpoint-timestamp consistency;
`convert-v2-to-v1` exists only for the legacy producer path and CI fixture.
Legacy v1 manifests remain accepted for restores.

Treat the dump as sensitive: it contains password hashes, session and
proxy-key digests, and encrypted provider/OIDC credentials. Mounted
master-key and authentication HMAC key files are excluded — back them up
separately in the secret manager. Losing any historical master key makes
records encrypted with that version unrecoverable.

The dump includes `installation_identity`. Restoring it preserves every
Valkey namespace and is correct for replacement recovery. It does not create
a second independent installation: stop the source or give the restored copy
a fresh Valkey logical database. Never connect source and restored databases
to the same Valkey keyspace concurrently.

At least weekly, restore the newest dump to an isolated database with
`scripts/restore-rehearsal.sh`. It requires the checksum and a contract-valid
manifest, refuses the production URL, requires `--replace` for a nonempty
destination, and verifies the restored migration count and
runtime-generation ordinal against the manifest. Record duration, both
counts, and the checksum. Start control and gateway processes with a fresh
Valkey, then verify setup, session login, runtime loading, and a
mock-provider request. Do not reuse production OIDC redirects or provider
credentials.

## Upgrade

### Naming migration prerequisites

This release removes the previous deployment-setting names and accepts no
aliases. Before starting the candidate, rename `OLP_PORT` to
`OLP_HOST_PORT`, `OLP_KEY_HASH_KEY_FILE` to `OLP_AUTH_HMAC_KEY_FILE`, and
`OLP_BACKUP_USAGE_CHECKPOINT_MAX_AGE_SECONDS` to
`OLP_BACKUP_REQUEST_METADATA_CHECKPOINT_MAX_AGE_SECONDS`. Keep the existing
authentication HMAC key bytes — replacing them invalidates API keys and
bootstrap-token digests. For Compose, move
`deploy/secrets/olp_key_hash_key` to `deploy/secrets/olp_auth_hmac_key` and
update `OLP_PUBLIC_ORIGIN` if the host port changed.

For Helm, replace `config.keyHashSecretName`/`config.keyHashSecretKey` with
`config.authHmacKeySecretName`/`config.authHmacKeySecretKey` and rename
monitoring values from `usage*` to `requestMetadata*`. Copy the existing
Secret without exposing or regenerating its data:

```console
kubectl --namespace "$NAMESPACE" get secret olp-key-hash-key -o json \
  | jq 'del(.metadata.creationTimestamp, .metadata.resourceVersion, .metadata.uid) \
        | .metadata.name = "olp-auth-hmac-key"' \
  | kubectl --namespace "$NAMESPACE" apply -f -
test "$(kubectl --namespace "$NAMESPACE" get secret olp-key-hash-key -o jsonpath='{.data.key}' | sha256sum)" = \
     "$(kubectl --namespace "$NAMESPACE" get secret olp-auth-hmac-key -o jsonpath='{.data.key}' | sha256sum)"
```

Keep the old Secret until the candidate workloads are healthy and the
rollback decision point has passed.

### Procedure

1. Verify the immutable OCI digest and any signature, provenance, SBOM, and
   vulnerability information the deployment process requires.
2. Run `scripts/upgrade-rehearsal.sh` with a recent backup and candidate
   binary against an isolated database and a fresh isolated Valkey — never
   the production Valkey, whose legacy stream may still receive traffic. The
   script restores and migrates twice and rejects an incomplete or
   non-idempotent result. For a manual N-1 or release rehearsal, set
   `OLP_REHEARSAL_EXPECTED_NEW_MIGRATIONS` to the exact expected count,
   restore the matching keys, and enable the candidate `doctor` smoke. CI
   builds its N-1 fixture from `release-metadata.env`; release operators
   advance its `OLP_PREVIOUS_RELEASED_SCHEMA_MIGRATION` marker in a
   follow-up commit after a release completes — never while qualifying it.
3. Enter a maintenance window. Stop inference admission at the edge and
   freeze every control mutation, including OIDC login/link initiation.
   Drain and scale every old inference-serving workload to zero; verify no
   active requests and no media-reconciliation process that can write
   PostgreSQL remain. Leave the old worker running until Stream pending and
   lag are durably zero, scale it to zero, and confirm both stay zero.
   Pre-upgrade persisted login flows may complete only through their
   existing ten-minute expiry; users whose flow expires restart it after
   the candidate is ready.
   This stop is mandatory for the legacy global
   `olp:v2:request-metadata` transition. Candidate `olp migrate` atomically
   renames that Stream to the durable installation namespace, preserving its
   consumer group and pending-entry state. It aborts if both legacy and
   namespaced Streams exist. Do not allow an N-1 gateway or worker to write
   the legacy key after migration.
4. With admission and the worker stopped, create the final PostgreSQL
   rollback backup using `OLP_BACKUP_TRAFFIC_QUIESCED=true` and snapshot
   mounted key files in the secret manager. This is the recovery point; a
   pre-quiescence backup is not a substitute.
5. Run the Helm upgrade with a timeout of at least 20 minutes. The
   pre-upgrade migration hook completes before the candidate control,
   worker, and gateway Deployments roll (possibly concurrently). Keep
   management and admission frozen until the migration succeeds and every
   workload is on the candidate; the database independently rejects N-1
   runtime publications, non-additive usage rollups, and OIDC completions.
   If a manual live scale-to-zero survived the three-way merge, scale each
   candidate Deployment back to its production replica count, wait for
   `kubectl rollout status`, and verify every running image digest.
   Preserve `maxUnavailable: 0`, the 10-second pre-stop delay, and the
   five-minute termination grace period.
6. Resume admission and OIDC initiation. For 30 minutes, verify readiness,
   zero request-metadata backlog, generation convergence, usage
   completeness, provider probes, error rate, and added latency.

The supported request-metadata delivery and exact-replay window is seven
days; durable event receipts are removed after that bound plus a five-minute
clock-skew grace. Page on backlog long before the window expires — an entry
first delivered outside it is rejected and recorded as uncertain
completeness evidence rather than risking a double-counted hourly aggregate.
Size PostgreSQL for up to `sustained_requests_per_second * 604800` receipt
rows plus the event/request unique indexes, and alert on table growth or
cleanup lag. During a delivery incident, restore or reconcile the Stream
within seven days; do not extend the window by suspending database
maintenance.

Migrations are forward-only. Once any migration beyond the last released
baseline (`release-metadata.env`) applies, an N-1 binary rollback is
unsupported: its runtime, usage-maintenance, and OIDC writes fail closed.
Instead, restore the final pre-upgrade database and mounted keys to a
replacement cluster with fresh Valkey, then verify migration state,
workloads, readiness, and runtime generation before redirecting traffic.

## Incident response

### All workers unavailable

Keep gateway admission open only while the local request-metadata spool has
capacity and the business accepts delayed accounting. Runtime mutations stay
durable in the PostgreSQL outbox but will not reach gateways by hints;
gateways continue their five-second PostgreSQL poll. Restore at least one
same-version worker, then require metadata pending and lag to reach zero,
runtime-outbox pending and claimed rows to reach zero, all four worker task
checkpoints to become healthy, and usage counts to reconcile before declaring
recovery. If the outage approaches the seven-day receipt window, stop
admission and preserve Valkey/AOF and PostgreSQL before any repair.

### Dependency failure

- **PostgreSQL or control** — freeze management changes and keep healthy
  gateways on their last-known-good generation. Restore PostgreSQL instead
  of restarting gateways.
- **Valkey** — hard-limited keys fail closed; keys without distributed
  limits may continue. Restore Valkey and verify lease cleanup. During the
  partial outage `/health/ready` stays successful but reports
  `status: degraded` and `limits: unavailable`, letting Kubernetes route
  unlimited keys; alert on the dependency fields and metrics. Valkey server
  time, not gateway process time, is authoritative for fixed UTC-minute
  RPM/TPM windows, `Retry-After`, and concurrency lease expiry.
- **Request-metadata persistence** — continue inference only with explicit
  business acceptance of incomplete cost data. Preserve logs and Stream
  state, suspend retention, record the affected interval, and reconcile
  request, attempt, usage, and gap counts. Never report an outage gap as
  zero cost.

### Unclean gateway epochs

An unclean process epoch is uncertain, not proof of loss. Readiness and
`olp_request_metadata_gateway_unresolved_epochs` remain degraded until an
owner or operator compares its bounds with the durable worker checkpoint,
Stream state, and request/attempt records. After recording the decision,
list and acknowledge the epoch:

```text
GET  /api/v1/request-metadata/gateway-epochs?state=unresolved
POST /api/v1/request-metadata/gateway-epochs/{process_epoch}/acknowledge
```

Acknowledgement is idempotent, session- and CSRF/Origin-protected, requires
settings-management permission, and emits an audit event. It clears the
unresolved readiness condition but not gap evidence:
`olp_request_metadata_historical_uncertain_gaps` and affected usage windows
remain incomplete. Retention never removes unacknowledged epochs and removes
acknowledged or gracefully closed leases only after rollup.

### Unrecoverable Valkey Stream

If the AOF and replicas are unrecoverable, stop admission and the worker. Do
not attach an empty Valkey until the missing interval is explicit. Derive
its event count and timestamps from the durable consumer checkpoint,
Valkey/AOF inventory, and monitoring, then record it idempotently:

```console
export OLP_CONFIRM_REQUEST_METADATA_STREAM_LOSS=record-explicit-gap
scripts/record-request-metadata-stream-loss.sh incident-123 42 exact \
  2026-07-13T01:00:00Z 2026-07-13T01:02:00Z
```

Use `lower-bound` only when an exact count is impossible and retain that
limitation in the incident record; cost exports remain incomplete. The
helper removes the stale consumer-health checkpoint, writes a content-free
audit event, and prevents double counting by incident ID. Start the
replacement Valkey and worker only after recording the gap, then require a
new checkpoint.

### Secret exposure

Revoke the proxy key or rotate the provider credential first, then activate
a new runtime generation. Rotate master keys only through the versioned
procedure below, retaining the old key until all records are rewritten and
verified. Record containment in the audit and incident records.

## Master-key rotation

`olp master-key` is a standalone database-administration mode: it serves no
HTTP, applies no migrations, prints no plaintext or envelope bytes, and
reports only versions, table names, row counts, and verification status. It
covers provider credentials, OIDC secrets and flow payloads, and encrypted
idempotency replays. Back up the database and key file first.

Use a three-stage keyring rollout:

1. Add the new key with the old version still active. Restart every `all`,
   `gateway`, `control`, and `worker` replica and confirm readiness.
2. Set `active_version` to the new version. Restart and verify every replica
   writes with it.
3. Re-encrypt and verify before removing the old key.

Obtain key values from the secret manager; never put them in shell history
or operational records:

```json
{
  "active_version": 2,
  "keys": [
    { "version": 1, "key": "<base64-old-32-byte-key>" },
    { "version": 2, "key": "<base64-new-32-byte-key>" }
  ]
}
```

With `OLP_DATABASE_URL` and `OLP_MASTER_KEY_FILE` pointing at production and
the two-version keyring, run:

```console
olp master-key status --batch-size 100
olp master-key reencrypt --dry-run --batch-size 100
olp master-key reencrypt --batch-size 100
olp master-key status --batch-size 100
olp master-key verify-retirement --version 1 --batch-size 100
```

`status` and `--dry-run` authenticate every envelope and fail when a
referenced version is absent. Re-encryption authenticates before updating
and commits bounded batches; the stored key version is the progress marker,
so rerunning after interruption resumes safely. Rehearse an interruption
after a logged batch, confirm both versions with `status`, then complete the
run and retain only its metadata logs.

`verify-retirement` rejects the active version, an unmounted version, or any
version still referenced. Only after zero references and successful envelope
authentication may the old key be removed. Restart all replicas with the
reduced keyring and run `status` again. On any failure, retain both keys and
investigate the reported table and row identifier without copying
ciphertext, nonces, credentials, OIDC state, or replay bodies into logs or
tickets.
