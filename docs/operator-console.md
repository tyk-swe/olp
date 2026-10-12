# Operator console

The console manages the installation at its own origin. Permissions, sessions,
ETags and audit events come from that installation's management API.

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
are described in [deployment](deployment.md#regional-fleets).

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

Browser-qualified examples: [branding](assets/screenshots/m11-operator-identity.png)
and [installations/views](assets/screenshots/m11-independent-installations.png).
