# Qualification

Use this guide to qualify a change or release and to track evidence still owed.
A fixture, compile-only run, reduced-rate benchmark or historical green suite
covers only its stated scope and revision. **Implemented** means code and focused
checks exist; **Shipped** additionally requires the applicable candidate gates.
Record commit/image digest, tool/provider versions, environment, commands,
artifacts, skips and unsupported combinations. Missing credentials or hardware
leave a gate open. This documentation consolidation does not rerun the historical
campaigns below.

## Change acceptance

- Extend `openapi/management.json` with declared security requirements; regenerate
  (`make api`) and update `internal/access/testdata/authorization.golden.json`.
  Authorization and isolation sweeps retain complete operation coverage.
- Promote nonsecret intent through configuration export/plan/apply, with bindings
  validated before activation. Publish immutable runtime policy; failed installs
  retain the last valid generation. Preserve original retained-resource owners,
  target/credential identity and live revocation.
- Ordinary admission reads the pinned runtime, key authority and Valkey. Retained
  mappings are explicit PostgreSQL exceptions. Disabled features add no work
  beyond the [performance budget](performance.md#performance-budget).
- Review persisted data, egress for every outbound step, seal/digest purposes and
  redacted upstream errors. New export, external evaluation, caching, tool
  execution and delegation need opt-in plus finite byte/time/concurrency/retention
  bounds. No unbounded queue or implicit fallback.
- Retry only proven replay-safe operations within the parent budget. Ambiguous
  creates/effects stay uncertain and reconcile. Record each billable/quota event
  once; missing usage/prices never become zero, and unbounded budgeted work
  refuses. Evaluators, embeddings, shadows and tools keep separate attempt facts.
- Add unit/fixture, tagged service, official-SDK and accessible Chromium evidence
  at the relevant boundary. Follow [Testing](../../tests/README.md) and the
  [console design system](../../console/README.md#design-system).
- Use forward-only migrations; update Helm values/schema/templates together.
  Check readiness, metrics, recovery and runbooks. Update the canonical guide,
  remaining roadmap outcomes and any focused plan in the same change.
- Run normal check/integration/race/build/Helm and applicable performance gates
  on the same candidate before a release claim. Fix failures or explicitly
  revise supported scope; historical or narrower tests cannot close them.

## RM5 retained APIs and retrieval

The implementation includes fine-tuning, native vector stores, query-only cloud
retrieval, native Conversations/compaction, container lifecycle/files, Evals,
Gemini caches, and the existing skills/managed-agent contracts. Focused fixtures
cover codecs, nested ownership, combined references, filters, cache expiry,
exact billing units and evidence normalization. Console pricing/resource helper
checks and static type checking supplement these checks. Provider dependency
locks remain unchanged.

Database/process scenarios cover concurrent submission/mutation fences, atomic
settlement rollback, unknown acceptance, revoked credentials, duplicate/overlap
evidence, cancellation with incurred usage, late budgets, stale claims, deletion,
whole-schema restoration and metadata-only persistence. They are compiled locally;
service execution is still owed. Run the applicable `TestRetainedSettlement*`,
`TestRetrievalBindingsKeepExplicitGrantsAndImmutableHistory` and recovery scenarios
with the integration prerequisites through CI. Accessible browser journeys,
SDK-service qualification and live provider/profile/model certification are
also still open; compile-only results do not close them.

Evals REST fixtures remain in scope despite removal from the locked SDK. Unknown
container/cache creation cannot be adopted without provider correlation proof.
Storage/container charges need verified interval/session evidence; snapshots and
age do not prove charges. Native filters/ranking, eval data-source shapes and
other methods not declared by the frozen codecs remain explicit refusals. Record
OpenAI's announced Evals and fine-tuning availability limits with any live
qualification evidence; the gateway has no date-based disabling logic.

## Performance and routing

Benchmark/release maintainers must freeze the reference manifest and candidate
image, run full-rate S1–S6, S1-shadow and the pinned LiteLLM S1–S5 comparison,
retain raw results and repeat from a clean checkout. See
[Performance](performance.md) for scenarios, validity, topology and thresholds.
S3 must qualify CPU, single-threaded Valkey budget scripts and metadata ingestion;
a key's hash tag is not spread by clustering. S6 must hold all streams together;
write-deadline or kernel-limited runs do not establish capacity.

Routing maintainers qualify capacity selection under saturation across two
gateways, hard quotas, one pipelined state read, shadow start/drop/completion
counts and the unconfigured allocation gate. Cache/release maintainers qualify
paired hit/bypass S1 at no more than 1 ms incremental p95, guarded replay,
purge/eviction/outage/rotation and provider prompt-cache controls. Fixture token
savings are not live savings.

## Providers and token estimates

Provider maintainers run `liveproviders` for each claimed model/deployment,
region, operation, transport and credential mode. Start with Azure/Vertex,
Bedrock/SageMaker, then watsonx and native Cohere/Mistral. Record API version,
usage, errors, cleanup and every skip. Fixtures and compiled live cases do not
establish paid-provider compatibility. Extended Vertex JSON Schema acceptance
requires live evidence; Anthropic beta features can exceed local price estimates.

Run `TestLiveOpenAIPromptTokensMatchTheEstimate` with paid credentials against
the exact-prefix corpus; tiktoken oracle fixtures do not replace this result.
Catalog maintainers separately measure Anthropic/Gemini calibration with vendor
count endpoints, publish provenance and re-sign; factors stay 1 until measured.
Counts beyond the exact prefix remain calibrated. Release maintainers verify
embedded/downloaded catalog and plugin-index bytes with release trust,
startup/doctor, tamper and rollback refusal. Development signatures and declared
CI secrets do not establish release signing.

## Identity and access

Access/deployment maintainers qualify the supported signed-SAML profile and
SCIM Users/Groups against the target production IdP/directory: rollover,
disablement, removed grants, replay and provisioning retries, exact versions and
refused unsupported features. Qualify workload issuer/JWKS rollover, expiry,
audience mismatch and disablement for each configured issuer, including refusal
within 60 seconds of stale authority during outage. Rerun access/isolation,
budget concurrency, caller-credential privacy and MFA/recovery browser gates on
the exact candidate. Fixtures do not assert unrestricted SAML/SCIM conformance.

## Observability and billing

Observability maintainers rerun durable delivery/restart/retention, privacy,
scoped-access, series-cap and pinned Langfuse/Phoenix collector suites on the
candidate, recording collector versions. For each claimed S3/GCS/Azure/SMTP/chat
or SaaS destination, record account/region/auth mode, egress, receive/readback,
retry/duplicate behavior and secret redaction. Local wire fakes do not establish
target-account access or delivery to a responder.

Billing maintainers qualify OpenAI/Anthropic imports using scoped admin keys,
at least one closed UTC day, pagination/restatements, currency, covered-day
ratio and matching account scope. Qualify a CloudEvents receiver, Stripe test
meter and each claimed object transport with readback/redelivery. Preserve
commitment boundaries, pending edits, disablement, empty/excluded-only periods,
exact allocation, scoped APIs, late facts, retained history and backup/restore.
FOCUS static checks and explicit unused/period reconciliation assertions do not
qualify every Dynamic rule or physical capacity utilization.

## Operator ecosystem

Release/automation maintainers rerun generation/drift, machine authorization and
isolation, all twelve Terraform/OpenTofu resource lifecycles, packaged browsers,
check/integration/build/Helm on the same candidate. Record both OLP/provider
commits and engine versions. Cloud/security maintainers qualify each claimed
secret store/KMS with workload identity, immutable version/integrity binding,
denied access, rotation and startup after outage. Protocol fixtures and live
Vault evidence do not replace target-cloud execution.

Deployment maintainers rehearse primary/replica interruption, regional worker
and Valkey loss at target admission rate. Record stale-snapshot spend exposure,
60-second authority refusal and local readiness. Restore the candidate backup
with required old master-key versions, validate secret resolution and independent
console isolation, and record measured recovery objectives. The regional fixture
observed $1.20 against a $1 budget before reconciliation; this is not a qualified
maximum overshoot or ingestion-lag bound.

## API surfaces

Protocol/resource maintainers use the [endpoint ledger](../../tests/clients/m9-endpoints.json)
and [API plan](plans/api-surface.md), keeping pending methods disabled.

| Slice | Local evidence boundary | Remaining acceptance |
| --- | --- | --- |
| Legacy completions | Protocol/gateway, probes, route inspection, guards/capture, token/usage and pinned SDK fixtures | Console/browser, each provider live, release performance |
| Text Decisions | Native and one-way mapping, labels/order/usage/pricing, caller-paid budget and inspection fixtures | Console/browser, configuration promotion, each provider live |
| OCR/search | Codecs (including the Brave GET-query transport), raw-HTTP process fixtures, route inspection and Chromium journey; no locked official SDK exists for these APIs | Full `make integration`, live tuples (including the exact Brave tuple) and release performance; provider wizard cannot choose nondefault operation profiles, so use management/configuration apply |
| Files/batches | Native files, resumable uploads, managed/cloud jobs and content-free settlement fixtures | Full batch/provider integration, database-backed official SDKs (the client fixture has no DB), console/Chromium, live providers and release performance |
| Native duplex (RM6.5) | Compiled-profile contract tests, gateway WebSocket relay fixtures in process, a fake Bedrock stream behind the SDK opener seam, per-vendor frame fixtures, and the endpoint ledger rows for xAI, Vertex AI Live and Bedrock Nova Sonic | Live sessions per exact tuple with real xAI, Google Cloud and AWS accounts, including the 8-minute Nova Sonic stream bound and Vertex setup acknowledgement against the aiplatform endpoint |
| Governed pass-through (RM6.6) | Adversarial routing normalization and publication refusals, gateway credential isolation, redirect and bound fixtures, configuration plan refusals and console editor units; no locked SDK covers the bound-native namespace, so raw HTTP fixtures are the evidence | Live tuple per bound connection and endpoint family, full `make integration`, release performance |
| Governed WebRTC (RM6.4) | Gateway session-control fixtures against a mock calls/control/hangup provider, compiled contract, distinct `webrtc` capability tuple, certification probe, ledger rows and a Chromium journey spec with mock provider | Full `make integration` browser run, live OpenAI tuple, durable debit-intent settlement with restart recovery (the slice settles on the realtime lease path and claims no durable settlement), Azure tuple under its own contract |
| Unbuilt families | Planned contracts only | Implementation plus every slice-specific gate; shared provisional ledger and live control/settlement prerequisites |

Run `CLIENTS=m9-go tests/clients/run.sh` for locked OpenAI Go completions and
Decisions plus raw-HTTP OCR/search fixtures, including required route headers,
refusals and credential isolation. Console selections:

```sh
pnpm --dir console test src/lib/features/inference/playground/PlaygroundPage.svelte.test.ts src/lib/features/inference/playground/nativeOperation.test.ts
```

The OCR/search journey is `console/tests/gateway/ocr-search.spec.ts`.
The governed WebRTC journey is
`console/tests/gateway/realtime-webrtc.spec.ts` against
`console/tests/gateway/mock-realtime-webrtc.mjs`. It is integration-gated and
has not run outside `make integration` yet.
Vertex document processing (RM6.3) local evidence covers the Document AI
dialect and Mistral mapping, plan-level processor addressing and page
projection, document-location prefix refusals at the gateway, and route
simulation. Its exact live tuple stays unqualified until an opt-in run of
`TestVertexDocumentProcessingLive` under the `liveproviders` build tag
sets `OLP_LIVE_DOCUMENTAI_PROJECT`, `OLP_LIVE_DOCUMENTAI_LOCATION` and
`OLP_LIVE_MODEL` naming a processor ID under ADC credentials.
A dedicated resource/accounting integration selection remains required;
compiling fixtures does not execute it. Every new family needs lifecycle,
nested-ID/pagination isolation, ambiguous-create, restart, cleanup, revocation,
usage-completeness and pricing evidence before its claim advances.

## Guardrail and capture qualification

The [guardrail plan](plans/guardrails.md) owns vendor coverage and exit criteria.
Focused capture unit/component tests exist; the original integration command
used `-run '^$'` and executed **zero service tests**. Execute the following with
the documented disposable PostgreSQL/Valkey environment; explicitly selected
tests must not skip for missing services.

| Command / work | Acceptance condition | Owner |
| --- | --- | --- |
| `go test -tags=integration -count=1 -timeout=5m ./tests/integration -run '^TestCapture'` | All capture fixtures execute (no missing-service skips). Pinning survives later activation and unrelated edits; missing/foreign/inactive/retired/unsupported definitions refuse. Migration preserves existing policy settings. Unary/stream sink readback contains only redacted values, caller output stays unchanged, durable request/attempt/audit/export metadata contains neither raw nor redacted payloads. Stale queued ETags never deliver. | Export / gateway maintainers |
| `make integration` | Service, migration, authorization/isolation, process and browser regressions pass, including guardrail evaluator accounting and capture stale-policy behavior. | Release maintainers |
| `make check` | Full formatting, vet, console and local test checks pass; generated contracts are reproducible. | Release maintainers |
| `make test-race GO_TEST_PACKAGES='./internal/export ./internal/protocols ./internal/gateway ./internal/guardrails'` | No races in collector lifecycle, shared compiled detectors, cancellation or streams; investigate unrelated benchmark-harness tests included by this target. | Gateway maintainers |
| `pnpm --dir console exec playwright test --config playwright.guardrails.config.ts` | Existing guardrail journey passes. Manually exercise Settings capture ordered selection, removal, pin visibility, ordinary edit and explicit refresh with keyboard and screen reader; attach screenshots. | Console maintainers |
| `go test -run '^$' -bench 'Benchmark(Builtins100KTokens|GuardrailStreaming|Unconfigured)$' -benchmem ./internal/guardrails` | Record CPU/hardware/Go version, latency and allocations; compare configured built-ins/streaming to unconfigured control. Microbenchmarks alone do not qualify S3. | Performance maintainers |
| `BENCH_SCENARIOS=S3 OLP_BENCH_ENFORCE=1 make bench` | Run full-scale S3 on the frozen reference hardware with pinned gateway/mock/loadgen CPUs. Record baseline plus built-in/capture/stream workloads, latency, throughput, allocations and memory. The stock S3 harness does not automatically add guardrails: configure and record those workloads before claiming guardrail performance acceptance. | Performance maintainers |

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

## Code-mode qualification

Qualification applies to an exact client/account/model/operation/transport
combination. This corpus provides **controlled-fixture evidence only**.
It does not declare a live subscription account eligible.

### Latest official clients

Every CI job resolves the publisher's npm `latest` channel for `@openai/codex`,
`@anthropic-ai/claude-code` and `opencode-ai`, then follows that release's matching
Linux x64 or arm64 optional package. Release metadata is refreshed on every run;
a registry or integrity failure fails qualification without using an older release.
The installer verifies each platform archive's published SHA-512 before extraction
and checks the binary's actual version against the resolved version. Archives are
cached by client, version and architecture, and reverified before reuse.

Setup checks, integration tests and the broader client suites share one resolution
through `OLP_CODECLI_ENV`. Standalone qualification runs resolve afresh unless
that shared environment was explicitly supplied. To resolve once for several
commands, run `source "$(./scripts/code-mode-qualification.sh resolve)"`.
`install codex|claude-code|opencode` always resolves afresh and prints one verified
binary path. The installer needs Node.js, curl and tar.

Exact versions, release and platform package URLs, SHA-512 values and binary
version output are written to `.local/codecli/evidence/run-*/resolution.json`.
CI uploads this directory as `coding-client-releases` even after failure;
failed resolutions retain the metadata obtained and the error. Compatibility
failures remain blocking. The public configuration's `client_version` string is
`"latest"`; exact resolved versions belong to run evidence.

The fixture JSON files retain historical observations separately from this
qualification target. These source links document the original Codex review:

- Source tag: [rust-v0.160.0](https://github.com/openai/codex/releases/tag/rust-v0.160.0).
- Exact source: [a956835d020762cb2b570053af06f643a11c0ecc](https://github.com/openai/codex/commit/a956835d020762cb2b570053af06f643a11c0ecc).
- [Provider configuration](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/model-provider-info/src/lib.rs).
- [Session/subagent headers](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/codex-api/src/requests/headers.rs).
- [Client lifecycle](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/core/src/client.rs).
- [WebSocket endpoint](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/codex-api/src/endpoint/responses_websocket.rs).
- [Streamed remote compaction](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/core/tests/suite/compact_remote.rs).
- [Official configuration reference](https://developers.openai.com/codex/config-reference).
- [Device enrollment/refresh evidence](../../plugins/codex/README.md).

The runner supplies a fresh home and environment allowlist containing only the
OLP key for provider authentication. It verifies no `auth.json` is created.
Tool execution uses harmless `printf`, `rg` and `cat` commands in isolated
working directories, with workspace-write permission only for the file task.
The model `gpt-5.4`, outputs and usage are synthetic; they
prove neither entitlement nor real consumption nor a maximum token bound.
Direct CLI runs (`tests/codecli/qualification_test.go`) answer from the
`official-tool`, `official-child` and `official-compact` journeys in
`tests/fixtures/codex-qualified/journey.go`. Runs that use OLP's generated
configuration load it with `Client.UseConfiguration`. Runs add their journey flags to
the shared `codecli.ControlledFlags` set: no approvals, a read-only sandbox,
web search disabled, and the selected WebSocket setting.

### Support and evidence matrix

| Surface | Evidence | Qualification |
| --- | --- | --- |
| Linux amd64 official CLI, npm latest | Unmodified binary with registry SHA-512 verification | Controlled only; see CI run evidence |
| Linux arm64 | Matching latest platform package can be resolved; not executed in this job | Unqualified platform |
| OLP-key-only/native model configuration | Public generated-config endpoint; CLI has no upstream login | Controlled only |
| Local tools/results, continuation and cold resume | Official CLI over HTTP/SSE and WebSocket, both direct peers and integrated processes | Controlled only |
| Child agents | Official collaboration spawn; distinct child identity, parent lineage and one durable root account | Controlled only |
| Local compaction | Official CLI auto-compaction and resume against controlled peers on both transports | Controlled only |
| Remote compaction and generated TOML | Official CLI reads the public generated configuration; streamed compaction trigger/checkpoint crosses the fleet on both transports | Controlled only |
| Cancellation | CLI SIGINT closes HTTP peer; integrated HTTP/WebSocket cancellation closes upstream and preserves uncertainty | Controlled only |
| Public management | Accounts, pools, routes, budgets, publication, ETags, idempotency, CSRF, project/personal-key isolation and secret non-disclosure | Generic-grant management fixtures; independent of subscription claims |
| Enrollment/refresh without inference | Production Codex WASM plugin, public device flow, encrypted refresh, changed-principal lapse and rejected ordinary probe/activation | Controlled fixed-host TLS authority |
| Atomic fleet pins | Twelve concurrent first turns across two gateways; descendants, gateway restart and retirement | Controlled only |
| Response references | Durable aliases survive restart; unknown/ambiguous aliases and retired trees refuse before dispatch | Controlled only |
| No replay/failover | Outage/disconnect fixtures count dispatch and preserve the existing account | Controlled only |
| Payload/header fidelity | Raw HTTP/SSE/WebSocket tests, repeated headers/trailers, gzip/zstd observation, raw queries and upstream-first handshake rejection | Controlled only |
| Live continuation authority | WebSocket pool/account/key/project-membership revocation and tree retirement; HTTP revocation across replicas | Controlled only |
| Optional hard budgets | Unbounded operation succeeds without matching budget and returns 422 with one; durable ledger overlap/reservation/uncertainty tests | No proven native operation bound |
| Console | Typed API loader, role/project gating, component/unit/type checks and Chromium lifecycle journeys | Controlled only; run the browser suite separately from shell qualification |
| Real subscription account | No credential supplied or used | Unqualified |
| Client retries and transport recovery | Default retries, interrupted SSE/WS, WS→HTTP after 426, 503 account quarantine; pool/account/key/project/retirement refusal across alternating gateways | Controlled only |
| Local files/search | Official `exec_command` writes a file, enumerates files, searches and reads it; tool outputs continue over both transports | Controlled only; no hosted search claim |
| Fresh review | Root WS handshake before review child, including upstream-426 HTTP fallback; HTTP-only unresolved child refuses | Controlled only |
| Review after a parent task | Official app-server `thread/start`, `turn/start`, then inline `review/start`; shared root/account on both transports | Controlled only |
| Model hint | Generated static hint reaches ingress and is removed upstream; disjoint-model account selection and pinned reconnect across replicas; official `-m` leaves the hint unchanged | Controlled only; regenerate configuration when changing models |

Unknown server-context item references fail closed. Without a model hint,
new WebSocket bindings require an eligible account covering every published model;
otherwise they refuse with `code_model_selection_required`. Existing bindings
retain their account and check model permission on each generation. Requested subprotocols, binary client messages and overlapping
generations on one socket are unqualified and refuse. Observation is bounded to
16 MiB, generations to ten minutes and sockets to one hour. Allowance observation
retains primary and secondary windows separately for each reported metered limit,
plus distinct credit metadata. Partial or stale observations cannot erase a newer
window. Exhausted windows gate admission independently until their resets.
Token and request counts also retain independent observation and reset metadata;
an omitted count is not a refill, and a percentage-window reset cannot release
an exhausted count. The console shows counts alongside percentage windows.
Successful generations finishing during an active account cooldown do not clear
it. Draft edits retain active route publication metadata; generated client setup
continues to read the published revision until the draft is explicitly published.

### Reproduction

From the repository root on Linux, with the contributor toolchain and Docker:

```sh
make check
make test-race
make build
OLP_TEST_PULL_POLICY=missing ./scripts/integration.sh code
OLP_TEST_PULL_POLICY=missing ./scripts/integration.sh shell
./scripts/code-mode-qualification.sh cli
```

`code` provisions disposable PostgreSQL/Valkey, builds OLP, applies migrations,
installs the latest official clients, runs every `TestCode*` integration test with
race detection and runs direct CLI qualification. `shell` runs the broader public
process, fleet, recovery and SDK suites without browser tests. The ordinary
unqualified integration runner also runs Chromium. The pull-policy option uses
cached images instead of forcing a registry pull.

The standalone `cli` command requires no database or upstream account and caches
the verified package under ignored `.local/codecli`. With an existing disposable
database, Valkey and built binary, export `OLP_TEST_DATABASE_URL`,
`OLP_TEST_VALKEY_URL` and absolute `OLP_TEST_BINARY`, then use
`./scripts/code-mode-qualification.sh process` (fleet/CLI) or `all` (both).
Explicitly selected tests fail when prerequisites are missing; compile-only
checks do not qualify process behavior.

Fleet fixtures enroll the compiled production Codex plugin through public APIs
against two controlled fixed-host TLS authorities with distinct principals.
Capture starts before enrollment. Production authorization is not relaxed to
accept generic plugin accounts. Independent public-management fixtures use the
generic grant plugin and cannot prove Codex subscription support.

The release-contract suite reads every embedded OpenAPI operation and checks
concrete process registration, including client-config. The generated
authorization golden records its read/project boundary.

### Coding-plan adapters: OpenCode Go and GLM Coding Plan

The same evidence class applies to the OpenCode Go and GLM Coding Plan
adapters: controlled fixtures only, with no live coding-plan key. Exact package
URLs and hashes are recorded per run as described above;
`tests/fixtures/coding-plans/evidence.json` retains historical references.

#### Latest official clients

- **Claude Code**, the matching `@anthropic-ai/claude-code-linux-{x64,arm64}`
  package of npm `latest`, checked as `<resolved-version> (Claude Code)`.
  Its gateway contract is the
  [gateway compatibility guide](https://code.claude.com/docs/en/llm-gateway-protocol):
  `POST /v1/messages?beta=true`, `x-claude-code-session-id`, and agent headers on
  subagent requests.
- **OpenCode**, the matching `opencode-linux-{x64,arm64}` package of `opencode-ai@latest`.
  The [historically reviewed session headers](https://github.com/anomalyco/opencode/blob/v1.18.34/packages/opencode/src/session/llm/request.ts)
  name `x-opencode-session-id` and, for a child session,
  `x-opencode-parent-session-id`. The x64 package needs AVX2; the `-baseline`
  build is unqualified.

These clients share the same resolved binaries with `tests/clients` in each job;
SDK and framework packages retain their existing pinning policy.

The harness in `tests/codecli/agents.go`, with its proxy trap and environment
allowlist in `tests/codecli/trap.go`, runs each client with the configuration
the public client-config endpoint generated: Claude Code sources the shell file
and OpenCode reads the `opencode.json` from its working directory. The
environment is an allowlist with a private home and XDG directories. A decoy
workstation key (`ANTHROPIC_API_KEY`, `OPENCODE_API_KEY`, `ZHIPU_API_KEY`) must
never reach OLP or the upstream. Proxy variables point at a trap that refuses
and records every request leaving loopback. OpenCode asks the trap for
`registry.npmjs.org` at startup to install its own dependencies; the trap
refuses it and no model traffic goes there. Claude Code runs with
`--permission-mode default` and the tools each journey allows, because auto
mode's classifier needs a real model.

#### Support and evidence matrix

| Surface | Evidence | Qualification |
| --- | --- | --- |
| Pasted-key enrollment | Production WASM plugins through public APIs and the console: key page at an approved origin, masked input, NULL expiry and refresh, fingerprint principal, no plaintext key in secrets or responses, ordinary probe and activation fenced, a different key refused as a rotation | Controlled only |
| OLP-key-only configuration | Generated Claude Code shell and OpenCode JSON run unmodified; decoy credentials overridden; no `auth.json` | Controlled only |
| Claude Code on GLM Coding Plan and OpenCode Go | Tool loop (`Read`), subagent (`Agent`) bound beneath its session, `--continue`; every model variable on the route | Controlled only; OpenCode Go only for Messages models |
| OpenCode on GLM Coding Plan and OpenCode Go | Tool loop (`read`), child session (`task`) bound beneath its parent, `--continue`; Chat Completions with `include_usage`, and Messages for OpenCode Go | Controlled only |
| Mixed subscriptions | Claude Code plans in plan mode through `opusplan` on an OpenCode Go model and implements on a GLM model; OpenCode's plan agent and build agent do the same. Each model reaches its own host with its own account's key, and the session's tree pins one account per model and holds at most one account of each subscription. Raw forwarding sends each path and model of a mixed route to its subscription; a cooling pin refuses its model while the tree's other models stay served | Controlled scripted vendors only; no live vendor has accepted another vendor's history |
| OpenCode Go Responses | Raw forwarding through `v1/responses`, client identity, `previous_response_id` reference; OpenCode's request shape captured | Controlled only; no real-client Responses journey |
| Payload and header fidelity | Byte-identical bodies and SSE (with pings), forwarded `anthropic-*` and client headers, `?beta=true`, OLP key consumed from `Authorization` or `X-API-Key`, the serving account's adapter's single credential header placed | Controlled only |
| Upstream placement | `api.z.ai` and `open.bigmodel.cn` paths for Messages and Chat; `opencode.ai/zen/go/v1` for all three; Anthropic-shaped refusals on Messages paths | Controlled fixed-host TLS peers |
| Conversation trees | Claude Code session, agent and nested agent; OpenCode parent and child; unresolved parent, anonymous and ambiguous identity refused before dispatch | Controlled only |
| No replay or failover | A 429 is relayed byte for byte, cools the account, and the retry refuses without dispatch or switching accounts | Controlled only |
| Adapter management | Mixed pools and publications accepted; account `adapter`, route and revision `adapters`, binding `pins`; client-config clients, native models per client, planning model, formats and 422s | Controlled only |
| Console | Client, model and planning-model pickers, adapter badges, model pins, masked key enrollment, axe at desktop and 390px on a mixed route | Browser suite |
| Real coding-plan account | No key supplied or used | Unqualified |

Not qualified: Codex-path or WebSocket traffic on these adapters, which refuses;
a live vendor accepting a conversation another vendor's model contributed to;
`count_tokens`; live Z.ai research MCP servers; vendor allowance windows; hard token
budgets; agent teams; shells other than POSIX `sh`-compatible ones; Linux
arm64 execution.

Research proxy behavior is covered separately by focused `TestCodeResearch*`
integration fixtures: a GPT-only route uses its enrolled global Z.ai account
for all three services through fixed-host TLS peers. The fixtures cover native
JSON and SSE discovery/results, cancellation, session isolation and termination,
live pool/key revocation, credential generations, disabled services and no replay
after a disconnected dispatch. Generator and console tests cover the three
client formats, account selection and Claude's companion `.mcp.json`. These
checks do not establish live-vendor or real-client research interoperability.

Focused lifecycle regressions additionally cover immediate idle SSE headers,
failed-handshake session reclamation past the per-key capacity, malformed and
interrupted initialization, and server pings during initialization and on GET
streams. Revocation during an open stream blocks its subsequent ping reply.

#### Reproduction

```sh
OLP_TEST_PULL_POLICY=missing ./scripts/integration.sh code
./scripts/code-mode-qualification.sh install claude-code
./scripts/code-mode-qualification.sh install opencode
```

`code` runs every `TestCode*` test, including `TestCodeCodingPlan*` and the
real-client `TestCodeQualificationClaudeCodeJourneys`,
`TestCodeQualificationOpenCodeJourneys` and
`TestCodeQualificationMixedSubscriptionJourneys`. `process` mode runs the journeys
with the other `TestCodeQualification` tests, reusing `OLP_CODECLI_ENV` when supplied
or freshly resolving all three clients otherwise. The environment exports their
binary paths and matching `_VERSION` values together.

### Remaining release gates

Record integrated OLP revision, client package/hash, platform, native model,
operation, authorized account class and transport for live qualification.
Controlled peers cannot establish paid-provider compatibility.

The minimum live prerequisite is an authorized non-FedRAMP ChatGPT subscription
account eligible for Codex, with permission to approve OLP's device flow at
OpenAI and run the listed tasks. No raw access/refresh token needs to be copied
to a developer workstation or committed.

No trustworthy conservative native hard-token bound has been established; see
the evidence review below. Matching enabled budgets refuse explicitly;
unbudgeted operations remain available. Positive native budget qualification
is still a release gate.

Exercise hosted search/files, live quota reset, credential rotation during an active task and real enrollment,
refresh/allowance/inference. Complete the console/browser suite separately.

For OpenCode Go and GLM Coding Plan, live qualification needs a subscriber's
own key, enrolled by paste, and must record which models each vendor serves on
which endpoint, whether it accepts the client's `anthropic-beta` values, and
its quota and concurrency refusals.
No live subscription support claim follows from these fixtures.

Raw synthetic captures exist in memory only. Durable diagnostics contain
metadata, never prompts, outputs, tools, raw headers or grant material.

### Followup qualification at the integrated base

The followup began at `0ca6f2a3e3417a41ec561dbdaabd4a2187740afe`.
`tests/integration/code_cli_followup_test.go` alternates requests through two real
gateways, checking diagnostics against transient upstream captures: one dispatch,
stable account/root, fresh authority and durable uncertainty. Prewarm with unknown
usage is not zero-token settlement. Its ten-case HTTP retry/WS reconnect matrix
uses official defaults, including upstream-426 fallback, 503 cooldown and
pool/account/key/project/tree revocation before retry. Current model-hint and
review-parent behavior is in [Code mode](../use/code-mode.md#conversations-and-authority).

Fresh HTTP-only child-first review remains unresolved: supporting it without a
prior real user task would require an explicit authenticated non-inference root
registration decision. Never silently bind an unknown child or create dummy
inference to establish a parent. An ingress rejecting WS before OLP cannot use
the qualified upstream-426 fallback claim.

File/search qualification requires `bubblewrap`, `ripgrep` and a working official
sandbox, which CI preflights. Private homes use the user cache: Codex rejects
helper aliases under system temporary directories. Integration uses Ubuntu 26.04;
native image checks remain Ubuntu 24.04 on both architectures. In the originally
reviewed 0.160.0 catalog, `gpt-5.4` fell back to metadata without freeform
`apply_patch`; the router rejected such calls. Freeform patch support for that
selected model remains unqualified; no fabricated catalog enables it.

#### Native hard-token bound research

Official evidence reviewed:

- [GPT-5.4 API model card](https://developers.openai.com/api/docs/models/gpt-5.4):
  1,050,000 context tokens and 128,000 maximum output tokens for the API model.
- [Reasoning guide](https://developers.openai.com/api/docs/guides/reasoning):
  reasoning occupies context and is billed as output, including invisible tokens.
- [Compaction guide](https://developers.openai.com/api/docs/guides/compaction):
  encrypted context is opaque; server-side compaction can run inside one
  Responses request and continue inference afterward.
- [Codex model metadata](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/protocol/src/openai_models.rs):
  optional context size, effective percentage and auto-compaction threshold are
  client context-management settings. They do not attest total server work.
- [Native request definitions](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/codex-api/src/common.rs):
  neither `ResponsesApiRequest` nor `ResponseCreateWsRequest` sends
  `max_output_tokens`. `reasoning.effort` is not a numeric token cap.
- [Codex pricing](https://developers.openai.com/codex/pricing): context, reasoning,
  tools, retrieval and caching affect subscription usage; prompt length alone
  is not a reliable estimate.

No reviewed official contract maps these API limits to a maximum **total reported
tokens per Codex subscription operation**, including repeated internal work,
remote compaction, tools, hidden/server context, cached input and reasoning.
Indirect `previous_response_id` and encrypted context also prevent proving that
request-byte length bounds input. Cached tokens remain an input subset, reasoning
an output subset; adding them twice would be incorrect. Each visible child/tool
continuation gets its own admission, but that does not bound hidden work inside
an individual upstream operation.

A positive adapter needs a provider-enforced, versioned bound for the exact
subscription model/operation and a documented accounting rule covering those
components. A controlled usage number, an operator constant, the API model card
alone, or a finite successful live run cannot prove that contract. No positive
bound adapter was added. Budgeted native generation continues to refuse with
`code_token_bound_unavailable`; ordinary unbudgeted generation is unaffected.

#### Followup shell commands

With the disposable services and built OLP binary described above:

```sh
source "$(./scripts/code-mode-qualification.sh resolve)"
go test -mod=readonly -race -tags=integration,codecli -run '^TestCodeCLIFollowup' -count=1 -timeout=8m -v ./tests/integration
go test -mod=readonly -race -tags=codecli -count=1 -timeout=5m -v ./tests/codecli
go test -mod=readonly -race ./internal/routes
go vet -tags=integration,codecli ./tests/codecli ./tests/fixtures/codex-qualified ./tests/integration ./internal/routes
bash -n scripts/code-mode-qualification.sh
git diff --check
```

No browser or live credentials are used. The existing `process` helper selects
`TestCodeQualification`; run the explicit followup command above to include this
new corpus when not using the broader `code` integration target.

Historical focused followup passed race-enabled initial/expanded authority/review
corpora, existing CodeQualification, codecli, route tests, vet and shell checks on
Go 1.27.1. Those selected runs did not claim whole integration/browser or live
subscription results. Later whole-source evidence is in
[historical qualification](#historical-qualification); remaining live/bound gates above
are unchanged.

## Historical qualification

These retained records predate this documentation change. They do not qualify
later revisions or close uncollected full-rate/live-provider/production-IdP gates.

- **October 10, 2026 operator implementation:** the default integration flow,
  internal/extension/SDK/client suites, packaged/Vite browsers, fresh/restored
  journeys, local race/check, generation/version/build/Helm passed. Terraform
  1.16.5 and OpenTofu 1.13.1 had separate twelve-resource acceptance against an
  immutable SDK/OLP version. Protocol fixtures and live Vault were distinct from
  target AWS/GCP/Azure evidence.
- **October 12, 2026 frozen source, 62 migrations:** runtime-ownership repair
  against `e5a87b859fe25aa7b3287111899a7f4074d1ada5`, with Go 1.27.2, Node
  26.11.1 and pnpm 11.24.0. The final normal check passed 85 Go packages,
  147 console files/1,228 tests and 91 script tests. Full integration took
  4,620 seconds: all 719 compiled public names once in disjoint cohorts, CodeCLI,
  eight internal packages, extension/SDK/ten client lanes and browser phases;
  841 top-level/1,975 nested Go tests had no failures/skips/races/panics.
  Console evidence covered 37 feature tests, six fresh hosted journeys and one
  restore with history preserved/growing after inference. Binary/static console,
  0.1.0 version and Helm/schema/confinement/egress checks passed. The benchmark
  gate covered 93 shared/26 added/zero removed names with six alternating
  200 ms samples; one flag cleared confirmation under the unchanged 10% rule.
  Earlier partial/failed preflights were not whole-source passes. CPU-overlapped
  benchmark data was discarded. Local allocation/latency improvements were not
  reference throughput or live-provider results.
- **October 16, 2026 cache implementation:** focused replay/isolation/sealing,
  outage, purge, provider controls, configuration and browser fixtures were
  recorded. Broad integration stopped at a WebSocket reconnect shutdown timeout;
  the isolated case passed, but the broad run never completed. The full-rate
  1 ms incremental-p95 gate remained open.
- **Undated capture-redaction record:** focused capture/guardrail Go tests,
  component/type/lint/vet and generation passed under a five-minute budget;
  integration compiled with `-run '^$'` and ran zero service tests. No screenshots
  were taken. Service readback, live adapters, full gates and S3 stayed open.

### Commitment accounting evidence

**2026-10-18**, base `ce190e7abda01e41f0a4abca29854f75b115b1fb`, commitment
completion working tree. SHA-256 over sorted changed implementation/test/contract
paths, NUL, bytes, NUL (including generated console contracts; excluding docs):
`4a228dca4fc8c37b98010e30941a4307c3a4f32e5a3bb284f8ac2daa74219f22`.
Focused accounting/FOCUS tests and database/export/usage/runtime integration
passed with race detection; contract regeneration and `make check` passed.
Full `make integration` was interrupted/cancelled and did not complete; new
public-API, restore and browser checks were source only in that run.
`BENCH_TIME=100ms make bench-gate` stopped after three sample pairs with **no gate
verdict**. Extended integration, reference performance, live billing and external
sink readback remained open.
