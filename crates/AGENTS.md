# crates/ — library crate guide

- **domain** — canonical model: provider configuration
  (`provider_configuration.rs`), routing eligibility + weighted rendezvous
  scoring (`routing.rs`), ports. Must never gain infrastructure dependencies.
- **protocols** — vendor wire translation: OpenAI/Anthropic/Gemini DTOs,
  request/response translation, SSE stream decoding. Mirrored per-provider
  trees (`translate/`, plus `openai/responses/` for the Responses API).
- **providers** — outbound networking: per-provider transports
  (openai/anthropic/gemini/azure_openai/bedrock/vertex), OIDC network calls,
  egress IP policy (`http_egress.rs`). Only crate allowed `reqwest`, `aws-*`,
  `google-cloud-auth`. Bedrock specifics: `providers/BEDROCK.md`.
- **storage** — PostgreSQL via sqlx (typed macros only — no string-key
  `Row::get`; enforced by `scripts/check-storage-sqlx.sh`), Valkey, Lua
  scripts in `storage/scripts/`, forward-only migrations in
  `storage/migrations/`. Query metadata lives in `/.sqlx` —
  regenerate with `make sqlx-prepare` after query/schema changes.

Conventions:

- The `test-util` cargo feature exposes internals to tests; prefer it over
  new `pub` surface area.
- Unit tests are `src/**/tests.rs` submodules; `tests/*_postgres.rs`
  integration tests are `#[ignore]`d and run via `make db-test`.
- The dependency DAG and per-crate dependency ownership are asserted by
  `scripts/check-boundaries.sh` — adding a dependency to the wrong crate
  fails CI.
