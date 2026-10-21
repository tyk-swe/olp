# Console

Use the console at the installation’s public origin to manage providers, routes,
access, usage and settings. Every action uses the management API’s permissions,
session, ETag and audit rules. Begin with [Getting Started](../getting-started.md).

## Notifications

Access → Notifications manages destinations, event rules and delivery status.
Channel-specific secrets are write-only; re-enter them only to replace them.
Viewers see read-only rows, and project managers see only permitted project events.
See [notification configuration](../operate/observability.md#notifications).

## Observability

Settings → Observability manages export sinks and owner-enabled payload capture.
Sink transport, stream set, format and project scope are fixed at creation;
rows show settings, delivery counters, safe errors and lag. FOCUS sinks expand
into revisioned datasets. See [observability](../operate/observability.md) for
formats, credentials, filters and delivery contracts.

Capture controls select sampling, fields, scope, byte bounds and ordered local
redactors. Rows show immutable revision pins; unrelated edits preserve them and
**Refresh pinned revisions on save** adopts current active revisions. Empty lists
are labelled unredacted. Row-level Disable remains available while the owner
master switch is off. See [payload capture](../operate/observability.md#payload-capture).

## Usage and requests

Usage filters project/session, refusing conflicting non-session attribution;
breakdowns and CSV exports retain visible filters/current axes. Request detail
links session attribution to filtered history. Summary and breakdown show
consumer charge beside cost and the month's uncharged-attempt count.

The **cost calculator** bounds a published route's request or prices a usage
record exactly as accounting would record it. *Route* mode simulates and lists
every target's minimum/maximum bound and exclusion reason. *Usage* mode prices
an attempt's token/media/tool usage against a provider's price, discount,
commitment and the applicable rate card, including the recorded components and
provenance; nothing is written or retained.

The **reconciliation panel** compares attributed spend with imported provider
invoices per UTC day for the page's range — clamped to 93 days — showing
attributed and invoiced cost, capture rate, unpriced share, provisional marks
and the unavailable reason, plus per-provider covered-day totals. An OpenAI or
Anthropic provider's detail page carries a **billing import** section for
configuring its admin key and scope, running an import on demand and removing
it; read-only roles see status only. See [finops](../operate/finops.md).

## Installation identity

Settings → Installation identity edits the local name/logo under installation-wide
settings authority and observed ETag. PNG/JPEG logos are validated, ≤64 KiB and
512 × 512 pixels; theme/accessibility stay unchanged. Branding changes neither
API endpoints nor portable configuration. Generated CLI/MCP branding operations
use the same authority/validation/concurrency.

## Independent installations

The installation label in the header opens a bookmark menu. Add a name and an
HTTPS origin; HTTP is accepted on loopback for development. Bookmarks contain
only names and origins, are limited to 20 entries and are stored in this
browser's local storage. No request is made to a bookmarked installation until
you select it.

Selecting an installation navigates to its console. Its own browser session
applies, or its sign-in page opens. Use a distinct hostname for every
installation: browser cookies are shared across ports on the same hostname.
The switcher rejects bookmarks that would share a hostname with another
installation. Separate subdomains work with OLP's host-only session cookies.

Independent installations have their own databases, authentication keys, master
keys and Valkey namespaces. The switcher does not combine their data or copy
credentials. Regional gateways sharing one installation's PostgreSQL primary
are described in [deployment](../operate/deployment.md#regional-fleets).

## Bulk member editing

Access → Members applies sequential current-page role/active edits with each
observed ETag, excluding self. Deactivation confirms and revokes sessions, not
API keys. Successes remain committed; failed rows stay selected for reviewed
fresh-data retry. Last-owner and external-ownership checks apply per member.

## Saved views

Usage/Request Explorer save filters (Usage includes time/axes), not cursors or
unrelated parameters. At most twenty views/page live in tab session storage,
scoped to verified session public ID; a new session retires old views. They never
bypass current permissions/retention. Origins keep separate browser storage.

Browser-qualified examples: [branding](../assets/operator-identity.png)
and [installations/views](../assets/independent-installations.png).

## Model catalog

The console’s **Catalog** page and `GET /api/v1/catalog` describe published route names available to the caller. Installation members and read-scoped management tokens see their permitted projects. Consumer keys need `models_read` and see only the routes admitted by their current key authority. Revoked keys and authority older than 60 seconds receive the same refusal as ordinary model discovery.

Each entry includes operations, common context and output limits, input/output modalities, privacy declarations, current decimal price ranges, and Python SDK examples. Unknown facts remain explicit. Prices preserve decimal precision and summarize enabled non-shadow targets; incomplete target coverage is marked. Examples use the route name and `OLP_API_KEY` from the environment. Examples appear only for certified unary surfaces. Generation covers eligible OpenAI, Anthropic and Gemini interfaces; strict routes use the pinned native dialect, including Responses rather than Chat Completions where required. Embeddings and token counting show eligible native interfaces. Streaming-only or uncertified choices do not receive a unary example.

Provider names, endpoints and upstream model names remain private by default. Under **Catalog disclosure settings**, select a published route and explicitly enable upstream model names. This choice affects both authenticated and public catalogs. Provider identities and credentials remain private.

Installation owners can enable an anonymous catalog in **Project policies → Public model catalog**. `/catalog/public/{project_id}` provides the public page. `GET /api/v1/catalog/public/{project_id}` then lists only that project’s published routes. Publishing prices is a separate checkbox; anonymous responses include `prices_visible` so clients can distinguish withheld pricing from missing current declarations. Disabled projects return 404. Publication and disclosure changes use observed ETags and ordinary authorization/audit handling.

Configuration exports preserve `public_catalog` and `public_catalog_prices` on projects and `expose_upstream_models` on routes. Changing publication through promotion requires installation access authority. Imported route disclosure is staged with its draft and takes effect when that draft is activated; normal draft activations preserve the route’s existing disclosure choice.

See the browser-qualified [catalog example](../assets/model-catalog.png). SDK code regions support keyboard focus and scrolling.
