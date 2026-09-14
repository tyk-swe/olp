# M3 evidence: complete OpenAI request path

Implemented and qualified on Linux amd64 on 2026-09-13. The Go build now
takes an operator from an empty installation through provider onboarding,
certification, route publication, key issuance, and unary and streaming
OpenAI SDK traffic. Operations are documented in the
[Go gateway guide](../../go-gateway.md); installation and identity remain in
the [Go access guide](../../go-access.md).

## M3-01

[Ingress limits](../../../internal/gateway/server.go) admit inference work
through a bounded semaphore (`503 request_admission_overloaded`), require
`application/json`, accept gzip only within the configured JSON body cap
before and after inflation (`413`/`415`), assign or validate `X-Request-Id`,
derive the client address only from `OLP_TRUSTED_PROXY_CIDRS`, and answer
inference CORS preflights. [Provider responses](../../../internal/gateway/executor.go)
are capped by `OLP_PROVIDER_MAX_RESPONSE_BYTES` and streamed events by
`OLP_PROVIDER_MAX_EVENT_BYTES`; deadlines are explicit per attempt (first
byte, then idle time between frames) instead of one unary timeout. Timeouts
and cancellation close upstream bodies.

[Egress policy](../../../internal/egress/egress.go) restores HTTPS defaults,
endpoint validation, DNS resolution with a non-public denylist, pinned
dialing, redirect refusal, bounded transports, and explicit operator
exceptions. [Tests](../../../internal/egress/egress_test.go) replay the
retained custom-endpoint corpus, the operator exceptions, unsafe DNS
answers, and redirect refusal against a pinned dial. Configuration bounds
live in [config](../../../internal/config/config.go).

## M3-02

The [OpenAI codecs](../../../internal/protocols/openai/) parse and validate
Chat Completions and Responses envelopes, preserve unknown source extensions
through re-encoding, rewrite the model, apply connection parameter defaults,
reject conflicting token limits and stateful Responses references, decode
unary responses and fragmented SSE streams with typed protocol errors, and
extract usage, tool calls, refusals, and finish reasons. The shared
[SSE decoder](../../../internal/protocols/sse/decoder.go) enforces the event
byte limit. [Codec tests](../../../internal/protocols/openai/openai_test.go)
run the retained request/response fixtures, the operation-family corpus, the
one-byte fragmented stream fixture, malformed and truncated events, missing
usage, oversized events, and in-stream errors.

## M3-03

[Provider lifecycle](../../../internal/providers/lifecycle.go),
[models](../../../internal/providers/models.go), and the
[connector](../../../internal/providers/connector.go) implement OpenAI and
OpenAI-compatible connection CRUD, bounded probes and model discovery,
declared capability review, per-tuple server certification, activation into
immutable revisions, disable, revision listing, diff, and restore-as-draft.
Draft edits only mark a pending activation; transport changes invalidate
certification evidence; activation requires a usable credential and enabled
models whose every capability is certified. Probe diagnostics store status,
time, and an upstream error code only. Other kinds and operations return
typed `422` problems.

## M3-04

[Credential pools](../../../internal/providers/credentials.go) provide the
default slot and up to 64 slots with priority, weight, enablement, model,
route, and key allowlists, and stored quota configuration for M4. Rotation
validates the new secret upstream, records a new version, and selects it for
the draft only; the active revision keeps its pinned version. Slot validation
is per slot, and version revocation advances the authority generation so
gateways stop selecting the version without a new activation. Secrets are
stored through the record-bound key ring and never returned.

## M3-05

[Route drafts](../../../internal/routes/server.go),
[publication](../../../internal/routes/publish.go), and
[simulation](../../../internal/routes/simulate.go) implement slugs, allowed
operations, target resolution against published models, priority tiers,
deterministic weighted selection, deadlines, attempt budgets, ETags,
activation into immutable revisions, diff, and restore. Validation and
activation reject unknown, inactive, unpublished, or uncertified targets.
[Selection](../../../internal/runtime/selection.go) reproduces the retained
attempt-order corpus in [tests](../../../internal/runtime/selection_test.go).
Model listing and retrieval expose only routes permitted by the key's scopes
and allowlist ([gateway tests](../../../internal/gateway/gateway_test.go)).

## M3-06

