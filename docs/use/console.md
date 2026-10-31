# Console

The console is the browser interface at an installation's public origin. It
manages providers, routes, access, usage and installation settings through the
management API. Console actions use the signed-in member's permissions, project
visibility, ETag concurrency rules and audit handling, so the interface never
grants authority that the API would refuse.
[Getting Started](../getting-started.md) covers first-owner setup and the first
provider workflow.

## Notifications

The Notifications page under Access manages destinations, event rules and
delivery status. Channel-specific secrets are write-only and are entered again
only when they need to be replaced. Viewers see read-only rows. Project managers
see events only for projects they are allowed to manage. Destination and
delivery contracts are defined in
[Observability](../operate/observability.md#notifications).

## Observability

The Observability page under Settings manages export sinks and owner-enabled
payload capture. A sink's transport, stream set, format and project scope are
fixed when it is created. Each row shows its settings, delivery counters, safe
error information and lag. FOCUS sinks expand into revisioned datasets. Formats,
credentials, filters and delivery behavior are covered in
[Observability](../operate/observability.md).

Capture controls select the sampling rule, captured fields, scope, byte bounds
and ordered local redactors. Rows display their immutable revision pins. An
unrelated edit preserves those pins, while **Refresh pinned revisions on save**
deliberately adopts the current active revisions. An empty redactor list is
labelled unredacted so the risk is visible before saving. A row can still be
disabled while the owner-level capture switch is off. The storage and failure
boundary is described under
[payload capture](../operate/observability.md#payload-capture).

## Usage and requests

Usage can filter by project or session and refuses conflicting non-session
attribution. Breakdowns and CSV exports retain the visible filters and current
axes. Request detail links a request's session attribution back to the matching
filtered history. Summary and breakdown views show consumer charge beside
provider cost and include the month's count of attempts that could not be
charged.

The cost calculator either bounds a request against a published route or prices
an existing usage record. Route mode simulates each target and lists its minimum
and maximum bounds or exclusion reason. Usage mode prices an attempt's token,
media and tool usage with the provider price, discount, commitment and
applicable rate card. It returns the recorded components and provenance without
writing or retaining a new accounting record.

The reconciliation panel compares attributed spend with imported provider
invoices by UTC day. Its visible range is clamped to 93 days and shows
attributed cost, invoiced cost, capture rate, unpriced share, provisional marks
and any unavailable-data reason. It also gives covered-day totals for each
provider. An OpenAI, Anthropic or Azure OpenAI provider detail page has a billing import
section for its admin key and scope, on-demand import and removal. Read-only
roles can inspect import status but cannot change it. See
[FinOps](../operate/finops.md) for the import and reconciliation model.

## Installation identity

The Installation identity page under Settings edits the local display name and
logo. Changes require installation-wide settings authority and the observed
ETag. A logo must be a validated PNG or JPEG no larger than 64 KiB and 512 by
512 pixels. Branding does not change API endpoints or portable configuration.
Generated CLI and MCP branding operations use the same authority, validation and
concurrency rules.

## Independent installations

The installation label in the header opens a bookmark menu for other OLP
consoles. A bookmark holds only a display name and origin. HTTPS is required
outside loopback development, at most 20 bookmarks are retained, and they live
in the current browser's local storage. OLP makes no request to a bookmarked
installation until the user selects it.

Selecting a bookmark navigates to that installation's console. Its own browser
session applies, or its sign-in page opens when no session exists. Use a
distinct hostname for every installation because browser cookies are shared
across ports on the same hostname. The switcher rejects a bookmark that would
share a hostname with another installation. Separate subdomains work with OLP's
host-only session cookies.

Independent installations keep separate databases, authentication keys, master
keys and Valkey namespaces. The switcher does not merge data or copy credentials
between them. Regional gateways that deliberately share one installation and
PostgreSQL primary are a different topology and are covered in
[Deployment](../operate/deployment.md#regional-fleets).

## Bulk member editing

The Members page under Access can apply role and active-state edits across the
current page. Each member updates sequentially under its observed ETag, and the
acting member is excluded. Deactivation requires confirmation and revokes
browser sessions without revoking API keys. Successful rows remain committed if
another row fails. Failed rows stay selected so the operator can review fresh
data before retrying. Last-owner and external-ownership checks still apply to
each member.

## Saved views

Usage and Request Explorer saved views retain filters, with Usage also retaining
its time selection and axes. They do not retain cursors or unrelated parameters.
Each page allows at most twenty views in tab session storage, scoped to the
verified session public ID. Starting a new session retires views from the
earlier session. A saved view never bypasses current permissions or retention,
and each origin keeps separate browser storage.

The browser-qualified [branding example](../assets/operator-identity.png) and
[installation and saved-view example](../assets/independent-installations.png)
show the retained console layouts.

## Model catalog

The Catalog page and `GET /api/v1/catalog` describe the published route names
available to the caller. Installation members and read-scoped management tokens
see their permitted projects. Consumer API keys need `models_read` and see only
routes admitted by their current key authority. Revoked keys and authority older
than 60 seconds receive the same refusal as ordinary model discovery.

Each entry lists supported operations, common context and output limits, input
and output modalities, privacy declarations, current decimal price ranges and
Python SDK examples where a certified unary example is available. Unknown facts
remain explicit rather than being filled with estimates. Prices preserve decimal
precision, summarize enabled non-shadow targets and mark incomplete target
coverage.

Examples use the route name and read `OLP_API_KEY` from the environment.
Generation examples cover eligible OpenAI, Anthropic and Gemini interfaces. A
strict route uses its pinned native dialect, including Responses rather than
Chat Completions where required. Embeddings and token counting show eligible
native interfaces. Streaming-only and uncertified choices do not receive a unary
example. SDK code regions support keyboard focus and scrolling.

Provider names, endpoints and upstream model names stay private by default.
Under Catalog disclosure settings, an operator can select a published route and
explicitly enable upstream model names. That choice affects both authenticated
and public catalogs, while provider identities and credentials remain private.

Installation owners can enable an anonymous catalog from the Public model
catalog controls under Project policies. The public page is
`/catalog/public/{project_id}`, and `GET /api/v1/catalog/public/{project_id}`
returns only that project's published routes. Publishing prices is a separate
choice. Anonymous responses include `prices_visible` so clients can distinguish
withheld pricing from a missing current price declaration. Disabled projects
return 404. Publication and disclosure changes use observed ETags, ordinary
authorization and audit handling.

Configuration exports preserve `public_catalog` and `public_catalog_prices` on
projects and `expose_upstream_models` on routes. Changing publication through
configuration promotion requires installation access authority. Imported route
disclosure is staged with its draft and takes effect when that draft is
activated. A normal draft activation preserves the route's existing disclosure
choice.

The browser-qualified [catalog example](../assets/model-catalog.png) shows the
resulting authenticated catalog.
