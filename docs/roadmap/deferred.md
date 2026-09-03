# Deferred

Deliberately outside the current execution period. Each entry names the trigger
that would bring it into [`backlog.md`](backlog.md); until a trigger fires, do
not start it.

| Item | Why not now | Reopen when |
|---|---|---|
| Exact-match response cache | Needs a content-hashing design that keeps the "never store prompts or outputs" invariant intact — keying on a hash of the request body is a form of storage and must be argued through, not assumed. Budgets and tracing come first because they reuse machinery that already exists. | A user asks for cost reduction on repeated prompts, or spend-control data shows a high share of identical requests |
| Semantic cache | Everything above, plus an embedding dependency and a similarity policy | Only after an exact-match cache has shipped and been measured |
| Guardrail / policy hooks (pre- and post-inference) | Product decision pending: in-process hooks versus an external policy endpoint. Bedrock guardrail outcomes already map to `FinishReason::ContentFilter`. | A concrete integration request, or a compliance requirement from a deployment |
| MFA for local login | OIDC is the production path and `OLP_LOCAL_LOGIN_ENABLED=false` removes the surface. Local login exists for bootstrap and small installations. | A deployment must run local login in production and says so |
| OpenAI Files, Batch, Realtime/WebSocket surfaces | Large surface area, stateful semantics that conflict with the stateless gateway model, no recorded demand | Two independent requests, or a provider makes one of them the only path to a needed capability |
| Provider-level and route-level budgets | Per-key budgets ship in week 6; broader scopes need aggregation semantics across keys and a UI story | Per-key budgets are in use and operators ask for the aggregate |
| Console i18n and dark-mode completeness | Cosmetic relative to everything above; the console is operator-facing | A non-English-speaking operator team adopts the console |
| Cargo features to split Bedrock/Vertex out of the default build | `AGENTS.md` discourages speculative structure; cold clippy is 4.5 min and the full gate 11 min, which is tolerable | A contributor reports build time as the reason they stopped |
| Licensing follow-ups (CLA, dual licensing, AGPL positioning) | Strategic, not engineering; the AGPL-3.0-only choice is deliberate | A prospective adopter says the licence is the blocker |
| Rewriting `docs/architecture.md` for end users | The archived plan added `docs/concepts.md` instead; the architecture document stays contributor-facing on purpose | Never — the two audiences stay separate |
