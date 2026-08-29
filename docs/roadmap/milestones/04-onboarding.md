# Milestone 4 — Onboarding, documentation, presentation

| | |
|---|---|
| Dates | Mon 2026-09-21 → Sun 2026-09-27 |
| Goal | A new user reaches a successful chat completion from the README alone; the docs say which operations work with which providers; the repository looks like what it is |
| Backlog items | DOC-01, DOC-02, DOC-03, DOC-04, TEST-03, GOV-05 |
| Prerequisites | `v2.2.0` published (examples reference the pulled image) |

## DOC-01 — "Your first request" (S)

- [ ] README section directly after Quick start: create a key in the console → `curl` `POST /v1/chat/completions` with `Authorization: Bearer <key>` and `"model": "<route-slug>"` → the response shape → the streaming variant
- [ ] Python and JavaScript snippets for all three surfaces, copied from the smoke tests so they are known-true: OpenAI SDK with `base_url` ending in `/v1`, Anthropic SDK with the `/anthropic` base, Google GenAI with the `/gemini` base
- [ ] Two sentences on why `model` is the route slug, why direct provider/model addressing is unavailable, and where the slugs are listed (`/v1/models`, console → Routes)

## TEST-03 — README example enforced (S)

- [ ] A `tests/e2e` contract test executes the README `curl` example verbatim (extract the fenced block by a stable marker comment) against the harness, asserting a 200 and the documented response shape — the suite already cites README lines, so a drift is a build failure

## DOC-02 — Concepts page (M)

- [ ] `docs/concepts.md`: routes and slugs; providers, drafts, revisions, certification; runtime generations and pinning; keys, route permissions, expiry, hard limits; attempts, usage, pricing; what is stored and what is never stored
- [ ] One request-lifecycle diagram (text or SVG in `docs/assets/`): admission → selection → attempt → terminal envelope
- [ ] Linked from the README documentation table; `docs/architecture.md` points to it for the user-facing model

## DOC-03 — Compatibility matrix (M)

- [ ] `docs/compatibility.md`: surface × operation × provider kind with `native` / `translated` / `refused` per cell and the request fields dropped or refused per translation
- [ ] The surface × operation part is generated: `cargo run --locked -p olp --example export_compatibility` (same pattern as `export_openapi`), with `make compat-check` added to `check-static` so the table cannot drift from `apps/olp/src/gateway/endpoint_policy/registry.rs`
- [ ] Per-provider notes written by hand from the conformance fixtures, citing fixture file names so `tests/conformance` stays the source of truth

## DOC-04 — Helm NetworkPolicy (S)

- [ ] `deploy/helm/templates/networkpolicy.yaml` behind `networkPolicy.enabled: false`, with values for edge ingress (namespaces / CIDRs to the public port), Prometheus (namespace / labels to 9090), and egress (PostgreSQL, Valkey, providers on 443; default allow-all egress documented as the starting point)
- [ ] `values.yaml`, `values.schema.json`, and `docs/deployment.md` change together; `make helm-verify` green
- [ ] `scripts/verify-helm-contract.sh` renders the `networkPolicy.enabled=true` case

## GOV-05 — Repository presentation (S)

- [ ] GitHub description: either the README one-liner or the current warning — but the README and the description tell the same story
- [ ] Topics: `llm-gateway`, `ai-gateway`, `openai-compatible`, `anthropic`, `gemini`, `bedrock`, `rust`, `sveltekit`, `self-hosted`
- [ ] `.github/ISSUE_TEMPLATE/` (bug, provider drift, feature), `.github/CODEOWNERS` (`* @tyk-swe`), `CODE_OF_CONDUCT.md`
- [ ] Discussions on, or `CONTRIBUTING.md` states that issues are the only channel
- [ ] `SECURITY.md`: acknowledgement window and the supported window expressed in versions rather than "2.x"

## Exit criteria

- [ ] Someone who has not seen the repository reaches a successful completion from the README; record the time it took
- [ ] `make check-static` includes `compat-check` and passes
- [ ] NetworkPolicy renders and passes `helm lint --strict`
- [ ] Description, topics, templates, CODEOWNERS in place

## Carry-over

_None yet._
