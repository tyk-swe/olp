# M6: Media and operational completeness

[Roadmap](README.md) | [Previous: providers and routing](05-provider-and-routing-parity.md) |
[Next: release and Rust retirement](07-release-and-rust-retirement.md)

**Status:** In progress; media regression fixes and release qualification underway. **Prerequisites:** M5 complete.

Complete current product capabilities with bounded image/audio operations,
durable video jobs, operational diagnostics, and the remaining console
journeys. Media uses the same authorization, routing, and accounting contracts
as generation while retaining its durable ownership requirements.

## Backlog

### M6-01

- [x] **Restore multipart and spool admission.**

**Depends on:** Milestone prerequisites.

**Deliver:** Implement bounded multipart parsing, inline media limits, file
handles, per-endpoint reservations, spool capacity accounting, cleanup, and
filesystem health. Integrate upload cancellation and process shutdown with the
existing request lifecycle.

**Accept:** Oversized bodies/items, excessive parts, malformed uploads, and
insufficient disk/capacity fail predictably. Partial uploads and cancellation
release local reservations and files. Restart cleanup does not remove live
work. Uploaded content is confined to its bounded operational lifetime and
never enters persistent request diagnostics.

**References:** [Multipart handling](../../src/inference/http/multipart.rs),
[media spool](../../src/media/spool.rs),
[spool tests](../../src/media/spool/tests.rs),
[endpoint body policy](../../src/inference/http/endpoint_policy/registry.rs).

### M6-02

- [x] **Restore image generation, editing, and variation.**

