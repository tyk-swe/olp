# Provider plugins

A provider plugin is an operator-installed extension that supplies a provider
profile's authentication and hosting around a built-in dialect. It never
defines a dialect: codecs and interaction contracts stay first-party. Plugins
are confined: OLP runs their code on [wazero](https://wazero.io) and grants
every capability they have, unless a deployment enables the experimental
[unconfined tier](#unconfined-plugins-experimental).
The [authoring guide](plugin-authoring.md) covers writing one.

This page covers installing, reviewing and approving plugins, creating
providers from their profiles, enrolling grants, promoting plugin providers
between installations, permitting unconfined plugins, and uninstalling them.

## Upstream terms of use

Some upstreams, including Anthropic and Google, prohibit third-party use of
their subscription sign-in. OLP stays neutral: it ships only the SDK and a
reference plugin against a fictional upstream, and **whether a plugin may be
used with an upstream is the operator's responsibility** under that upstream's
terms of use.

## Installing

An owner installs a plugin from the console's **Plugins** page, or with
`POST /api/v1/plugins` carrying the module as an `application/wasm` body. A
module is at most 32 MiB, and an installation holds at most 64 plugin digests.
Installation needs an owner signed in with a user session and installation-wide
access, like management-token administration. The same requirement applies to
approval, uninstalling and permitting unconfined plugins; management tokens
cannot perform these operations.

OLP stores the module by the SHA-256 digest of its bytes, which identifies the
plugin from then on. Before storing it, OLP instantiates the module within the
plugin limits and reads the manifest it declares. It refuses the upload with a
`422` problem naming the reason:

| Code | Reason |
| --- | --- |
| `plugin_module_invalid` | Not WebAssembly, not a plugin, a WASI command instead of a reactor, it imports something OLP does not provide, or it declares more than the runtime allows (see [Confinement and limits](#confinement-and-limits)). |
| `plugin_abi_unsupported` | Built for another plugin ABI version. During 0.x the ABI carries no compatibility promise; rebuild the plugin with the SDK of this OLP version. |
| `plugin_manifest_invalid` | The manifest is invalid or declares something this OLP does not understand. The problem's `errors` names the field, such as `manifest.origins[0]` or `manifest.profiles[0].hosting.headers.Authorization`. |
| `plugin_dialect_unknown` | A declared profile names a dialect plugin profiles can't serve, such as `manifest.profiles[0].dialect`. |
| `plugin_timed_out`, `plugin_failed` | The module exceeded its time limit, or trapped, exited or exhausted its memory, while starting or reporting its manifest. |

Uploading a digest that is already installed changes nothing and returns it.
Several digests of the same plugin (the manifest `name`) may be installed side by
side, so a new build can be installed and reviewed before anything moves to it.

## Reviewing and approving

`GET /api/v1/plugins` and the Plugins page show each installed digest with what
its manifest declares:

- **profiles**, each naming the built-in dialect it serves, whether providers
  using it authenticate with a static credential or a
  [grant](#grant-enrollment), the options its providers set, and its hosting
  adaptation: the address its requests go to, at one of the plugin's origins,
  the headers and query parameters it declares, any model listing and failure
  classification, any envelope and rewrites of the dialect's bodies, and
  whether its upstream serves only streaming requests. A profile whose
  manifest entry has `signing: true` also runs the plugin's signing hook on
  every request;
- **origins**, the only `scheme://host[:port]` origins the plugin may ever
  reach.

An installed plugin can't be used until an owner approves its origins. Approval
covers exactly the declared origins: `POST /api/v1/plugins/{digest}/approve`
takes the plugin's current ETag in `If-Match` and lists the origins being
approved, and OLP refuses the approval with `plugin_origins_mismatch` unless the
list matches the declaration. A digest's manifest never changes, so an approval
never widens; a new digest of the same plugin needs its own approval.

## Reviewed plugin index

Every release ships a signed index of reviewed plugins,
[`internal/pluginindex/index.json`](../internal/pluginindex/index.json), which
the binary verifies at start-up exactly as it verifies the
[reference catalog](catalog.md#canonical-form-and-signature). For each plugin it
lists the source repository and directory, and for each reviewed release the
WebAssembly module's SHA-256 digest, ABI version, size, the origins and
profiles its manifest declares, the source commit the digest builds from and
when it was reviewed.

**Plugins → Plugin index** shows the index beside what is installed, with
each release marked not installed, installed or approved, and each installed
plugin says whether its digest is listed. `GET /api/v1/plugin-index` returns
the same. Browsing installs nothing: an owner still uploads the module,
approves exactly its declared origins and, for unconfined plugins, permits it.
OLP ties an uploaded module to a reviewed release only when its digest
matches.

The index's plugins build reproducibly: with the same Go toolchain,
`go run ./internal/pluginindex/cmd/pluginindex check OUT_DIR` rebuilds each
listed plugin and fails unless its digest is the one listed. Each release runs
it and publishes the modules it built, `plugin-index.json` and
`plugin-index.json.sig` as release assets. To verify a module yourself, rebuild
it from the listed commit with
`GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildvcs=false -buildmode=c-shared`
and compare its `sha256sum` with the listed digest.

A plugin enters the index through a reviewed pull request that adds its
release with `pluginindex entry DIR`: public source, a reproducible build, a
justification for every origin, and a review of the upstream's terms of use.

## Providers from plugin profiles

Approved plugin profiles appear under kind `plugin` in the provider wizard/API.
`profile_revision` is the installed digest, not the self-declared version:

```json
{
  "kind": "plugin",
  "auth_mode": "static_credential",
  "profile_id": "reference-chat",
  "profile_revision": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
}
```

Grant profiles use `auth_mode: grant`. Missing installation, approval or profile
refuses the draft. Hosting defines endpoint, credential placement, listing,
envelopes/rewrites, failure rules and optional forced streaming; no built-in
vendor/header/discovery defaults apply. Codecs/policy/accounting see plain dialect
bodies; forced streaming aggregates unary replies within response bounds.

Grant-derived base URLs must canonicalize to approved origins; placeholder
`https://grant.invalid` is never dispatched. Plugin attempts have no vendor list
price: publish a `plugin` price with `provider_id`, otherwise they remain unpriced.
First matching declared failure rule wins, then built-in classification.

Authorization/address/header-only profiles and signing hooks may be strict.
Envelopes/rewrites/forced streaming/carried traffic require transformed routes;
strict activation refuses `target_capability`. Changing digest/options requires
fresh certification/activation. Mounted connectors support static credentials
only and must match the published configuration.

### Options

Profiles declare nonsecret `options_schema`; providers supply
`options.plugin_options`, for example `{"workspace":"acme"}`. Required/undeclared/
enum/pattern errors identify the option. Values fill declared hosting fields and
accompany signing/enrollment/refresh. Changes invalidate certification, report
`plugin_options_changed` and participate in promotion; never put secrets here.

### Signing hooks

Hooks run once per upstream request after final body/hosting, not per event,
returning credential-redacted headers. A plugin-reported credential error cools
its slot; runtime/load/timeout/header failure is connectivity, not credential
blame, and counts toward circuit health. Processes cache compiled modules and
bounded instances, usually prewarming pinned/approved builds; first use can compile.
See the [authoring contract](plugin-authoring.md#signing-hook).

## Grant enrollment

Grants hold rotating upstream authorization beneath an immutable credential
version. The wizard saves a draft, then enrolls or tests an already-live matching
build. Enrollment supports callback/code paste-back, masked pasted secrets, and
device authorization. Operators must comply with upstream terms.

| Operation | Request |
| --- | --- |
| Start | `POST /api/v1/providers/{id}/grant-enrollments`, draft If-Match; optional `slot_id` |
| Continue | `POST .../grant-enrollments/{enrollment_id}/continue`, JSON `input` |
| Poll | `POST .../grant-enrollments/{enrollment_id}/poll` |
| Cancel | `DELETE .../grant-enrollments/{enrollment_id}` |

`configure` permission and the initiating principal are required. Start returns
expiry/slot and authorization URL or device verification URL/user code/interval.
The operator signs in upstream; continue/poll creates a new credential version,
stages it on the selected draft slot, then tests. Activation remains explicit.

Requests drive device polling; control has no enrollment background job. Early
polls return status without upstream I/O; slow-down adds five seconds, maximum
five minutes. Completed/denied/expired/cancelled stops polling. Abandoned polling
leaves pending; other failed steps consume the enrollment. Paste-back continuation
is one-use regardless of outcome: clear submitted input and start again on failure.

Regular enrollment lasts ten minutes; device lifetime is capped at thirty.
Encrypted session state permits cross-replica continuation. Refusals include
`grant_state_mismatch`, expired (410), used/stale (409), failed and plugin errors.
Expired secrets purge immediately; metadata an hour later.

Reads show plugin digest, observed principal, grant facts and expiry, never access/
refresh token. Gateways receive access tokens only; refresh uses a separate secret
purpose. Activation refuses slot/profile authentication mismatch.

## Grant refresh

Workers scan every five seconds, refreshing a quarter-lifetime before expiry,
no more than ten minutes early. Active configuration takes precedence over draft;
matching plugin digest/profile/options/network and approved origins remain pinned.
Unexpired retained resources also keep the exact original revision refreshable.
Per-grant advisory locks plus durable in-flight attempt fencing prevent replay of
rotating refresh tokens. Missing outcome after two minutes lapses, never reuses.

Refresh advances token generation under the same credential version/principal/
facts. Gateways reload changed tokens on authority polling without publication.
Credential rejection requests early refresh; successful refreshed service clears
cooldown. Unknown expiry refreshes only on demand.

Failure provably before token exposure retries from thirty seconds to ten minutes.
Any ambiguous confined HTTP exposure or started unconfined operation retains the
fence until lapse. `invalid_grant`, changed principal, absent refresh method or
uninstalled/unapproved build is permanent. Temporarily disabled/missing unconfined
runtime remains retryable. Answered refreshes are recorded during shutdown.

Grants unused by configuration or retained resources retire on the next pass
(unless in flight), discard refresh token
and emit audit but no lapse notification. Revocation ends refresh immediately.

### Lapsed grants

Lapse is terminal: discard refresh token, advance authority, skip slots with
`credential_lapsed` without spending attempts. Probing/binding/validation refuses
422; policy-only edits may retain the lapsed version. Reads/console show status;
enabled lapse rules notify version/slots/principal without secrets. Re-enroll,
validate and activate to restore service.

### Re-enrolling and the observed principal

Re-enrollment stages a new version on `slot_id`, like rotation. Every live slot
in a revision must observe one principal; mixed accounts refuse
`principal_mismatch`. Same-account enrollment preserves serving identity;
another account adds `serving_binding_changed` and cannot resume old continuations.
Pool separate accounts as separate provider targets.

Grants belong to exact plugin digest and profile ID. Changing either requires
enrollment for each slot;
old-build grants refuse `credential_mismatch`. Active old revisions keep serving/
refreshing until the new build is activated.

## Configuration promotion

Artifacts carry digest/profile/options, never plugin bytes or grant material.
Install/approve the same build at destination (permit unconfined builds where the
tier is enabled) or planning reports a plugin blocker. Static credentials use
`secret_bindings`; grant slots import without credentials and need destination
slot enrollment before activation. Other-build grants cannot satisfy the binding.

## Uninstalling

`DELETE /api/v1/plugins/{digest}` requires If-Match and an owner session. It
removes module/approval or unconfined permission, not image executables.
Any draft or immutable published revision pin blocks deletion (`plugin_pinned`),
preserving restorability and retained resources. Reinstallation needs approval.
Unreferenced enrolled grants retire and discard refresh tokens without notification;
audit names the owner. Backups include modules/manifests, not image executables.

## Confinement and limits

Plugin calls run on instances of the module within these limits:

| Limit | Value |
| --- | --- |
| Linear memory per instance | 64 MiB |
| Stack per call: parameters, locals and operands of every frame entered | 8 MiB |
| Instances of a module at once | 4 |
| Time per call, including waiting for and instantiating an instance | 10 seconds |
| Message from the plugin | 1 MiB |
| Log output per call | 16 KiB, counting 64 bytes per record besides its text; 2 KiB per message or attribute |

The runtime runs WebAssembly 2.0 without reference types, so a module has at
most one table, which never grows. Before compiling a module, OLP refuses one
whose table exceeds 65,536 elements, a function with more than 4,096 locals or
more than 4,194,304 locals in all, or one importing anything but functions:
wazero allocates what a module declares before any limit applies, so these
bounds keep an upload from exhausting OLP's memory while it is installed. A
module that the Go toolchain builds stays far within them.

A call takes an idle instance, or instantiates the module when none is idle,
and returns it for later calls. A call that exceeds a limit fails with
`plugin_timed_out` or `plugin_failed`, and its instance is discarded, so it
leaves nothing behind; the OLP process is unaffected.

Installing reads a manifest once, on wazero's interpreter, which is ready
soonest, and a worker process refreshes grants on it too. Signing hooks run on
modules compiled to machine code, which take longer to prepare but then sign
about ten times faster; `BenchmarkSign` in `internal/plugins` measures both.

The runtime grants a plugin a clock, randomness and logging, and nothing else:
no filesystem, environment or arguments. Grant enrollment steps and grant
refresh are also granted HTTP, which reaches only the plugin's approved
origins, over the provider's network path (its proxy, trust roots and network
credential) and the egress policy; a redirect comes back to the plugin rather
than being followed. What a plugin logs, including its standard output and
standard error and reported failure diagnostics, reaches OLP's log through the
same per-call output budget, with the secret values of the call redacted,
such as the refresh token a grant refresh receives, attributed by
`plugin_digest` and `plugin_method`.

## Unconfined plugins (experimental)

**Trust an unconfined executable as much as OLP itself.** It runs with OLP's OS
privileges, receives secrets/content, can read process-accessible files and can
bypass egress. This tier is not confinement.

### Enabling the tier

Set deployment-only `OLP_UNCONFINED_PLUGIN_DIR`/`--unconfined-plugin-dir`
(Helm `config.unconfinedPluginDir`) on control, gateway and worker. The absolute
directory contains executable regular files with bounded safe names. Release
images contain none; add static Linux binaries to the distroless image:

```dockerfile
FROM ghcr.io/tyk-swe/olp:0.1.0
COPY --chmod=0555 acme /opt/olp/plugins/acme
```

### Reviewing and permitting

An owner session lists `/api/v1/unconfined-plugins` without execution, then POSTs
`/{executable}/review` with CSRF. Permit POST takes reviewed `digest` and
`acknowledge_risk: true`, plus one-use purpose-bound `plugin_permit` authentication
within five minutes. Re-execution verifies digest; changes refuse 409. Each new
build needs permission. Permitted profiles expose `plugin.unconfined: true`.

### Running

Linux starts a digest-verified sealed-memory copy, empty environment/no arguments,
in its own process group. One subprocess handles concurrent calls and persists
between them. Calls have ten seconds including startup; timeout/exit/invalid or
>1 MiB output fails calls and kills the group. Next call restarts. Cancellation
still requires a reply within ten seconds; carried requests otherwise follow their
request lifetime. Memory is **not bounded**.

Per-call logs use confined redaction/bounds. Unattributed stderr has one lifetime
16 KiB allowance and redacts current/recent (ten-second) call secrets.

### Carrying traffic

`carries_traffic` hands finished HTTP/SSE requests and response streams to the
plugin, including probes/discovery/certification. Endpoint shape is checked, but
network-path/egress address enforcement does not govern its sends. No WebSockets/
realtime dialects. All such profiles require transformed routes.

Only proved-not-sent failure can fail over. Ambiguous dispatch/server failure
returns `502 ambiguous_upstream_result` without retry and counts circuit failure;
explicit rejection uses ordinary classification. Cancellation propagates upstream.
Forwarding buffers at most 8 MiB ahead of the reader, then fails slow delivery.

### Without the tier

List paths return `unconfined_plugins_disabled`; profiles disappear, activation/
pinning refuse, promotion blocks, serving skips targets and refresh retries with
backoff. Stored permission survives; re-enabling restores it.

## Permissions and audit

| Operation | Who |
| --- | --- |
| List and read plugins | Any role, and management tokens with `read` |
| Install, approve, uninstall | An owner with a user session |
| List, review and permit unconfined plugins | An owner with a user session; permitting also takes a recent authentication |
| Create and change plugin providers | The `configure` scope, as for any provider |
| Enroll grants | The `configure` scope |

Audit records `plugin.install`, `plugin.approve`, `plugin.permit` and
`plugin.uninstall` with the owner as actor and the digest as resource. A
repeated upload of an installed digest records nothing. It records
`provider.grant.enroll` for every start and continuation that reaches the
plugin, and once for each device authorization that ends: a success with the
new credential version as resource, a failure, including a start the plugin
failed and a device authorization denied or expired, whether the upstream or
OLP's own deadline expired it, with the provider. It records
`provider.grant.lapse` when a grant [lapses](#lapsed-grants), and
`provider.grant.retire` when a worker [retires](#grant-refresh) a grant nothing
uses, or an owner [uninstalls](#uninstalling) the plugin that enrolled it.
Audit never records what was pasted back or obtained.

Modules and manifests are stored in PostgreSQL in `olp.plugins`, so database
[backups](operations.md#backup-and-restore) include them. Unconfined plugins'
executables are not: they belong to the deployment's image.
