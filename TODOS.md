# TODOS

Nothing open. Items carried from the v2.0.1 and v2.1.0 reviews (PRs #111, #114)
and the v2.1.1 recovery pass are all closed. Priorities were: P0 blocks release,
P1 next, P2 soon, P3 when convenient, P4 someday.

## Completed

### Gateway

- Unknown Anthropic content blocks no longer bypass inline-media admission.
  `decode_blocks` refuses any block type outside the round-trip allowlist, and
  refuses an allowlisted block carrying a base64 source at any depth, which
  covers a `document` wrapping a base64 image under `source.type: "content"`.
  `count_tokens` decodes through the same function, so both Anthropic entry
  points are closed. Staging the payloads instead would have needed a matching hydrator arm plus
  an extension walk in `operation_media_handles`, which only sees canonical
  `ContentPart` handles, so a handle parked in an extension would never be
  released. **Completed:** unreleased
- A `Retry-After`-bearing provider 429 counts toward the circuit; a bare 429
  still does not. `counts_toward_circuit` takes the hint as an
  `Option<Duration>` threaded from the two transport call sites; the two
  canonical-stream sites pass `None`, having no header evidence to offer.
  **Completed:** unreleased

### Persistence

- OIDC-sync invitation retirement records no actor rather than blaming the
  demoted user. Migration 0044 relaxes `invitations_revocation_complete` to
  `revoked_by IS NULL OR revoked_at IS NOT NULL`, so the constraint still
  rejects an actor on a row that was never revoked. The same change covers the
  three audit rows the sync writes. **Completed:** unreleased
- The staged dead schema is dropped in migration 0045, and
  `release-metadata.env` advances to 0043. **Completed:** unreleased
- Media-job 5-second poll gate is one `POLL_GATE_SECONDS` constant bound as a
  query parameter by all three queries. **Completed:** v2.1.1 (commit 3ee9ed2)
- The four `audit_events` insert helpers and the inline INSERT literals are one
  `record_audit_event` writer with one column list. **Completed:** v2.1.1
  (commit 3ee9ed2)

### Tests

- `retire_invitations_on_access_loss` is covered through `update_user_role`,
  through an OIDC role-mapping demotion, and through a manual revoke, asserting
  the recorded actor on the invitation and on the audit rows each time.
  **Completed:** unreleased
- Bedrock `service_code_status` mapping. **Completed:** unreleased

### Management API

- No client depends on 422 for an invalid page size. Nothing in the console,
  the SDK smoke tests, the e2e suite or the docs reads it; the console cannot
  even provoke the error, because every page size in `pageSizes.ts` is inside
  1..=200. The 400 is now declared on all 21 paginated endpoints, seventeen of
  which had never declared it, and a contract test fails the build if one stops.
  **Completed:** unreleased
- Duplicate `PageQuery` structs merged into `management/pagination.rs`.
  **Completed:** unreleased

### Earlier

- Read-only note markup extracted into `ReadOnlyNote.svelte`. **Completed:** 2026-08-26
