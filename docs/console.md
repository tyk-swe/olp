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
sinks send JSON, OTLP log sinks send OTLP, object-store sinks send JSONL —
with destination, project scope, route and outcome filters, and an optional
sealed credential document (transport headers and workload-identity material
travel inside it; it is never read back). A sink's type, stream set, format
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
byte limits, and enable/disable, all under ETag concurrency.

## Usage and requests

Usage accepts project and session filters; a session filter conflicts with an
explicit non-session attribution key. The breakdown selector includes project
and session dimensions. Both usage and request exploration export the visible
filters as CSV (usage exports under the current dimension); downloads run
through the shared `downloadBlob` helper. Request detail shows the session
attribution as a link back to the request list filtered on that session.
