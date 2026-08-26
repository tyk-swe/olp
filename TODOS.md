# TODOS

Open items carried from the v2.0.1 and v2.1.0 reviews (PRs #111, #114) and the v2.1.1 recovery pass. Priorities: P0 blocks release, P1 next, P2 soon, P3 when convenient, P4 someday.

## Gateway

### Unknown Anthropic content blocks bypass inline-media admission
**Priority:** P2
**Where:** `crates/olp-engine/src/protocols/anthropic/translate/decode.rs` (raw block passthrough), `json_media.rs`
A `document` block with base64 `source.data` is forwarded verbatim outside the media size/count limits. Allowlist round-trip block types or stage their base64 like images.

### Provider-wide 429s no longer open the circuit
**Priority:** P3
**Where:** `crates/olp-engine/src/inference/circuit.rs` (`counts_toward_circuit`)
Per-key 429s should stay out, but a `Retry-After`-bearing org-wide 429 could still count.

## Persistence

### OIDC-sync invitation retirement attributes revoked_by to the demoted user
**Priority:** P2
**Where:** `crates/olp-db/src/oidc/identities.rs` (`retire_invitations_on_access_loss` call)
Use a system actor or the `expired_at` path so the invitation does not read as revoked by the person who lost access.

### Drop the staged dead schema once 2.1.0 is fully rolled out
**Priority:** P2
**Where:** `crates/olp-db/migrations/0038_drop_unused_schema.sql`, `0039_relax_duplicate_attempt_windows.sql`
`oidc_authorization_flows.client_digest`, the `request_metadata_loss_reporter_state` table, and `attempt_usage_facts.attempt_started_at` / `.attempt_completed_at` are all unread and unwritten by 2.1.0, but the 2.0.1 binary still names them in its INSERTs. Dropping them during the rollout would fail closed on any N-1 replica still serving, so 2.1.0 only relaxes the constraints. Drop the column, the table, and the two columns in a migration that ships after no 2.0.1 replica can be running.

### Media-job 5-second poll gate is a literal in three SQL strings
**Priority:** P3
**Where:** `crates/olp-db/src/media_jobs/lifecycle.rs`, `reconciliation.rs`

### Four audit_events insert helpers and ~22 inline INSERT literals
**Priority:** P3
**Where:** `crates/olp-db/src/authentication.rs` (`insert_security_audit`), `identity.rs` (`insert_audit`), `oidc/helpers.rs` (`insert_audit`), `configuration/resources/helpers.rs` (`audit_in_transaction`)
Four near-identical helpers plus the inline literals scattered across the crate each spell out the same column list, so a new audit column has to be threaded through all of them. Collapse them into one writer.

## Tests

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

## Completed

- Upstream 401/403 surface as 502 `upstream_authentication_failed` / `upstream_permission_denied` on every protocol surface. **Completed:** v2.1.1
- Retry-After capped at 30s and applied only to a same-target retry; the backoff sleep now runs after the circuit permit check. **Completed:** v2.1.1
- Sole-target retry honours route `max_attempts` and never re-sends a billing-uncertain attempt. **Completed:** v2.1.1
- `FinishReason::Error` renders as `error` (OpenAI chat), `refusal` (Anthropic) and `status: failed` (Responses). **Completed:** v2.1.1
- Paused-clock `execute()` tests and the RetryAfter conformance contract. **Completed:** v2.1.1

- Idempotent replay for revoke/activate/disable/restore. **Completed:** v2.0.1 (2026-08-25)
- `allowed_routes: []` documentation matches enforcement. **Completed:** v2.0.1 (2026-08-25)
- Bedrock shares the Retry-After parser. **Completed:** v2.0.1 (2026-08-25)
- ReauthenticateDialog vitest coverage for the wrong-password and empty-password paths. **Completed:** 2026-08-26
- Read-only note markup extracted into `ReadOnlyNote.svelte`. **Completed:** 2026-08-26
