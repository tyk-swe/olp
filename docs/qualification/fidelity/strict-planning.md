# Strict interaction admission

Select an explicit provider profile and revision, then publish a route with
`"fidelity":{"mode":"strict"}` (or an explicit empty fidelity object). Old
omitted contracts remain legacy and retain their historical snapshot digests.
Publication, draft validation and runtime installation compile the configured
profile, defaults, policy and model binding. These templates are bounded by the
published route targets, with no prompt- or schema-keyed global cache.

The planner prepares each eligible target's outbound document once. Dispatch,
policy inspection and reservation estimation use that prepared invocation.
Caller values, including valid explicit null, zero, false and empty values, take
precedence over absent-only defaults. Tool/schema arrays remain atomic. Conflicting
token scopes are rejected rather than aliased. Strict routes reject redaction at
activation; explicitly transformed routes apply their policy to the effective
request after defaults, including configured tool descriptions.

## Admitted scope

| Interaction | Contract and boundary |
| --- | --- |
| Native Chat, Responses, Anthropic Messages, Gemini GenerateContent and Bedrock Converse | Original native structure, values, results and events, with declared model/resource/transport identity changes. Matching direct/Azure/Vertex/Bedrock hosting profiles keep their endpoint/authentication contracts. |
| Stateless Chat → Anthropic text unary | Alternating user/assistant text ending in a user turn; explicit or configured target output cap; guarded single text result, usage and terminal reason. Scope, tool/state, reasoning, unsupported controls and lossy result forms are rejected. |
| Native complete-history reasoning/function-tool interactions | Native data is retained. This path uses the native client history contract, without mandatory OLP persistence or helper. Whole translated SDK/recovery qualification remains #214. |
| Provider-retained state, background work or native resource references | Unauthorized retention fails `policy_conflict`. Authorized retention currently fails `state_carrier`; unresolved references fail `resource_affinity`. #214 must add historical prepared plans, stored contract identity, authority and next-turn reconstruction before removing this guard. |
| Provider-hosted tools and unqualified effects | Fail `state_carrier` before dispatch until their lifecycle and authority contract is implemented. Ordinary native function tools remain supported. |
| Current console playground projection | Fails `state_carrier` on strict routes before dispatch. #217 must preserve native or explicitly negotiated observation/continuation before enabling it. Legacy/transformed playground behavior remains available. |
| Other operation runners | Strict activation fails `target_capability` until #215/#216 supply their independent contracts. Existing legacy operation coverage is retained. |

These classes compare against the selected model's native invocation. They do not
claim that different providers have equal intelligence. Unknown native extensions
remain intact where authorization and policy permit them. A restrictive content
policy rejects unknown/opaque areas it cannot inspect. Incoming semantic headers
and query values either match the admitted profile or fail before dispatch;
gateway authentication selectors never enter upstream semantic settings.

## Failures and Attempts

Safe incompatibility categories include `instruction_scope`, `reasoning_budget`,
`state_carrier`, `target_capability`, `policy_conflict`, and `resource_affinity`.
OpenAI returns code/param, Anthropic retains its error envelope with code/param,
Gemini includes `google.rpc.ErrorInfo`, and Bedrock returns its exception header.
The existing request ID correlates the result. An unknown translated control keeps
the historical public `unsupported_parameter` code; inspection also names its
precise unsatisfied requirement.

Semantic eligibility precedes target ranking. Effective defaults are checked
against model capacity and routing constraints without shrinking the requested
controls. Current API and network credential revocation is rechecked before quota
reservation and Attempt consumption. Local quota refusals remain budgeted Attempts.

The first admitted dispatch establishes serving identity. Later Attempts require
that same provider/profile/revision/model/principal/region/resource binding; an
unknown principal also pins the credential slot. The configured binding key stays
pinned even when two aliases resolve to one model: their defaults may differ.
A matching model alias is not proof of equivalence. A transport failure after writing may leave provider work
accepted, so strict execution never blindly retries it.

Strict post-dispatch server errors include `X-Should-Retry: false` for clients
that honor the hint. It is not universal retry control or exactly-once delivery:
some SDKs ignore it, a lost connection cannot carry it, and callers can submit new
requests. Strong recovery clients must explicitly control connection retries and
use the qualified continuation contract. Unqualified caller idempotency headers
are rejected rather than silently dropped or treated as provider deduplication.

