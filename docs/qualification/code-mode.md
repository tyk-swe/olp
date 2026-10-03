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
Tool execution uses harmless `printf`, `rg` and `cat` commands in isolated
working directories, with workspace-write permission only for the file task.
The model `gpt-5.4`, outputs and usage are synthetic; they
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

No trustworthy conservative native hard-token bound has been established; see
the evidence review below. Matching enabled budgets refuse explicitly;
unbudgeted operations remain available. Positive native budget qualification
is still a release gate.

Exercise hosted search/files, live quota reset, credential rotation during an active task and real enrollment,
refresh/allowance/inference. Complete the console/browser suite separately.
No full issue-294 or live subscription support claim follows from these fixtures.

Raw synthetic captures exist in memory only. Durable diagnostics contain
metadata, never prompts, outputs, tools, raw headers or grant material.

## Followup qualification at the integrated base

The followup starts at OLP `0ca6f2a3e3417a41ec561dbdaabd4a2187740afe`.
`tests/integration/code_cli_followup_test.go` runs the official client through a
transparent ingress observer alternating requests across two real gateway
processes. Public management diagnostics are compared with upstream captures:
one dispatched attempt per received request, stable account/root, fresh pool
checks, and durable uncertainty for interrupted inference. Client prewarm has
no final usage and is checked separately for zero reserved budget; its unknown
usage is not manufactured into a zero-token settlement.

The fixture interrupts exactly one request, then responds normally. HTTP stream
retry and WS reconnect recover. Upstream handshake 426 triggers client-owned
HTTP fallback. A real upstream 503 produces a one-minute account cooldown:
Codex retries automatically but OLP refuses further dispatch on that pin.
Revoking pool membership, account eligibility, the API key, project membership,
or the tree between the first dispatch and retry similarly refuses before
upstream. The ten-case matrix runs both HTTP retry and WS reconnect with the
official client. No retry settings are overridden to make these pass.
Official defaults are four request retries and five stream retries; the provider
retries 5xx/transport failures but not 429 at its HTTP request layer. See
[retry policy](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/codex-client/src/retry.rs)
and the provider configuration source linked above.

### Model selection before WebSocket upgrade

The official custom-provider handshake has no dynamic native-model header in
these captures. In `core/src/client.rs`, `responses_headers()` uses model-specific
`codex_responses_headers` only for authenticated Codex backend routes;
`requires_openai_auth=false` and `env_key=OLP_API_KEY` do not qualify for that path.
The generated static `http_headers` entry therefore supplies
`X-OLP-Code-Model=<selected model>` to OLP only. The native body is untouched.

`TestOfficialCodexModelSelectionHintAndOverride` proves that `-m
gpt-5.3-codex` sends that model in the native WS body while the configured hint
remains `gpt-5.4`. Regenerating for `gpt-5.3-codex` without `-m` is also tested:
both the hint and body use that selected native model. Regenerate the complete
configuration with the management endpoint's `model` query parameter before a
new conversation. A pinned tree
cannot acquire a different account by changing this header. Server-side
disjoint-model selection and no-hint refusal behavior remain an integration gate
on this followup's base, not a passing claim from the single-model corpus.

### Review parent establishment

[ReviewTask](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/core/src/tasks/review.rs)
delegates to a new session with `SubAgentSource::Review` and the real parent's
thread ID. With WebSockets disabled, fresh `exec review` sends a distinct
`thread-id`, `x-codex-parent-thread-id`, and `x-openai-subagent: review` before any
parent request. The public test requires `code_parent_unresolved`, zero bindings,
zero attempts and zero upstream dispatches. Storage policy is unchanged.

With generated WebSocket support enabled, the official
[startup prewarm](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/core/src/session/startup_prewarm.rs)
opens the root connection first. OLP's existing connection binding establishes
the root without parent inference, then the child inherits it. This also passes
when upstream returns 426 and the child uses HTTP. The test asserts the first
handshake is the root, any root request is `generate=false`, and both bindings
share the same account and root. An ingress proxy rejecting WS before OLP would
not establish this root; such a topology remains unqualified.

The official app-server alternative runs `thread/start`, a real `turn/start`
user task, waits for completion, then calls `review/start` with `delivery=inline`.
Both HTTP and WS tests observe the review child inheriting the established root.
This is not permission to synthesize a setup generation. If fresh **HTTP-only**
review must work with no prior user task, the minimal unresolved product decision
is whether to introduce explicit authenticated non-inference root registration.
It cannot be implemented by silently binding an unresolved child.

The local file/search task uses the officially advertised `exec_command` tool.
Linux qualification requires `bubblewrap` and `ripgrep` on `PATH`, and a host
that permits the official Codex sandbox to run. CI checks sandbox startup before
starting the integration suite. See the [official sandbox prerequisites](https://developers.openai.com/codex/concepts/sandboxing#prerequisites)
for distribution-specific setup; qualification does not disable the sandbox or
change host security policy.
The integration job uses Ubuntu 26.04 for its distribution-provided bubblewrap
support. Native release image checks continue to run on Ubuntu 24.04 for both
architectures.
The client harness creates each private home under the user cache and removes it
after the test. Codex refuses helper aliases under the system temporary directory;
those aliases are required by distribution bubblewrap versions without `--argv0`.
The 0.160.0 bundled catalog lacks `gpt-5.4`, so this client uses fallback metadata
with no freeform `apply_patch` tool. An attempted freeform call was rejected by
the official tool router. Freeform patch support for this selected model remains
unqualified; the test does not inject a fabricated model catalog to enable it.

### Native hard-token bound research

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

### Followup shell commands

With the disposable services and built OLP binary described above:

```sh
export OLP_CODEX_BINARY="$PWD/.local/codecli/0.160.0-linux-x64/package/vendor/x86_64-unknown-linux-musl/bin/codex"
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

Followup results on Go 1.27.1, with race detection:

- The full initial `^TestCodeCLIFollowup` corpus passed in 107.941s. After
  expanding revocation to all five authorities and both transports and adding
  structured review output, the affected tests were rerun with
  `-run '^TestCodeCLIFollowup(RetryRechecksAuthority|FreshReviewParentEstablishment|ReviewAfterExplicitParentTask)'`:
  all passed in 208.174s. Recovery and file/search code was unchanged.
- `-run '^TestCodeQualification' -count=1 -timeout=12m ./tests/integration`
  passed in 151.161s, including public lifecycle/isolation, fleet pin races,
  retirement, authority, cancellation and official tool/child/compaction journeys.
- The final `./tests/codecli` command above passed in 5.638s, including matching
  regenerated model configuration and the deliberate `-m` mismatch.
- Route race tests, scoped vet, script syntax and whitespace checks passed.

These are selected shell/process tests; they do not claim the complete
integration/browser suite or live subscription qualification.
