# OIF source and operation contracts

The #211 slice introduces immutable sources and operation-owned views behind
the existing adapters. It does not activate strict semantics on legacy routes
or claim that legacy translations preserve complete interactions. The frozen
five counterexamples still characterize their legacy behavior.

JSON source documents retain exact native subtrees and number lexemes. Explicit
null, absence, zero, false, empty strings and empty arrays remain distinct.
Duplicate decoded object names, invalid UTF-8 and unmatched surrogate escapes
fail rather than receiving JSON parser repairs. Defaults are bounded at 64 MiB,
128 nested levels and 1,048,576 values; existing request/response/event transport
limits normally enforce smaller limits. Array JSON Pointer indexes must be
canonical unsigned decimal positions. Overlays reject overlapping paths and
array insertion/removal, retain provenance and never modify the original source.

Generation views retain native scope, ordered text/tool/native blocks, tool
result dependencies, schemas and candidate boundaries. They are independent of
the old flattened legacy mapper. The neutral registry links operations and
dialect revisions at build time; it does not load code from requests. Blob
references identify exact bytes owned by existing media/resource authorities,
and confer no fetch or authorization permission.

Requests, results and events are separate envelopes. The OpenAI-named request
and completion types remain outer compatibility adapters while callers migrate.
Unary results enter OIF before model rewriting; stream events enter it before
projection. Native streaming retains one bounded source event plus existing
bounded dialect grammar state, instead of collecting a stream-sized document.
The optional synchronous event observer is for operation projection and
continuation dependencies under the existing backpressure contract.
The observer receives structurally valid source before the dialect's state
grammar validates it; observing a terminal-looking frame alone is not a
tool-ready or durable-state signal.

Validation receipts are tied to the exact immutable document and dialect that
the request parser validated. Constructors, changed overlays and effective
defaults still undergo dialect validation. Legacy adapters exchange an owned
destination field copy, avoiding redundant serialize/parse cycles without
changing controls, assets or source validation.

Identity preparation validates dialect-owned pointer/origin/value-kind rules
and rejects prior semantic transforms, even if a caller labels them identity
changes. Those rules are representation permissions, not empirical quality or
whole-interaction proof. The planner must additionally validate profiles,
policy inspection, control/default semantics, clients and continuations.
Legacy preparation records a legacy mapping and actual inherited-default
branches. A native identity request keeps its original Responses input shape;
an explicit Responses destination cannot silently select Chat instead.

Independent tests cover exact bytes and defensive copies; duplicate/Unicode
ambiguity; all three parser bounds; spoofed identity changes; ordered reasoning,
parallel calls and result dependencies from the frozen native fixture; scope,
presence and tool schema details; ordered result candidates and refusals; and
native stream sources before rewriting. Public gateway tests assert precise
zero-dispatch input rejection and provider/client conservation. The common
management-provisioned integration baseline remains unchanged.

Validation executed on 2026-09-22 for runtime revision
`291d4b23a3fa14cdf60f684f8c5a0e3f9a584ff1`:

- `make check`: passed contract generation, formatting, Go vet/local tests,
  console checks and 617 tests in 68 files, and 14 script tests.
- Source, protocol, SSE, gateway and independent fidelity tests with `-race`:
  passed, including invalid constructor/default/overlay/dialect controls for
  validation reuse. Source fuzzing executed 43,368 cases without a failure.
- Official JavaScript and Python OpenAI, Anthropic and Gemini SDK smoke suites:
  passed native success/stream/error contracts. These are the existing smoke
  workflows, not reasoning-tool continuation qualification.
- Disposable-service `TestFidelityPublicNativeAndTranslatedConservationControls`
  and `TestResponseLifecycle` with `-race`: passed through management provisioned
  public APIs, including ownership, historical mappings and credential revocation.
- The unchanged frozen benchmark ran twice from clean committed revisions.
  The [initial capture](../../evidence/fidelity-performance/oif-source-v1/initial.json)
  failed the native-asset sample floor. Removing redundant validation and
  serialize/parse work produced the
  [optimized capture](../../evidence/fidelity-performance/oif-source-v1/optimized.json),
  which passed every gateway workload and the sample floor. Its overall comparison
  **did not pass**: the unchanged slow-relay control's p99 inter-event-gap metric
  was 3,943 µs against a frozen 3,544 µs limit. Both results remain in the
  [comparison record](../../evidence/fidelity-performance/oif-source-v1/comparisons.json).
  No harness, workload, baseline or budget was changed. Full performance
  qualification remains open for the integrated runtime under
  [#218](https://github.com/tyk-swe/olp/issues/218).

These checks establish deterministic source conservation only. Reasoning-tool
continuation, durable continuation recovery, empirical quality and complete
interaction qualification remain their separate specification gates.
