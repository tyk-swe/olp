# Changelog

All notable changes to OpenLLMProxy are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
semantic versioning and match `Cargo.toml`, `console/package.json`,
`deploy/helm/Chart.yaml` and `deploy/Dockerfile`.

## [2.1.1] - 2026-08-26

Recovery release: the repository is brought back in line with its own rules
(AGENTS.md size limits, Makefile/CI lockstep, an accurate N-1 baseline) and
the gateway bugs left open by the 2.0.1 review are closed. No migrations.

### Changed

**Gateway (client-visible)**
- A provider that rejects its own credential (upstream 401/403) now returns
  502 with `error.code` `upstream_authentication_failed` or
  `upstream_permission_denied` on the OpenAI, Anthropic and Gemini surfaces.
  Previously the upstream status was repeated verbatim, so an expired
  provider key looked like an invalid gateway key. 400/404/405/409/413/415/422
  still pass through.
- `FinishReason::Error` (Gemini `MALFORMED_FUNCTION_CALL`, Bedrock
  `MalformedToolUse`, …) is no longer reported as a clean completion: OpenAI
  chat emits `finish_reason: "error"`, Anthropic `stop_reason: "refusal"`,
  and the Responses API `status: "failed"` with an `error` body.
- A provider `Retry-After` applies only to a retry of that same target and is
  capped at 30 seconds; it no longer delays failover to an unrelated provider,
  and the backoff sleep now runs after the next target's circuit check.
- The sole-target retry honours the route's `max_attempts` and is never taken
  after a billing-uncertain failure such as a first-byte timeout.

**Repository**
- `release-metadata.env` pins migration `0037`, the last one shipped in
  2.0.1; it had never been advanced past `0021`, so the upgrade rehearsal was
  exercising a jump no deployment performs. `make release-version` now checks
  the baseline names a tracked migration and that the Rust toolchain pin
  agrees across `rust-toolchain.toml`, the CI action and the Dockerfile.
  The rehearsal itself seeded a table renamed in 0028 and expected a v1
  backup manifest; both now match the 0037 baseline, and the rehearsal
  reports the real 37 → 43 upgrade.
- `make source-size` (in `make check` and CI) enforces the 30 KB file and
  100-line function rules against `scripts/source-size-baseline.txt`, which
  may only shrink. The worst offenders were split: `run_maintenance`,
  `activate_provider`, `complete_oidc_login`, `serve`, `collect_metrics`,
  `append_async_worker_metrics`, `collect_readiness`, `callback_inner` and
  the Anthropic stream encoder.
- One `audit_events` writer replaces four helpers and 22 inline INSERTs.
- CI dispatches through the Makefile; `make coverage` uses the `ci` nextest
  profile like CI does; `cargo deny` is the single advisory policy.
- The Helm values schema covers security-context and scheduling keys and
  rejects unknown top-level keys.
- Console: prettier runs on the whole tree instead of a hand-maintained
  allowlist; login and invitation-acceptance logic lives under
  `src/lib/features`; one `errorMessage()`, one password policy, one set of
  page-size constants.

### Removed
- Orphaned `skills-lock.json`, `.Jules/`, Storybook ignore entries, the dead
  2.0 environment-rename table in `docs/operations.md`, and the separate
  `cargo audit` CI step.

## [2.1.0] - 2026-08-26

Backend capability that shipped without a console, and console pages that
fetched data they never showed, are now wired end to end. Dead code found in
the same audit is gone; the dead schema is retired but not yet dropped, because
the 2.0.1 binary still writes part of it during a rolling upgrade.

### Added

**Management API**
- `GET /api/v1/audit` filters: `action`, `resource_type`, `resource_id`,
  `actor_user_id`, `outcome`, `occurred_after`, `occurred_before`. Events now
  record `source_ip` (same trusted-proxy rules as admission) and a coarse
  `user_agent_family` for session-driven actions; worker-originated rows leave
  both null. Blank or whitespace-only string filters are ignored rather than
  matched literally. A malformed parameter returns 400
  `invalid_query_parameters`; an `occurred_after` that is not strictly earlier
  than `occurred_before` returns 422 field validation, matching usage, requests
  and media jobs.
