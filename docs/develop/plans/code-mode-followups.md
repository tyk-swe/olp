# Coding-client protocol, account and catalog follow-ups

[#554](https://github.com/tyk-swe/olp/issues/554) specifies the remaining work under
[#522](https://github.com/tyk-swe/olp/issues/522). The
[pinned comparison](code-mode-parity.md) remains the historical baseline at OLP
`fe594971b783c1a1ac9be7064fd9026ce7b396d1` and CLIProxyAPI v8.0.47,
`90c447d3a8727d115aa7552ec93ce023736ed492`. This assessment was checked on
2026-10-30 against OLP `91eef760d97212ea2613f2a7cf35f96456fb14f4`.
The comparison's original gaps must not be read as a current implementation list.
GitHub issues own the detailed specifications. This index records their scope and
the evidence boundary rather than copying those specifications into the repo.

## Follow-up index

The six issue bodies are prepared for publication under #554. Each includes a
coding-client need, tools/reasoning/inputs/transports, account-owned state,
content-retention limits, dependencies, observable acceptance and exclusions.
Publication links will replace the #554 references once the issues exist.

| Follow-up | Current boundary and next decision | Tracking |
| --- | --- | --- |
| Messages token counting | `count_tokens` is an unsupported probe. Verify current Claude Code demand and a native subscription count contract before adding HTTP unary counting. Translated estimates must not claim provider-exact counts or a total generation bound | [#554](https://github.com/tyk-swe/olp/issues/554) |
| Legacy Completions | Ordinary Completions exists. No selected coding-client need is established for a code-mode endpoint. First compare a complete prompt/suffix workflow with existing ordinary or modern-protocol access | [#554](https://github.com/tyk-swe/olp/issues/554) |
| Public Gemini | Ordinary Gemini routing and restricted internal Antigravity translation are separate from public code-mode `generateContent`. Select one client/account workflow and preserve required native parts and signatures before claiming support | [#554](https://github.com/tyk-swe/olp/issues/554) |
| Advanced Responses | Native foreground Responses, Codex compact, WebSocket/prewarm and owned response continuation already exist. Inventory additional operations against current client demand, including search, background work, stored references and opaque reasoning | [#554](https://github.com/tyk-swe/olp/issues/554) |
| Remaining account families | Assess Kimi OAuth, xAI, Devin and Meta individually. Verify authorized login, principal/refresh, coding tools, transport, continuation and retention before selecting at most one adapter implementation | [#554](https://github.com/tyk-swe/olp/issues/554) |
| Model-catalog refresh | Authorized discovery and generated setup already share eligible published models. Investigate one non-inference metadata source without treating its roster as entitlement, changing exact aliases or publishing new serving models | [#554](https://github.com/tyk-swe/olp/issues/554) |

## Implementation and evidence

[Adapter declarations](../../../internal/codeadapter/codeadapter.go) identify the
implemented operation/profile boundaries. [Ingress](../../../internal/gateway/code_ingress.go)
classifies only the supported code protocols and handles `count_tokens` as a
probe. [Ordinary routing](../../../internal/gateway/native.go) separately exposes
legacy Completions, token-count and Gemini operations. Ordinary support does not
establish subscription acceptance.

[Responses classification](../../../internal/codexwire/operation.go) implements
compact/prewarm while refusing background generation, server-side conversations
and stored input-item references. [Generic state validation](../../../internal/codewire/state.go)
adds its own opaque/signed state, compact and server-tool restrictions.
[Public forwarding fixtures](../../../internal/gateway/code_forward_test.go)
and [classification regressions](../../../internal/codexwire/observe_test.go)
provide controlled seams. Source declarations and authored fixtures alone do not
show a current client or real account accepting an operation.

[Discovery](../../../internal/gateway/code_models.go) uses
[current eligible offers](../../../internal/resources/code_models.go) and the
[same catalog](../../../internal/codeclient/catalog.go) as generated setup.
Account model lists remain explicit. The existing
[signed reference catalog](../../../internal/catalog/catalog.go) and
[route model projection](../../../internal/modelcatalog/model.go) should be
assessed before adding refresh storage. Metadata refresh cannot grant access or
change conversation ownership, and no maintenance inference is authorized.

### Gemini and account evidence

[Antigravity translation](../../../internal/gateway/code_antigravity.go) wraps an
internal project/model/request envelope behind explicit `chat_to_antigravity`.
Its controlled OpenCode Chat path supports single-candidate text/function tools
and refuses unrepresentable input and signed/thought/unknown output parts.
The `gemini-` model restriction is neither a current model inventory nor proof of
entitlement. Public Gemini, Responses, Messages, WebSocket, reasoning controls,
media and server-tool support do not follow from that implementation.

The official [Thinking guide](https://ai.google.dev/gemini-api/docs/thinking),
checked on 2026-10-30 for #542, states that generateContent signatures may attach
to any part, including function calls and final response parts, and preserve
reasoning continuity. Refusing those parts avoids silent loss, but no current
model or live Antigravity tool workflow is qualified. Public Gemini must preserve
its own native contract or refuse required unsupported state.

The pinned competitor [route registrations](https://github.com/router-for-me/CLIProxyAPI/blob/90c447d3a8727d115aa7552ec93ce023736ed492/internal/api/server_routes.go#L65-L138),
[authenticator manager](https://github.com/router-for-me/CLIProxyAPI/blob/90c447d3a8727d115aa7552ec93ce023736ed492/internal/cmd/auth_manager.go)
and [executor registrations](https://github.com/router-for-me/CLIProxyAPI/blob/90c447d3a8727d115aa7552ec93ce023736ed492/sdk/cliproxy/service_executors.go#L309-L329)
are source evidence only. Kimi OAuth is distinct from the existing Kimi Code
pasted-key plan. Gemini CLI login is not registered in that authenticator manager.
API keys, Vertex credentials, AI Studio relay and Antigravity OAuth must not be
collapsed into one enrollment claim. The pinned [SIWC credential partition](https://github.com/router-for-me/CLIProxyAPI/blob/90c447d3a8727d115aa7552ec93ce023736ed492/docs/management-api-v8.md#L145-L189)
is not a new subscription family and needs its own selected-client requirement
before expanding import scope. Dynamic adapters remain deferred until a selected
workflow needs them.

The separate [#556](https://github.com/tyk-swe/olp/issues/556) implementation in
progress adds explicit `chat_to_codex` translation through shared codecs. It
remains unreviewed as of this assessment and establishes no live support, public
Gemini ingress or broad advanced Responses coverage.

## Qualification and shared contracts

No live calls, client execution, new fixtures or feature implementation were
performed for #554. Existing [qualification records](../qualification.md#code-mode-qualification)
remain controlled evidence with explicit live gaps. Future qualification records
the integrated OLP revision, current official client package/version and integrity,
platform, account/profile, model, operation, protocol, transport, setup and
sanitized output. A catalog entry, successful enrollment or passing fixture does
not establish entitlement or a usable live coding workflow. Missing evidence
stays unqualified.

[ADR 0001](../../adr/0001-explicit-code-mode-compatibility.md) keeps native forwarding
the default and separates translation, exact-model policy, identity/prompt changes
and paid participation. [ADR 0002](../../adr/0002-state-aware-code-account-recovery.md)
keeps account-owned state and uncertain execution from moving or replaying.
The [glossary](../../../GLOSSARY.md) supplies the shared terms. None of these
follow-ups authorizes persisted coding transcripts or new content retention.
The [native hard-token bound](../qualification.md#native-hard-token-bound-research)
remains a separate qualification gap. [#506](https://github.com/tyk-swe/olp/issues/506)
continues to own current-client drift independently of these follow-ups and live gates.
