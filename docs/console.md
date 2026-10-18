# Console

The management console is a client-only SvelteKit app under `console/`; this
page lists the observability controls it exposes.

## Notifications

The notification panel on the Access page manages destinations and rules.
Destination types are webhook, Slack, Microsoft Teams, Discord, PagerDuty, and
email; each asks for the channel-specific origin/endpoint and its sealed
credential (a webhook URL, a routing key, an SMTP `{username,password}` pair,
or an optional HMAC signing secret). Stored secrets are never displayed —
re-enter only when replacing them; a required-secret channel must be disabled
before its credential can be cleared. Rules cover all notification events;
event-specific configuration fields match the backend names (threshold,
recovery threshold, window and cooldown seconds, minimum samples, lead days,
metric, period, lead time), and empty fields take the backend defaults.
Project-scoped events follow the backend's scope rules —
installation-only events are unavailable to project managers; viewers see
read-only rows. The deliveries table shows status, attempts, last error code,
and timing.

## Observability

The observability panel on Settings manages export sinks and payload capture.
Sinks create/update/enable/delete with the legal stream/format matrix — HTTPS
sinks send JSON or CloudEvents, OTLP log sinks send OTLP, object-store sinks
send JSONL or FOCUS 1.4 gzip CSV, and Stripe meter sinks send meter events —
with destination, project scope, route and outcome filters, and an optional
sealed credential document (transport headers and workload-identity material
travel inside it; it is never read back). Format-specific settings render as
their own controls: a FOCUS sink asks for cost basis (immutable after
creation) and an optional start date, a CloudEvents sink an optional source,
and a Stripe sink an event name, metered value, project-to-customer map and
optional payload keys with a restricted-key password field. Each row summarizes
its settings, and a FOCUS sink expands a datasets view of charge dates,
revisions, delivery state and status. A sink's type, stream set, format
and project scope are fixed at creation; edits cover name, destination,
per-event filters and credential replacement or clearing only. Per-stream
status rows show pending counts, bounded pending error categories (status,
timeout, dial failures — never response bodies), delivered/failed totals,
gap counts and last success/attempt timestamps plus approximate lag.

Policy project and route scopes are fixed at creation; edits cover the sink,
sample ratio, include set, selectors and byte limits under ETag. A separate
row-level Disable posts only `{enabled:false}` — it is the one mutation allowed
while the owner master switch is off, so an active policy can always be narrowed
without widening capture. The capture
section toggles the owner-only installation switch, then lists metadata-only
sink options for policies — sink id, name, and type, never destinations or
credentials — and creates project policies with route, exact-string sample
ratio, include sets (input/output/tool_calls), key and end-user selectors,
byte limits, and enable/disable, all under ETag concurrency. Ordered local
redactor selection shows immutable revision pins. Unrelated edits preserve them;
**Refresh pinned revisions on save** explicitly adopts current active revisions.
Empty lists are labelled unredacted; see [capture](payload-capture.md).

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
it; read-only roles see status only. See [finops](finops.md).

Creates own stable mutation ID plus submitted payload/ETag through ambiguous
retry; changed intent gets a new ID. Confirmed writes never repeat on refresh
failure. Session/resource/tab fences reject stale callbacks and clear drafts/
secrets on abandonment/sign-out/unmount. Saving row drafts cannot be replaced
mid-flight. Grant polling/continuation is shared across onboarding/pools/code mode;
one-use pasted input clears on submission, including failures, never persists.
