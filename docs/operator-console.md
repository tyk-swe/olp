# Operator console

The console manages the installation at its own origin. Permissions, sessions,
ETags and audit events come from that installation's management API.

## Installation identity

Settings → Installation identity edits the name and logo shown in the console.
An installation-wide member with the settings operation can save changes. The
form uses its observed ETag; a conflicting edit requires reloading the current
identity. Existing names and the default OpenLLMProxy mark remain available.

Logos are embedded PNG or JPEG images, limited to 64 KiB and 512 × 512 pixels.
OLP validates the image before storing it. The console retains its dark theme,
semantic colors, readable text and reduced-motion behavior. A logo does not
change the management API or inference endpoints. Branding is local to the
installation and is not part of configuration promotion.

The generated CLI and management MCP expose `get_installation_branding` and
`update_installation_branding` with the same authority, validation and ETag rules.

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

Access → Members supports selecting members on the current page and changing
their role, deactivating them or reactivating them. Your own account cannot be
selected. Deactivation asks for confirmation because it revokes sessions.

Each selected member is updated through the ordinary management API using its
observed ETag. Updates run sequentially and report successful and failed members
separately. A stale or protected member does not roll back an earlier successful
update. Failed members stay selected so you can review them and retry with fresh
data. Server checks, including last-owner protection and externally managed
identity restrictions, apply to every update. Review attributed API keys after
deactivation; those keys are not automatically revoked.

## Saved views

Usage and Request Explorer offer Saved views. Name the current filters, apply a
saved view or delete it. Usage views keep the selected time window and axes;
history views keep their metadata filters. Paging cursors and unrelated query
parameters are excluded.

Views are limited to 20 per page, stored in browser session storage and scoped to
the verified management session's public identifier. They survive reloads in
that tab. A new session, including signing in as another member, starts without
the earlier session's views and retires its stored presets. Installation origins
have separate browser storage. Saved views never bypass current permissions or
request-retention rules.
