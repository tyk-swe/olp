# Upstream MCP server registration

Register project-owned upstream servers through `/api/v1/mcp-servers`. This API
is separate from the [generated management MCP endpoint](../operate/automation.md#management-mcp).
Reads require management `read` and project visibility; mutations require
`configure` and project-change authority. Installation-wide configuration
promotion retains those checks. Up to 128 active registrations are permitted.

Create requires a project UUID, name, `transport: streamable_http`, endpoint and
explicit enabled flag. PUT replaces editable metadata with an observed If-Match;
project and transport are immutable. Every mutation requires Idempotency-Key.
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

Certification supports MCP versions 2025-03-26, 2025-06-18 and 2025-11-25, JSON or
SSE responses on Streamable HTTP, negotiated session headers and bounded session
cleanup. Discovery admits at most 16 pages, 128 uniquely named tools and one MiB
of metadata. Individual descriptions/schemas and JSON depth/node counts are
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

## Calling published tools over Streamable HTTP

Callers with the `tools` scope and a toolset allowlist can also reach the same
published tools as the JSON tool endpoints at `POST /mcp`, using Streamable HTTP
at MCP 2025-03-26, 2025-06-18 or 2025-11-25. A session binds one toolset, chosen
with the `toolset_id` query parameter when the key may use more than one, and
stays bound to the installation, project, principal and pinned toolset and
server revisions it initialized against. Sessions expire after 15 idle minutes
and each key may hold at most 64 at once. Send the returned `Mcp-Session-Id`
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