The existing Attempt metadata carries `routing.interaction`: fidelity, plan class,
upstream state (`not-sent`, `outcome-unknown`, `accepted`, `terminal`) and client
state (`unobserved`, `partially-observed`, `actionable`, `terminal`). HTTP commitment
and usage evidence remain separate. Stream failures retain native in-band error
or incomplete termination; they do not add a success marker or replay inference.
No prompts, opaque state, header values or credential material enter these records.
Client states describe conservative exposure at the delivery boundary, not an
application acknowledgement. A failed partial write may already be actionable.
Validated native usage remains attributed when a later projection or policy guard
refuses delivery; terminal provider work is distinct from client observation.

## Qualification

The management-provisioned strict tests capture independent provider requests and
exercise native cloud profiles, scalar/array Responses, semantic headers, query-key
Gemini SSE, native AWS SDK Converse, qualified text and precise zero-dispatch
refusals. They verify default presence, effective tool-policy handling, current
authority, no replay after accepted work, and separate acceptance/observation
evidence. The no-inference inspector uses the same compiler/binder and publishes
safe receipts and evidence scope; declared/discovered connectivity remains
distinct from tested interaction support.

The unchanged v1 benchmark's full 22-combination semantic smoke passes with
explicit strict route/profile overlays. This one-iteration smoke is **not** G6
timing qualification. The original distributions, harness, runner and budgets
remain frozen. Whole G2 reasoning/tool/recovery qualification, empirical quality,
and final G6 remain open in their dependent tickets.

Validation executed on 2026-09-22 with Go 1.27.1, Node 26.8.2 and pnpm 11.24.0:

- `make check` passed generated-contract, formatting, Go vet and local Go checks;
  Svelte/TypeScript reported zero errors/warnings; 68 console files with 617 tests
  and all 19 script tests passed.
- `go test -mod=readonly -race -count=1 -timeout=180s ./internal/interaction
  ./internal/runtime ./internal/routes ./internal/gateway ./internal/protocols
  ./internal/usage ./internal/connectors` passed. Gateway race tests took 55.164 s.
- The management-provisioned integration selection below passed with race
  detection in 83.470 s. A subsequent focused retention check also passed, covering
  both omitted and explicit `store:true` with denied/allowed state policy.
- `tests/sdk-smoke/run.sh` and `tests/sdk-smoke-python/run.sh` passed the existing
  official OpenAI/Anthropic/Gemini contracts. Each Anthropic native continuation
  fixture retained 19 events and completed two tool results in two verified
  dispatches. These existing SDK checks do not qualify translated continuation.
- The two pre-merge review regressions first reproduced unsafe alias-default
  Failover and incomplete output-policy coverage. The corrected alias test and
  public native output-policy/usage tests passed with race detection.

With the disposable service environment from the testing guide configured:

```sh
go test -mod=readonly -tags=integration -race -count=1 -timeout=300s ./tests/integration \
  -run 'TestStrict|TestPublicEffectiveDefaults|TestRouteFidelity|TestFidelityPolicy|TestPublishedProviderProfilesPreserveCloudInvocation|TestProviderProfileTLSDefaultsAndNetworkCredentialLifecycle|TestResponseLifecycle|TestBedrockIngress|TestPopulatedInstallationAppliesForwardMigration'
```

Reproduce the unchanged benchmark's semantic smoke (no budget comparison):

```sh
export OLP_FIDELITY_BENCH_ROUTE_CONTRACT='{"native":{"fidelity":{"mode":"strict"}},"translated":{"fidelity":{"mode":"strict"}},"rejected":{"fidelity":{"mode":"strict"}}}'
export OLP_FIDELITY_BENCH_PROVIDER_CONTRACT='{"native":{"profile_id":"compatible-chat","profile_revision":"1"},"translated":{"profile_id":"anthropic-messages","profile_revision":"1"},"rejected":{"profile_id":"anthropic-messages","profile_revision":"1"}}'
GOMAXPROCS=4 go test -mod=readonly -run '^$' -bench '^BenchmarkFidelity$' -benchtime=1x -count=1 ./internal/gateway
```

The final smoke passed all 22 combinations in 1.624 s under these explicit
contracts. That duration is test execution time, not a gateway latency claim.
No frozen reference inventory, v1 payload/oracle/runner, or original performance
budget was changed. Fixture existence and this smoke are not full G1–G7 evidence.