- `installation_name` on `SessionResponse`; the setup form can set it. The
  unauthenticated `GET /api/v1/setup/status` deliberately returns only
  `setup_required`.
- `created_by_email` on providers, routes and route drafts;
  `invited_by_email`/`accepted_by_email`/`revoked_by_email` on invitations;
  `updated_by_email` on the OIDC configuration.
- Provider 422 responses carry `error_codes` (`{field: [code]}`) alongside
  `errors`, positionally aligned with them: `error_codes[field][i]` classifies
  `errors[field][i]`, and an uncoded message is padded with an empty string
  rather than shifting its neighbours.
- `uncertainty_gap_id` on gateway-epoch responses.
- Readiness reports `media_spool_used_bytes`/`media_spool_capacity_bytes`;
  metrics gain `olp_media_spool_{used,capacity}_bytes` and
  `olp_request_metadata_loss_reported_total{kind}`. Loss checkpoints are logged
  instead of discarded.
- Migrations 0040-0043 add the indexes the new audit filters and the
  provider-health window need: `audit_events(action, occurred_at DESC, id DESC)`,
  `(resource_type, resource_id, occurred_at DESC, id DESC)`,
  `(actor_user_id, occurred_at DESC, id DESC)`, and
  `attempts(provider_id, started_at DESC)`. Each builds CONCURRENTLY in its
  own migration.

**Console**
- Providers: disable and restore-as-draft actions (the "still routed" guidance
  appears only for the `configuration_resource_in_use` problem, not every
  409), a `disabled` badge that wins over a stale active-revision label,
  editing/rotation/discovery controls hidden while disabled with a pointer to
  Restore as draft, per-revision viewer with the historical model/capability
  inventory, probe type and discovered-model count, a restore notice stating
  that no historical credential is ever restored, certification `error_code`,
  `discovered_at`, client-side 16-tuple cap, `connector_ready` as an activation
  gate.
- Health: asynchronous plane, worker staleness, runtime outbox, media
  reconciliation, persistence pipeline and media spool sections with ages
  checked against the runbook thresholds (the outbox's oldest pending row is
  held to its own 20-second bound); provider-health window selector; full
  gateway-epoch acknowledgement provenance. Distributed limits reported as
  `not_configured` show as a warning rather than a failure.
- Usage/Requests/Settings: cached-input tokens everywhere the API returns them,
  `cached_input_per_million` in the pricing form, approximate-range and
  priced-count notes, `completed_at` on requests and attempts, who-updated on
  settings and pricing revisions.
- Media jobs: api key, provider and created-at range filters; Created and
  Updated in the list, with completion, polling, retention and deletion
  timestamps on the job detail panel; stacked layout on narrow viewports.
- Playground: temperature and max-output-token controls; refusals,
  `finish_reason`, `provider_model` and full usage are shown (a refusal no
  longer renders as an empty result).
- Audit page filters (an end earlier than the start is refused inline before
  any request) and origin columns; installation name in the shell, truncated
  rather than crowding the account menu; invitation expiry selector and
  accepted/revoked/created timestamps; API keys read "No expiry" / "Never
  rotated" instead of a dash; route draft provenance and revision anchors;
  activation now adopts the returned `draft_etag`.
- Numbers, byte sizes and timestamps are formatted in a fixed locale so they
  read the same on every operator's machine.
- `ReadOnlyNote` component replaces 12 copies of the same markup;
  `ReauthenticateDialog` unit tests; eslint now checks `.svelte` scripts and
  `noUnusedLocals` is on.

### Removed
- Unreachable `CapabilitySource::Probed`, `ResponsesCodecError::{UnsupportedTool,
  TokenCountSemanticsUnsupported}`, `Error::InvalidRuntimeSnapshot`,
  `decode_video_content`, the superseded domain-level last-owner check, and
  never-read `AcceptedInvitation` fields. Test-only store/engine helpers are
  gated behind `test-util`.
