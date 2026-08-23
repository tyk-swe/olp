# Bundle 1 — Specification

Three independently shippable changes: two retire the largest operational risks
in the gateway, one adds a legacy compatibility endpoint.

| # | Change | Size |
|---|---|---|
| 1 | Soft limit enforcement tier | S |
| 2 | Same-target retry | M |
| 3 | `POST /v1/completions` | S |

## Scope note

`/audio/translations` was originally scoped here as S. It is neither, and it is
not in this bundle. `ProviderKind::supports_capability` keys on
`(OperationKind, Surface, TransportMode)`
(`crates/olp-engine/src/domain/routing/provider.rs`), so reusing
`OperationKind::Transcription` would let a Transcription certification probe
certify an unprobed translations tuple — a default-deny violation. It needs its
own `OperationKind`, reaching 26 source files, the `prices_operation_check`
constraint (`migrations/0021`), the capability matrix, and a new probe. Sized M,
scheduled separately.

`/v1/completions` is a deprecated endpoint kept for legacy clients. Changes 1
and 2 carry this bundle's operational value and stand without it.

---

## 1. Soft limit enforcement tier

### Problem

`reserve()` (`crates/olp-engine/src/inference/limits.rs`) fails closed on any
backend error, missing backend, or 1-second timeout, returning
`distributed_limits_unavailable`. Keys with no limits skip the limiter entirely;
keys with any limit set take the fail-closed path. `ApiKeyLimits` offers no
other mode (`crates/olp-engine/src/domain/auth.rs`).

Configuring limits across all keys — which governance implies — makes Valkey a
hard dependency for 100% of gateway traffic with no degraded mode. Fail-closed
is correct for a hard limit; the gap is that no other kind exists.

### Behavior

One per-key mode, not per-dimension. `ApiKeyLimits` is three `Option`s behind a
flat OR in `has_hard_limits()`, and no named use case mixes modes within a key.

```
limit_enforcement: hard | soft    (default: hard)
```

| | Backend reachable | Backend error, missing, or timeout |
|---|---|---|
| `hard` | enforce; 429 on exceedance | 503 `distributed_limits_unavailable` |
| `soft` | enforce; 429 on exceedance | **admit**, record bypass |

Soft governs *unavailability*, never *exceedance*.

### Bypass must be observable

A silent bypass is a hole, not a soft limit. Every soft-path admission
increments `olp_limit_bypass_total{reason}` — `backend_error`,
`backend_missing`, or `timeout` — and emits a `WARN` carrying the key's lookup
id.

**There is no live metrics registry.** `CachedMetrics`
(`apps/olp/src/observability/cache.rs`) refreshes from PostgreSQL rollups every
fifteen seconds; the metrics body is a string assembled per scrape. Follow the
`olp_open_target_circuits` precedent (`observability/metrics.rs:154`): hold
process-local state reachable from `ObservabilityState` and render it directly
into the body. An `AtomicU64` per reason is sufficient. Counters are per
replica, which is correct for a Prometheus scrape.

The counter is the alertable signal — continuous increments mean an outage the
deployment is absorbing. The audit stream is for administrative changes and is
not the home for a per-request runtime event.

### All fail-closed sites

`reserve()` contains two independent paths, each with its own backend-missing
arm and error arm:

- the `http_reserved_tokens` reconciliation branch
- the main reservation branch

Both must honour the mode. Patching one ships a key that is soft on admission
and hard on reconciliation.

### Schema

Forward-only migration `0034`:

```sql
ALTER TABLE api_keys
    ADD COLUMN limit_enforcement text NOT NULL DEFAULT 'hard'
        CHECK (limit_enforcement IN ('hard', 'soft'));
```

Existing rows default to `hard`, so upgrade preserves behavior.
`compile_api_keys` (`crates/olp-db/src/runtime/compiler.rs`) selects the column.
The new field carries `#[serde(default)]` so a release compiled by an older
binary still deserializes. `ApiKeyLimits` derives `Default`, so the mode enum
needs a derived `Default` of `Hard` — not an afterthought, it is what makes the
absent-field case fail closed.

