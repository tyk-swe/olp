# M7 completion handoff

Implementation includes capture revision publication, local detector compilation,
complete-field redaction, native stream assembly, operator controls, forward-only
migration 0074 and the [60-entry pinned inventory](../guardrail-vendor-coverage.md).
M7 remains **In progress** until service, live-vendor and reference-hardware
acceptance is recorded. No PR merge or release qualification is implied.

## Focused checks for this change

Commands are limited to 60 seconds each, with a five-minute total validation
budget. Contract generation completed with `make api` (charged conservatively
as 60 seconds in that budget). The following targeted checks passed:

```sh
go test -mod=readonly -timeout=45s ./internal/export ./internal/protocols -run Capture -count=1
go test -mod=readonly -timeout=45s ./internal/export ./internal/protocols ./internal/gateway ./internal/guardrails -run 'Capture|Guardrail|VendorContracts|DetectorCorpus' -count=1
pnpm --dir console test src/lib/features/observability/ObservabilityPanel.svelte.test.ts
go test -mod=readonly -tags=integration -run '^$' -timeout=45s ./tests/integration
pnpm --dir console exec svelte-check --tsconfig ./tsconfig.json
go vet ./internal/export ./internal/protocols ./internal/gateway
pnpm --dir console exec eslint src/lib/features/observability/ObservabilityPanel.svelte src/lib/features/observability/ObservabilityPanel.svelte.test.ts src/lib/features/observability/api.ts
```

The integration command compiled fixtures and executed **zero service tests**.
Component tests cover unchanged-pin edits, explicit refresh and detector order.
Go fixtures cover eligibility/scope, decoded JSON, independent captured input,
valid replacements, cancellation, bounds, malformed spans, split secrets,
interleaved tool arguments and native terminal snapshots. No screenshots were
taken. Changed-file formatting, whitespace, JSON, documentation links, migration
sequence and exact inventory enumeration also passed. No command timed out;
recorded validation used about 182 of 300 seconds, including the conservative
60-second charge for contract generation.

## Human acceptance still required

Run with the repository's documented PostgreSQL/Valkey test environment and
`OLP_TEST_DATABASE_URL` / `OLP_TEST_VALKEY_URL` configured. The full service harness
provisions its own isolated services when those are unavailable.

| Command / work | Acceptance condition | Owner |
| --- | --- | --- |
| `go test -tags=integration -count=1 -timeout=5m ./tests/integration -run '^TestCapture'` | All capture fixtures execute (no missing-service skips). Pinning survives later activation and unrelated edits; missing/foreign/inactive/retired/unsupported definitions refuse. Migration preserves existing policy settings. Unary/stream sink readback contains only redacted values, caller output stays unchanged, durable request/attempt/audit/export metadata contains neither raw nor redacted payloads. Stale queued ETags never deliver. | Export / gateway maintainers |
| `make integration` | Service, migration, authorization/isolation, process and browser regressions pass, including guardrail evaluator accounting and capture stale-policy behavior. | Release maintainers |
| `make check` | Full formatting, vet, console and local test checks pass; generated contracts are reproducible. | Release maintainers |
| `make test-race GO_TEST_PACKAGES='./internal/export ./internal/protocols ./internal/gateway ./internal/guardrails'` | No races in collector lifecycle, shared compiled detectors, cancellation or streams; investigate unrelated benchmark-harness tests included by this target. | Gateway maintainers |
| `pnpm --dir console exec playwright test --config playwright.guardrails.config.ts` | Existing guardrail journey passes. Manually exercise Settings capture ordered selection, removal, pin visibility, ordinary edit and explicit refresh with keyboard and screen reader; attach screenshots. | Console maintainers |
| `go test -run '^$' -bench 'Benchmark(Builtins100KTokens|GuardrailStreaming|Unconfigured)$' -benchmem ./internal/guardrails` | Record CPU/hardware/Go version, latency and allocations; compare configured built-ins/streaming to unconfigured control. Microbenchmarks alone do not qualify S3. | Performance maintainers |
| `BENCH_SCENARIOS=S3 OLP_BENCH_ENFORCE=1 make bench` | Run full-scale S3 on M1 reference hardware with pinned gateway/mock/loadgen CPUs. Record baseline plus built-in/capture/stream workloads, latency, throughput, allocations and memory. The stock S3 harness does not automatically add M7 guardrails: configure and record those workloads before claiming M7 performance acceptance. | Performance maintainers |

Live qualification is not automated by an existing live-guardrail script. For
**each** of Bedrock, Azure Content Safety, Azure Prompt Shields, Model Armor,
Lakera v2 and Presidio, publish an operator-configured definition with its real
endpoint/version/credential and `content_egress: true`, then an immutable policy.
Evaluate positive, negative and malformed/timeout fixtures through the apply API:

```sh
curl --fail-with-body "$OLP_BASE_URL/v1/guardrails/apply" \
  -H "Authorization: Bearer $OLP_GUARDRAILS_KEY" \
  -H 'Content-Type: application/json' \
  --data-binary @qualification-request.json
```

`qualification-request.json` must name the real `policy_revision_id`, phase and
synthetic bounded segments. Keep credential values out of artifacts. Connector
maintainers must record vendor/API versions, authentication method, admitted
phases/actions, positive/negative results, replacement span validity where
supported, both failure policies, actual deadline cancellation, and content-free
evidence. Repeat behind an operator-controlled delay/error fixture for deadline
and circuit-breaker qualification. No real customer content is needed.

Guardrail and release maintainers must review every inventory row, record any
vendor-specific support/exclusion decision with rationale and owner, and retain
the aggregate vendor `Partial` row while gaps remain. No unchecked M7 acceptance
item becomes checked merely because this PR contains its implementation.
