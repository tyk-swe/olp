# Code-mode qualification

Qualification is evidence for an exact client/account/model/operation/transport
combination. The checked-in corpus is **controlled-fixture evidence only**.
Nothing in this document declares a live subscription account eligible.

## Pinned official client

The runner downloads the official npm platform artifact for Codex **0.153.2**,
verifies SHA-512 before extraction, and checks `codex-cli 0.153.2` before execution.
It does not implement a replacement CLI. Package URLs and integrity values are
checked in at `tests/fixtures/codex-qualified/evidence.json` and enforced by
`scripts/code-mode-qualification.sh`.

- Source tag: [`rust-v0.153.2`](https://github.com/openai/codex/releases/tag/rust-v0.153.2).
- Exact source: [`657a993cbee87acf52d14b758ce49dbd46d1b8eb`](https://github.com/openai/codex/commit/657a993cbee87acf52d14b758ce49dbd46d1b8eb).
- [Provider configuration fields](https://github.com/openai/codex/blob/657a993cbee87acf52d14b758ce49dbd46d1b8eb/codex-rs/model-provider-info/src/lib.rs):
  `base_url`, `env_key`, Responses wire API, OpenAI-auth requirement and WebSocket capability.
- [Session and subagent headers](https://github.com/openai/codex/blob/657a993cbee87acf52d14b758ce49dbd46d1b8eb/codex-rs/codex-api/src/requests/headers.rs).
- [Client lifecycle](https://github.com/openai/codex/blob/657a993cbee87acf52d14b758ce49dbd46d1b8eb/codex-rs/core/src/client.rs):
  turn state, parent identity, retries/fallback and WebSocket prewarm.
- [WebSocket endpoint](https://github.com/openai/codex/blob/657a993cbee87acf52d14b758ce49dbd46d1b8eb/codex-rs/codex-api/src/endpoint/responses_websocket.rs).
- [Official configuration reference](https://developers.openai.com/codex/config-reference).

The controlled runner supplies a fresh home and an environment allowlist. Only
`OLP_API_KEY` is passed as provider authentication; existing API keys, Codex home
and workstation login are excluded. It checks that no `auth.json` was created.
Local tool execution is a harmless fixture `printf` in a temporary working
directory with read-only sandbox mode and approval prompts disabled.

## Support and evidence matrix

| Surface | Evidence in this qualification branch | Release status |
| --- | --- | --- |
| Linux amd64 official CLI 0.153.2 | Executed real pinned binary against controlled peers | Controlled only |
| Linux arm64 | Exact official package integrity pinned; binary not executed | Unqualified platform |
| OLP-key-only/native-model config | Controlled HTTP/SSE and WebSocket requests; no account login | Real subscription path still unqualified |
| Local shell tool, tool result, continuation, cold resume | Official CLI executed for HTTP/SSE and WebSocket; stable `thread-id` asserted | Controlled only |
| CLI cancellation | Real CLI SIGINT closes controlled HTTP stream | OLP cancellation still requires integrated process run |
| Child identity | Official client collaboration spawn executed for HTTP/SSE and WebSocket; distinct child and parent/subagent headers asserted | Controlled only |
| Local compaction | Official client auto-compaction after resume executed for HTTP/SSE and WebSocket; same thread/auth/model and actual compaction prompt asserted | Controlled only; remote provider compaction unqualified |
| Public account/pool/route/budget management | PostgreSQL-backed public API tests: enrolled fixture principals, publication, ETags, CSRF, replay, isolation, personal-key ownership, secret non-disclosure | Generic grant evidence only |
| Synthetic inference during setup | Capture starts before enrollment; account setup, pool writes, publication and budget management produce zero inference | Refresh/worker/health/activation journeys still need Codex-specific integration |
| Atomic binding across processes, tree/restart/retirement | Acceptance assertions authored and compile; initial foundation process run returns 404 | Not qualified |
| No retry/failover, exact body/header/query, uncertainty | Controlled process assertions authored | Not run past transport dependency |
| Per-generation WebSocket authority, account/key/project revocation, cancellation | Controlled process assertions authored | Not run past transport dependency |
| Unknown bound with optional budgets | Public process test requires 422 only while budget is enabled | Not run past transport dependency |
| Proven native hard budget | No trustworthy Codex bound established | Blocking qualification gap |
| Generated management configuration and console | Not part of these executed tests | Parent integration/browser qualification required |
| Authorized real Codex subscription enrollment/refresh/inference | No upstream credential supplied or used | Not qualified |
| Compaction, transport fallback/reconnect, quota/credential rotation in a full OLP task | No complete executed OLP journey | Blocking qualification gaps |

The native `gpt-5.4` fixture model and returned token usage are synthetic test
inputs. They do not assert entitlement, pricing, real consumption or a proven
maximum. In particular, sixteen reported fixture tokens do not justify a
sixteen-token pre-dispatch reservation.

## Running the suites

Recorded on Linux amd64 with Go 1.27.1, against the foundation plus the
qualification changes:

| Check | Result |
| --- | --- |
| `./scripts/code-mode-qualification.sh cli` | PASS under race detection: tool/resume, child, local compaction on HTTP/SSE and WebSocket; HTTP cancellation |
| `go test -mod=readonly -race -tags=integration -count=1 -timeout=5m -run '^TestCodeQualificationPublic' -v ./tests/integration` | PASS: both public management tests, disposable PostgreSQL |
| `go test -mod=readonly -tags=integration,codecli -run '^$' ./tests/integration` | PASS: compile only |
| `go vet -tags=integration,codecli ./tests/codecli ./tests/fixtures/codex-qualified ./tests/integration` | PASS |
| `bash -n scripts/code-mode-qualification.sh`, `gofmt`, `git diff --check` | PASS |
| `make build-go` | PASS |
| `make test-go GO_TEST_PACKAGES='./internal/codemode ./internal/resources ./internal/usage ./internal/routes ./internal/providers'` | PASS |
| `make test-scripts` | PASS: four script tests |
| `TestCodeQualificationFleetAtomicTreeRestartAndRetirement` with real PostgreSQL, Valkey and built gateway | FAIL: twelve first-bind requests return 404; foundation has no code transport |
| Remaining integrated fleet/CLI, real-provider and browser journeys | UNRUN |

From the repository root, with the contributor Go toolchain, `curl`, `openssl`
and `tar`:

```sh
./scripts/code-mode-qualification.sh cli
```

This runs only the official CLI against local controlled peers. It requires no
database, real provider account or browser. It installs the pinned binary under
the ignored `.local/codecli` directory. Neither this result nor a generic OAuth
fixture is production account qualification.

Public management tests use the ordinary integration PostgreSQL prerequisite:

```sh
go test -mod=readonly -race -tags=integration -count=1 -timeout=5m \
  -run '^TestCodeQualificationPublic' -v ./tests/integration
```

Build the integrated OLP binary using `make build-go`, then supply
`OLP_TEST_DATABASE_URL`, `OLP_TEST_VALKEY_URL` and absolute `OLP_TEST_BINARY`.
The process suite starts two real gateway subprocesses sharing one disposable
database and Valkey namespace. Management uses the existing public HTTP harness;
assertions do not write binding, accounting or permission rows directly.

```sh
./scripts/code-mode-qualification.sh process
# Or run both controlled client and integrated process suites:
./scripts/code-mode-qualification.sh all
```

Explicit selection with missing prerequisites fails. There are no passing skips
for absent transport, auth, configuration or credentials. `codecli` is an opt-in
build tag because the official binary and fully integrated transport are required;
an ordinary `go test ./...` does not qualify these journeys.

### Integration boundary

The qualification branch starts at foundation
`6c8a5f7490ff58b20b6bd8476fbd0af28662e8e7`, which provides `CodeLedger` and runtime
publication but does not register a code transport or authorizer. Its first fleet
test was attempted and failed at `/code/qualification/responses` with 404. The
remaining process assertions are unrun, not successful tests.

`newCodePublicFixtureWithPeer` currently enrolls the existing **reference grant
plugin** to test management independently. For an integrated Codex process run,
replace that helper's provider/enrollment setup with the supported Codex adapter's
controlled enrollment fixture, preserving its two distinct observed principals,
public API provisioning and capture-before-setup behavior. Do not modify the
production adapter to accept reference-plugin accounts merely to pass this suite.
No account is inserted directly into the database or silently marked qualified.

The common integration runner must select `integration,codecli` after installing
the pinned client and building the integrated process. Include `tests/codecli`
and `tests/fixtures/codex-qualified` in the common explicit formatting list. These
shared runner changes belong to integration ownership.

## Release gates

Before claiming full code-mode support, record the exact integrated OLP revision,
client package/hash, platform, operation/model, authorized account class and
transport alongside command results. Exercise a full local task using OLP's
generated configuration: local tools, child agents, compaction, cancellation,
resume/reconnect, native model selection and both transports. Verify enrollment,
refresh and identity-preserving rotation against an authorized real account.
Keep all account material out of evidence files and logs.

Measure client retries separately from OLP dispatch: test retries disabled when
asserting one dispatch, then exercise explicit client retries against the same
root pin. WebSocket prewarm in this release uses `response.create` with
`generate=false`; it is client connection setup, not permission for a later
generation. The controlled fixture handles and captures it rather than
silently dropping it. Every subsequent generating message still needs admission.

Prove hard bounds for each claimed budgeted operation using official semantics;
do not supply fixture constants as adapter evidence. Retain overlap/concurrency,
UTC rollover, overrun and unknown-usage tests. Existing foundation ledger tests
exercise reservation machinery but cannot establish an upstream bound. Also
complete compressed payload/error fidelity, redirect refusal, generation
reconnect, revocation on every WebSocket generation, and lifecycle refresh/health
observations. These remain gates; the support scope is not reduced to make a
fixture pass.

Raw synthetic captures in the peer exist in memory only. Durable diagnostics
must stay metadata-only, including refusal and failure paths. Never attach real
prompts, outputs, tool payloads, raw headers or grant material to test reports.
