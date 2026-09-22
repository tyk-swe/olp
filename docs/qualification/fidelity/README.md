# Fidelity qualification baseline

This versioned baseline supports [#210](https://github.com/tyk-swe/olp/issues/210)
and the [implementation specification](../../../OLP-implementation-spec.md).
It was authored against source `8580b39905dc4da9278de8e53ceac7d2412ad6a5` before
changing the production hot path. Its five legacy counterexamples reproduce
observable representation losses; they are **not passing strict fidelity
results or measured model-quality regressions**.

## Independent reference and denominator

[Version 1](../../../tests/fixtures/fidelity/v1/manifest.json) records synthetic
fixture provenance, dialect/API/client contracts and SHA-256 hashes. The test
pins the manifest itself. These references are authored independently of the
production codecs. Add a new version for a changed contract; do not rewrite
version 1 to make implementation output pass. The historical
[reference inventory](../../../tests/fixtures/reference-inventory.json) and its
18 hashed fixtures remain unchanged.

The new [representative inventory](../../../tests/fixtures/fidelity/v1/inventory.json)
contains 47 operation/profile/mode/client rows, including native generation,
real SDK continuations, cloud profiles, embeddings, rerank, moderation, native
counts, media, durable work and realtime. This is a qualification denominator,
not a support claim: rows intentionally remain unqualified until scoped tests
and evidence establish their contract. An unsupported representation stays in
the denominator as incompatible or unknown. The historical inventory separately
protects the full existing surface from accidental removal.

The [counterexamples](../../../tests/fixtures/fidelity/v1/counterexamples.json)
contain seven independently authored native invocations: the five recorded
losses, one unknown-extension/presence control and one simple translated
control. Only replacing the route model with the configured native model is
allowed in the native comparisons. The five baseline losses are:

| Case | Observed legacy behavior | Strict requirement |
| --- | --- | --- |
| Ordered text/tool/text | Text after a call moves before it | Preserve order and relationships or reject before dispatch |
| Instruction scopes | System/developer scopes collapse | Qualified scope mapping or instruction-scope incompatibility |
| Structured output | Name, description and explicit strictness disappear | Preserve the full contract or reject |
| Presence | Explicit null becomes absent | Dialect-qualified equivalence or presence incompatibility |
| Token budget | Distinct control scopes share an unqualified scalar | Qualified budget equivalence or reasoning-budget incompatibility |

`TestLegacyCounterexamplesAndPositiveNativeControls` characterizes the existing
legacy codecs, with positive native controls for every case. Its five logged
losses deliberately remain visible. Later strict admission tests must use
these inputs to assert preservation or a precise incompatibility with zero
provider dispatch. They must retain positive controls, so rejecting every
request cannot pass qualification.

The independent [oracle](../../../tests/fidelity/reference.go) uses no
production codec. It compares exact JSON values while ignoring object-key
order and insignificant whitespace. Array order, number spelling/precision,
absence, null, false, zero, empty values, string identity and opaque state
remain distinct. Duplicate object keys, invalid UTF-8 and unpaired Unicode
surrogates are rejected rather than silently repaired. Errors name only the
JSON pointer, never the mismatched sensitive value. SSE comparisons retain
event order, data, event names, IDs and terminal presence; they ignore only
comments and transport chunk boundaries for these fixture contracts. These
comparisons are stricter than a claim of mathematical numeric equivalence;
any broader equivalence must be separately qualified.

Deliberate mutations cover deletion, duplication, reordering, instruction
relocation, invented defaults, changed budgets, schema metadata, call IDs,
precision, Unicode repair, late native state, terminal deletion and unknown
event drift. Fuzz seeds cover opaque-string changes and ordered sequences.
The independently authored Anthropic stream and next-native-request fixtures
include thinking/signature material and parallel calls. Their existence does
not establish SDK continuation or crash recovery; those require the later
real-client and process tests.

## Common external acceptance seam

Provision provider connections, credentials, model capabilities, routes and
API keys through the public management API. Send inference through the real
published gateway to a scripted local provider. Compare captured native
requests with independently authored expectations, then inspect client-visible
results/events and the client's next provider-bound request. Assert provider
accepted-work/resource counters, accounting and resource authorization where
relevant. Count only inference dispatch after setup probes/certification, and
retain rejected/incomplete/ambiguous/unknown cases in reported denominators.

`TestFidelityPublicNativeAndTranslatedConservationControls` implements this
seam with the existing `provisionRoute`/`newAccessHarness` helpers. It checks
native unknown fields, exact large integers, null/zero/false/empty values, a
native response extension, a successful simple translation, and a precise
translated-extension rejection with zero additional provider dispatch. It
uses no production mapper as its expectation oracle. The fixture is local and
uses no paid account. It does not substitute for real SDK serialization,
process loss, provider side effects or live model-quality evidence.

For continuation, reuse the official SDK fixture launchers; compare the next
request after the SDK assembles the streamed result and serializes tool
results. For durability, reuse `m4Install` and `testutil.Process.Kill` with
controlled acceptance/state-publication boundaries. Local accounting
idempotency is not evidence of provider-side exactly-once execution.

## Running and interpreting evidence

```sh
go test -mod=readonly -count=1 -v ./tests/fidelity
go test -mod=readonly -race -count=1 ./tests/fidelity
go test -mod=readonly -run '^$' -fuzz '^FuzzReferenceRetainsOpaqueState$' -fuzztime=5s ./tests/fidelity
# Requires a disposable PostgreSQL with CREATE DATABASE authority:
go test -mod=readonly -race -tags=integration -count=1 \
  -run '^TestFidelityPublicNativeAndTranslatedConservationControls$' ./tests/integration
```

An explicitly selected integration test fails if `OLP_TEST_DATABASE_URL` is
missing; it does not silently skip. `make integration` supplies isolated
services for the full suite. The fixture and oracle suites are included by
ordinary `make test-go`.

The initial validation record is [baseline-validation.md](baseline-validation.md).
The additive [registered unary operation evidence](unary-operations.md) records
native and qualified non-generation behavior, public profile coverage and the
explicit raw-vector client contract.
Performance baselines and predeclared replacement budgets are recorded
separately by the benchmark harness. Do not turn a passing oracle test,
fixture count, named SDK test, or benchmark harness into a claim that the full
specification gates have passed.

[The quality study](quality-plan.md) is preregistered and **unknown**. No live
trial or intelligence-parity claim is authorized by these deterministic tests.