### API and console

`POST` and `PATCH /api/v1/api-keys` accept `limit_enforcement`; `GET` returns
it. Validate in `normalize_api_key_policy`
(`apps/olp/src/management/configuration/api_keys/policy.rs`). Console adds one
control to `ApiKeyPolicyForm.svelte`.

### Acceptance

- `hard` + backend down → 503, counter unchanged.
- `soft` + backend down → 200, `reason="backend_error"` +1, `WARN` logged.
- `soft` + backend missing → 200, `reason="backend_missing"` +1.
- `soft` + backend up + over RPM → 429.
- `soft` + reconciliation branch + backend down → 200, counter +1.
- Upgrade over existing rows → every key reads `hard`.

---

## 2. Same-target retry

### Problem

`Route::validate` rejects `max_attempts > targets.len()`
(`crates/olp-engine/src/domain/routing/route.rs:48`), and
`AttemptDisposition::Retry` advances to the next `AttemptPlan` rather than
re-running the current one (`crates/olp-engine/src/inference/failover.rs`).

N targets buy N−1 failovers to *different* providers and zero retries against
the same one. A transient 502 costs a target for the remainder of the request; a
single-target route has no resilience at all.

### Design — per-target retry count

`Target` gains `max_retries: u16` (default 0). `select_attempts` emits each
ranked target `1 + max_retries` times consecutively before advancing to the next
target in its priority group. The invariant becomes:

```
max_attempts <= Σ (1 + target.max_retries)
```

*Rejected alternative:* relaxing the invariant and letting attempts repeat
implicitly. That makes the plan depend on arithmetic the operator cannot see,
and `POST /api/v1/route-drafts/{id}/simulate` exists precisely so the plan is
inspectable before activation. Per-target counts keep it enumerable.

`AttemptPlan` gains `retry_index: u16`. Without it two repeats against one
target are byte-identical, and `simulate` cannot show the operator what it will
actually do. Truncation already returns early when `attempts.len() == maximum`
inside the target loop, so a partially-consumed retry sequence truncates
correctly with no further change.

### The invariant lives in three places

It is already duplicated today, and all three must move together:

| Site | Current check |
|---|---|
| `crates/olp-engine/src/domain/routing/route.rs:48` | `max_attempts > targets.len()` |
| `crates/olp-db/src/configuration/route_lifecycle.rs:64` | same, on draft write |
| `crates/olp-db/src/configuration/validation.rs:96` | same, on validation |

Extract one shared bound helper rather than editing three expressions. Three
independent implementations of an attempt-budget rule that disagree is exactly
the failure mode `simulate` exists to prevent.

### Backoff

Exponential with full jitter: `RETRY_BACKOFF_BASE = 100ms`,
`RETRY_BACKOFF_CAP = 2s`, clamped to the route deadline. `failover.rs` already
computes `route_deadline` and
`attempt_deadline = route_deadline.min(started + attempt.timeout)`. When the
remaining budget is shorter than the delay, skip the retry and advance rather
than sleeping past the deadline.

Constants, not route configuration. Promote to schema when a deployment needs
different values.

### Composes with existing mechanisms

No new machinery is required — do not rebuild these:

- **Circuit breaker** is consulted per attempt, so a repeated target
  self-limits once the threshold trips.
- **`response_committed`** already bars further attempts after bytes ship, so a
  mid-stream failure is never retried.
- **`error.retryable`** (`failover.rs:520`) remains the gate for whether a
  failure may be retried at all.

### Accounting

A retry is a distinct attempt. `attempts` is unique on `(request_id, ordinal)`
(`migrations/0001_initial.sql:336`) and `attempt_usage_facts` keys on
`attempt_id` — both ordinal-shaped, neither target-shaped. Two attempts against
one target therefore take distinct ordinals and insert cleanly, with independent
usage attribution. No accounting change, no ingestion change.

### Schema

Forward-only migration `0035`:

