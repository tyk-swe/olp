# Upstream MCP server registration

Register project-owned upstream servers through `/api/v1/mcp-servers`. This API
is separate from the [generated management MCP endpoint](../operate/automation.md#management-mcp).
Reads require management `read` and project visibility; mutations require
`configure` and project-change authority. Installation-wide configuration
promotion retains those checks. Up to 128 active registrations are permitted.

Create requires a project UUID, name, an explicit `streamable_http` or `sse`
transport, endpoint and enabled flag. PUT replaces editable metadata with an
observed If-Match; project and transport are immutable. Every mutation requires Idempotency-Key.
On PUT, omitting `unit_price`, `tool_prices` or a rate and concurrency limit
keeps the current value, `unit_price: null` removes the price, and a null limit
resets that limit to its default.
A successful create or update negotiates an upstream MCP session, sends initialized
notification and discovers tools before committing. Certification never invokes
tools. It runs outside database transactions; authority and preconditions are
checked again before commit. Invalid or unavailable upstreams cannot partially
replace a registration. Exact replay returns the original result without
repeating discovery.

Optional write-only `credential` supplies static upstream bearer material.
Omission retains it when the endpoint is unchanged; null removes it. Changing
the endpoint requires an explicit replacement credential or removal, including
through configuration promotion. The installation key ring seals it under
`mcp_credential`; responses expose only `has_credential`. Certification refuses
metadata reflecting that bearer value, including escaped schema strings. Endpoint
validation and every connection use provider egress policy, with redirects
refused. Workload identity and wrapped master rings operate through the ordinary
installation secret lifecycle. No credential is embedded in an endpoint URL.

Streamable HTTP certification supports MCP versions 2025-03-26, 2025-06-18 and
2025-11-25, JSON or SSE responses, negotiated session headers and bounded session
cleanup. Explicit `sse` registration uses legacy HTTP+SSE at exactly 2024-11-05.
Its initial GET advertises a POST endpoint on the same origin. Session query
parameters are allowed, but every destination still passes egress validation
before credentials attach and redirects are refused. Modern certification never
falls back to legacy. Discovery admits at most 16 pages, 128 uniquely named
tools and one MiB of metadata. Individual descriptions/schemas and JSON depth/node counts are
bounded. Input schemas must be object schemas and compile without remote schema
retrieval; missing lists, duplicate names, cursor cycles and invalid schemas
refuse certification. Tool order and schema member order are canonicalized for
SHA-256 catalog digests.

GET returns the pinned certified catalog and registration ETag. Collection
responses contain summaries without loading full schemas. Ordinary reads never
adopt live upstream changes. Explicit conditional updates approve new immutable
revisions. `/api/v1/mcp-servers/{server_id}/revisions/{revision_id}` reads retained
catalogs inside the same project boundary. Immutable database revisions reject
changes. DELETE retires/disables a registration and removes its sealed bearer
material; identity, certified revisions and audit history remain. Those retained
project references prevent deleting their project boundary.

Configuration export includes logical project/name, transport, endpoint, enabled
state, reviewed catalog digest, the unit and tool prices, the rate and
concurrency limits, and an optional deterministic destination binding.
It excludes local IDs, credential bytes and revision history. Plan identifies
reuse/create/replace and required bindings. Apply certifies changed registrations
outside its transaction and requires the reviewed digest before any document
changes commit. Unchanged destination registrations reuse their pinned catalog,
credential and ETag without consulting potentially changed upstream metadata.
Signing/bearer bindings are supplied separately and never saved in CLI plan
files. External secret-store bindings apply to provider credentials; MCP static
bearers use sealed destination bindings.

A completed configuration apply is replayed after current authorization and local
request normalization, before upstream discovery. Later catalog drift or endpoint
unavailability cannot make that completed request fail or overwrite a later
registration. New applies release mutation locks for discovery and repeat
reauthorization, replay lookup and destination preconditions before commit.

## Discovering capabilities and simulating a call

`GET /v1/mcp/tools` uses the calling key's toolset allowlist and current
publication authority. Each tool includes its pinned unit price, `priced`,
`free` or `unpriced` state, whether its accounting owners enforce a budget, and
effective server and key rate/concurrency limits. Key limits are the tighter
of the registered server policy and key policy. The response also states the
request/result byte bounds and call timeout used by execution.

`POST /v1/mcp/tools/simulate` accepts `toolset_id`, `name` and `arguments` in the
same form as the JSON call endpoint, without requiring a `call_id`. It checks
current authority, the published tool definition, argument schema and whether
an enforced budget requires a missing price. It returns `configuration_valid`,
the same tool capability record and the checks that remain for execution.
Simulation does not contact an upstream, run an evaluator, reserve accounting,
consume a rate allowance or dispatch a tool. Live catalog, credentials,
guardrails, available budget and concurrent admission are checked by an actual
call, so a valid simulation is not a reservation or a guarantee of admission.

## Inspecting execution and stopping new calls

The console's **Tool calls** page at `/tool-calls` shows project-filtered
attempts, outcomes, prices and settlement. Observed cost and charge stay
separate from unresolved reserved amounts. Expand an attempt to inspect its
dispatch identity, pinned revisions, content digests and bounded guardrail
decisions. Arguments, result content and credentials are not stored in these
records or returned to this page. Its management API is
`GET /api/v1/mcp-tool-calls`, with optional `project_id`, `api_key_id` and
`toolset_id` filters; access remains constrained to authorized projects.

Disable or retire the registered server, retire the published toolset, revoke
the calling key or remove its toolset allowlist entry to stop new calls at the
shared authority freshness boundary. Existing attempt records and settlement
history remain readable to authorized project operators. A pending unknown
outcome keeps its reservation until the existing resolution operation records
evidence, even after execution is disabled.

## Calling published tools over legacy HTTP+SSE

Legacy clients explicitly open `GET /mcp/sse?toolset_id=<id>` with the same tools
credential used for JSON calls. The stream first emits an `endpoint` event with
the session-specific `/mcp/messages?session_id=<id>` POST path. Send JSON-RPC
messages to that path using the same credential. POST accepts messages with HTTP
202 and responses arrive as `message` events on the open stream. Initialization
must request MCP 2024-11-05. The modern `/mcp` endpoint retains its own protocol
selection and neither endpoint adopts a session from the other transport.

Consumer and upstream transport choices are independent. Both consumer transports
resolve the same registered server, pinned catalog and toolset, then use the
same authority, schema, guardrail, price, limit, call-identity and durable-debit
checks. A legacy upstream must be explicitly registered with `transport` set to
`sse`. Changing its transport requires a new registration. The publication
console displays the registered transport and certified protocol alongside the
catalog.

Legacy GET and subsequent POST requests need connection affinity to the same
gateway process. Shared session metadata does not relay tool bodies between
replicas. A POST reaching a process without its live stream refuses before
dispatch. Closing the stream deletes its session, and reconnecting requires a
new session. Sessions retain the same fifteen-minute idle timeout and per-key
cap as Streamable HTTP. Stalled upstream handshakes, slow response delivery and
calls are bounded by the existing operation timeout.

A dropped stream does not undo an admitted call or make it safe to repeat an
effect. The existing durable attempt, call identifier and outcome-resolution
rules still apply. Disabling the registered server stops new calls while
retained attempt and settlement history remains readable.

## Calling published tools over Streamable HTTP

Callers with the `tools` scope and a toolset allowlist can also reach the same
published tools as the JSON tool endpoints at `POST /mcp`, using Streamable HTTP
at MCP 2025-03-26, 2025-06-18 or 2025-11-25. A session binds one toolset, chosen
with the `toolset_id` query parameter when the key may use more than one, and
stays bound to the installation, project, principal and pinned toolset and
server revisions it initialized against. Sessions expire after 15 idle minutes
and each key may hold at most 64 at once. Verified end-user identity also
binds session ownership and retained call results. End users sharing a workload
principal share its session cap. Send the returned `Mcp-Session-Id`
header, with the negotiated `MCP-Protocol-Version`, on later requests, and close
the session with `DELETE /mcp`. Calls list and run only the pinned tools, over
the same authorized and accounted path as `GET /v1/mcp/tools` and
`POST /v1/mcp/tools/call`. Sampling, elicitation, roots and arbitrary resource
or prompt access are refused explicitly, and no standalone `GET /mcp` event
stream is offered. Session records live in the shared Valkey store that every
replica already uses for shared limits, and they hold binding identifiers,
pinned revisions, timestamps and digests only, never message bodies,
credentials or results. Idle expiry runs on store TTLs and the per key cap
counts against a per principal index in that store, so both hold across every
replica instead of per process, and a session answers only to the principal
that opened it. Where no shared store is configured, session creation refuses
as unavailable while the JSON tool endpoints keep serving.

Opening a session certifies the live catalog of every server in the pinned
toolset against the digest recorded in its pinned server revision. If a
catalog cannot be verified at all, the session is not opened. Each JSON tool
call and each session `tools/call` dispatch certifies its server the same way
before anything is dispatched, because upstream transports dial per call and
no upstream session pool exists yet. Certification therefore runs per dispatch
until a session pool exists. Certification only lists tools and never invokes
one. When the live definitions differ from the pinned ones, calls to that
server refuse with a `tool_definition_changed` error and nothing is dispatched
or settled, and a server found drifted in a session stays blocked for that
session. The same refusal applies per server when a server has since been
certified into a newer revision than the one the toolset pins, and healthy
servers in the same toolset stay callable. A pinned schema that cannot compile
refuses with `tool_schema_invalid` instead, since nothing about the upstream
definition changed. The published toolset and the retained server revision
stay readable through the management API while calls are blocked, so the
change can be reviewed and a fresh revision published on purpose. Publishing a
reviewed revision that pins the current catalog unblocks new JSON calls and
newly opened sessions, and it leaves toolsets pinned to other servers alone. A
session opened before the re-publish stays bound to the revision it started
with.

Listings surface drift instead of quietly dropping tools. A JSON listing that
selects one toolset with `toolset_id` refuses with `tool_definition_changed`
when any of its pins has drifted from the serving snapshot, and a session
`tools/list` does the same for its bound toolset. The unfiltered JSON listing
may omit toolsets and tools it cannot resolve, so use the management API reads
as the review surface for a blocked definition.

A `notifications/tools/list_changed` notification seen on an upstream response
stream invalidates that server certification for the session, and the next
call to the server certifies again before it can dispatch, as every dispatch
now does. Certification compares tool definitions at the moment it runs, and
it does not attest how the upstream implementation behaves between checks.

Every tool call needs a caller call identifier. Send it as `call_id` on the
JSON endpoint and as `olp.call_id` inside `params._meta` on a session
`tools/call`. The caller call identifier is a caller chosen string of 1 to
256 bytes with no control characters and at least one non-space character.
It is not the attempt identifier. The `attempt_id` in responses and the
`{call_id}` path value on the management tool call endpoints name the
gateway management record for one dispatched attempt, while the caller call
identifier is the retry key the caller picks and reuses. The identifier
stays bound to the calling key, the pinned toolset revision, the tool and a
keyed digest of the exact argument bytes for 24 hours, and only that digest
is stored. Argument binding is byte exact, so the same JSON value written
with different key order or spacing counts as a different call. Repeating
the same call with the same identifier never runs the tool again. A repeat
returns the retained result while it is still held in gateway memory, and
after that it answers `result_not_retained` with the original attempt and
dispatch identifiers and the retained status. While the first call is still
running, in either the prepared window before dispatch or the dispatched
window before its outcome is recorded, an exact repeat answers
`call_in_progress` with that same retained identity. Reusing the identifier
with different arguments, a different tool or a different pinned revision
refuses with `call_id_conflict`. When a dispatched call cannot be confirmed
either way, it stays `outcome_unknown` and its debit stays held across
restarts, with no automatic redispatch and no expiry release. A project
manager closes a held debit exactly once through
`POST /api/v1/mcp-tool-calls/{call_id}/resolve`, either as `settled` from
later observed evidence or as `not_accepted` from an audited finding, each
carrying the SHA-256 digest of the reviewed evidence. Resolving updates the
tool call record in the same step, so later repeats report the resolved
status instead of unknown. Repeating a resolve with the same outcome and
evidence returns the existing record. Expired identifier bindings are swept
after 24 hours, and the digest stays on each tool call record so older
records keep naming the call they came from after an identifier is reused.
Tool call records report the debit state with its bounds, observed amounts
and remaining unresolved amounts as separate fields, so an unresolved bound
never reads as observed provider cost.

Registrations also carry tool call pricing and pace. Prices are pinned in the
same immutable certified revision as the catalog, while the rate and
concurrency limits below are mutable server settings. `unit_price` prices every
dispatched call to the server and `tool_prices` overrides it per tool name,
both in the installation pricing currency with no conversion. An explicit
`0` means the call is known free. A missing price makes the call unpriced.
An unpriced call refuses with `tool_unpriced` before dispatch while any key,
budget group, aggregate or end-user ceiling applies, because no finite bound
can be reserved for it, and the refused call still writes a content-free
`refused_unpriced` tool call record. Otherwise it dispatches and records an
unpriced fact that reporting keeps distinct from a free call, including an
unpriced attempt count against every recorded owner. The provisional debit
reserves the pinned price as both its cost and charge bound before dispatch
and settles at that price for every dispatched call, including calls the
upstream answers with an error. Tool calls have one operator unit price and
no separate rate card, so the settled cost and the settled consumer charge are
that same unit price under either budget basis. That equality is the tool
pricing contract, not a missing conversion. Discovery and certification never
bill. The debit records the key owner plus every applicable budget group,
aggregate and end-user owner, so each capped boundary enforces the same bound,
and settlement adds the priced amount into every recorded owner's spend
windows in the same step. A `settled` resolve now checks its observed amounts
against those held bounds and refuses amounts past them. Tool call records
carry the pinned `unit_price`, a `pricing_state` of `priced`, `free` or
`unpriced`, and the `currency` their bounds use.

`calls_per_minute` and `max_concurrency` bound the server across all keys,
and `per_key_calls_per_minute` and `per_key_max_concurrency` bound each key
against the server. Both pairs default to 60 calls per minute and 8
concurrent calls and stay inside installation limits of 60000 calls per
minute and 1024 concurrent calls. The effective per-key limit is the tighter
of the server per-key value and the key's own request or concurrency limit,
so a key limit can only tighten the per-key pair and never loosen it. Calls
past either limit refuse with `tool_rate_limited` before anything is reserved
or dispatched, a call that never dispatches gives its rate allowance back,
and only a dispatched call consumes it. Pricing and limits round trip through
the management API, the console pricing form on the agent publications
page, and configuration export, plan and apply, without ever exposing the
sealed credential. Saving new pricing certifies a new server revision, so
toolsets keep calling at the price pinned in the revision they already
reviewed until a fresh toolset revision pins the new one. Saving only new
limits updates the server in place, certifies no new revision, and never
drifts a pinned toolset, because serving reads the current server limits.

Revoking a key, removing a toolset from its allowlist, retiring a publication
and disabling or retiring a server all refuse new calls and end live session
eligibility once the gateway authority view refreshes, which is the same
freshness boundary the rest of the gateway enforces. A session that loses its
authority this way is refused and closed on next use, while a temporary
authority outage only refuses requests until authority is readable again.

## Personal grants

An operator can configure personal OAuth on a registered server from **Personal tool grants** in the console. The configuration declares the client ID, audience, scope set, authorization endpoint, token endpoint and optional device authorization endpoint. Each destination must pass the installation's egress policy. The configuration belongs to the certified server revision and round trips through portable configuration. Personal grant records and their secrets never enter that export.

A server with personal OAuth requires an unexpired, unrevoked grant for the exact server, endpoint, client, audience, scopes, project and individual. A missing or mismatched grant refuses execution with `user_grant_required`. The registered static credential is never used as a fallback. Scope order and duplicate scopes do not change the individual grant binding.

A project manager can start key-bound enrollment with `POST /api/v1/mcp-servers/{server_id}/grant-enrollments` and an `api_key_id`. Only that signed-in member can claim the returned link. For a project end user, an authenticated workload caller with a verified end-user claim can request a link with `POST /v1/mcp/servers/{server_id}/grant-enrollments` and a `toolset_id`. The server must belong to that caller's authorized toolset. A header label cannot create this link or select another individual's grant.

Enrollment links expire after ten minutes and can be claimed once. The console removes the secret from the address bar when it opens the link, keeps it only in transient page state until submission to the authenticated claim endpoint, and clears it after a successful claim. The console shows pending, active, expired, denied, failed and revoked standing without access or refresh tokens. A grant holder or project manager can revoke an active grant.

After claiming an enrollment, choose browser authorization or device authorization. Browser authorization uses PKCE S256 and a one-use state value. The callback returns to Personal tool grants, removes the code and state from the address bar, and lets the signed-in member complete the exchange. Device authorization shows the provider verification URL and user code. Check approval after the displayed polling interval. Pending and slow-down responses preserve the enrollment, while denial and expiry appear in standing.

Access tokens, refresh tokens, PKCE verifiers and device codes are sealed separately from public metadata. An expired access token is refreshed once per grant under a distributed lock before dispatch. Token HTTP requests run outside database mutation transactions. Concurrent callers reuse the completed refresh, and an abandoned or failed rotating-token attempt refuses further use until re-enrollment. No device or token HTTP exchange follows redirects. Per-call upstream connections and consumer sessions remain bound to their individual.

The PKCE, device, refresh and isolation behavior is exercised against the repository OAuth fixture. Live identity-provider qualification is separate.

## Delegated tool identity

A server using personal OAuth can also require a delegated bearer for downstream calls. In Personal tool grants, configure the server's HTTPS issuer and intended audience, then supply a PKCS8 Ed25519 private key. The installation seals this key separately from grant tokens. The API accepts `delegation` metadata and a write-only `delegation_private_key`; later updates can omit the key to preserve it. Changing delegation creates a new server revision that must be published in the toolset.

The shared consumer path first checks live publication authority and the caller's exact personal grant. It then signs a short-lived JWT for the configured audience. Its subject identifies the authenticated key, verified workload end user or delegated agent, with server and project claims. Delegated agents use `agent:<agent UUID>`, the signed token expires no later than their delegation, and the personal grant remains bound to the consenting key. Header labels cannot choose another subject. A different audience rejects the token. The upstream must trust the configured public key, issuer and audience. This is fixture-qualified delegated signing, not a general identity-provider discovery service.

Tool calls show bounded decisions for registered-server security, personal-grant permission and delegated signing. They do not store signed tokens or their claims. Portable configuration includes issuer and audience but excludes signing material. Install the signing key locally before importing delegation metadata to another installation. Revoking the personal grant, revoking caller authority or disabling the registered server prevents new execution. An empty delegated issuer in the console disables signing, while existing execution history remains readable.

## Entra on-behalf-of grants

The Entra mode uses a verified user request to obtain the registered server's
access token. Configure the tenant issuer as a workload issuer, including its
JWKS, accepted middle-tier audience, project mapping and end-user claim. Then
set `oauth.entra_obo.issuer` to that exact issuer, `oauth.client_id` to the
middle-tier client, and `oauth.audience` to the downstream resource. Every
configured scope must start with that resource audience and a slash, such as
`api://<resource>/.default`. Set the declared token endpoint and supply the
write-only `entra_client_secret` once through the management API or Personal
tool grants console. The installation seals this dedicated credential
separately from grant tokens. Later updates can omit it only while the issuer,
middle-tier client and token endpoint stay unchanged. Changing any of those
requires an explicit replacement credential. Configuration export includes the
trust profile but excludes the client secret. Before importing a changed
credential boundary, install that profile and its replacement secret through
the management API or console.

The incoming assertion must pass the current workload issuer's signature,
expiry, mapping and authority checks. It must have the middle-tier audience and
nonempty delegated `scp` claims. Tokens marked `idtyp: app`, generic workload
tokens without delegated scopes, API keys and end-user header labels cannot
start the exchange. Entra mode and local delegated JWT signing are mutually
exclusive because both supply the downstream Authorization header.

The first eligible request exchanges its original, transient assertion at the
declared token endpoint using the standard JWT bearer on-behalf-of parameters.
Redirects are refused. The token and optional refresh token are sealed in the
existing per-user grant store and bind the server, project, endpoint, client,
resource, scopes and verified individual. Concurrent refresh uses the existing
grant lock and attempt fence. Without a refresh token, renewal requires a new
verified user request under the same lock. Revoked grants stay revoked and
never fall back to a service credential. Browser and device enrollment do not
apply to Entra mode.

The assertion is carried only in the current request and is never stored with
sessions, grants, attempts or configuration. Operator views show only bounded
`agent_365` decisions and grant standing. JSON and MCP consumers share these
checks. The local Entra-shaped fixture covers delegated versus application
identity, audience and issuer refusal, user isolation, refresh, revocation,
redirect refusal and secret exclusion. It is not live tenant qualification.

Protocol references, checked 2026-10-28, are the Microsoft
[on-behalf-of flow](https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-on-behalf-of-flow)
and [access token claims](https://learn.microsoft.com/en-us/entra/identity-platform/access-token-claims-reference).

## Experimental stdio executables

Stdio registration is experimental and uses the existing unconfined executable
contract. An operator installs a native executable in the directory named by
`OLP_UNCONFINED_PLUGIN_DIR` on every serving node. The process runs with the
operating system privileges of the gateway, can read its files and can reach
networks directly. Its own network access is outside the HTTP egress policy.
Use only executables trusted as much as the gateway itself.

An owner opens **Plugins**, then **Unconfined MCP executables**, reviews the
installed file's SHA-256 digest, acknowledges the native execution risk and
confirms their identity. Review reads metadata without starting the executable.
Permission applies to the exact executable name and digest. The corresponding
management operations are `GET /api/v1/mcp-executables`, followed by POST to
`/api/v1/mcp-executables/{executable}/review` and `/permit`. Permission requires
an owner browser session, CSRF protection and one-use recent authentication for
`plugin_permit`. A project registration cannot grant native execution permission.

Register the approved executable with `transport: "stdio"` and a `stdio` object
containing `executable` and `digest`. Omit the HTTP endpoint, credential and OAuth
configuration. For example, the registration body can be:

```json
{
  "name": "Installed tools",
  "project_id": "<project UUID>",
  "transport": "stdio",
  "stdio": {
    "executable": "installed-tools",
    "digest": "<reviewed SHA-256 digest>"
  },
  "enabled": true,
  "unit_price": "0"
}
```

The registered transport and executable binding are immutable. Certification
starts a sealed copy of the approved build and checks its MCP catalog before a
toolset can pin it. Subsequent calls recheck permission, the installed digest
and the catalog. Replacing a binary, revoking its permission or changing its
catalog refuses new dispatches. Revoke the permission in the console or with
`DELETE /api/v1/mcp-executables/{executable}/permissions/{digest}`.

Each operation gets an isolated subprocess with no extra command arguments, an
empty environment and the operator's executable directory as its working
directory. Stdin and stdout carry bounded MCP messages. Stderr is discarded and
is not a diagnostic persistence channel. Processes and their process groups
are stopped when the operation completes, times out or is canceled. Consumer
requests cannot select a command, executable, path, environment, working
directory or transport. Those names remain available as ordinary properties
inside a tool's schema-defined `arguments` object.

Published stdio tools use the same JSON and MCP consumer paths, authority,
guardrails, pinned prices, limits, call identifiers and durable accounting as
HTTP tools. A process exit, oversized reply or cancellation after possible
dispatch retains an unknown outcome and its exposure. Reusing that call ID does
not run the executable again. Configuration export includes the pinned name and
digest, but omits owner permissions; importing a document cannot approve code.

Local fixture qualification builds and runs an actual installed executable,
checks owner permission and digest refusal, calls through both consumer
surfaces, and covers catalog drift, isolated callers, concurrency, settlement,
process termination and retained unknown outcomes. This evidence qualifies the
implemented experimental contract, not a sandbox or arbitrary third-party
executable.

Tool results support text, image and audio content plus object-valued structured
content. Other content variants and unsupported result fields are refused
explicitly. A malformed or unsupported result after dispatch retains an
`outcome_unknown` attempt and its provisional debit, because the tool may have
completed even though its result could not be accepted. Empty content arrays
and empty text are valid results.
