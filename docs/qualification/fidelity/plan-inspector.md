# No-inference interaction inspection

The existing routing simulators inspect actual strict requests with the same
compiled interaction planner, effective-input policy check and routing
eligibility checks used for execution. They do not submit inference, run tools,
open provider/proxy connections or exchange authentication tokens.

`POST /api/v3/route-drafts/{draft_id}/simulate` retains its object response and
`targets[].decision` entries. An optional native `request` enables actual
inspection. Omission keeps tuple-only routing simulation and returns
`interaction.status: not_inspected`, with no class, effective request or evidence.

`POST /api/v3/routing/simulate` retains its existing response array. Supply the
native body in `operation.request`; its `model` selects the route as before.
The optional `operation.route` selects the route outside the native body for
URL-bound dialects such as Gemini. A tuple-only `operation.request.route` remains
supported without manufacturing a prompt or claiming semantic qualification.

Both inputs accept `dialect`, `semantic_headers`, `query_settings` and an
optional current `api_key_id`. Select `openai-responses` explicitly for Responses;
the default OpenAI generation dialect is Chat. Strict inspection requires a
native body. Legacy shared request forms remain available on legacy routes.
Semantic input context is restricted to registered profile names; credentials,
transport overrides, duplicate header aliases and invalid header control bytes
are rejected. Header and query values are always redacted in results.

Each decision's `interaction` describes the ingress dialect, OIF representation,
target and return dialects, profile revision, selected serving revision/model,
field dispositions, provenance, delivery/lifetime/submission/effects,
continuation/buffering/retry obligations and
scoped codec evidence. Serving account, snapshot and resource declarations are
reported by presence only; a declaration is not verified provider behavior.
`admitted` means semantic and effective-input policy preparation succeeded.
The outer `eligible`, `reason` and `attempt` still apply current authority,
capacity, routing constraints and attempt budgets. Preparation failures retain
safe `incompatibility` codes, fields, requirements and explanations. Local input
policy refusal has status `blocked`, including a block caused by defaulted tool
descriptions or schemas.

The effective request is a field summary, not an exported prompt. Known content,
tools, schemas and native state expose presence/kind/emptiness only. Unknown
property names are counted without being returned. Only allowlisted scalar
controls expose an exact `value_json` string; for example a seed of
`9007199254740993` remains that exact string after ordinary JavaScript JSON
parsing. Clients must retain this spelling instead of converting it to a
floating-point number. Scalar spellings longer than 256 bytes stay redacted.
Dispositions are capped at 64 displayed entries, with collapsed/omitted entries
counted explicitly. The management request is bounded at 1 MiB and the existing
route target limit is 64. Display strings detach from native source buffers;
per-target effective bodies are released after capacity/constraint evaluation.

API and network credential revocations are read from current authority even when
the provider/route revision has not changed. Selected API-key authorization and
provider-state permission apply to draft and published inspection. No selected
key grants no provider-state permission: the native Responses default of
`store: true` is refused as a policy conflict without silently inserting
`store: false`. Even an authorized key receives `state_carrier` until #214
qualifies historical plan/resource reconstruction; permission alone does not
establish continuation support. Explicit stateless native requests remain available. Effective defaults
also participate in target token bounds and parameter constraints before ranking.

Planner byte/event ceilings are reported as upper bounds. The installed gateway
transport configuration can impose tighter limits. Legacy/transformed previews
retain an explicit legacy status and never receive a strict qualification label.
Codec fixture evidence does not establish empirical model quality, negotiated
translated continuation or complete specification qualification.

Validation on 2026-09-22 used disposable services and independently scripted
local providers. Public tests cover native Chat and Gemini, qualified text
translation, stable semantic rejections, exact large integers, redaction, current
key/network authority, implicit Responses state, effective default capacity and
defaulted tool policy. The mTLS provider and CONNECT proxy counters assert
zero new upstream/proxy requests during route activation and inspection. Existing
routing preference and credential restriction simulations remain covered.

The focused public suites passed with race detection in 16.340 seconds after
planner checkpoint `4b920a22` and the retained-state guard `89eaf629` were
integrated with this inspector slice. Route/runtime race tests also passed.
Go vet, management-contract generation and console type/lint checks passed.
Reproduce the focused public checks with disposable services configured:

```sh
go test -mod=readonly -tags=integration -race -count=1 -timeout=5m ./tests/integration -run '^(TestStrictPlanInspector.*|TestRoutingSimulationsHonorAvailablePreferences|TestCustomHeaderAuthenticationAndSimulationMatchLiveCredentialRestrictions)$'
```

These checks qualify the inspector slice; they do not complete #213 or G1–G7.
