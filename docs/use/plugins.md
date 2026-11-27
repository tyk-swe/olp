# Plugins

Plugins are operator-installed extensions that supply provider authentication and hosting around built-in dialects, custom guardrail evaluators, or route selector predicates. A provider plugin never defines a dialect of its own because codecs and interaction contracts remain first-party OLP code.

Plugins are confined by default. OLP runs their WebAssembly modules on [wazero](https://wazero.io) and explicitly grants each capability a call may use. The exception is the experimental [unconfined tier](#unconfined-plugins-experimental) for native provider executables, which a deployment must deliberately enable. Guardrail plugins and route selector predicate plugins always require confined WebAssembly modules.

This guide covers installation, review and approval, provider profiles, guardrail and route selector plugins, grant enrollment and refresh, configuration promotion and removal. The [plugin authoring guide](../develop/plugin-authoring.md) covers how to build a plugin.

## Upstream terms of use

Some upstreams, including Anthropic and Google, prohibit third-party use of their subscription sign-in. OLP stays neutral. The repository ships the plugin SDK, a reference plugin against a fictional upstream, and six reviewed code-mode plugins (`codex`, `kimi-code`, `minimax-plan`, `opencode-go`, `qwen-token-plan` and `zai-coding` under `internal/codemode/`) built reproducibly via `cmd/pluginindex` and published as release assets. In every case, **whether a plugin may be used with an upstream is the operator's responsibility** under that upstream's terms of use.

## Installing

An owner installs a plugin from the console's **Plugins** page (`/plugins`), or with `POST /api/v1/plugins` carrying the module as an `application/wasm` body. A module is at most 32 MiB, and an installation holds at most 64 plugin digests in total across uploaded WebAssembly modules and permitted unconfined plugins combined. Installation requires an owner signed in with a browser user session and installation-wide access, like management-token administration. The same requirement applies to approval, uninstalling and permitting unconfined plugins because management tokens cannot perform these operations.

OLP stores the module by the SHA-256 digest of its bytes, which identifies the plugin from then on. Before storing it, OLP instantiates the module within the plugin limits and reads the manifest it declares. It refuses the upload with a `422` problem naming the reason.

| Code | Reason |
| --- | --- |
| `plugin_module_invalid` | Not WebAssembly, not a plugin, a WASI command instead of a reactor, imports something OLP does not provide, or declares more than the runtime allows (see [Confinement and limits](#confinement-and-limits)). |
| `plugin_abi_unsupported` | Built for another plugin ABI version. During 0.x the ABI carries no compatibility promise, so rebuild the plugin with the SDK of this OLP version. |
| `plugin_manifest_invalid` | The manifest is invalid or declares something this OLP does not understand. The problem's `errors` names the field, such as `manifest.origins[0]` or `manifest.profiles[0].hosting.headers.Authorization`. |
| `plugin_dialect_unknown` | A declared profile names a dialect plugin profiles cannot serve, such as `manifest.profiles[0].dialect`. |
| `plugin_timed_out`, `plugin_failed` | The module exceeded its time limit, or trapped, exited or exhausted its memory while starting or reporting its manifest. |

Uploading a digest that is already installed changes nothing and returns it. Several digests of the same plugin (the manifest `name`) may be installed side by side, so a new build can be installed and reviewed before anything moves to it.

## Reviewing and approving

`GET /api/v1/plugins` and the **Plugins** page show each installed digest together with what its manifest declares. A manifest declares up to 16 canonical `scheme://host[:port]` **origins** (the only origins the plugin may ever reach), an optional `guardrail: true` flag for confined guardrail plugins, and **profiles** (1 to 16 provider profiles, or an empty list when `guardrail: true`). Each profile names the built-in dialect it serves, whether providers using it authenticate with a static credential or a [grant](#grant-enrollment), the options its providers set, and its hosting adaptation. Hosting covers the upstream address at one of the plugin's origins, declared headers and query parameters, model listing and failure classification, body envelopes and rewrites, and whether its upstream serves only streaming requests. A profile whose manifest entry has `signing: true` also runs the plugin's signing hook on every request.

An installed plugin cannot be used until an owner approves its origins. Approval covers exactly the declared origins. `POST /api/v1/plugins/{plugin_digest}/approve` takes the plugin's current ETag in `If-Match` and lists the origins being approved, and OLP refuses the approval with `plugin_origins_mismatch` unless the list matches the declaration. A digest's manifest never changes, so an approval never widens and a new digest of the same plugin needs its own approval.

## Reviewed plugin index

Every release ships a signed index of reviewed plugins, [`internal/plugins/pluginindex/index.json`](../../internal/plugins/pluginindex/index.json), which the binary verifies at start-up exactly as it verifies the [reference catalog](../develop/catalog.md#canonical-form-and-signature). For each plugin it lists the source repository and directory, and for each reviewed release it records the WebAssembly module's SHA-256 digest, ABI version, size, the origins and profiles its manifest declares, the source commit the digest builds from and when it was reviewed.

The **Plugin index** section directly on the **Plugins** page (`/plugins`) shows the index beside what is installed, with each release marked not installed, installed or approved, and each installed plugin says whether its digest is listed. `GET /api/v1/plugin-index` returns the same information. Browsing installs nothing, so an owner still uploads the module, approves exactly its declared origins and, for unconfined plugins, permits it. OLP ties an uploaded module to a reviewed release only when its digest matches.

The index's plugins build reproducibly. With the same Go toolchain, `go run ./cmd/pluginindex check OUT_DIR` rebuilds each listed plugin and fails unless its digest matches the newest release in the index. Each release runs `check` and publishes the built `.wasm` modules along with `plugin-index.json` and `plugin-index.json.sig` as release assets. To verify a module yourself, rebuild it from the listed commit with `GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 GOFLAGS= go build -mod=readonly -trimpath -buildvcs=false -buildmode=c-shared` and compare its `sha256sum` with the listed digest.

A plugin enters the index through a reviewed pull request that adds its release with `pluginindex entry DIR`, supplying public source, a reproducible build, a justification for every origin and a review of the upstream's terms of use.

## Providers from plugin profiles

Approved plugin profiles appear under kind `plugin` in the provider wizard and management API. `profile_revision` is the installed digest rather than the self-declared version.

```json
{
  "kind": "plugin",
  "auth_mode": "static_credential",
  "profile_id": "reference-chat",
  "profile_revision": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
}
```

Grant profiles use `auth_mode: grant`. Missing installation, approval or profile refuses the draft. Hosting defines the endpoint, credential placement, listing, envelopes and rewrites, failure rules and optional forced streaming, with no built-in vendor, header or discovery defaults applied. Codecs, policy and accounting see plain dialect bodies, and forced streaming aggregates unary replies within response bounds.

Grant-derived base URLs must canonicalize to approved origins, and the placeholder `https://grant.invalid` is never dispatched. Plugin attempts have no vendor list price, so publish a `plugin` price with `provider_id` or they remain unpriced. The first matching declared failure rule wins before built-in classification runs.

Authorization, address and header-only profiles and signing hooks may be strict. Envelopes, rewrites, forced streaming and carried traffic require transformed routes, and strict activation refuses `target_capability`. Changing digest or options requires fresh certification and activation. Mounted connectors support static credentials only and must match the published configuration.

### Options

Profiles declare nonsecret `options_schema`, and providers supply `options.plugin_options`, for example `{"workspace":"team-alpha"}`. Required, undeclared, enum and pattern errors identify the option. Values fill declared hosting fields and accompany signing, enrollment and refresh. Changes invalidate certification, report `plugin_options_changed` and participate in configuration promotion. Never put secrets in plugin options.

### Signing hooks

Hooks run once per upstream request after final body and hosting assembly, never per event, returning credential-redacted headers. A plugin-reported credential error cools its slot, while a runtime, load, timeout or header failure is treated as connectivity rather than credential blame and counts toward circuit health. Processes cache compiled modules and bounded instances, usually prewarming pinned and approved builds, while first use can compile on demand. See the [authoring contract](../develop/plugin-authoring.md#signing-hook).

## Guardrail and route selector plugins

Confined WebAssembly plugins can also extend guardrails and route selection without running provider traffic.

A guardrail plugin sets `guardrail: true` in its manifest and may declare an empty `profiles` list. Unconfined executables cannot declare `guardrail: true`. After an owner installs and approves the module, a `plugin` guardrail definition pins the manifest name in `plugin_id` and the module's SHA-256 digest in `plugin_version`. At runtime release installation, OLP preloads and pins up to 16 guardrail plugin modules so ordinary provider traffic cannot evict them. Calls invoke `abi.MethodGuardrail` (`"guardrail"`) with no HTTP capability and with diagnostics suppressed so inspected content never leaks into logs. See [Guardrails](guardrails.md#external-and-model-evaluation) for definition and policy setup.

A route selector predicate plugin implements `abi.MethodRoutePredicate` (`"route_predicate"`). A route selector consults it through `{when: {plugin: {digest: "<sha256>"}}}` on an approved confined WebAssembly module (`422 route_plugin_unavailable` at route publication if the digest is missing, unapproved or unconfined). The call receives only the admission features already computed by the gateway (`route`, `selector`, `operation`, `input_tokens`, `output_tokens`, `streaming`, `tools`, `modalities`, `structured_output` and `reasoning_effort`) with no HTTP capability and a 250-millisecond timeout. If the plugin returns `match: false` or fails for any reason, that selector does not match and evaluation falls through to the next selector.

## Grant enrollment

Grants hold rotating upstream authorization beneath an immutable credential version. The wizard saves a draft, then enrolls or tests an already-live matching build. Enrollment supports callback or code paste-back, masked pasted secrets and device authorization. Operators must comply with upstream terms.

| Operation | Request |
| --- | --- |
| Start | `POST /api/v1/providers/{provider_id}/grant-enrollments`, draft `If-Match` and optional `slot_id` |
| Continue | `POST /api/v1/providers/{provider_id}/grant-enrollments/{enrollment_id}/continue`, JSON `input` |
| Poll | `POST /api/v1/providers/{provider_id}/grant-enrollments/{enrollment_id}/poll` |
| Cancel | `DELETE /api/v1/providers/{provider_id}/grant-enrollments/{enrollment_id}` |

`configure` permission and the initiating principal are required. Start returns expiry, slot and either an authorization URL or a device verification URL, user code and polling interval. The operator signs in upstream, and continue or poll creates a new credential version, stages it on the selected draft slot and tests it. Activation remains explicit.

Requests drive device polling because control has no enrollment background job. Early polls return status without upstream I/O, and slow-down adds five seconds up to a maximum of five minutes. Completed, denied, expired or cancelled status stops polling. Abandoned polling leaves the enrollment pending, while other failed steps consume it. Paste-back continuation is one-use regardless of outcome, so clear submitted input and start again on failure.

Regular enrollment lasts ten minutes, and device lifetime is capped at thirty minutes. Encrypted session state permits cross-replica continuation. Refusals include `grant_state_mismatch`, expired (410), used or stale (409), failed and plugin errors. Expired secrets purge immediately, and metadata purges an hour later.

Reads show plugin digest, observed principal, grant facts and expiry, never the access or refresh token. Gateways receive access tokens only, while refresh uses a separate secret purpose. Activation refuses slot and profile authentication mismatches.

## Grant refresh

Workers scan every five seconds, refreshing a quarter-lifetime before expiry and no more than ten minutes early. Active configuration takes precedence over draft, and matching plugin digest, profile, options, network and approved origins remain pinned. Unexpired retained resources also keep the exact original revision refreshable. Per-grant advisory locks plus durable in-flight attempt fencing prevent replay of rotating refresh tokens. A missing outcome after two minutes lapses the grant rather than reusing the token.

Refresh advances token generation under the same credential version, principal and facts. Gateways reload changed tokens on authority polling without publication. Credential rejection requests early refresh, and successful refreshed service clears cooldown. Unknown expiry refreshes only on demand.

Failure provably before token exposure retries from thirty seconds to ten minutes. Any ambiguous confined HTTP exposure or started unconfined operation retains the fence until lapse. `invalid_grant`, a changed principal, an absent refresh method or an uninstalled or unapproved build is permanent. A temporarily disabled or missing unconfined runtime remains retryable. Answered refreshes are recorded during shutdown.

Grants unused by configuration or retained resources retire on the next pass (unless in flight), discard their refresh token and emit an audit record without a lapse notification. Revocation ends refresh immediately.

### Lapsed grants

Lapse is terminal. OLP discards the refresh token, advances authority and skips slots with `credential_lapsed` without spending attempts. Probing, binding and validation refuse with 422, while policy-only edits may retain the lapsed version. Reads and the console show status, and enabled lapse rules notify with version, slots and principal without secrets. Re-enroll, validate and activate to restore service.

### Re-enrolling and the observed principal

Re-enrollment stages a new version on `slot_id` like rotation. Every live slot in a revision must observe one principal, and mixed accounts refuse with `principal_mismatch`. Same-account enrollment preserves serving identity, while another account adds `serving_binding_changed` and cannot resume old continuations. Pool separate accounts as separate provider targets.

Grants belong to the exact plugin digest and profile ID. Changing either requires enrollment for each slot, and old-build grants refuse with `credential_mismatch`. Active old revisions keep serving and refreshing until the new build is activated.

## Configuration promotion

Artifacts carry digest, profile and options, never plugin bytes or grant material. Install and approve the same build at the destination (and permit unconfined builds where the tier is enabled) or planning reports a plugin blocker. Static credentials use `secret_bindings`, while grant slots import without credentials and need destination slot enrollment before activation. Other-build grants cannot satisfy the binding.

## Uninstalling

`DELETE /api/v1/plugins/{plugin_digest}` requires `If-Match` and an owner browser session. It removes the stored module and approval, or the unconfined permission, without deleting image executables. Any provider draft, published provider revision, code-mode route revision, `plugin` guardrail revision, route draft selector (`{when: {plugin: {digest}}}`) or published route revision selector pinning the digest blocks deletion with `409 plugin_pinned`, preserving restorability and retained resources. Reinstallation requires approval again. Unreferenced enrolled grants retire and discard refresh tokens without notification, and audit names the owner. Backups include stored modules and manifests rather than image executables.

## Confinement and limits

Plugin calls run on instances of the module within these limits.

| Limit | Value |
| --- | --- |
| Linear memory per instance | 64 MiB |
| Stack per call (parameters, locals and operands of every entered frame) | 8 MiB |
| Instances of a module at once | 4 |
| Time per call, including waiting for and instantiating an instance | 10 seconds (250 milliseconds for route selector predicates) |
| Message from the plugin | 1 MiB |
| Confined HTTP request or response body | 512 KiB |
| Log output per call | 16 KiB, counting 64 bytes per record besides its text, and 2 KiB per message or attribute |

The runtime runs WebAssembly 2.0 without reference types, so a module has at most one table, which never grows. Before compiling a module, OLP refuses one whose table exceeds 65,536 elements, a function with more than 4,096 locals or more than 4,194,304 locals in all, or one importing anything other than functions. Because wazero allocates what a module declares before any limit applies, these bounds keep an upload from exhausting OLP's memory during installation. A module built by the Go toolchain stays far within them.

A call takes an idle instance, or instantiates the module when none is idle, and returns it for later calls. A call that exceeds a limit fails with `plugin_timed_out` or `plugin_failed`, and its instance is discarded so it leaves nothing behind while the OLP process stays unaffected.

Installing reads a manifest once on wazero's interpreter, which is ready soonest, and a worker process refreshes grants on it too. Signing hooks run on modules compiled to machine code, which take longer to prepare but sign faster in repeated use.

The runtime grants a plugin a clock, randomness and logging, and nothing else, with no filesystem, environment or command arguments. Grant enrollment steps and grant refresh are also granted HTTP, which reaches only the plugin's approved origins over the provider's network path (its proxy, trust roots and network credential) and egress policy, with request and response bodies each capped at 512 KiB. Redirects come back to the plugin rather than being followed. Guardrail and route predicate calls receive no HTTP capability. What a plugin logs, including standard output, standard error and reported failure diagnostics, reaches OLP's log through the same per-call output budget with the secret values of the call redacted, such as the refresh token a grant refresh receives, attributed by `plugin_digest` and `plugin_method` (while guardrail calls suppress diagnostics completely).

## Unconfined plugins (experimental)

**Trust an unconfined executable as much as OLP itself.** It runs with OLP's OS privileges, receives secrets and content, can read process-accessible files and can bypass egress. This tier provides no confinement.

### Enabling the tier

Set deployment-only `OLP_UNCONFINED_PLUGIN_DIR` or `--unconfined-plugin-dir` (Helm `config.unconfinedPluginDir`) on control, gateway and worker pods. The absolute directory contains executable regular files with bounded safe names. Release images contain none, so add static Linux binaries to the distroless image.

```dockerfile
FROM ghcr.io/tyk-swe/olp:0.2.0
COPY --chmod=0555 example-plugin /opt/olp/plugins/example-plugin
```

### Reviewing and permitting

An owner session lists `/api/v1/unconfined-plugins` without execution, then posts to `/api/v1/unconfined-plugins/{executable}/review` with CSRF proof. `POST /api/v1/unconfined-plugins/{executable}/permit` takes the reviewed `digest` and `acknowledge_risk: true`, plus one-use purpose-bound `plugin_permit` recent authentication within five minutes. Re-execution verifies the digest, and changes refuse with 409. Each new build needs its own permission. Permitted unconfined plugins share the 64-digest installation ceiling with uploaded WebAssembly plugins. Permitted profiles expose `plugin.unconfined: true`.

### Running

Linux starts a digest-verified sealed-memory copy with an empty environment and no arguments in its own process group. One subprocess handles concurrent calls and persists between them. Calls have ten seconds including startup, and a timeout, exit, invalid reply or output over 1 MiB fails calls and kills the process group, with the next call restarting it. Cancellation still requires a reply within ten seconds, while carried requests otherwise follow their request lifetime. Memory is **not bounded**.

Per-call logs use confined redaction and bounds. Unattributed stderr has one lifetime 16 KiB allowance and redacts current and recent (ten-second) call secrets.

### Carrying traffic

`carries_traffic` hands finished HTTP and SSE requests and response streams to the plugin, including probes, discovery and certification. Endpoint shape is checked, while network-path and egress address enforcement do not govern its sends. WebSockets and realtime dialects are unsupported, and all such profiles require transformed routes.

Only a proved-not-sent failure can fail over. Ambiguous dispatch or server failure returns `502 ambiguous_upstream_result` without retry and counts as a circuit failure, while explicit rejection uses ordinary classification. Cancellation propagates upstream. Forwarding buffers at most 8 MiB ahead of the reader, then fails slow delivery.

### Without the tier

List paths return `unconfined_plugins_disabled`, profiles disappear, activation and pinning refuse, promotion blocks, serving skips targets and refresh retries with backoff. Stored permission survives, and re-enabling the tier restores it.

## Permissions and audit

| Operation | Who |
| --- | --- |
| List and read plugins | Any role, and management tokens with `read` |
| Install, approve, uninstall | An owner with a user session |
| List, review and permit unconfined plugins | An owner with a user session, with permitting also requiring recent authentication |
| Create and change plugin providers | The `configure` scope, as for any provider |
| Enroll grants | The `configure` scope |

Audit records `plugin.install`, `plugin.approve`, `plugin.permit` and `plugin.uninstall` with the owner as actor and the digest as resource. A repeated upload of an installed digest records nothing. Audit records `provider.grant.enroll` for every start and continuation that reaches the plugin, and once for each device authorization that ends. A successful device authorization records the new credential version as resource, while a failure (including a failed start or a denied or expired device authorization, whether expired upstream or by OLP's own deadline) records the provider. Audit records `provider.grant.lapse` when a grant [lapses](#lapsed-grants), and `provider.grant.retire` when a worker [retires](#grant-refresh) an unused grant or an owner [uninstalls](#uninstalling) the plugin that enrolled it. Audit never records what was pasted back or obtained.

Modules and manifests are stored in PostgreSQL in `olp.plugins`, so database [backups](../operate/operations.md#backup-and-restore) include them. Unconfined plugin executables are not stored in PostgreSQL because they belong to the deployment's container image.