- Migrations 0038 and 0039 retire dead schema: the orphaned `(created_at, id)`
  cursor indexes, the `usage_facts` route index, the redundant outbox pending
  index, the per-browser OIDC limiter indexes and its security-context
  requirement, the `client` public-auth rate-limit scope, and the `probed`
  capability source (surviving `probed` rows are rewritten to `declared`).
  `oidc_authorization_flows.client_digest`,
  `request_metadata_loss_reporter_state` and
  `attempt_usage_facts.attempt_{started,completed}_at` are deliberately not
  dropped: the 2.0.1 binary still names all three in its own writes, so 0039
  only drops the two columns' NOT NULL and the drops themselves ship in a
  later release.

### Changed
- Credential rotation, model discovery and capability review carry the
  `state <> 'disabled'` guard in the UPDATE itself, matching the draft-save
  path, so none of them can park a disabled provider back in `draft` without
  `restore_as_draft`.
- A test asserts every mounted management route appears in the OpenAPI
  document, and that every management source file mounting routes is parsed.

## [2.0.1] - 2026-08-25

### Fixed

**Protocol translation**
- Anthropic content blocks are classified by their `type` field. A `thinking`
  block that also carries `id`/`name`/`input` no longer turns into a tool use
  that the decoder then rejects; a known type with a malformed body fails
  closed instead of passing through as opaque.
- Anthropic thinking blocks round-trip, and reasoning models are accepted
  instead of rejected.
- Tool-call ids, finish reasons, zero-argument tool encoding and continuation
  deltas are correct across surfaces; the Responses lifecycle completes.
- Cross-surface extensions are dropped on the response path instead of
  answering 502.
- Canonical `output_tokens` excludes reasoning tokens; accounting meters the
  reasoning-inclusive sum. Anthropic cache tokens are no longer double
  counted. `stream_options.include_usage` is honoured. Image MIME types are
  carried canonically.

**Gateway and limits**
- Upstream 4xx statuses are forwarded, streaming failures before the first
  byte return a real status, and error types match the documented OpenAI set.
- Multipart requests accept `model` after `file`.
- `Retry-After` is parsed with jittered backoff and a bounded sole-target
  retry; per-key 429s stay out of the circuit breaker; circuits are keyed on
  routing id.
- Reservations are refunded on failure, truncated streams are billed as
  uncertain, unfittable requests get a 400, and route timeouts govern the
  first-byte deadline.
- Cached-input pricing tier (migration 0037).

**Management API**
- API keys use merge-patch semantics; owners cannot change their own role;
  drafts are consumed on activation; one pagination contract; `Location` on
  creates; 400/409 responses declared in OpenAPI.
- Retrying a revoke, activate, disable or restore with the same
  `Idempotency-Key` replays the recorded response instead of answering 409.
  The recorded envelope previously tripped a check constraint, so the replay
  never happened.
- The `PATCH /api-keys/{id}` schema now says what `allowed_routes: []`
  does: it clears the allowlist, which leaves the key unrestricted.

**Persistence**
- Disabling a provider keeps its revision and draft targets; activation is
  checked per target.
- Usage buckets are UTC-explicit (migration 0035 rewrites the 0019
  constraint).
- Polled video jobs are not counted stale; backoff resets on success;
  rejected auth attempts are still metered.
- Invitations are retired with the inviter and stamped `expired_at`
  (migration 0036).
- Release verification pages past corrupt rows; worker staleness uses the
  database clock; loss evidence is recorded before telemetry.

**Console**
- Every write control is gated on the matching manage capability and the
  role is read reactively, so read-only members no longer see actions they
  cannot perform.
- The right query keys are invalidated after key rotate/revoke, model
  toggles and member changes, so the screen no longer shows stale state.
- API failures are no longer rendered as "Not configured"; server problem
  detail is surfaced.
- "— ms", "#—", null usage completeness, compact per-request tokens and the
  "Step 1 of 6" copy are fixed in the request explorer.
- The provider wizard resets connector fields and the credential when the
  kind changes, gains a Back button with the kind locked once a draft
  exists, and replaces `window.prompt` with a masked dialog.

**CI**
- Regenerated the stale fuzz lockfile; the outbox advisory-lock takeover
  retries within a bound; WebKit, Firefox and mobile Chromium screenshot
  baselines refreshed with a 50 px tolerance; line coverage lifted above the
  62% gate.

## [2.0.0] - 2026-07-19

Initial 2.0 release.
