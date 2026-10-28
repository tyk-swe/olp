# Upstream MCP server registration

Register project-owned upstream servers through `/api/v1/mcp-servers`. This API
is separate from the [generated management MCP endpoint](../operate/automation.md#management-mcp).
Reads require management `read` and project visibility; mutations require
`configure` and project-change authority. Installation-wide configuration
promotion retains those checks. Up to 128 active registrations are permitted.

Create requires a project UUID, name, `transport: streamable_http`, endpoint and
explicit enabled flag. PUT replaces editable metadata with an observed If-Match;
project and transport are immutable. Every mutation requires Idempotency-Key.
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
state, reviewed catalog digest and an optional deterministic destination binding.
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
toolset against the digest recorded in its pinned server revision, and each
JSON tool call certifies its server the same way before anything is
dispatched. Certification only lists tools and never invokes one. When the
live definitions differ from the pinned ones, calls to that server refuse with
a `tool_definition_changed` error and nothing is dispatched or settled. The
same refusal applies when a server has since been certified into a newer
revision than the one the toolset pins. The published toolset and the retained
server revision stay readable through the management API while calls are
blocked, so the change can be reviewed and a fresh revision published on
purpose. Publishing a reviewed revision that pins the current catalog
unblocks new JSON calls and newly opened sessions, and it leaves toolsets
pinned to other servers alone. A session opened before the re-publish stays
bound to the revision it started with.

A `notifications/tools/list_changed` notification seen on an upstream response
stream invalidates that server certification for the session, and the next
call to the server certifies again before it can dispatch. Calls on an
already certified session do not rediscover the catalog. Certification
compares tool definitions at the moment it runs, and it does not attest how
the upstream implementation behaves between checks.

Revoking a key, removing a toolset from its allowlist, retiring a publication
and disabling or retiring a server all refuse new calls and end live session
eligibility once the gateway authority view refreshes, which is the same
freshness boundary the rest of the gateway enforces. A session that loses its
authority this way is closed, while a temporary authority outage only refuses
requests until authority is readable again.
