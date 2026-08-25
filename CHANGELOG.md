# Changelog

All notable changes to OpenLLMProxy are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
semantic versioning and match `Cargo.toml`, `console/package.json`,
`deploy/helm/Chart.yaml` and `deploy/Dockerfile`.

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

## [2.0.0]

Initial 2.0 release.