[Publication](../../../internal/runtime/publish.go) compiles every active
provider revision and latest route revision into one validated snapshot with
a SHA-256 digest stored as a numbered runtime release. The
[manager](../../../internal/runtime/manager.go) installs newer releases only
when validation, digest, and credential decryption succeed, keeps the previous
release otherwise, refreshes key and credential-revocation authority every
five seconds independently of releases, and marks authority stale 60 seconds
after the start of the last successful read. Requests pin the release they
were admitted with; stale authority rejects new admissions with
`503 authority_unavailable`, and revoked credential versions are never
selected from retained releases.

## M3-07

The [executor](../../../internal/gateway/executor.go) composes key
authentication, positive scope and route authorization, snapshot pinning,
route selection, the attempt budget, per-attempt deadlines, failure
classification, circuit health, credential and rate-limit cooldowns, and
streaming commitment. Retryable classes fail over before commitment within
the original budget and deadline; a committed stream reports later failures
in-band and never restarts elsewhere. Retry-After propagates when every
attempt is rate limited; credential and endpoint failures stay distinct.
Cancellation closes upstream work and releases admission once.
[Gateway tests](../../../internal/gateway/gateway_test.go) cover the
retry-taxonomy fixture, fragmented streams, pre-commit failover, post-commit
failure, oversized responses, cooldowns, circuit opening, admission limits,
client cancellation, and the authentication matrix.

## M3-08

[Events](../../../internal/gateway/events.go) define the terminal envelope:
request ID, actor, route and revision, release sequence, family, mode,
outcome, status, commitment, timing, explicit usage presence, and ordered
attempts with target, provider revision, slot, credential version, class, and
timing. `finish` emits exactly one envelope per request through a
`sync.Once`; the log sink records metadata only, and the provider-health
endpoint aggregates the same facts. Durable delivery and pricing remain M4.

## M3-09

The reused provider, route, key, and playground console features run against
the Go contracts. The [gateway journey](../../../console/tests/gateway/openai-route.spec.ts)
and its [mock upstream](../../../console/tests/gateway/mock-openai.mjs) drive a
browser user from an empty installation through the provider wizard
(probe, discovery, capability review, server certification, draft test,
activation), route drafting with stale-draft protection, blocked validation
while the provider is disabled, simulation, activation, key creation with
route selection, the built-in connection test, an in-flight stream that
survives a provider disable, the playground, and key revocation, at both the
packaged and Vite origins. The [access journey](../../../console/tests/access/control.spec.ts)
now expects the gateway-enabled overview and route picker. Because the
gateway is available before M4 lands, the overview, request explorer, and
pricing settings read the `retention_enforced` and `limits_enforced`
capability flags and show an explicit "not retained/enforced yet" state
instead of calling the `501` placeholders. The
[SDK fixture](../../../tests/sdkfixture/main.go) serves real inference so the
official OpenAI SDK checks in `tests/sdk-smoke` run unary, streaming, model
listing, and native error contracts against Go.

Screenshots: [provider onboarding](gateway-screenshots/go-provider-onboarding.png),
[blocked route activation](gateway-screenshots/go-route-activation-blocked.png),
[active route](gateway-screenshots/go-route-active.png),
[key connection test](gateway-screenshots/go-key-connection-test.png), and
[playground](gateway-screenshots/go-playground.png).

Two visible console gaps remain by design until M5: the route editor still
lists operations and routing preferences that Go rejects with typed `422`
problems, and routing-policy saves return `501`.

## Validation

- `make go-check`: formatting, vet, all Go unit suites with the race detector,
  and console checks pass.
- `go test -race -tags=integration,oidctest ./tests/integration`: passes
  against PostgreSQL 18 and Valkey, including
  [the end-to-end scenario](../../../tests/integration/gateway_test.go) that
  walks from setup to SDK traffic, exercises failed certification, blocked
  activation, in-flight publication changes, key and credential revocation,
  rotation pinning, revision diffs, and routing simulation.
- `OLP_SDK_SMOKE_BACKEND=go OLP_SDK_SMOKE_SURFACES=openai tests/sdk-smoke/run.sh`:
  official OpenAI SDK success and error contracts pass against the Go fixture.
- `pnpm --dir console exec playwright test --config playwright.go.config.ts`:
  access, foundation, and gateway journeys pass at both origins.
- Process qualification in [process tests](../../../tests/integration/process_test.go)
  checks that `gateway` and `all` serve `/v1` with native authentication
  errors, `control` does not, and readiness reports the authority dependency.
