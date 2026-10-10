# Upstream MCP server registration

Register project-owned upstream servers through `/api/v1/mcp-servers`. This API
is separate from the [generated management MCP endpoint](operator-cli.md#management-mcp).
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
Omission retains it, null removes it. The installation key ring seals it under
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