**Depends on:** [M6-01](#m6-01).

**Deliver:** Implement the existing OpenAI image endpoints, JSON/multipart
inputs, masks and image options, provider response handling, capability checks,
media pricing units, and bounded output delivery.

**Accept:** Native success/errors, count/size options, invalid masks/uploads,
oversized responses, cancellation, and routing constraints have fixtures.
Only currently supported provider tuples can activate these operations.
Media accounting and missing-usage treatment use M4's shared implementation.

**References:** [Image codecs](../../src/protocols/openai/images.rs),
[image transport](../../src/providers/openai/transport/operations/images.rs),
[media conformance](../../src/providers/openai/transport/tests/media_ops.rs),
[compatibility](../compatibility.md).

### M6-03

- [x] **Restore speech and transcription.**

**Depends on:** [M6-01](#m6-01).

**Deliver:** Implement speech output and transcription uploads, supported
voice/language/format controls, binary/content-type handling, applicable
streaming behavior, usage extraction, and native errors.

**Accept:** Fixtures cover supported formats, multipart bounds, invalid
parameters, truncated/oversized output, timeouts, and client disconnects.
Audio delivery cannot leave leases or files behind. Provider-specific media
support and incomplete pricing/usage remain explicit.

**References:** [Audio codecs](../../src/protocols/openai/audio.rs),
[audio transport](../../src/providers/openai/transport/operations/audio.rs),
[audio tests](../../src/providers/openai/transport/tests/audio.rs),
[media HTTP](../../src/inference/http/media.rs).

### M6-04

- [x] **Restore durable video admission and job ownership.**

**Depends on:** [M6-01](#m6-01).

**Deliver:** Implement asynchronous creation and durable job records, gateway
job IDs, access ownership, request identity, provider job references, pinned
route/provider/slot/credential/price revisions, and durable reservations.
Distinguish confirmed rejection from an ambiguous upstream create outcome.

**Accept:** Client cancellation does not erase an admitted upstream job or
release durable work prematurely. An ambiguous creation cannot trigger blind
failover or a duplicate create. Jobs cannot cross API-key ownership boundaries.
Admission races and crash points preserve the evidence needed for reconciliation.

**References:** [Video execution](../../src/inference/video.rs),
[media creation](../../src/media/service/creation.rs),
[job lifecycle](../../src/media/jobs/lifecycle.rs),
[reservation tests](../../tests/persistence/media_jobs_postgres/reservation.rs).

### M6-05

- [x] **Restore media reconciliation and historical credentials.**

**Depends on:** [M6-04](#m6-04).

**Deliver:** Implement polling/reconciliation workers, durable state transitions,
list/get/content/delete behavior, deletion records, accounting completion, and
recovery after worker restart. Resolve the exact historical credential and
connection retained by the job through rotation and provider changes.

**Accept:** Repeated polls/deletes and worker retries do not duplicate charges
or release reservations twice. Rotation preserves job access; explicit secret
revocation obeys durable deletion/reference guards. Uncertain remote deletion
and reconciliation gaps remain visible. Local spool loss does not erase a
durable provider-owned job reference.

**References:** [Media service](../../src/media/service/),
[job reconciliation](../../src/media/jobs/reconciliation.rs),
[worker](../../src/media/worker.rs),
[media persistence](../../tests/persistence/media_jobs_postgres.rs).

### M6-06

- [x] **Connect media management and console workflows.**

**Depends on:** [M6-02](#m6-02), [M6-03](#m6-03), [M6-05](#m6-05).

**Deliver:** Complete management reads/actions, media job list/detail/content/
delete pages, applicable playground operations, media capability selection,
and request/history presentation. Regenerate contracts and reuse existing
upload and job-state components.

**Accept:** UI polling, cancellation, authorization, pending/failed/deleted
states, and reconciliation warnings agree with durable records. Current media
SDK endpoints work through the Go fixture. Media content and provider secrets
never appear in history or ordinary management payloads.

**References:** [Media HTTP](../../src/media/http.rs),
[media console](../../console/src/lib/features/media/),
[playground](../../console/src/lib/features/inference/playground/),
[media HTTP tests](../../tests/system/media_jobs_http_postgres.rs).

### M6-07

- [x] **Complete metrics, tracing, readiness, and worker health.**

**Depends on:** [M6-05](#m6-05).

**Deliver:** Finish Prometheus metrics, metadata-only JSON logs, optional
OTLP/HTTP request and attempt tracing, provider/runtime health snapshots,
queue completeness, worker checkpoints, and media/spool health. Keep exporter
credentials in secret files and the observability listener private.

**Accept:** An unset tracing endpoint constructs no exporter. Startup rejects
invalid tracing/secret configuration. Inbound context and upstream propagation
follow the existing validation rules; exporter headers never reach providers.
Health is bounded, role-aware, and honest about stale snapshots or failed
workers. Traces/metrics/logs contain no payloads or secret header values.

**References:** [Observability](../../src/observability/),
[trace configuration](../configuration.md),
[OTLP contracts](../../tests/contract/otlp.rs),
[telemetry contracts](../../tests/contract/telemetry.rs).

### M6-08

- [x] **Close console parity and accessibility gaps.**

**Depends on:** [M6-06](#m6-06), [M6-07](#m6-07).

**Deliver:** Audit every current route/feature against the capability map.
Complete overview/setup progress, model/provider/route history, usage/history,
access/settings/audit, media, and health workflows. Simplify duplicated
presentation or client state while preserving useful existing components.

**Accept:** No completed feature depends on a Rust endpoint, placeholder
response, or hidden unfinished workflow. Loading/empty/error/stale/conflict
states, keyboard operation, accessible names, responsive layouts, and
session transitions pass the retained checks. Record screenshots for visible changes.

**References:** [Console pages](../../console/src/routes/),
[overview](../../console/src/lib/features/overview/),
[health console](../../console/src/lib/features/runtime/health/),
[console journey checks](../../console/tests/journeys/console-polish.ts).

### M6-09

- [ ] **Qualify media failure paths and complete product evidence.**

**Depends on:** [M6-08](#m6-08).

**Deliver:** Port remaining media/protocol fixtures and execute end-to-end
media, privacy, and console scenarios. Exercise bounded capacity with maximum
configured uploads/responses, slow readers, concurrent work, mass disconnects,
worker restarts, and credential rotation during active jobs.

**Accept:** Resource reservations, file cleanup, job ownership, and accounting
remain correct across failures. Record memory/spool observations against the
configured limits. All current feature families have passing Go evidence;
remaining M7 work is release/process qualification and retirement.

**References:** [Media spool tests](../../src/media/spool/tests.rs),
[media lifecycle tests](../../src/media/jobs/tests.rs),
[operation corpus](../../tests/fixtures/protocols/selected-operation-families.json),
[production capacity contract](../production-guarantees.md).

## Exit scenarios

- Exercise supported image, speech, transcription, and every video endpoint.
- Cancel uploads and streams; restart workers around creation, polling, and deletion.
- Rotate credentials during retained jobs and exercise explicit revocation guards.
- Verify media accounting, bounded disk/memory use, and metadata privacy.
- Run telemetry/health contracts and the complete console journey set at both origins.
- Close every product capability row before handing the application to M7.
