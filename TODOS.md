# TODOS

Open items from the v2.0.1 pre-landing review (PR #111). Priorities: P0 blocks release, P1 next, P2 soon, P3 when convenient, P4 someday.

## Gateway

### Upstream 401/403 are forwarded to clients as authentication_error / permission_error
**Priority:** P1
**Where:** `apps/olp/src/gateway/error.rs` (`UpstreamRejected` presentation)
An expired provider credential now looks to an SDK like the caller's own gateway key is invalid. Consider keeping 401/403 as 502 with a distinct `code` while passing 400/404/413/422 through.

### Retry-After from one target delays failover to a different target
**Priority:** P1
**Where:** `crates/olp-engine/src/inference/failover.rs` (`retry_backoff`, `jittered.max(retry_after)`, `MAX_UPSTREAM_RETRY_AFTER` 300s)
A 429 with `Retry-After: 240` holds the connection, concurrency slot and TPM reservation for four minutes before trying an unrelated provider. Apply the hint only to the sole-target retry and cap it.

### Sole-target retry ignores route max_attempts and re-sends first-byte timeouts
**Priority:** P2
**Where:** `crates/olp-engine/src/inference/failover.rs` (`with_sole_target_retry`)
`max_attempts = 1` now yields two attempts; a generation that timed out before first byte is re-sent and can be billed twice.

### FinishReason::Error is reported as finish_reason "stop"
**Priority:** P2
**Where:** `apps/olp/src/gateway/openai_chat_response.rs`, `crates/olp-engine/src/protocols/anthropic/mod.rs`
Gemini `MALFORMED_FUNCTION_CALL` and Bedrock `MalformedToolUse` now read as clean completions. Pick a non-success value or emit an error event.

### Unknown Anthropic content blocks bypass inline-media admission
**Priority:** P2
**Where:** `crates/olp-engine/src/protocols/anthropic/translate/decode.rs` (raw block passthrough), `json_media.rs`
A `document` block with base64 `source.data` is forwarded verbatim outside the media size/count limits. Allowlist round-trip block types or stage their base64 like images.

### Retry sleep runs before the next target's circuit permit is checked
**Priority:** P3
**Where:** `crates/olp-engine/src/inference/failover.rs` (`wait_before_retry` before `try_acquire_permit`)

### Provider-wide 429s no longer open the circuit
**Priority:** P3
**Where:** `crates/olp-engine/src/inference/circuit.rs` (`counts_toward_circuit`)
Per-key 429s should stay out, but a `Retry-After`-bearing org-wide 429 could still count.

## Persistence

### OIDC-sync invitation retirement attributes revoked_by to the demoted user
**Priority:** P2
**Where:** `crates/olp-db/src/oidc/identities.rs` (`retire_invitations_on_access_loss` call)
Use a system actor or the `expired_at` path so the invitation does not read as revoked by the person who lost access.

### Media-job 5-second poll gate is a literal in three SQL strings
**Priority:** P3
**Where:** `crates/olp-db/src/media_jobs/lifecycle.rs`, `reconciliation.rs`

## Tests

### Failover execute() has no test for the sleep / deadline-break / sole-target path
**Priority:** P2
**Where:** `crates/olp-engine/src/inference/failover.rs`
Use `#[tokio::test(start_paused = true)]` with a fake transport: fail-once-then-succeed, committed failure, deadline expiring during backoff.

### Conformance matrix still marks RetryAfter inapplicable
**Priority:** P2
**Where:** `tests/conformance/tests/conformance/provider_connectors/matrix.rs`
Connectors now populate `UpstreamSignal.retry_after`; flip the disposition and assert against the existing `retry-after: 7` mocks.

### ReauthenticateDialog has no vitest or Playwright coverage
**Priority:** P2
**Where:** `console/src/lib/components/ReauthenticateDialog.svelte`, `ProfilePage.svelte`
Cover wrong password (dialog stays open with inline error) and empty password.

### retire_invitations_on_access_loss only tested via update_user_access
**Priority:** P3
**Where:** `crates/olp-db/tests/integration/persistence_correctness_postgres.rs`
Add demotion via `update_user_role` and via OIDC role mapping, and assert the audit rows.

### Bedrock service_code_status mapping untested
**Priority:** P3
**Where:** `crates/olp-engine/src/providers/bedrock/transport.rs`

## Management API

### Operations list endpoints moved invalid limit from 422 to 400
**Priority:** P3
**Where:** `apps/olp/src/management/pagination.rs`
Intentional unification; confirm no client depends on 422.

### Duplicate PageQuery structs
**Priority:** P4
**Where:** `apps/olp/src/management/pagination.rs`, `apps/olp/src/management/operations/helpers.rs`

## Console

### Read-only note markup copied across ~9 components
**Priority:** P4
Extract a `ReadOnlyNote.svelte`.

## Completed

- Idempotent replay for revoke/activate/disable/restore. **Completed:** v2.0.1 (2026-08-25)
- `allowed_routes: []` documentation matches enforcement. **Completed:** v2.0.1 (2026-08-25)
- Bedrock shares the Retry-After parser. **Completed:** v2.0.1 (2026-08-25)
