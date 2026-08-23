# Bundle 1 — Implementation Plan

Sequencing for [`spec.md`](spec.md). Behavior, schema, and acceptance criteria
live there and are not repeated.

**~2.5 engineer-weeks**, one engineer, including the regeneration and review
surcharge. Confidence is moderate for PRs 1, 2, and 5; lower for PR 4, which is
larger than it looks.

## Ordering

The three changes share no files. They are sequenced by risk retired per day,
not by dependency — with one exception: migrations are sequential, so whichever
of PR 1 and PR 4 merges first takes `0034`. The numbers below assume the order
shown; renumber at merge if it changes.

```
PR 1  soft limits — engine + storage        ships alone
PR 2  soft limits — API + console           needs PR 1
PR 3  same-target retry — engine            ships alone, inert
PR 4  same-target retry — schema + API + console   needs PR 3
PR 5  /v1/completions                        ships alone
```

PR 1 first: largest availability risk retired, smallest change in the bundle.
PR 5 last: the only item with no operational value, so it is the one to drop if
the bundle runs long.

---

## PR 1 — Soft limits: engine and storage

**Touches** `domain/auth.rs`, `inference/limits.rs`, `olp-db/runtime/compiler.rs`,
`observability/metrics.rs`, migration `0034`.

1. Add the enforcement field to `ApiKeyLimits` with `#[serde(default)]`.
2. Migration `0034` adds `api_keys.limit_enforcement`.
3. `compile_api_keys` selects the column.
4. Branch **both** fail-closed paths in `reserve()` — the `http_reserved_tokens`
   reconciliation branch and the main reservation — including their
   backend-missing arms.
5. Add the bypass counters as process-local `AtomicU64`s reachable from
   `ObservabilityState`, rendered directly into the metrics body. Follow
   `olp_open_target_circuits`; there is no registry to register with.

**Regenerate** `make sqlx-prepare`.

**Validate** `make check`; `make db-test` for the compiler change; a limiter
test installing a failing backend and asserting admit-vs-503 per mode.

---

## PR 2 — Soft limits: API and console

**Touches** `management/configuration/api_keys/`,
`console/.../access/api-keys/ApiKeyPolicyForm.svelte`.

1. Accept and return `limit_enforcement` on create, update, and read.
2. Validate in `normalize_api_key_policy` beside the existing limit fields.
3. Console control plus its unit test.

**Regenerate** `make openapi`, `make screenshots`.

**Validate** `make check`, `make console-verify`.

---

## PR 3 — Same-target retry: engine, inert at zero

**Touches** `domain/routing/route.rs`, `domain/routing/selection.rs`,
`inference/failover.rs`.

1. Add `max_retries` to `Target` with `#[serde(default)]`, defaulting to 0.
2. Replace `route.rs`'s target-count check with the shared summed bound helper.
3. `select_attempts_filtered` emits each ranked target `1 + max_retries` times
   consecutively — **after** the eligibility predicate, before `max_attempts`
   truncation — stamping `retry_index` on each `AttemptPlan`.
4. Backoff in `failover.rs`, clamped to `route_deadline`.

With every `max_retries` at 0 this is a no-op: the emitted plan must be
byte-identical to today. That equivalence is the primary test and lets the
engine ship before any schema or UI exists.

**Validate** `make check`; routing fixtures under `crates/olp-engine/tests/`.
Both `tests/fixtures/routing/attempt-order.json` and `retry-taxonomy.json` must
be unchanged — the second already pins retryability classification, which this
change consumes and must not alter.

**Ordering trap:** emitting repeats before the eligibility predicate lets a
filtered target consume attempt budget it never uses. The predicate runs first
by design — keep it that way.

---

## PR 4 — Same-target retry: schema, API, console

**Touches** migration `0035`, `olp-db/runtime/compiler.rs`,
`olp-db/configuration/route_lifecycle.rs`, `olp-db/configuration/validation.rs`,
`management/configuration/routes/`, console route editor.

1. Migration `0035` adds `max_retries` to both target tables.
2. `compile_snapshot` selects it into `Target`.
3. **Move both `olp-db` validators to the shared bound helper.** The invariant
   is duplicated three ways today — `route.rs:48`,
   `route_lifecycle.rs:64`, `validation.rs:96` — and PR 3 only fixes the first.
   Until this lands, no route can be configured with `max_retries > 0`.
4. Draft create and update accept the field; `validate` enforces the bound.
5. `simulate` output lists repeated targets; revision diff reports changes.
6. Console field per target row.

**Regenerate** `make sqlx-prepare`, `make openapi`, `make screenshots`.

**Validate** `make check`, `make db-test`, `make e2e`, `make console-verify`.

**This is the PR most likely to overrun** — the three-site invariant is the
reason, not the schema. A draft that validates but simulates a different plan
than it executes is worse than no simulation at all.

---

## PR 5 — `/v1/completions`

**Touches** `protocols/openai/`, `gateway/endpoint_policy/registry.rs`,
`.../router.rs`, the handler module.

1. Codec: decode, encode, stream, plus 400s for every unsupported field.
2. `Handler::OpenAiCompletions` variant and router wiring.
3. One `fixed_endpoint!` entry — `/openai/v1/completions` with the
   `/v1/completions` alias.
4. Protocol fixtures under `tests/fixtures/protocols/`.

**Regenerate** none — no SQL, no management API type, no visible console change.

**Validate** `make check`, `make e2e`, `make sdk-smoke`.

**Placement:** give the completions codec its own module rather than extending
`chat/`. Not a size constraint — `chat.rs` and its submodules total ~30 KB
across three files, well clear of the `AGENTS.md` ceiling — but the two formats
share only encoding details and diverge on request shape and streaming.

---

## Risk register

| Risk | PR | Mitigation |
|---|---|---|
| Soft mode applied to one fail-closed path only | 1 | Acceptance covers the reconciliation branch; test both |
| Bypass counter has nowhere to live | 1 | Process-local atomics rendered into the body; `olp_open_target_circuits` is the pattern |
| Retry invariant left inconsistent across its three sites | 3, 4 | One shared bound helper, adopted by all three in PR 4 |
| `simulate` diverges from execution | 4 | Same helper; one e2e case asserting plan equality |
| Repeats consume budget for filtered targets | 3 | Emit repeats after the eligibility predicate |
| Retry sleeps past the route deadline | 3 | Skip-and-advance when the remaining budget is shorter than the delay |
| Coverage floor regression | all | `make coverage` before opening each PR, not after review |

## Definition of done

- Every acceptance criterion in `spec.md` has a test that fails without the
  change.
- `make check` and `make coverage` pass on each PR independently.
- `make db-test` and `make e2e` pass on PRs 1, 4, and 5.
- Migrations `0034` and `0035` apply forward against a database at `0033`, and
  an unmodified gateway binary still reads a snapshot compiled by the new one.
- `tests/fixtures/routing/attempt-order.json` is unchanged, proving the retry
  work is inert at `max_retries = 0`.
- The attempt-budget bound has exactly one implementation.