```sql
ALTER TABLE route_draft_targets
    ADD COLUMN max_retries smallint NOT NULL DEFAULT 0
        CHECK (max_retries >= 0 AND max_retries <= 5);

ALTER TABLE route_revision_targets
    ADD COLUMN max_retries smallint NOT NULL DEFAULT 0
        CHECK (max_retries >= 0 AND max_retries <= 5);
```

The cap keeps the attempt plan bounded. `compile_snapshot` selects it into
`Target`, which carries `#[serde(default)]` for the reason given above.

### API and console

- Draft create and update accept `max_retries` per target.
- `validate` enforces the summed bound.
- `simulate` lists repeated targets explicitly — the signal that the plan stayed
  honest.
- Revision diff reports `max_retries` changes.
- Console route editor gains one field per target row.

### Acceptance

- Single-target route, `max_retries = 2`, provider returns 502, 502, 200 → one
  request, three attempts, 200 to the client.
- Same with a third 502 → 502 to the client, three attempt records.
- Route deadline expiring after attempt 2 → no third attempt, deadline error.
- Non-retryable error → no retry regardless of `max_retries`.
- Streaming failure after the first byte → no retry.
- Circuit opening mid-sequence → remaining repeats suppressed.
- `validate` rejects `max_attempts` above the summed bound, at all three sites.
- `simulate` lists repeated targets in execution order.
- `max_retries = 0` everywhere → attempt plan byte-identical to today;
  `tests/fixtures/routing/attempt-order.json` and `retry-taxonomy.json` both
  unchanged.

---

## 3. `POST /v1/completions`

### Why this is S

`OperationKind::Generation` already serves three distinct wire formats —
`/chat/completions`, `/responses`, `/anthropic/v1/messages`
(`apps/olp/src/gateway/endpoint_policy/registry.rs`) — each as its own
`Handler`. The established pattern: a new wire format is a new `Handler` plus a
codec, not a new `OperationKind`.

So no enum change, no `prices_operation_check` migration, no capability-matrix
entry, no new certification probe. `(Generation, OpenAi, Unary | Streaming)` is
`shared_canonical_operation`, supported by every provider kind and already
certified wherever Generation is.

### Behavior

Register `POST /openai/v1/completions` with the `/v1/completions` alias:
`Surface::OpenAi`, `OperationKind::Generation`, `TokenEstimate::Generation`,
`BodyAdmission::Standard`, and a new `Handler::OpenAiCompletions` wired in
`endpoint_policy/router.rs`.

Codec in `crates/olp-engine/src/protocols/openai/`:

- **decode** — `prompt` (string) becomes a single user message in
  `GenerationRequest`.
- **encode** — canonical result renders as `choices[].text`,
  `object: "text_completion"`.
- **stream** — `choices[].text` deltas, `finish_reason` on the terminal chunk,
  `[DONE]` sentinel.

Unsupported fields are rejected with 400 and a field error, never dropped:
array-of-strings and token-array `prompt`, `logprobs`, `echo`, `best_of`,
`suffix`. Best-effort parameter dropping is against repository policy
(`AGENTS.md`, README).

### Acceptance

- Non-streaming request against a Generation-certified route → 200,
  `object: "text_completion"`.
- Streaming request → SSE with `text` deltas and `[DONE]`.
- Array `prompt` → 400 with a field error.
- `best_of` present → 400.
- Route targeting an Anthropic provider → canonical Generation renders upstream
  as messages; response returns as `text`.
- Model listing and visibility unchanged.

---

## Generated artifacts

A pull request missing any of these fails `make check` or the CI PostgreSQL job.

| Trigger | Command |
|---|---|
| New or changed SQL query | `make sqlx-prepare` (CI verifies via `make sqlx-check`) |
| New or changed management API type | `make openapi` |
| Visible console change | `make screenshots` |
| Any change | `make check`, `make coverage` (62% line floor) |
| Storage or limiter change | `make db-test`, `make e2e` |

Migrations are forward-only and sequential from `0034`.
