# Code-mode qualification

Qualification applies to an exact client/account/model/operation/transport
combination. This corpus provides **controlled-fixture evidence only**.
It does not declare a live subscription account eligible.

## Pinned official client

The runner downloads official npm Codex **0.160.0**, verifies the platform
archive's SHA-512 before extraction, and checks `codex-cli 0.160.0`.
Exact package URLs and hashes are in
`tests/fixtures/codex-qualified/evidence.json` and enforced by
`scripts/code-mode-qualification.sh`.

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
Tool execution uses a harmless fixture `printf`, read-only sandbox and temporary
working directory. The model `gpt-5.4`, outputs and usage are synthetic; they
prove neither entitlement nor real consumption nor a maximum token bound.

## Support and evidence matrix

| Surface | Evidence | Qualification |
| --- | --- | --- |
| Linux amd64 official CLI 0.160.0 | Unmodified integrity-pinned binary executed | Controlled only |
| Linux arm64 | Exact official package hash pinned; not executed | Unqualified platform |
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
| Console | Typed API loader, role/project gating and component/unit/type checks | Browser execution remains parent-owned |
| Real subscription account | No credential supplied or used | Unqualified |
| Native file/search, automatic fallback, explicit CLI retries and live quota/rotation during a complete task | Not fully exercised | Remaining qualification gaps |

Unknown server-context item references fail closed. New model-independent
WebSocket bindings require an eligible account covering every model on the
published route. Requested subprotocols, binary client messages and overlapping
generations on one socket are unqualified and refuse. Observation is bounded to
16 MiB, generations to ten minutes and sockets to one hour. Allowance observation
currently represents the primary reported subscription window only.

## Reproduction

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
installs the pinned official client, runs every `TestCode*` integration test with
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

## Remaining release gates

Record integrated OLP revision, client package/hash, platform, native model,
operation, authorized account class and transport for live qualification.
Controlled peers cannot establish paid-provider compatibility.

The minimum live prerequisite is an authorized non-FedRAMP ChatGPT subscription
account eligible for Codex, with permission to approve OLP's device flow at
OpenAI and run the listed tasks. No raw access/refresh token needs to be copied
to a developer workstation or committed.

No trustworthy conservative native hard-token bound has been established.
Prompt estimates, model context limits, `max_output_tokens` or fixture constants
are not proof. Matching enabled budgets refuse explicitly; unbudgeted operations
remain available. Positive native budget qualification is still a release gate.

Exercise native files/search, automatic HTTP fallback, explicit client retries,
live quota reset, credential rotation during an active task and real enrollment,
refresh/allowance/inference. Complete the console/browser suite separately.
No full issue-294 or live subscription support claim follows from these fixtures.

Raw synthetic captures exist in memory only. Durable diagnostics contain
metadata, never prompts, outputs, tools, raw headers or grant material.
